// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package mmap

import (
	"bytes"
	"errors"
	"io"
	"io/ioutil"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestOpen(t *testing.T) {
	const filename = "mmap_test.go"
	r, err := Open(filename)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got := make([]byte, r.Len())
	if _, err := r.ReadAt(got, 0); err != nil && err != io.EOF {
		t.Fatalf("ReadAt: %v", err)
	}
	want, err := ioutil.ReadFile(filename)
	if err != nil {
		t.Fatalf("ioutil.ReadFile: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d bytes, want %d", len(got), len(want))
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("\ngot  %q\nwant %q", string(got), string(want))
	}
}

// writeFixture writes one file with the given content and returns its path.
func writeFixture(t *testing.T, name string, content []byte) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("write fixture %q: %v", path, err)
	}
	return path
}

// TestReaderAtContract pins the reader contract every platform must implement: length, random
// access, the exact end of the content, and an idempotent close.
func TestReaderAtContract(t *testing.T) {
	content := []byte("mmap contract fixture")
	reader, err := Open(writeFixture(t, "fixture.bin", content))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if reader.Len() != len(content) {
		t.Fatalf("Len() = %d, want %d", reader.Len(), len(content))
	}

	buf := make([]byte, len(content))
	n, err := reader.ReadAt(buf, 0)
	if err != nil && err != io.EOF {
		t.Fatalf("ReadAt: %v", err)
	}
	if n != len(content) || !bytes.Equal(buf, content) {
		t.Fatalf("ReadAt() = %d bytes %q, want %q", n, buf, content)
	}
	if got := reader.At(len(content) - 1); got != content[len(content)-1] {
		t.Fatalf("At(%d) = %q, want %q", len(content)-1, got, content[len(content)-1])
	}

	// An offset at the end of the content is the end of the file, not an error.
	if n, err := reader.ReadAt(make([]byte, 1), int64(len(content))); n != 0 || err != io.EOF {
		t.Fatalf("ReadAt at the end = (%d, %v), want (0, %v)", n, err, io.EOF)
	}
	// An offset past the end is invalid.
	if _, err := reader.ReadAt(make([]byte, 1), int64(len(content))+1); err == nil {
		t.Fatal("ReadAt past the end = nil error")
	}

	// Closing is idempotent and makes the mapping unusable.
	if err := reader.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("second Close = %v, want nil", err)
	}
	if _, err := reader.ReadAt(buf, 0); err == nil {
		t.Fatal("ReadAt after Close = nil error")
	}
}

// TestOpenEmptyMappingIsNotEmpty pins the empty mapping against the closed-reader sentinel: an open
// reader over a zero-length file is empty, not closed, so it behaves like an empty *os.File or
// bytes.Reader. Reporting it as closed breaks every io.ReaderAt consumer that probes an empty
// source, such as archive/zip.
func TestOpenEmptyMappingIsNotClosed(t *testing.T) {
	reader, err := Open(writeFixture(t, "empty-open.bin", nil))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if reader.Len() != 0 {
		t.Fatalf("Len() = %d, want 0", reader.Len())
	}

	if n, err := reader.ReadAt(make([]byte, 4), 0); n != 0 || err != io.EOF {
		t.Fatalf("ReadAt(4, 0) = (%d, %v), want (0, %v)", n, err, io.EOF)
	}
	if n, err := reader.ReadAt(nil, 0); n != 0 || err != nil {
		t.Fatalf("ReadAt(nil, 0) = (%d, %v), want (0, nil)", n, err)
	}
	if window, err := reader.Slice(0, 4); err != nil || len(window) != 0 {
		t.Fatalf("Slice(0, 4) = (%d bytes, %v), want an empty window", len(window), err)
	}

	// Only a closed reader reports itself as closed.
	if err := reader.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := reader.ReadAt(make([]byte, 1), 0); err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("ReadAt after Close = %v, want a closed-reader error", err)
	}
	if _, err := reader.Slice(0, 1); err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("Slice after Close = %v, want a closed-reader error", err)
	}
}

// TestOpenEmptyFile pins the empty mapping: it has no content, it closes cleanly, and a reader
// over it reports the end of the content instead of a broken reader.
func TestOpenEmptyFile(t *testing.T) {
	path := writeFixture(t, "empty.bin", nil)

	reader, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if reader.Len() != 0 {
		t.Fatalf("Len() = %d, want 0", reader.Len())
	}
	if reader.File() == nil {
		t.Fatal("a reader without a mapping retained no descriptor")
	}

	stream := NewReader(reader)
	if n, err := stream.Read(make([]byte, 4)); n != 0 || err != io.EOF {
		t.Fatalf("Read() = (%d, %v), want (0, %v)", n, err, io.EOF)
	}

	if err := reader.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if reader.File() != nil {
		t.Fatal("File() after Close = a descriptor, want nil")
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("second Close = %v, want nil", err)
	}
}

