// Package fileio owns temporary command files and atomic JSON replacement.
package fileio

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// tempName is replaceable by hardlink collision tests. Allocation, not a stat, owns a name.
var tempName = func(dir string) (string, error) {
	var suffix [12]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", err
	}
	return filepath.Join(dir, ".tmp_"+hex.EncodeToString(suffix[:])), nil
}

// Create exclusively allocates a command scratch file beside its eventual destination.
func Create(dir string) (*os.File, error) {
	return os.CreateTemp(dir, ".tmp_*")
}

// Link exclusively allocates a temporary hardlink; it never removes a colliding entry.
func Link(src, dir string) (string, error) {
	// Link is itself the exclusive allocation operation, including for dangling symlinks.
	for attempt := 0; attempt < 100; attempt++ {
		name, err := tempName(dir)
		if err != nil {
			return "", fmt.Errorf("generate temporary name failed, %w", err)
		}
		if err := os.Link(src, name); err != nil {
			if errors.Is(err, os.ErrExist) {
				continue
			}
			return "", err
		}
		return name, nil
	}
	return "", fmt.Errorf("allocate temporary link failed, %w", os.ErrExist)
}

// CleanupError identifies an owned path that could not be removed and must remain recorded.
type CleanupError struct {
	Path string
	Err  error
}

func (e *CleanupError) Error() string {
	return fmt.Sprintf("cleanup owned temporary %q failed, %v", e.Path, e.Err)
}
func (e *CleanupError) Unwrap() error { return e.Err }

// Remove removes only a path the caller already owns through successful allocation or saved state.
func Remove(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return &CleanupError{Path: path, Err: err}
	}
	return nil
}

// WriteJSON replaces one document only after encoding, syncing and closing its exclusive file.
func WriteJSON(path string, value any, indent bool) (err error) {
	// Keep the prior document readable on every failure before rename.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	output, err := NewOutput(path)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, output.Discard())
	}()

	// Finish the new document before publishing it; cleanup errors supplement the primary error.
	enc := json.NewEncoder(output.File)
	if indent {
		enc.SetIndent("", "  ")
	}
	if err := enc.Encode(value); err != nil {
		return fmt.Errorf("encode JSON failed, %w", err)
	}
	if err := output.File.Sync(); err != nil {
		return fmt.Errorf("sync JSON failed, %w", err)
	}
	if err := output.Commit(true); err != nil {
		return fmt.Errorf("replace JSON failed, %w", err)
	}
	return nil
}
