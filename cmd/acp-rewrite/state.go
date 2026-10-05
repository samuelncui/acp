package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/samuelncui/acp/internal/fileio"
)

type rewriteEntry struct {
	Path  string      `json:"path"`
	Links []string    `json:"links,omitempty"`
	Info  os.FileInfo `json:"-"`
}
type rewriteState struct {
	Root     string         `json:"root"`
	Pending  []rewriteEntry `json:"pending,omitempty"`
	Busy     []rewriteEntry `json:"busy,omitempty"`
	Missing  []rewriteEntry `json:"missing,omitempty"`
	TmpFiles []string       `json:"tmp_files,omitempty"`
	saved    bool           // In-memory progress matches the last successfully loaded or saved document.
}

// A persistence failure stops new entries; the current entry is still present on disk.
type persistenceError struct{ error }

func (e *persistenceError) Unwrap() error { return e.error }

var (
	copyRewrite     = rewriteFile
	writeState      = saveState
	writeReport     = saveReport
	removeTemporary = fileio.Remove
)

func loadState(path string) (*rewriteState, error) {
	// A missing document is a fresh run; ambiguous or lossy documents are fatal.
	var state rewriteState
	if err := fileio.ReadJSON(path, &state); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	// Validate saved identities once, before any caller may clean or rewrite their paths.
	if err := checkState(&state); err != nil {
		return nil, err
	}
	state.saved = true
	return &state, nil
}

func checkState(state *rewriteState) error {
	// Empty entry paths include null rows decoded into the entry's zero value.
	if err := fileio.CheckJSONPaths(state.Root); err != nil {
		return err
	}
	for _, entries := range [][]rewriteEntry{state.Pending, state.Busy, state.Missing} {
		for _, entry := range entries {
			if err := checkPaths(entry.Path); err != nil {
				return err
			}
			if err := checkPaths(entry.Links...); err != nil {
				return err
			}
		}
	}
	return checkPaths(state.TmpFiles...)
}

func checkPaths(paths ...string) error {
	for _, path := range paths {
		if path == "" {
			return fmt.Errorf("saved path is empty")
		}
	}
	return fileio.CheckJSONPaths(paths...)
}

func saveState(path string, state *rewriteState) error {
	if state == nil {
		return nil
	}

	// Paths enter through load or scan; checkpoints only publish the current progress.
	state.saved = false
	err := fileio.WriteJSON(path, state, true)
	rememberCleanup(state, err)
	state.saved = err == nil
	return err
}

func rememberCleanup(state *rewriteState, err error) {
	var cleanup *fileio.CleanupError
	if errors.As(err, &cleanup) {
		for _, tmp := range state.TmpFiles {
			if tmp == cleanup.Path {
				return
			}
		}
		state.TmpFiles = append(state.TmpFiles, cleanup.Path)
		state.saved = false
	}
}

func cleanupTmpFile(state *rewriteState, path string) error {
	// One entry cleans only its own allocation; a prior failed cleanup cannot fail a later entry.
	if err := removeTemporary(path); err != nil {
		return err
	}
	state.TmpFiles = removeTmp(state.TmpFiles, path)
	state.saved = false
	return nil
}

func cleanupTmpFiles(state *rewriteState) error {
	// The persisted list is ownership, not a glob or a naming convention.
	var errs []error
	kept := state.TmpFiles[:0]
	for _, tmp := range state.TmpFiles {
		if err := removeTemporary(tmp); err != nil {
			kept = append(kept, tmp)
			errs = append(errs, err)
		}
	}
	if len(kept) != len(state.TmpFiles) {
		state.saved = false
	}
	state.TmpFiles = kept
	return errors.Join(errs...)
}

func removeTmp(tmpFiles []string, tmp string) []string {
	next := tmpFiles[:0]
	for _, t := range tmpFiles {
		if t == tmp {
			continue
		}
		next = append(next, t)
	}
	return next
}

func removeEntry(entries []rewriteEntry, path string) []rewriteEntry {
	for index, entry := range entries {
		if entry.Path == path {
			return append(entries[:index], entries[index+1:]...)
		}
	}
	return entries
}
