//go:build linux

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestRewriteRejectsInvalidUTF8BeforeChangingFiles(t *testing.T) {
	for _, hardlink := range []bool{false, true} {
		name := "regular"
		if hardlink {
			name = "hardlink"
		}
		t.Run(name, func(t *testing.T) {
			// Linux can store two distinct names that ordinary JSON would collapse.
			root := t.TempDir()
			data := filepath.Join(root, "data")
			if err := os.Mkdir(data, 0o700); err != nil {
				t.Fatal(err)
			}
			valid := filepath.Join(data, "file-\ufffd")
			invalid := filepath.Join(data, "file-\xff")
			if err := os.WriteFile(valid, []byte("valid sibling"), 0o600); err != nil {
				t.Fatal(err)
			}
			if hardlink {
				if err := os.Link(valid, invalid); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(invalid, []byte("invalid sibling"), 0o600); err != nil {
				t.Fatal(err)
			}
			before := make(map[string]os.FileInfo)
			for _, path := range []string{valid, invalid} {
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				before[path] = info
			}

			// Neither preparing nor executing work may report success or replace either inode.
			statePath := filepath.Join(root, "state.json")
			reportPath := filepath.Join(root, "report.json")
			for _, dry := range []string{"true", "false"} {
				if code := run(context.Background(), []string{"-p=false", "-dryrun=" + dry, "-state", statePath, "-report", reportPath, data}); code != 1 {
					t.Fatalf("dryrun=%s exit=%d, want 1", dry, code)
				}
				for path, info := range before {
					if after, err := os.Stat(path); err != nil || !os.SameFile(info, after) {
						t.Fatalf("source %q replaced: %v", path, err)
					}
				}
				state, err := loadState(statePath)
				if err != nil || state == nil || len(state.Pending) != 0 || len(state.Busy) != 0 {
					t.Fatalf("lossy work was persisted: %#v / %v", state, err)
				}
			}
		})
	}
}

func TestRewriteAllowsByteDocumentBasenames(t *testing.T) {
	// Document basenames are opened natively; only their temporary paths enter the queue.
	root := t.TempDir()
	data := filepath.Join(root, "data")
	if err := os.Mkdir(data, 0o700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(data, "source")
	if err := os.WriteFile(source, []byte("source content"), 0o600); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(root, "state-\xff.json")
	reportPath := filepath.Join(root, "report-\xff.json")

	// Preparing and then resuming this queue must retain ordinary native document access.
	for _, dry := range []string{"true", "false"} {
		if code := run(context.Background(), []string{"-p=false", "-dryrun=" + dry, "-state", statePath, "-report", reportPath, data}); code != 0 {
			t.Fatalf("dryrun=%s exit=%d, want 0", dry, code)
		}
	}
	jobs, _, err := loadReport(reportPath)
	if err != nil || jobs[source] == nil || jobs[source].FullPath != source {
		t.Fatalf("report with native basename = %#v / %v", jobs, err)
	}
}
