package mmap

type Reader struct {
	*ReaderAt
	index int64
}

func NewReader(readerAt *ReaderAt) *Reader {
	return &Reader{ReaderAt: readerAt}
}

func (r *Reader) Read(buf []byte) (n int, err error) {
	n, err = r.ReadAt(buf, r.index)
	r.index += int64(n)
	return
}
