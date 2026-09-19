package mmap

import (
	"io"
	"os"
)

type Reader struct {
	*ReaderAt
	index int64
}

// File returns the descriptor the mapping was created from, or nil once the reader is closed. The
// reader owns that descriptor for its whole lifecycle: Close removes the mapping and then closes
// it, so a caller that still needs the descriptor keeps the reader open until it is done.
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
