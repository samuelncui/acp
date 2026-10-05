//go:build linux

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samuelncui/acp"
)

func TestCopyBytePathsPreservesFilesButRefusesLossyReport(t *testing.T) {
	// Native copying retains byte names even when JSON cannot represent them.
	root := t.TempDir()
	source := filepath.Join(root, "source-\xff")
	target := filepath.Join(root, "target-\xff")
	content := []byte("native byte path content")
	if err := os.WriteFile(source, content, 0o600); err != nil {
		t.Fatal(err)
	}
	collector, err := runJobs(t, context.Background(), nil, acp.AccurateJob(source, []string{target}))
	snapshot := collector.getter()
	if err != nil || hasFailure(snapshot) {
		t.Fatalf("native copy failed: %v / %#v", err, snapshot)
	}
	if copied, err := os.ReadFile(target); err != nil || !bytes.Equal(copied, content) {
		t.Fatalf("copied content = %q / %v", copied, err)
	}
	rows := snapshot.Jobs
	if len(rows) != 1 || rows[0].FullPath != source || len(rows[0].SuccessTargets) != 1 || rows[0].SuccessTargets[0] != target {
		t.Fatalf("in-memory result changed path identity: %#v", rows)
	}

	// An explicit report failure keeps the previous document; no report remains valid usage.
	path := filepath.Join(root, "report.json")
	before := []byte(`{"files":[]}`)
	if err := os.WriteFile(path, before, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := storeReport(snapshot, path, false); err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("save report = %v, want an explicit UTF-8 error", err)
	}
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, before) {
		t.Fatalf("previous report changed: %q / %v", after, err)
	}
	if err := storeReport(snapshot, "", false); err != nil {
		t.Fatalf("no report requested: %v", err)
	}
}
