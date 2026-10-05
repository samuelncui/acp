package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samuelncui/acp"
)

// BenchmarkRewriteCheckpointSnapshots measures one durable state/report snapshot pair.
// Per-entry full snapshots still incur O(N²) total serialization as a queue grows; this
// benchmark measures that existing policy without rewriting source files or adding a journal.
// The fixture uses only stable persisted fields so the same harness can compare versions.
func BenchmarkRewriteCheckpointSnapshots(b *testing.B) {
	for _, count := range []int{100, 1000} {
		b.Run(fmt.Sprintf("entries=%d", count), func(b *testing.B) {
			// Fixed content and metadata keep both source versions on the same document shapes.
			root := b.TempDir()
			state := &rewriteState{Root: root, Pending: make([]rewriteEntry, 0, count)}
			jobs := make(map[string]*acp.Job, count)
			modified := time.Unix(1700000000, 0).UTC()
			for index := 0; index < count; index++ {
				name := fmt.Sprintf("file-%06d", index)
				path := filepath.Join(root, name)
				state.Pending = append(state.Pending, rewriteEntry{Path: path})
				jobs[path] = &acp.Job{
					Base: root, Path: []string{name}, FullPath: path, Status: acp.JobStatusFinished,
					SuccessTargets: []string{path}, Size: 4096, Mode: 0o640,
					ModTime: modified, WriteTime: modified, SHA256: strings.Repeat("a", 64),
				}
			}
			statePath := filepath.Join(root, "state.json")
			reportPath := filepath.Join(root, "report.json")

			// Every measured iteration uses the actual atomic writers and identical indentation.
			b.ReportAllocs()
			b.ResetTimer()
			for index := 0; index < b.N; index++ {
				if err := saveState(statePath, state); err != nil {
					b.Fatal(err)
				}
				if err := saveReport(reportPath, false, jobs, nil); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
