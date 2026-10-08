package acp

import (
	"bytes"
	"errors"
	"io"
	"syscall"
	"testing"
	"testing/synctest"
	"time"
)

func (m *ltfsModel) motion(state string, rate int64) {
	// The physical server owns motion; snapshots retain each meaningful state transition.
	m.lock.Lock()
	defer m.lock.Unlock()
	if m.state == state && len(m.motions) > 0 && m.motions[len(m.motions)-1].rate == rate {
		return
	}
	m.state = state
	m.signal()
	m.motions = append(m.motions, tapeMotion{time.Since(m.started), state, rate})
}

func (m *ltfsModel) serveDrive() {
	// Physical consumption uses buffered bytes and elapsed time; adaptive control changes only the bounded tape speed.
	defer close(m.driveDone)
	var deadline time.Time
	streaming, ran := false, false
	for {
		m.lock.Lock()
		for len(m.driveRequests) == 0 && !m.backendDone {
			changed := m.changed
			m.lock.Unlock()
			<-changed
			m.lock.Lock()
		}
		if len(m.driveRequests) == 0 {
			m.lock.Unlock()
			break
		}
		r := m.driveRequests[0]
		m.driveRequests[0] = nil
		m.driveRequests = m.driveRequests[1:]
		position := m.position
		m.lock.Unlock()

		// A positioning command occupies the same physical device and never overlaps data service.
		delay := time.Duration(0)
		if r.position != position {
			m.motion("locating", 0)
			delay += m.config.seek
			if m.config.seekCost != nil {
				delay += m.config.seekCost(position, r.position)
			}
			m.seeks++
			streaming = false
		}
		if !streaming && !r.read && m.config.resumeBytes > 0 {
			m.motion("stopped", 0)
			m.lock.Lock()
			for m.drivePending < m.config.resumeBytes && !m.finalizing && m.failure == nil {
				changed := m.changed
				m.lock.Unlock()
				<-changed
				m.lock.Lock()
			}
			m.lock.Unlock()
		}
		if !ran {
			delay += m.config.startup
		}
		if !streaming {
			m.motion("starting", 0)
			deadline = time.Now()
		}

		// One native byte is charged once; the configured rate never follows producer throughput.
		rate := m.matchSpeed(time.Now())
		m.motion("streaming", rate)
		if rate > 0 {
			delay += time.Duration(r.size * int64(time.Second) / rate)
		}
		m.lock.Lock()
		failure := m.failure
		m.lock.Unlock()
		if m.config.service != nil {
			failure = errors.Join(failure, m.config.service(*r))
		}
		if !r.read && m.config.capacity > 0 && r.position+r.size > m.config.capacity {
			failure = errors.Join(failure, syscall.ENOSPC)
		}
		if r.read && r.queued.After(deadline) {
			deadline = r.queued
		}
		deadline = deadline.Add(delay)
		if wait := time.Until(deadline); wait > 0 {
			time.Sleep(wait)
		}
		finished := time.Now()
		m.lock.Lock()
		m.busy += delay
		m.overshoot = max(m.overshoot, max(time.Duration(0), finished.Sub(deadline)))
		if failure == nil {
			m.position = r.position + r.size
			m.completed += r.size
			if !r.read {
				m.extents = append(m.extents, tapeExtent{r.path, r.position, r.size})
			}
		} else {
			m.failure = failure
			m.failedBytes += r.size
		}
		if !r.read {
			m.drivePending -= r.size
		}
		streaming, ran = r.read || m.drivePending > 0, true
		r.err = failure
		m.curve = append(m.curve, tapeSample{host: m.pending, drive: m.drivePending, elapsed: time.Since(m.started), bytes: m.completed, position: m.position, pending: m.pending + m.drivePending})
		starved := !r.read && m.drivePending == 0 && !m.finalizing && failure == nil && rate > 0
		if starved {
			m.backhitches++
			m.state = "backhitch"
			m.motions = append(m.motions, tapeMotion{time.Since(m.started), "backhitch", 0})
		}
		close(r.done)
		m.signal()
		m.lock.Unlock()

		// Empty means stopped: reverse/reposition time lets the producer refill the independent buffer.
		if starved {
			if m.config.backhitch > 0 {
				time.Sleep(m.config.backhitch)
			}
			m.lock.Lock()
			m.busy += m.config.backhitch
			m.state = "stopped"
			m.signal()
			m.lock.Unlock()
		}
	}
	m.motion("stopped", 0)
}

