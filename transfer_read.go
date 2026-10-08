package acp

import (
	"context"
	"errors"
	"fmt"
	"io"
)

func (t *transfer) readContent() {
	// Every exit closes both data channels, releases the source turn and starts one source Close.
	defer func() {
		if value := recover(); value != nil {
			t.readErr = t.panicFailure("source reader", value)
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
			t.sourceErr = t.job.finishSource()
		})
	}()
	if !t.wait(t.readTurn) || t.noContent {
		return
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
		if done {
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
	done := errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
	if err != nil && !done {
		return 0, false, fmt.Errorf("read source failed, %w", err)
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
