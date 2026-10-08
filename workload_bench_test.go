package acp

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/sirupsen/logrus"
)

func BenchmarkCopyWorkload(b *testing.B) {
	// Keep disk regressions alongside YATM-shaped linear workloads in one shared harness.
	for _, fixture := range []struct {
		name                 string
		files, size, targets int
		linear               bool
		mode                 ReadMode
		mixed                bool
	}{
		{"LargeOneTarget", 1, 64 << 20, 1, false, ReadBuffered, false},
		{"LargeThreeTargets", 1, 64 << 20, 3, false, ReadBuffered, false},
		{"SmallOneTarget", 512, 4096, 1, false, ReadBuffered, false},
		{"SmallThreeTargets", 512, 4096, 3, false, ReadBuffered, false},
		{"LinearSmallBuffered", 256, 256 << 10, 1, true, ReadBuffered, false},
		{"LinearSmallMapped", 256, 256 << 10, 1, true, ReadMapped, false},
		{"LinearLargeBuffered", 1, 64 << 20, 1, true, ReadBuffered, false},
		{"LinearLargeMapped", 1, 64 << 20, 1, true, ReadMapped, false},
		{"LinearMixedBuffered", 132, 8 << 20, 1, true, ReadBuffered, true},
		{"LinearMixedMapped", 132, 8 << 20, 1, true, ReadMapped, true},
	} {
		b.Run(fixture.name, func(b *testing.B) {
			// Build identical real-file fixtures outside the timer; all destinations use the same filesystem.
			root := b.TempDir()
			sources := make([]string, fixture.files)
			content := make([]byte, fixture.size)
			for index := range content {
				content[index] = byte(index*31 + 7)
			}
			var total int64
			for index := range sources {
				sources[index] = filepath.Join(root, fmt.Sprintf("source-%04d", index))
				size := fixture.size
				if fixture.mixed && index%33 != 32 {
					size = 256 << 10
				}
				total += int64(size)
				if err := os.WriteFile(sources[index], content[:size], 0o600); err != nil {
					b.Fatal(err)
				}
			}

			// Match Archive hashing and target scheduling without changing ordinary disk cases.
			logger := logrus.New()
			logger.SetOutput(io.Discard)
			options := []Option{WithHashPolicy(HashRead), WithLogger(logger)}
			if fixture.linear {
				options = []Option{WithHashPolicy(HashReadRefresh), WithLogger(logger),
					SetToDevice(LinearDevice(true)), SetFromDevice(WithReadMode(fixture.mode))}
			}
			b.SetBytes(total)
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				// Allocate a fresh destination tree, keeping fixture setup and deletion outside the measurement.
				b.StopTimer()
				targetRoot, err := os.MkdirTemp(root, "targets-")
				if err != nil {
					b.Fatal(err)
				}
				items := make([]Item, 0, len(sources))
				for index, source := range sources {
					targets := make([]string, fixture.targets)
					for target := range targets {
						targets[target] = filepath.Join(targetRoot, fmt.Sprintf("target-%d", target), fmt.Sprintf("%04d", index))
					}
					items = append(items, &SimpleJob{Path: source, Dsts: targets})
				}

				// Fail measured runs on any item or target error.
				completed := 0
				onResults := func(results []Result) error {
					for _, result := range results {
						if result.Err != nil {
							return result.Err
						}
						for _, target := range result.Targets {
							if target.Err != nil {
								return target.Err
							}
						}
						completed++
					}
					return nil
				}

				// Measure the whole copy lifecycle, including hashing, sync, metadata and result delivery.
				b.StartTimer()
				err = runStream(context.Background(), onResults, items, options...)
				b.StopTimer()

				// Remove only this iteration's destinations after checking complete delivery.
				if err != nil || completed != fixture.files {
					b.Fatalf("copy: completed=%d err=%v", completed, err)
				}
				if err := os.RemoveAll(targetRoot); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
			}
		})
	}
}
