// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !linux && !windows && !darwin
// +build !linux,!windows,!darwin

// Package mmap provides a way to memory-map a file.
package mmap

import (
	"errors"
	"fmt"
	"os"
)

// ReaderAt reads a memory-mapped file.
//
// Like any io.ReaderAt, clients can execute parallel ReadAt calls, but it is
// not safe to call Close and reading methods concurrently.
type ReaderAt struct {
	file *os.File
	len  int
}

// Close closes the reader. It is idempotent: a reader that is already closed reports success,
// because the file it owned is already gone. A closed reader reports an empty mapping, exactly
// like the mapping-backed platforms, so a reader that reads after Close sees io.EOF.
func (r *ReaderAt) Close() error {
	if r.file == nil {
		return nil
	}

	file := r.file
	r.file = nil
	r.len = 0
	return file.Close()
}

// Len returns the length of the underlying memory-mapped file.
func (r *ReaderAt) Len() int {
	return r.len
}

// At returns the byte at index i.
func (r *ReaderAt) At(i int) byte {
	if i < 0 || r.len <= i {
		panic("index out of range")
	}
	var b [1]byte
	r.ReadAt(b[:], int64(i))
	return b[0]
}

// ReadAt implements the io.ReaderAt interface.
func (r *ReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if r.file == nil {
		return 0, errors.New("mmap: closed")
	}
	if off < 0 || int64(r.len) < off {
		return 0, fmt.Errorf("mmap: invalid ReadAt offset %d", off)
	}

	return r.file.ReadAt(p, off)
}

// Slice returns the mapped bytes in [off, off+limit), clamped to the end of the file. The
// whole range is validated before anything is allocated, so a bad range cannot allocate a
// buffer first, and a limit past the end of the file cannot wrap around the bounds check.
func (r *ReaderAt) Slice(off, limit int64) ([]byte, error) {
	if r.file == nil {
		return nil, errors.New("mmap: closed")
	}

	l := int64(r.len)
	if off < 0 || limit < 0 || l < off {
		return nil, fmt.Errorf("mmap: invalid ReadAt offset %d", off)
	}
	if limit > l-off {
		limit = l - off
	}

	buf := make([]byte, limit)
	if limit == 0 {
		return buf, nil
	}
	n, err := r.ReadAt(buf, off)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

// Open opens the named file for reading. The reader owns the descriptor it opened: Close closes
// it, and this platform reads through the descriptor instead of a mapping.
func Open(filename string) (*ReaderAt, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}

	size := fi.Size()
	if size < 0 {
		_ = f.Close()
		return nil, fmt.Errorf("mmap: file %q has negative size", filename)
	}
	if size != int64(int(size)) {
		_ = f.Close()
		return nil, fmt.Errorf("mmap: file %q is too large", filename)
	}

	return &ReaderAt{
		file: f,
		len:  int(fi.Size()),
	}, nil
}
