package fileio

import (
	"errors"
	"os"
	"path/filepath"
)

// Output owns a destination descriptor and, when staged, its exclusive temporary path.
// Owners hand it off sequentially; callers must wait for all users before discarding it.
// A direct device has a File and Path but no Temporary, so cleanup never removes it.
type Output struct {
	File      *os.File
	Path      string
	Temporary string
}

// NewOutput creates an open temporary in the destination's directory.
func NewOutput(path string) (*Output, error) {
	// Successful allocation transfers both the descriptor and the path to this owner.
	file, err := Create(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	return &Output{File: file, Path: path, Temporary: file.Name()}, nil
}

// Close releases the descriptor once, including when closing it fails.
func (o *Output) Close() error {
	// A failed close must not leave a descriptor that a later cleanup attempts to close again.
	if o.File == nil {
		return nil
	}
	file := o.File
	o.File = nil
	return file.Close()
}

// Commit closes the descriptor and replaces Path, retaining ownership on rename failure.
func (o *Output) Commit(overwrite bool) error {
	// Publication must not proceed when the descriptor could not be closed.
	if err := o.Close(); err != nil {
		return err
	}
	if o.Temporary == "" {
		return nil
	}

	// Only a successful replacement relinquishes the temporary path.
	rename := os.Rename
	if !overwrite {
		rename = renameNoOverwrite
	}
	if err := rename(o.Temporary, o.Path); err != nil {
		return err
	}
	o.Temporary = ""
	return nil
}

// Discard closes the descriptor and removes its temporary, retaining any failed removal.
func (o *Output) Discard() error {
	// Attempt removal even after a close failure, retaining both errors.
	err := o.Close()
	if o.Temporary == "" {
		return err
	}
	if removeErr := Remove(o.Temporary); removeErr != nil {
		return errors.Join(err, removeErr)
	}
	o.Temporary = ""
	return err
}

// RewriteItem passes an already-open output and observed source metadata to the copy engine.
// The command persists Temporary before submission and discards Output only after Wait.
type RewriteItem struct {
	Path   string
	Info   os.FileInfo
	Output *Output
}

// Source returns the original file's path.
func (i *RewriteItem) Source() string { return i.Path }

// Targets reports the final path; Output owns the separate temporary used during copying.
func (i *RewriteItem) Targets() []string { return []string{i.Path} }
