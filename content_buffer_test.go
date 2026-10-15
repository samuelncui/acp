package acp

import (
	"context"
	"io"
	"io/fs"
	"sync"
	"testing"
	"time"
)

func TestContentUsesBoundedChunksUnderTargetBackpressure(t *testing.T) {
	// A blocked endpoint writer must not let an arbitrarily large member fill memory ahead of it.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entered, release, exceeded := make(chan struct{}), make(chan struct{}), make(chan struct{})
	reader := &contentCountingReader{remaining: 32 << 20, exceeded: exceeded}
	item := &testContentItem{SimpleJob: &SimpleJob{Path: "large member", Dsts: []string{"stream target"}}}
	item.source = testContentSource(func(context.Context) (fs.File, error) {
		return &testContentFile{
			ReadCloser: io.NopCloser(reader),
			metadata:   ContentMetadata{Info: contentInfo{name: "large", size: reader.remaining, mode: 0600}},
		}, nil
	})
	item.targets = []ContentTarget{testContentTarget(func(context.Context, ContentMetadata) (ContentWriter, error) {
		return &testContentWriter{
			Writer: &contentBlockingWriter{entered: entered, release: release},
			finish: func(error) error { return nil },
		}, nil
	})}
	callback, batches := collectResults()
	stream, err := NewStream(
		ctx,
		callback,
		SetFromDevice(LinearDevice(true)),
		SetToDevice(LinearDevice(true)),
		WithHashPolicy(HashRead),
	)
	if err != nil {
		t.Fatal(err)
	}
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(func() { unblock(); _ = stream.Close() })
	if err := stream.Submit(item); err != nil {
		t.Fatal(err)
	}

	// Existing immutable chunk queues impose backpressure while the first Write retains its chunk.
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case <-exceeded:
		t.Error("source read more than four chunks ahead of blocked Write")
	case <-time.After(100 * time.Millisecond):
	}
	unblock()
	if err := stream.Wait(); err != nil {
		t.Fatal(err)
	}
	if result := batches()[0][0]; result.Err != nil || result.Targets[0].Err != nil || result.Size != 32<<20 {
		t.Fatalf("result=%#v", result)
	}
}

type contentCountingReader struct {
	remaining int64
	read      int64
	exceeded  chan struct{}
	once      sync.Once
}

func (r *contentCountingReader) Read(data []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n := min(len(data), int(r.remaining))
	clear(data[:n])
	r.remaining -= int64(n)
	r.read += int64(n)
	if r.read > 4*batchSize {
		r.once.Do(func() { close(r.exceeded) })
	}
	return n, nil
}

type contentBlockingWriter struct {
	entered, release chan struct{}
	once             sync.Once
}

func (w *contentBlockingWriter) Write(data []byte) (int, error) {
	w.once.Do(func() { close(w.entered); <-w.release })
	return len(data), nil
}
