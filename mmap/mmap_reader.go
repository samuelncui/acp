package mmap

import (
	"fmt"
	"io"
	"os"
	"runtime"
)

// Open opens the named file with one metadata read. Close releases its mapping and descriptor.
func Open(filename string) (*ReaderAt, error) {
	return OpenWithInfo(filename, nil)
}

// OpenWithInfo opens the named file using metadata already collected by the caller. The caller
// keeps the file stable while it is read. A nil info obtains metadata from the opened descriptor.
func OpenWithInfo(filename string, info os.FileInfo) (*ReaderAt, error) {
	// Retain descriptor ownership until the mapping has been acquired successfully.
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer func() {
		if f != nil {
			_ = f.Close()
		}
	}()
	if info == nil {
		info, err = f.Stat()
		if err != nil {
			return nil, err
		}
	}

	// Reject lengths that cannot be represented by a mapping on this platform.
	size := info.Size()
	if size < 0 {
		return nil, fmt.Errorf("mmap: file %q has negative size", filename)
	}
	if size != int64(int(size)) {
		return nil, fmt.Errorf("mmap: file %q is too large", filename)
	}

	// Transfer both resources to the reader; explicit Close owns their release.
	r, err := mapFile(f, int(size))
	if err != nil {
		return nil, err
	}
	runtime.SetFinalizer(r, (*ReaderAt).Close)
	f = nil
	return r, nil
}

type Reader struct {
	*ReaderAt
	index int64
}

// File returns the descriptor the mapping was created from, or nil once the reader is closed. The
// reader owns that descriptor for its whole lifecycle: Close removes the mapping and then closes
// it, so a caller that still needs the descriptor keeps the reader open until it is done. It must
// also keep the reader reachable, because the finalizer closes the descriptor once the reader is
// unreachable, and holding only the returned *os.File does not keep the reader alive.
func (r *ReaderAt) File() *os.File {
	return r.file
}

func NewReader(readerAt *ReaderAt) *Reader {
	return &Reader{ReaderAt: readerAt}
}

// Read implements io.Reader. The end of the mapped content is reported as io.EOF, never as a
// zero-length read with a nil error, so a caller that reads until EOF cannot spin.
func (r *Reader) Read(buf []byte) (n int, err error) {
	if r.index >= int64(r.Len()) {
		return 0, io.EOF
	}

	n, err = r.ReadAt(buf, r.index)
	r.index += int64(n)
	return
}
