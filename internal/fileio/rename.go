package fileio

import (
	"errors"
	"os"
	"sync"
)

// The fallback coordinates this process's writers only on filesystems without exclusive rename.
var (
	exclusiveRename = renameExclusive
	renameFallback  sync.Mutex
)

func renameNoOverwrite(from, to string) error {
	// Let the filesystem decide existence and case/alias identity in one operation.
	err := exclusiveRename(from, to)
	if !errors.Is(err, errors.ErrUnsupported) {
		if err != nil {
			return &os.LinkError{Op: "rename", Old: from, New: to, Err: err}
		}
		return nil
	}

	// Some supported platforms and filesystems expose only replacing rename. Keep the
	// existence decision and replacement together; external writers are outside the model.
	renameFallback.Lock()
	defer renameFallback.Unlock()
	if _, err := os.Lstat(to); err == nil {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: os.ErrExist}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(from, to)
}
