package acp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sync"
)

func (t *transfer) readContent() {
	// Every exit closes both data channels, releases the source turn and starts one source Close.
	defer func() {
		if value := recover(); value != nil {
			t.readErr = t.panicFailure("source reader", value)
		}
		// An Open error or panic still releases writers waiting for observed metadata.
		if t.sourceReady != nil {
			t.sourceReadyOnce.Do(func() { close(t.sourceReady) })
		}
		// A content source decides whether its failure ends the stream; a native source's failure
		// reaches the content target's Finish, which decides for the target side.
		if t.job.contentSource != nil && t.readErr != nil && !errors.Is(t.readErr, ErrMemberSettled) {
			t.stopEndpoint(t.readErr)
		}
		close(t.chunks)
		if t.hashes != nil {
			close(t.hashes)
		}
		t.ahead.finishRead(t.job.order)
		close(t.readDone)
		go t.copyer.wrap(context.Background(), func() {
			defer close(t.sourceDone)
			defer func() {
				if value := recover(); value != nil {
					t.sourceErr = t.panicFailure("source close", value)
				}
			}()
			t.sourceErr = t.sourceFailure(t.job.finishSource())
			if t.job.contentSource != nil && !errors.Is(t.sourceErr, ErrMemberSettled) {
				t.stopEndpoint(t.sourceErr)
			}
		})
	}()
	if !t.wait(t.readTurn) || t.noContent {
		return
	}

	// Observe the acquired file once before allowing any output allocation or header emission.
	if t.job.contentSource != nil {
		var file fs.File
		var err error
		panicErr := protectCall("ContentSource.Open", func() {
			file, err = t.job.contentSource.Open(t.copyer.ctx)
		})
		t.job.reader = file
		// Only the source's own failures are tagged; a stop or a panic is not the source's.
		err = errors.Join(t.sourceFailure(err), panicErr, t.copyer.ctx.Err())
		if err == nil && file == nil {
			err = t.sourceFailure(fmt.Errorf("content source returned no file"))
		}
		if err == nil {
			var metadata ContentMetadata
			panicErr = protectCall("content source metadata", func() {
				metadata.Info, err = file.Stat()
				err = errors.Join(t.sourceFailure(err), t.copyer.ctx.Err())
				if err != nil {
					return
				}
				if attrs, ok := file.(XattrReader); ok {
					metadata.Xattrs, err = attrs.Xattrs()
					err = errors.Join(t.sourceFailure(err), t.copyer.ctx.Err())
					if err != nil {
						return
					}
				}
				t.job.metadata, err = snapshotContentMetadata(metadata)
				err = t.sourceFailure(err)
			})
			err = errors.Join(err, panicErr, t.copyer.ctx.Err())
		}
		if err != nil {
			t.readErr, t.job.itemError = err, err
			return
		}
		t.job.stat = contentStat(t.job.metadata)
		t.sourceReadyOnce.Do(func() { close(t.sourceReady) })
		if t.job.stat.size == 0 {
			return
		}
	}

	// A decoder starts with the source's read turn, so prepared items hold no decoder state.
	if t.job.decode != nil {
		if t.readErr = t.decodeSource(); t.readErr != nil {
			return
		}
	}

	// Quota-limited short blocks are not EOF; only the actual reader outcome ends content.
	var offset int64
	for !t.stopped() {
		eof := t.job.stat != nil && offset >= t.job.stat.size
		want, err := t.ahead.wait(t.job.order, t.start+offset, batchSize, eof, t.stop)
		if err != nil {
			t.readErr = err
			return
		}
		if t.stopped() {
			break
		}
		n, done, err := t.readChunk(want)
		offset += int64(n)
		if err != nil {
			t.readErr = err
			return
		}
		// Decoded plaintext ends exactly at its declared size; surplus fails without draining it.
		if t.job.decode != nil && (offset > t.job.stat.size || (done && offset < t.job.stat.size)) {
			t.readErr = t.sourceFailure(fmt.Errorf("%w, decoded size differs from declared size %d", ErrEncodedContent, t.job.stat.size))
			return
		}
		if done {
			if t.job.endpoints && offset != t.job.stat.size {
				t.readErr = t.sourceFailure(fmt.Errorf("content size mismatch, read=%d, expected=%d: %w",
					offset, t.job.stat.size, io.ErrUnexpectedEOF))
			}
			return
		}
	}
	t.readErr = t.stoppedError()
}