// TestReaderRetainsItsDescriptor pins the descriptor ownership every platform must implement: the
// reader keeps the descriptor it opened, reads through it after the path is gone, and closes it
// together with its mapping.
func TestReaderRetainsItsDescriptor(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a Windows file cannot be unlinked while it is open")
	}

	content := []byte("descriptor contract fixture")
	path := writeFixture(t, "descriptor.bin", content)
	reader, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// The reader owns an open descriptor for its whole lifecycle, which is what lets its caller
	// keep using the file after the path itself is gone.
	file := reader.File()
	if file == nil {
		t.Fatal("File() = nil, want the descriptor the reader opened")
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := file.Stat(); err != nil {
		t.Fatalf("retained descriptor is not open: %v", err)
	}

	buf := make([]byte, len(content))
	n, err := reader.ReadAt(buf, 0)
	if err != nil && err != io.EOF {
		t.Fatalf("ReadAt: %v", err)
	}
	if n != len(content) || !bytes.Equal(buf, content) {
		t.Fatalf("ReadAt() = %d bytes %q, want %q", n, buf, content)
	}

	// Closing releases the descriptor as well, and stays idempotent.
	if err := reader.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if reader.File() != nil {
		t.Fatal("File() after Close = a descriptor, want nil")
	}
	if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("descriptor stat after Close = %v, want %v", err, os.ErrClosed)
	}
	if _, err := reader.ReadAt(buf, 0); err == nil {
		t.Fatal("ReadAt after Close = nil error")
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("second Close = %v, want nil", err)
	}
}

// TestOpenMissingFile pins the open failure: a path that cannot be opened is an error.
func TestOpenMissingFile(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "missing.bin")); err == nil {
		t.Fatal("Open() error = nil, want a missing-file failure")
	}
}

// TestSliceBounds pins the window contract: a window inside the content is returned as-is, a
// window past the end is clamped, and an invalid window is rejected instead of panicking.
func TestSliceBounds(t *testing.T) {
	content := []byte("0123456789")
	reader, err := Open(writeFixture(t, "slice.bin", content))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = reader.Close() })

	got, err := reader.Slice(2, 3)
	if err != nil || string(got) != "234" {
		t.Fatalf("Slice(2, 3) = %q / %v, want %q", got, err, "234")
	}

	// A window that reaches past the end is clamped to the content.
	got, err = reader.Slice(8, 100)
	if err != nil || string(got) != "89" {
		t.Fatalf("Slice(8, 100) = %q / %v, want %q", got, err, "89")
	}

	// A limit near the integer maximum must not wrap the bounds check and panic.
	got, err = reader.Slice(4, math.MaxInt64)
	if err != nil || string(got) != "456789" {
		t.Fatalf("Slice(4, MaxInt64) = %q / %v, want %q", got, err, "456789")
	}

	// The empty window at the very end of the content is valid.
	got, err = reader.Slice(int64(len(content)), 0)
	if err != nil || len(got) != 0 {
		t.Fatalf("Slice(%d, 0) = %q / %v, want an empty window", len(content), got, err)
	}

	invalid := []struct {
		name       string
		off, limit int64
	}{
		{name: "negative offset", off: -1, limit: 1},
		{name: "negative limit", off: 0, limit: -1},
		{name: "offset past the end", off: int64(len(content)) + 1, limit: 1},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := reader.Slice(tt.off, tt.limit); err == nil {
				t.Fatalf("Slice(%d, %d) error = nil", tt.off, tt.limit)
			}
		})
	}
}

// TestReaderReturnsEOFAtEnd pins the io.Reader contract: the end of the mapped content is
// io.EOF, never a zero-length read with a nil error that would spin the caller.
func TestReaderReturnsEOFAtEnd(t *testing.T) {
	content := []byte("reader contract fixture")
	reader, err := Open(writeFixture(t, "reader.bin", content))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = reader.Close() })

	stream := NewReader(reader)
	var got bytes.Buffer
	buf := make([]byte, 4)
	for {
		n, err := stream.Read(buf)
		if n > 0 {
			got.Write(buf[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Read() error = %v", err)
		}
		if n == 0 {
			t.Fatal("Read() returned no bytes and no error at the end of the content")
		}
		if got.Len() > len(content) {
			t.Fatal("Read() produced more bytes than the mapping holds")
		}
	}
	if !bytes.Equal(got.Bytes(), content) {
		t.Fatalf("read %q, want %q", got.Bytes(), content)
	}

	// The end of the content stays io.EOF for a zero-length read.
	if n, err := stream.Read(nil); n != 0 || err != io.EOF {
		t.Fatalf("Read(nil) at the end = (%d, %v), want (0, %v)", n, err, io.EOF)
	}
}
