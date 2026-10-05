//go:build linux

package acp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
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

	// A target failure is an item outcome that keeps the portable no-space sentinel. The write
	// itself reports ENOSPC, which is what the sentinel describes.
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
	// A refusing device fails the target while the producer still feeds the writer, which is the
	// path that must release every queued read buffer.
	trackChunkPool(t)

	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Symlink("/dev/full", target); err != nil {
		t.Fatal(err)
	}

	size := int64(batchSize) * 4
	readErr := errors.New("read failed")
	item := newFixtureItem(filepath.Join(root, "source"), target)
	// A linear target writes without pre-allocating, so the device's ENOSPC arrives while the
	// producer still has buffers queued, which is the path under test.
	copyer := newTestStream(t, Overwrite(true), SetToDevice(LinearDevice(true)))
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
	}, &failingContentReader{err: readErr, batches: 4}, false)

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

// TestNonLinearTargetOnRefusingDeviceReportsATargetFailure pins the pre-allocation path of a
// non-linear target against a device that refuses `fallocate`: the failure is that target's
// outcome, it must not claim the device is out of space, and the pre-existing target remains.
// It needs no privileges, so it always runs on Linux.
func TestNonLinearTargetOnRefusingDeviceReportsATargetFailure(t *testing.T) {
	// Exercise preallocation through an overwrite target whose path the run does not own.
	root := t.TempDir()
	input := writeSourceFile(t, root, "source", []byte("fixture"))
	target := filepath.Join(root, "target")
	if err := os.Symlink("/dev/full", target); err != nil {
		t.Fatal(err)
	}
	item := newFixtureItem(input, target)

	// The device refusal stays a target failure and must not be reclassified as exhausted space.
	err := runFixture(context.Background(), newStreamFixture(item), []Item{item}, Overwrite(true))
	if err != nil {
		t.Fatalf("runFixture() error = %v, want nil: a refused target is an item outcome", err)
	}
	result, terminalErr := item.terminal(t)
	if terminalErr != nil {
		t.Fatalf("item failed: %v", terminalErr)
	}
	if len(result.Targets) != 1 || result.Targets[0].Err == nil {
		t.Fatalf("target outcome = %#v, want a failed target", result.Targets)
	}
	if errors.Is(result.Targets[0].Err, ErrTargetNoSpace) {
		t.Fatalf("target outcome = %v, want the device's own refusal: %v reports no space, and a "+
			"device that cannot pre-allocate is not out of space", result.Targets[0].Err, ErrTargetNoSpace)
	}

	// Failed overwrite cleanup preserves the existing link instead of removing an unowned path.
	if destination, err := os.Readlink(target); err != nil || destination != "/dev/full" {
		t.Fatalf("pre-existing target link = %q, error = %v; want /dev/full", destination, err)
	}
}

// TestFullVolumePreallocationReportsNoSpace pins the pre-allocation failure of a non-linear
// target: a full volume must still report
// ErrTargetNoSpace and remove the file it could not create. It needs a real ENOSPC, so it mounts a
// small tmpfs and skips when the host refuses that.
func TestFullVolumePreallocationReportsNoSpace(t *testing.T) {
	volume := filepath.Join(t.TempDir(), "volume")
	if err := os.Mkdir(volume, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mount("tmpfs", volume, "tmpfs", 0, "size=16m"); err != nil {
		t.Skipf("mount a small tmpfs: %v", err)
	}
	t.Cleanup(func() { _ = unix.Unmount(volume, 0) })

	const size = 4 * 1024 * 1024
	root := t.TempDir()
	input := writeSourceFile(t, root, "source", make([]byte, size))
	target := filepath.Join(volume, "target")
	item := newFixtureItem(input, target)

	copyer := newTestStream(t, Overwrite(true))
	// Fill the disposable volume so actual preallocation reports ENOSPC.
	filler, err := os.Create(filepath.Join(volume, "filler"))
	if err != nil {
		t.Fatal(err)
	}
	block := make([]byte, 64*1024)
	for {
		if _, err := filler.Write(block); err != nil {
			if !errors.Is(err, syscall.ENOSPC) {
				t.Fatalf("fill the volume: %v", err)
			}
			break
		}
	}
	if err := filler.Close(); err != nil {
		t.Fatal(err)
	}

	fixture := newStreamFixture(item)
	copyer.onResults = fixture.onResults
	job := newWriteJob(&baseJob{
		copyer:  copyer,
		item:    item,
		path:    input,
		stat:    &stat{size: size, mode: 0o644},
		targets: []string{target},
	}, io.NopCloser(bytes.NewReader(make([]byte, size))), false)

	// A refused target is an item outcome, not a pipeline failure.
	if err := runWriteJob(copyer, job); err != nil {
		t.Fatalf("runWriteJob() error = %v, want nil", err)
	}
	result, terminalErr := item.terminal(t)
	if terminalErr != nil {
		t.Fatalf("item failed: %v", terminalErr)
	}
	if len(result.Targets) != 1 || !errors.Is(result.Targets[0].Err, ErrTargetNoSpace) {
		t.Fatalf("target outcome = %#v, want %v", result.Targets, ErrTargetNoSpace)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a target that could not be pre-allocated must be removed, stat error = %v", err)
	}
}
