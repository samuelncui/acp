package fileio

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOutputNoOverwrite(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		for _, exists := range []bool{false, true} {
			t.Run(fmt.Sprintf("fallback=%v/existing=%v", fallback, exists), func(t *testing.T) {
				// Two owned outputs contend for one name, with both native and fallback rename.
				previous := exclusiveRename
				if fallback {
					exclusiveRename = func(string, string) error { return errors.ErrUnsupported }
				}
				t.Cleanup(func() { exclusiveRename = previous })
				path := filepath.Join(t.TempDir(), "target")
				if exists {
					if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				outputs := make([]*Output, 2)
				for i := range outputs {
					out, err := NewOutput(path)
					if err != nil {
						t.Fatal(err)
					}
					defer out.Discard()
					if _, err := out.File.WriteString(fmt.Sprint(i)); err != nil {
						t.Fatal(err)
					}
					outputs[i] = out
				}

				// Final-path ownership is decided once; a loser retains its temporary for cleanup.
				start, done := make(chan struct{}), make(chan error, len(outputs))
				for _, out := range outputs {
					go func() { <-start; done <- out.Commit(false) }()
				}
				close(start)
				succeeded, refused := 0, 0
				for range outputs {
					switch err := <-done; {
					case err == nil:
						succeeded++
					case errors.Is(err, os.ErrExist):
						refused++
					default:
						t.Errorf("commit failed: %v", err)
					}
				}
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if exists {
					if succeeded != 0 || refused != 2 || string(data) != "original" {
						t.Fatalf("success=%d refused=%d content=%q", succeeded, refused, data)
					}
				} else if succeeded != 1 || refused != 1 || (string(data) != "0" && string(data) != "1") {
					t.Fatalf("success=%d refused=%d content=%q", succeeded, refused, data)
				}
				for _, out := range outputs {
					if err := out.Discard(); err != nil {
						t.Fatal(err)
					}
				}
				if paths, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".tmp_*")); err != nil || len(paths) != 0 {
					t.Fatalf("temporaries=%v / %v", paths, err)
				}
			})
		}
	}
}

func TestOutputCommitAndDiscard(t *testing.T) {
	// The old destination remains readable until the open staged file is committed.
	dir := t.TempDir()
	path := filepath.Join(dir, "target")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := NewOutput(path)
	if err != nil {
		t.Fatal(err)
	}
	defer output.Discard()
	tmp, file := output.Temporary, output.File
	if filepath.Dir(tmp) != dir || !strings.HasPrefix(filepath.Base(tmp), ".tmp_") {
		t.Fatalf("temporary = %q", tmp)
	}
	if _, err := file.WriteString("new"); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "old" {
		t.Fatalf("uncommitted destination = %q / %v", data, err)
	}

	// Commit relinquishes both resources; repeated settlement cannot remove the final path.
	for _, settle := range []func() error{func() error { return output.Commit(true) }, func() error { return output.Commit(true) }, output.Close, output.Discard} {
		if err := settle(); err != nil {
			t.Fatal(err)
		}
	}
	if output.File != nil || output.Temporary != "" || output.Path != path {
		t.Fatalf("ownership after commit = %+v", output)
	}
	if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("descriptor still open: %v", err)
	}
	if _, err := os.Stat(tmp); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary remains: %v", err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "new" {
		t.Fatalf("committed destination = %q / %v", data, err)
	}
}

func TestOutputCommitFailureRetainsTemporary(t *testing.T) {
	// A refused rename preserves the destination and keeps the closed temporary for cleanup.
	dir := t.TempDir()
	path := filepath.Join(dir, "directory")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	output, err := NewOutput(path)
	if err != nil {
		t.Fatal(err)
	}
	defer output.Discard()
	tmp := output.Temporary
	if err := output.Commit(true); err == nil {
		t.Fatal("rename into a directory succeeded")
	}
	if output.File != nil || output.Temporary != tmp {
		t.Fatalf("failed commit ownership = %+v", output)
	}
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		t.Fatalf("original destination changed: %v", err)
	}

	// A later discard removes only the retained allocation and is repeatable.
	for range 2 {
		if err := output.Discard(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(tmp); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary remains: %v", err)
	}
}

func TestOutputCloseFailureDoesNotCommit(t *testing.T) {
	// Force Close to fail without changing the destination or the owned temporary.
	dir := t.TempDir()
	path := filepath.Join(dir, "target")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := NewOutput(path)
	if err != nil {
		t.Fatal(err)
	}
	defer output.Discard()
	if err := output.File.Close(); err != nil {
		t.Fatal(err)
	}
	tmp := output.Temporary

	// The descriptor is relinquished even on failure; cleanup can still remove the allocation.
	if err := output.Commit(true); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("commit error = %v", err)
	}
	if output.File != nil || output.Temporary != tmp {
		t.Fatalf("ownership after failed close = %+v", output)
	}
	if err := output.Close(); err != nil {
		t.Fatalf("second close = %v", err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "old" {
		t.Fatalf("destination changed: %q / %v", data, err)
	}
}

func TestOutputDiscardRetainsFailedRemovalAndCloseError(t *testing.T) {
	// Use a nonempty owned directory to make removal fail independently of descriptor close.
	dir := t.TempDir()
	owned := filepath.Join(dir, ".tmp_owned")
	if err := os.Mkdir(owned, 0o700); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(owned, "child")
	if err := os.WriteFile(child, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(child)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	output := &Output{File: file, Temporary: owned}

	// Failed cleanup keeps its path and both failure identities for a later explicit retry.
	err = output.Discard()
	var cleanup *CleanupError
	if !errors.Is(err, os.ErrClosed) || !errors.As(err, &cleanup) || cleanup.Path != owned {
		t.Fatalf("discard errors = %v", err)
	}
	if output.File != nil || output.Temporary != owned {
		t.Fatalf("discard ownership = %+v", output)
	}
	if err := os.Remove(child); err != nil {
		t.Fatal(err)
	}
	if err := output.Discard(); err != nil || output.Temporary != "" {
		t.Fatalf("discard retry = %+v / %v", output, err)
	}
}

func TestOutputDirectTargetIsNeverRemoved(t *testing.T) {
	// A direct target owns its descriptor, but never owns a removable temporary path.
	path := filepath.Join(t.TempDir(), "direct")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	output := &Output{File: file, Path: path}
	for _, settle := range []func() error{output.Discard, func() error { return output.Commit(true) }, output.Close} {
		if err := settle(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("direct target was removed: %v", err)
	}
}
