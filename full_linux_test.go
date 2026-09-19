//go:build linux

package acp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRunMapsDeviceFullToTargetNoSpace(t *testing.T) {
	// Route a disposable target symlink to Linux's deterministic ENOSPC device.
	root := t.TempDir()
	input := writeSourceFile(t, root, "source", []byte("fixture"))
	target := filepath.Join(root, "target")
	if err := os.Symlink("/dev/full", target); err != nil {
		t.Fatal(err)
	}
	item := newFixtureItem(input, target)

	// A target failure is an item outcome that keeps the portable no-space sentinel.
	err := runFixture(
		context.Background(),
		newStreamFixture(item),
		[]Item{item},
		Overwrite(true),
		SetToDevice(LinearDevice(true)),
	)
	if err != nil {
		t.Fatalf("runFixture() error = %v, want nil", err)
	}
	result, terminalErr := item.terminal(t)
	if terminalErr != nil {
		t.Fatalf("item failed: %v", terminalErr)
	}
	if len(result.Targets) != 1 || !errors.Is(result.Targets[0].Err, ErrTargetNoSpace) {
		t.Fatalf("target outcome = %#v, want %v", result.Targets, ErrTargetNoSpace)
	}
}

func TestDeviceFullWriteFailureDrainsQueuedBuffers(t *testing.T) {
	// A full device fails the first write while the producer still feeds the writer, which
	// is the path that must release every queued read buffer.
	trackChunkPool(t)

	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Symlink("/dev/full", target); err != nil {
		t.Fatal(err)
	}

	size := int64(batchSize) * 4
	readErr := errors.New("read failed")
	item := newFixtureItem(filepath.Join(root, "source"), target)
	copyer := newTestStream(t, Overwrite(true))
	// The push engine reports every outcome through the results callback, so the fixture is the
	// observer of this stage-level run.
	fixture := newStreamFixture(item)
	copyer.onResults = fixture.onResults
	job := newWriteJob(&baseJob{
		copyer:  copyer,
		item:    item,
		path:    filepath.Join(root, "source"),
		stat:    &stat{size: size, mode: 0o644},
		targets: []string{target},
	}, &failingContentReader{err: readErr, batches: 4}, size, false)

	// The writer must fail, release its queued buffers, and let the pipeline return.
	done := make(chan error, 1)
	go func() {
		done <- runWriteJob(copyer, job)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("pipeline failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the pipeline deadlocked while draining a full target")
	}

	result, terminalErr := item.terminal(t)
	if terminalErr != nil {
		t.Fatalf("an item whose targets failed must still complete: %v", terminalErr)
	}
	if len(result.Targets) != 1 || !errors.Is(result.Targets[0].Err, ErrTargetNoSpace) {
		t.Fatalf("target outcome = %#v, want %v", result.Targets, ErrTargetNoSpace)
	}
}