func (t *transfer) readChunk(want int) (int, bool, error) {
	// Backing ownership is established before reading; no read slot is held during queue waits.
	chunk, err := t.ahead.acquire(t.job.order, t.stop)
	if err != nil {
		return 0, false, err
	}
	defer chunk.release()
	release, err := t.ahead.readSlot(t.job.order, t.stop)
	if err != nil {
		return 0, false, err
	}
	var n int
	func() {
		defer release()
		n, err = io.ReadFull(t.job.reader, chunk.data[:want])
	}()
	// A decoder's own unexpected EOF is a decode failure, and a settled one is the member's failure;
	// neither is the end of content.
	done := (errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)) &&
		!errors.Is(err, ErrEncodedContent) && !errors.Is(err, ErrMemberSettled)
	if err != nil && !done {
		return 0, false, t.sourceFailure(err)
	}
	if n == 0 {
		return 0, done, nil
	}

	// Hash and target consumers own independent references to the same immutable bytes.
	chunk.data = chunk.data[:n]
	consumers := [2]chan *chunkBuffer{t.chunks, t.hashes}
	for _, dst := range consumers {
		if dst == nil {
			continue
		}
		reference := chunk.retain()
		select {
		case dst <- reference:
		case <-t.stop:
			reference.release()
			return n, false, t.stoppedError()
		case <-t.copyer.hardStop:
			reference.release()
			return n, false, t.stoppedError()
		}
	}
	return n, done, nil
}

// decodeSource wraps the native reader in the item's decoder. A failed start leaves the native
// reader in place, so source Close still releases the descriptor; a started decoder is closed
// with it, and any native read failure it already met surfaces on its first Read.
func (t *transfer) decodeSource() error {
	source := &decodedSource{native: t.job.reader}
	var err error
	panicErr := protectCall("item source decoder", func() { source.decoded, err = t.job.decode(nativeSource{source}) })
	switch {
	case panicErr != nil:
		return panicErr
	case err != nil:
		if failure := source.readFailure(); failure != nil {
			return t.sourceFailure(failure)
		}
		return t.sourceFailure(fmt.Errorf("%w, start decoder failed, %w", ErrEncodedContent, err))
	case source.decoded == nil:
		return t.sourceFailure(fmt.Errorf("source decoder returned no reader"))
	}
	t.job.reader = source
	return nil
}

// decodedSource reads an item decoder's plaintext and keeps decoder failures apart from the
// native source's own read errors. It closes the decoder before the native reader.
type decodedSource struct {
	decoded io.ReadCloser
	native  io.ReadCloser

	// A decoder may read its source on another goroutine.
	lock    sync.Mutex
	readErr error
}

// nativeSource is the native reader an item decoder consumes; it records the first read error.
type nativeSource struct{ source *decodedSource }

func (r nativeSource) Read(data []byte) (int, error) {
	n, err := r.source.native.Read(data)
	if err != nil && err != io.EOF {
		r.source.lock.Lock()
		if r.source.readErr == nil {
			r.source.readErr = err
		}
		r.source.lock.Unlock()
	}
	return n, err
}

func (s *decodedSource) readFailure() error {
	s.lock.Lock()
	defer s.lock.Unlock()
	return s.readErr
}

func (s *decodedSource) Read(data []byte) (int, error) {
	n, err := s.decoded.Read(data)
	if err == nil {
		return n, nil
	}
	if failure := s.readFailure(); failure != nil {
		return n, failure
	}
	if err == io.EOF {
		return n, err
	}
	return n, fmt.Errorf("%w, %w", ErrEncodedContent, err)
}

func (s *decodedSource) Close() (err error) {
	// The native descriptor is released even when the decoder's Close panics.
	defer func() { err = errors.Join(err, s.native.Close()) }()
	// A native read failure keeps its identity here too, as in Read.
	if closeErr := s.decoded.Close(); closeErr != nil {
		if failure := s.readFailure(); failure != nil {
			return failure
		}
		err = fmt.Errorf("%w, close decoder failed, %w", ErrEncodedContent, closeErr)
	}
	return err
}

// sourceFailure marks a source failure with ErrSourceRead once, keeping the cause's identity. A
// context error after the run was cancelled is a stop, not a failure, so it is not tagged; a
// source's own deadline while the run continues is its failure.
func (t *transfer) sourceFailure(err error) error {
	if err == nil || errors.Is(err, ErrSourceRead) {
		return err
	}
	if t.copyer.ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return err
	}
	return fmt.Errorf("%w, %w", ErrSourceRead, err)
}
