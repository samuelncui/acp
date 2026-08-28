package acp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCleanupRemovesTargetAfterMetadataFailure(t *testing.T) {
	// Use a dangling symlink so metadata restoration fails while removal remains possible.
	root := t.TempDir()
	sourcePath := filepath.Join(root, "source")
	if err := os.WriteFile(sourcePath, []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	stat, err := newStat(sourcePath, info)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "target")
	if err := os.Symlink(filepath.Join(root, "missing"), target); err != nil {
		t.Skipf("create test symlink: %v", err)
	}

	// Cleanup must discard the failed target before publishing its final result.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	option := newOption()
	if err := option.check(); err != nil {
		t.Fatal(err)
	}
	copyer := &Copyer{option: option, eventCh: make(chan Event, 8)}
	job := &baseJob{
		copyer:         copyer,
		src:            &source{base: root, path: "source"},
		path:           sourcePath,
		stat:           stat,
		successTargets: []string{target},
	}
	copyed := make(chan *baseJob, 1)
	copyed <- job
	close(copyed)
	if sinkFailed := copyer.cleanupJob(ctx, cancel, copyed); sinkFailed {
		t.Fatal("cleanup reported an unexpected Sink failure")
	}
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat failed target error = %v, want %v", err, os.ErrNotExist)
	}
	report := job.report()
	if !errors.Is(report.FailTargets[target], os.ErrNotExist) {
		t.Fatalf("metadata failure = %v, want %v", report.FailTargets[target], os.ErrNotExist)
	}
}