func tapeProfile(stress bool) ltfsModelConfig {
	// Native values come from the HP LTO5 family manual; hysteresis and motion costs are assumptions.
	minimum := int64(47_000_000)
	if stress {
		minimum = 100_000_000
	}
	return ltfsModelConfig{adaptive: true, dispatchers: 1, bufferBytes: 1024 << 20, driveBufferBytes: 256_000_000, minimumRate: minimum, bytesPerSecond: 140_000_000, resumeBytes: 153_600_000, backhitch: 2500 * time.Millisecond}
}

func TestMockTapeCloseAcceptsButFinalizeDrains(t *testing.T) {
	// A short file fits in the drive buffer without reaching the streaming restart watermark.
	synctest.Test(t, func(t *testing.T) {
		m := newLTFSModel(ltfsModelConfig{dispatchers: 1, bufferBytes: 4 << 20, driveBufferBytes: 4 << 20, minimumRate: 100_000_000, bytesPerSecond: 140_000_000, resumeBytes: 2 << 20})
		out := &ltfsOutput{model: m, path: "file"}
		if _, err := out.Write(make([]byte, 1<<20)); err != nil {
			t.Fatal(err)
		}
		if err := out.Close(); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if m.completed != 0 || m.drivePending != 1<<20 || m.pending != 0 {
			t.Fatalf("Close conflated acceptance and durability: completed=%d drive=%d host=%d", m.completed, m.drivePending, m.pending)
		}

		// Finalization permits the final tail and joins actual physical service exactly once.
		if err := m.validate(); err != nil {
			t.Fatal(err)
		}
		m.shutdown()
		if m.accepted != 1<<20 || m.completed != 1<<20 || len(m.extents) != 1 || m.extents[0].position != 0 || m.backhitches != 0 {
			t.Fatalf("tape accounting: %+v", m)
		}
		if m.busy < time.Duration((1<<20)*int64(time.Second)/140_000_000) {
			t.Fatal("physical bytes escaped the speed ceiling")
		}
	})
}

func TestMockTapeSubMinimumSupplyCausesBackhitch(t *testing.T) {
	// A 256 MB drive buffer absorbs temporary differences; sustained 80 MB/s eventually starves a fixed 100 MB/s consumer.
	for _, stress := range []bool{false, true} {
		t.Run(map[bool]string{false: "native47", true: "stress100"}[stress], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				config := tapeProfile(stress)
				config.adaptive = false // Fixed-rate control proves the byte ledger independently of speed matching.
				config.bufferBytes = 1024 << 20
				config.backhitch = 2500 * time.Millisecond // Explicit assumed reposition cost, independent of producer timing.
				m := newLTFSModel(config)
				out := &ltfsOutput{model: m, path: "stream"}
				chunk := make([]byte, 256<<10)
				for range 8192 {
					if _, err := out.Write(chunk); err != nil {
						t.Fatal(err)
					}
					time.Sleep(time.Duration(int64(len(chunk)) * int64(time.Second) / 80_000_000))
				}
				if err := out.Close(); err != nil {
					t.Fatal(err)
				}
				if err := m.validate(); err != nil {
					t.Fatal(err)
				}
				m.shutdown()

				// A transient low feed sample is insufficient; actual buffer exhaustion drives the transition.
				if stress && m.backhitches == 0 {
					t.Fatal("100 MB/s floor did not expose sustained 80 MB/s supply")
				}
				if !stress && m.backhitches != 0 {
					t.Fatalf("fixed 47 MB/s consumption caused %d unexpected backhitches", m.backhitches)
				}
				for _, motion := range m.motions {
					if motion.state == "streaming" && (motion.rate < config.minimumRate || motion.rate > config.bytesPerSecond) {
						t.Fatalf("invalid tape speed: %+v", motion)
					}
				}
				t.Logf("minimum=%d MB/s accepted=%d backhitches=%d physical_busy=%s", config.minimumRate/1_000_000, m.accepted, m.backhitches, m.busy)
			})
		})
	}
}

