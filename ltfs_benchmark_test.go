package acp

import (
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func BenchmarkLTFSPipeline(b *testing.B) {
	// Each operation copies and physically finalizes 2 GiB; fixture creation is outside timing.
	const total = 2 << 30
	for _, stress := range []bool{false, true} {
		profile := "native47"
		if stress {
			profile = "stress100"
		}
		distributions := []string{"small", "large", "mixed"}
		if stress {
			distributions = append(distributions, "small-delayed-source", "medium-delayed-read")
		}
		for _, distribution := range distributions {
			for _, barrier := range []bool{true, false} {
				mode := "pipeline"
				if barrier {
					mode = "file-barrier-reference"
				}
				b.Run(profile+"/"+distribution+"/"+mode, func(b *testing.B) {
					// Reuse real source bytes; destinations cross the same filesystem seam as production.
					root := b.TempDir()
					sizes := []int{256 << 10}
					if distribution == "large" {
						sizes = []int{16 << 20}
					}
					if distribution == "mixed" {
						sizes = []int{256 << 10, 16 << 20}
					}
					if distribution == "medium-delayed-read" {
						sizes = []int{1 << 20}
					}
					paths := make(map[int]string)
					hashes := make(map[int][32]byte)
					for _, size := range sizes {
						content := make([]byte, size)
						state := uint64(0x987654321)
						for i := range content {
							state ^= state << 13
							state ^= state >> 7
							state ^= state << 17
							content[i] = byte(state)
						}
						path := filepath.Join(root, fmt.Sprintf("source-%d", size))
						if err := os.WriteFile(path, content, 0600); err != nil {
							b.Fatal(err)
						}
						paths[size], hashes[size] = path, sha256.Sum256(content)
					}
					var items []Item
					for remaining, index := total, 0; remaining > 0; index++ {
						size := sizes[index%len(sizes)]
						if size > remaining {
							size = sizes[0]
						}
						items = append(items, &SimpleJob{Path: paths[size], Dsts: []string{filepath.Join(root, fmt.Sprintf("target-%d", index))}})
						remaining -= size
					}
					b.ReportAllocs()
					b.SetBytes(total)
					b.ResetTimer()

					// The barrier control intentionally waits for each file result; it is not a historical ACP build.
					var backhitches int
					var hostPeak, drivePeak int64
					var busy, overshoot int64
					for iteration := 0; iteration < b.N; iteration++ {
						config := tapeProfile(stress)
						// Keep the full configured 1 GiB LTFS pool and nominal 256 MB drive buffer.
						m := newLTFSModel(config)
						settled := make(chan struct{}, 1)
						seen := 0
						var resultErr error
						opts := []Option{WithHashPolicy(HashReadRefresh), WithResultBatch(1), SetToDevice(LinearDevice(true))}
						if distribution == "medium-delayed-read" {
							opts = append(opts, SetFromDevice(DeviceThreads(4)))
						}
						c, err := NewStream(context.Background(), func(results []Result) error {
							for _, r := range results {
								seen++
								want := hashes[int(r.Size)]
								if r.Err != nil || len(r.Targets) != 1 || r.Targets[0].Err != nil || string(r.SHA256) != string(want[:]) {
									resultErr = fmt.Errorf("invalid benchmark result: %+v", r)
								}
								if barrier {
									settled <- struct{}{}
								}
							}
							return nil
						}, opts...)
						if err != nil {
							b.Fatal(err)
						}
						sourceFS := c.fs()
						if distribution == "small-delayed-source" {
							sourceFS = delayedSourceFilesystem{transferFilesystem: sourceFS, delay: 4 * time.Millisecond}
						}
						if distribution == "medium-delayed-read" {
							sourceFS = delayedSourceFilesystem{transferFilesystem: sourceFS, readDelay: 20 * time.Millisecond}
						}
						c.filesystem = ltfsFilesystem{transferFilesystem: sourceFS, model: m}
						for _, item := range items {
							if err := c.Submit(item); err != nil {
								b.Fatal(err)
							}
							if barrier {
								<-settled
							}
						}
						if err := c.Wait(); err != nil {
							b.Fatal(err)
						}
						if err := m.validate(); err != nil {
							b.Fatal(err)
						}
						m.shutdown()
						b.StopTimer()
						if resultErr != nil || seen != len(items) || m.completed != total || m.peakWrite != 1 {
							b.Fatalf("invalid measured operation: %v results=%d bytes=%d writers=%d", resultErr, seen, m.completed, m.peakWrite)
						}
						backhitches += m.backhitches
						hostPeak = max(hostPeak, m.peak)
						drivePeak = max(drivePeak, m.drivePeak)
						busy += int64(m.busy)
						overshoot = max(overshoot, int64(m.overshoot))
						if directory := os.Getenv("ACP_MOCK_REPORT_DIR"); directory != "" && iteration == 0 {
							writeTapeModelReport(b, directory, m)
						}
						b.StartTimer()
					}
					b.StopTimer()
					b.ReportMetric(float64(backhitches)/float64(b.N), "backhitches/op")
					b.ReportMetric(float64(hostPeak)/(1<<20), "host-peak-MiB")
					b.ReportMetric(float64(drivePeak)/(1<<20), "drive-peak-MiB")
					b.ReportMetric(float64(busy)/float64(b.N)/1e9, "physical-busy-s/op")
					b.ReportMetric(float64(overshoot)/1e6, "timer-overshoot-ms")
				})
			}
		}
	}
}

func writeTapeModelReport(b testing.TB, directory string, m *ltfsModel) {
	b.Helper()
	// Curves retain startup and final drain; every sample conserves host, drive and physical bytes.
	if err := os.MkdirAll(directory, 0755); err != nil {
		b.Fatal(err)
	}
	name := strings.ReplaceAll(b.Name(), "/", "_")
	file, err := os.Create(filepath.Join(directory, name+".csv"))
	if err != nil {
		b.Fatal(err)
	}
	writer := csv.NewWriter(file)
	_ = writer.Write([]string{"elapsed_seconds", "physical_bytes", "tape_position", "host_bytes", "drive_bytes"})
	for _, sample := range m.curve {
		_ = writer.Write([]string{strconv.FormatFloat(sample.elapsed.Seconds(), 'f', 9, 64), strconv.FormatInt(sample.bytes, 10), strconv.FormatInt(sample.position, 10), strconv.FormatInt(sample.host, 10), strconv.FormatInt(sample.drive, 10)})
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		b.Fatal(err)
	}
	if err := file.Close(); err != nil {
		b.Fatal(err)
	}

	// Record the controller inputs and decisions independently of physical completion samples.
	controlFile, err := os.Create(filepath.Join(directory, name+".control.csv"))
	if err != nil {
		b.Fatal(err)
	}
	controlWriter := csv.NewWriter(controlFile)
	_ = controlWriter.Write([]string{"elapsed_seconds", "drive_bytes", "estimated_input_Bps", "target_Bps", "actual_Bps"})
	for _, s := range m.controls {
		_ = controlWriter.Write([]string{strconv.FormatFloat(s.elapsed.Seconds(), 'f', 9, 64), strconv.FormatInt(s.buffer, 10), strconv.FormatFloat(s.feed, 'f', 3, 64), strconv.FormatFloat(s.target, 'f', 3, 64), strconv.FormatFloat(s.rate, 'f', 3, 64)})
	}
	controlWriter.Flush()
	if err := controlWriter.Error(); err != nil {
		b.Fatal(err)
	}
	if err := controlFile.Close(); err != nil {
		b.Fatal(err)
	}

	// Scenario parameters distinguish vendor facts and declared control/motion assumptions.
	info := map[string]any{"scenario": b.Name(), "bytes": m.completed, "minimum_bytes_per_second": m.config.minimumRate, "maximum_bytes_per_second": m.config.bytesPerSecond, "host_buffer_bytes": m.config.bufferBytes, "drive_buffer_bytes": m.config.driveBufferBytes, "restart_bytes": m.config.resumeBytes, "backhitch_seconds": m.config.backhitch.Seconds(), "initial_stream_bytes_per_second": m.config.streamRate, "adaptive": m.config.adaptive, "control_period_seconds": m.config.controlPeriod.Seconds(), "smoothing_seconds": m.config.smoothing.Seconds(), "watermark_fraction": m.config.targetFill, "buffer_correction_seconds": m.config.correction.Seconds(), "rate_slew_bytes_per_second_squared": m.config.slewPerSecond, "backhitches": m.backhitches, "seeks": m.seeks, "physical_busy_seconds": m.busy.Seconds(), "max_timer_overshoot_seconds": m.overshoot.Seconds(), "hash_policy": "HashReadRefresh", "source_fixture": "One reused native source per size; warm cache I/O, real content re-read and hashed", "note": "Single FUSE dispatcher; independent physical drain with adaptive control; full 256 MB drive buffer; motion and hysteresis are assumptions. File-barrier reference is a serialized control, not baseline ACP."}
	if strings.Contains(b.Name(), "small-delayed-source") {
		info["synthetic_source_open_seconds"] = 0.004
	}
	if strings.Contains(b.Name(), "medium-delayed-read") {
		info["synthetic_first_read_seconds"] = 0.020
		info["source_threads"] = 4
		info["source_file_bytes"] = 1 << 20
	}
	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, name+".json"), append(data, '\n'), 0644); err != nil {
		b.Fatal(err)
	}
}

