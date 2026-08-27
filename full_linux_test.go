//go:build linux

package acp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRunStreamMapsDeviceFullToTargetNoSpace(t *testing.T) {
	// Route a disposable target symlink to Linux's deterministic ENOSPC device.
	root := t.TempDir()
	input := filepath.Join(root, "source")
	target := filepath.Join(root, "target")
	if err := os.WriteFile(input, []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/dev/full", target); err != nil {
		t.Fatal(err)
	}
	source := &sliceStreamSource{requests: []*StreamRequest{{
		ID: 1, Source: input, Targets: []string{target},
	}}}

	// The synchronous stream boundary must preserve the portable no-space sentinel.
	err := RunStream(
		context.Background(), source, new(collectingStreamSink),
		Overwrite(true), SetToDevice(LinearDevice(true)),
	)
	if !errors.Is(err, ErrTargetNoSpace) {
		t.Fatalf("RunStream() error = %v, want %v", err, ErrTargetNoSpace)
	}
}