func TestMockTapeFiniteBuffersBackpressureWrite(t *testing.T) {
	// Hold the physical drive while one MiB of drive space and two MiB of LTFS space fill.
	synctest.Test(t, func(t *testing.T) {
		gate := make(chan struct{})
		m := newLTFSModel(ltfsModelConfig{bufferBytes: 2 << 20, driveBufferBytes: 1 << 20, service: func(tapeRequest) error { <-gate; return nil }})
		out := &ltfsOutput{model: m, path: "bounded"}
		written := make(chan struct{})
		go func() { _, _ = out.Write(make([]byte, 4<<20)); close(written) }()
		synctest.Wait()
		select {
		case <-written:
			t.Fatal("Write exceeded finite combined caches")
		default:
		}
		if m.accepted != 3<<20 || m.pending != 2<<20 || m.drivePending != 1<<20 {
			t.Fatalf("host=%d drive=%d accepted=%d", m.pending, m.drivePending, m.accepted)
		}

		// A physical release creates space, and shutdown must conserve every accepted byte.
		close(gate)
		<-written
		if err := out.Close(); err != nil {
			t.Fatal(err)
		}
		if err := m.validate(); err != nil {
			t.Fatal(err)
		}
		m.shutdown()
	})
}

func TestMockTapeLocateAndEOM(t *testing.T) {
	// Logical order alone does not give metres; a test supplies the cost of a noncontiguous transition.
	synctest.Test(t, func(t *testing.T) {
		m := newLTFSModel(ltfsModelConfig{bytesPerSecond: 140_000_000, seekCost: func(from, to int64) time.Duration {
			if from != to {
				return 50 * time.Second
			}
			return 0
		}})
		for _, position := range []int64{0, 1 << 20, 20 << 20} {
			r := &ltfsReader{ReadCloser: io.NopCloser(bytes.NewReader(make([]byte, 1<<20))), model: m, path: "read", position: position}
			if _, err := io.Copy(io.Discard, r); err != nil {
				t.Fatal(err)
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
		}
		if err := m.validate(); err != nil {
			t.Fatal(err)
		}
		m.shutdown()
		if m.seeks != 1 || m.busy < 50*time.Second || m.backhitches != 0 {
			t.Fatalf("seeks=%d backhitches=%d busy=%s", m.seeks, m.backhitches, m.busy)
		}
	})

	// Device acceptance can precede a physical EOM; finalization must expose it with conserved failed bytes.
	synctest.Test(t, func(t *testing.T) {
		m := newLTFSModel(ltfsModelConfig{bufferBytes: 4 << 20, driveBufferBytes: 4 << 20, capacity: 1 << 20})
		out := &ltfsOutput{model: m, path: "eom"}
		for range 2 {
			_, _ = out.Write(make([]byte, 1<<20))
		}
		_ = out.Close()
		if err := m.finalize(); !errors.Is(err, syscall.ENOSPC) {
			t.Fatalf("physical EOM=%v", err)
		}
		m.shutdown()
		if m.accepted != m.completed+m.failedBytes || m.pending != 0 || m.drivePending != 0 {
			t.Fatalf("EOM lost bytes: %+v", m)
		}
	})
}

func TestMockTapeRefillIsNotLimitedByTicketQueue(t *testing.T) {
	// More than 1024 small backend commands may be needed to fill a byte-based restart watermark.
	synctest.Test(t, func(t *testing.T) {
		m := newLTFSModel(ltfsModelConfig{bufferBytes: 1 << 20, driveBufferBytes: 256 << 10, resumeBytes: 160 << 10, minimumRate: 100_000_000, bytesPerSecond: 140_000_000})
		out := &ltfsOutput{model: m, path: "small-records"}
		for range 1500 {
			if _, err := out.Write(make([]byte, 128)); err != nil {
				t.Fatal(err)
			}
		}
		if err := out.Close(); err != nil {
			t.Fatal(err)
		}
		if err := m.validate(); err != nil {
			t.Fatal(err)
		}
		m.shutdown()
		if m.completed != 1500*128 {
			t.Fatalf("drained=%d", m.completed)
		}
	})
}
