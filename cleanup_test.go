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
	sourcePath := writeSourceFile(t, root, "source", []byte("fixture"))
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
	copyer := newTestCopyer(t)
	item := newFixtureItem(sourcePath, target)
	job := &baseJob{
		copyer:         copyer,
		item:           item,
		src:            &source{base: root, path: "source"},
		path:           sourcePath,
		stat:           stat,
		targets:        []string{target},
		successTargets: []string{target},
	}
	copyed := make(chan *baseJob, 1)
	copyed <- job
	close(copyed)
	copyer.cleanup(context.Background(), copyed)

	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat failed target error = %v, want %v", err, os.ErrNotExist)
	}
	result, terminalErr := item.terminal(t)
	if terminalErr != nil {
		t.Fatalf("a metadata failure is a target outcome, not an item failure: %v", terminalErr)
	}
	if len(result.Targets) != 1 || !errors.Is(result.Targets[0].Err, os.ErrNotExist) {
		t.Fatalf("target outcome = %#v, want %v", result.Targets, os.ErrNotExist)
	}
}
