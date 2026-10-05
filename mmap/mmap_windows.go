// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package mmap provides a way to memory-map a file.
package mmap

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

// ReaderAt reads a memory-mapped file.
//
// Like any io.ReaderAt, clients can execute parallel ReadAt calls, but it is
// not safe to call Close and reading methods concurrently.
type ReaderAt struct {
	data []byte
	file *os.File
}

// Close releases the mapping and then the descriptor it was created from. It is idempotent: a
// second call releases nothing, and an empty mapping still closes the descriptor it retains.
func (r *ReaderAt) Close() error {
	// Detach owned resources so another Close has nothing left to release.
	data := r.data
	file := r.file
	r.data = nil
	r.file = nil

	// Take the finalizer out of the picture before releasing either resource, so a later
	// collection can only ever repeat this no-op.
	runtime.SetFinalizer(r, nil)

	// Unmap before closing the descriptor, retaining errors from both operations.
	var unmapErr error
	if len(data) != 0 {
		unmapErr = syscall.UnmapViewOfFile(uintptr(unsafe.Pointer(&data[0])))
	}
	if file == nil {
		return unmapErr
	}
	return errors.Join(unmapErr, file.Close())
}

// Len returns the length of the underlying memory-mapped file.
func (r *ReaderAt) Len() int {
	return len(r.data)
}

// At returns the byte at index i.
func (r *ReaderAt) At(i int) byte {
	return r.data[i]
}

// ReadAt implements the io.ReaderAt interface.
func (r *ReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if r.data == nil {
		return 0, errors.New("mmap: closed")
	}
	if off < 0 || int64(len(r.data)) < off {
		return 0, fmt.Errorf("mmap: invalid ReadAt offset %d", off)
	}
	n := copy(p, r.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// ReadAt implements the io.ReaderAt interface.
func (r *ReaderAt) Slice(off, limit int64) ([]byte, error) {
	if r.data == nil {
		return nil, errors.New("mmap: closed")
	}

	l := int64(len(r.data))
	if off < 0 || limit < 0 || l < off {
		return nil, fmt.Errorf("mmap: invalid ReadAt offset %d", off)
	}

	// Compare against the remaining length instead of off+limit: a large limit would wrap
	// around and turn this bounds check into a slice-out-of-range panic.
	if limit > l-off {
		return r.data[off:], nil
	}

	return r.data[off : off+limit], nil
}

// mapFile acquires the mapping; the caller retains descriptor ownership on failure.
func mapFile(f *os.File, size int) (*ReaderAt, error) {
	// An empty file has no mapping, but it still has the descriptor this reader owns. The mapping
	// of a zero-length file is an empty, non-nil slice, so only a closed reader reports a nil one.
	data := make([]byte, 0)
	// A mapping that has not reached a reader is still owned by this acquisition scope.
	defer func() {
		if len(data) != 0 {
			_ = syscall.UnmapViewOfFile(uintptr(unsafe.Pointer(&data[0])))
		}
	}()
	if size != 0 {
		low, high := uint32(size), uint32(int64(size)>>32)
		fmap, err := syscall.CreateFileMapping(syscall.Handle(f.Fd()), nil, syscall.PAGE_READONLY, high, low, nil)
		if err != nil {
			return nil, err
		}
		defer syscall.CloseHandle(fmap)
		ptr, err := syscall.MapViewOfFile(fmap, syscall.FILE_MAP_READ, 0, 0, uintptr(size))
		if err != nil {
			return nil, err
		}
		data = unsafe.Slice((*byte)(unsafe.Pointer(ptr)), size)
	}

	// Transfer the mapping to the reader before the opener transfers its descriptor.
	r := &ReaderAt{data: data, file: f}
	data = nil
	return r, nil
}
