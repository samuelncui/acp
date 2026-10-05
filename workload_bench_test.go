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
	for _, fixture := range []struct {
		name                 string
		files, size, targets int
	}{
		{"LargeOneTarget", 1, 64 << 20, 1},
		{"LargeThreeTargets", 1, 64 << 20, 3},
		{"SmallOneTarget", 512, 4096, 1},
		{"SmallThreeTargets", 512, 4096, 3},
	} {
		b.Run(fixture.name, func(b *testing.B) {
			// Build identical real-file fixtures outside the timer; all destinations use the same filesystem.
			root := b.TempDir()
			sources := make([]string, fixture.files)
			content := make([]byte, fixture.size)
			for index := range content {
				content[index] = byte(index*31 + 7)
			}
			for index := range sources {
				sources[index] = filepath.Join(root, fmt.Sprintf("source-%04d", index))
				if err := os.WriteFile(sources[index], content, 0o600); err != nil {
					b.Fatal(err)
				}
			}
			logger := logrus.New()
			logger.SetOutput(io.Discard)
			b.SetBytes(int64(fixture.files) * int64(fixture.size))
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
				err = runStream(context.Background(), onResults, items, WithHashPolicy(HashRead), WithLogger(logger))
				b.StopTimer()
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