// delayedSourceFilesystem isolates synthetic source latency from physical tape service.
type delayedSourceFilesystem struct {
	transferFilesystem
	delay, readDelay time.Duration
}

func (f delayedSourceFilesystem) Open(path string, mode ReadMode, info os.FileInfo) (itemSource, error) {
	// Open latency remains distinct from the first content read's latency.
	time.Sleep(f.delay)
	source, err := f.transferFilesystem.Open(path, mode, info)
	if err != nil {
		return source, err
	}
	if f.readDelay > 0 {
		source.reader = &firstReadDelayReader{ReadCloser: source.reader, delay: f.readDelay}
	}
	return source, nil
}

// firstReadDelayReader delays one real content Read, never EOF probes or later chunks.
type firstReadDelayReader struct {
	io.ReadCloser
	delay time.Duration
}

func (r *firstReadDelayReader) Read(p []byte) (int, error) {
	if len(p) > 0 && r.delay > 0 {
		time.Sleep(r.delay)
		r.delay = 0
	}
	return r.ReadCloser.Read(p)
}

func TestFirstReadDelayPreservesContentAndDelaysOnce(t *testing.T) {
	// The sensitivity delays content acquisition, without charging empty probes or EOF again.
	synctest.Test(t, func(t *testing.T) {
		reader := &firstReadDelayReader{ReadCloser: io.NopCloser(strings.NewReader("payload")), delay: 20 * time.Millisecond}
		defer reader.Close()
		start := time.Now()
		if n, err := reader.Read(nil); n != 0 || err != nil || time.Since(start) != 0 {
			t.Fatalf("empty probe changed: n=%d err=%v elapsed=%s", n, err, time.Since(start))
		}

		// Real source bytes and EOF semantics survive the single first-content latency.
		data, err := io.ReadAll(reader)
		if err != nil || string(data) != "payload" || time.Since(start) != 20*time.Millisecond {
			t.Fatalf("delayed content changed: data=%q err=%v elapsed=%s", data, err, time.Since(start))
		}
		if n, err := reader.Read(make([]byte, 1)); n != 0 || err != io.EOF || time.Since(start) != 20*time.Millisecond {
			t.Fatalf("EOF was delayed: n=%d err=%v elapsed=%s", n, err, time.Since(start))
		}
	})
}
