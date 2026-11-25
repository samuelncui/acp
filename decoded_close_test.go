package acp

import (
	"context"
	"errors"
	"io"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
)

// closeTrackingSource records whether a native source's reader was closed, and optionally makes
// its reads fail with readErr.
func closeTrackingSource(path string, closed *atomic.Bool, readErr error) Option {
	return withFS(fsHooks{open: func(name string, mode ReadMode, info os.FileInfo) (itemSource, error) {
		source, err := openNativeSource(name, mode, info)
		if err == nil && name == path {
			reader := source.reader
			if readErr != nil {
				reader = failingReader{ReadCloser: reader, err: readErr}
			}
			source.reader = &testContentReader{Reader: reader, close: func() error { closed.Store(true); return reader.Close() }}
		}
		return source, err
	}})
}

// stickyDecoder reads its source through and finishes Close with close.
type stickyDecoder struct {
	io.Reader
	close func() error
}

func (d *stickyDecoder) Close() error { return d.close() }

func TestDecoderClosePanicStillClosesTheNativeSource(t *testing.T) {
	root := t.TempDir()
	path := writeSourceFile(t, root, "stored", []byte("plain"))
	var closed atomic.Bool
	track := closeTrackingSource(path, &closed, nil)
	item := &decodedItem{SimpleJob: &SimpleJob{Path: path}, size: 5, decode: func(src io.Reader) (io.ReadCloser, error) {
		return &stickyDecoder{Reader: src, close: func() error { panic("decoder close exploded") }}, nil
	}}
	onResults, _ := collectResults()
	_ = runStream(context.Background(), onResults, []Item{item}, WithHashPolicy(HashRead), track)
	if !closed.Load() {
		t.Fatal("native source was not closed after the decoder's Close panicked")
	}
}

func TestDecoderCloseKeepsANativeReadFailure(t *testing.T) {
	// A decoder that returns the native read failure from Close does not turn it into a decode failure.
	root := t.TempDir()
	path := writeSourceFile(t, root, "stored", []byte("plain"))
	var closed atomic.Bool
	track := closeTrackingSource(path, &closed, syscall.EIO)
	item := &decodedItem{SimpleJob: &SimpleJob{Path: path}, size: 5, decode: func(src io.Reader) (io.ReadCloser, error) {
		var sticky error
		return &stickyDecoder{Reader: readerFunc(func(p []byte) (int, error) {
			n, err := src.Read(p)
			if err != nil && err != io.EOF {
				sticky = err
			}
			return n, err
		}), close: func() error { return sticky }}, nil
	}}
	onResults, snapshot := collectResults()
	if err := runStream(context.Background(), onResults, []Item{item}, WithHashPolicy(HashRead), track); err != nil {
		t.Fatal(err)
	}
	result := resultsByItem(t, snapshot())[item]
	if !errors.Is(result.Err, syscall.EIO) || errors.Is(result.Err, ErrEncodedContent) {
		t.Fatalf("item error = %q, want the native failure without ErrEncodedContent", result.Err)
	}
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }
