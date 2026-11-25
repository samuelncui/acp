package acp

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestStreamReportsSubmissionFailureAsRunError(t *testing.T) {
	// A submission the run no longer accepts is a run error, and the items it did accept keep
	// their own outcomes.
	root := t.TempDir()
	input := writeSourceFile(t, root, "source.txt", []byte("fixture"))
	accepted := newFixtureItem(input, filepath.Join(root, "target.txt"))
	refused := newFixtureItem(input, filepath.Join(root, "other.txt"))

	fixture := newStreamFixture(accepted)
	stream, err := NewStream(context.Background(), fixture.onResults, Overwrite(true))
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Submit(accepted); err != nil {
		t.Fatalf("Submit() error = %v, want the first item accepted", err)
	}

	// Close ends the feed, so the run accepts no more work.
	if err := stream.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := stream.Submit(refused); err == nil {
		t.Fatal("Submit() error = nil after Close, want a refusal")
	}

	result, terminalErr := accepted.terminal(t)
	if terminalErr != nil {
		t.Fatalf("item %q failed: %v", accepted.source, terminalErr)
	}
	if len(result.Targets) != 1 || result.Targets[0].Err != nil {
		t.Fatalf("item %q targets = %v", accepted.source, result.Targets)
	}
	if err := stream.Wait(); err == nil {
		t.Fatal("Wait() error = nil, want the refused submission")
	}
	if refused.callbackCount() != 0 {
		t.Fatalf("the refused item received %d outcomes, want none", refused.callbackCount())
	}
}

func TestRunAppliesBoundedBackpressure(t *testing.T) {
	root := t.TempDir()
	input := writeSourceFile(t, root, "source.txt", nil)

	const (
		total       = int64(2048)
		readBuffer  = 8
		consumedMax = total / 2
	)

	var consumed, produced, maximum int64
	items := make([]*fixtureItem, 0, total)
	for index := int64(0); index < total; index++ {
		item := newFixtureItem(input, filepath.Join(root, "target", fmt.Sprintf("%04d", index)))
		item.hook = func(*Result, error) { atomic.AddInt64(&consumed, 1) }
		items = append(items, item)
	}

	// A bounded read buffer must stop the producer from submitting the whole input up front.
	stream, err := NewStream(
		context.Background(),
		newStreamFixture(items...).onResults,
		WithReadBuffer(readBuffer),
		WithResultBuffer(1), WithResultBatch(1),
		SetToDevice(LinearDevice(true)),
	)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for _, item := range items {
			if err := stream.Submit(item); err != nil {
				break
			}

			id := atomic.AddInt64(&produced, 1)
			outstanding := id - atomic.LoadInt64(&consumed)
			for {
				previous := atomic.LoadInt64(&maximum)
				if outstanding <= previous || atomic.CompareAndSwapInt64(&maximum, previous, outstanding) {
					break
				}
			}
		}
		_ = stream.Close()
	}()

	// The producer owns the feed, so it closes the stream before the run can be waited for.
	<-done
	if err := stream.Wait(); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt64(&consumed); got != total {
		t.Fatalf("consumed %d items, want %d", got, total)
	}
	if got := atomic.LoadInt64(&maximum); got >= consumedMax {
		t.Fatalf("maximum outstanding items = %d, want less than %d", got, consumedMax)
	}
}

func TestRunStopsFeedingItemsAfterGracefulStop(t *testing.T) {
	// The first delivered result stops the caller's feed, and the feed learns it from Submit.
	root := t.TempDir()
	input := writeSourceFile(t, root, "source.txt", []byte("fixture"))
	item := newFixtureItem(input, filepath.Join(root, "target.txt"))
	fixture := newStreamFixture(item)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := NewStream(ctx, fixture.onResults, WithResultBuffer(1), WithResultBatch(1))
	if err != nil {
		t.Fatal(err)
	}
	fixture.hook = cancelOnFirstResult(cancel)

	done := make(chan error, 1)
	go func() {
		if err := stream.Submit(item); err != nil {
			done <- err
			return
		}

		<-ctx.Done()
		done <- stream.Submit(newFixtureItem(input, filepath.Join(root, "other.txt")))
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Submit() error = %v, want %v", err, context.Canceled)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the run did not release the feed after the caller cancelled")
	}

	_ = stream.Close()
	if err := stream.Wait(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait() error = %v, want %v", err, context.Canceled)
	}
	if _, err := item.terminal(t); err != nil {
		t.Fatalf("item failed: %v", err)
	}
}

func TestRunDoesNotReportAStopAfterACompleteRun(t *testing.T) {
	// A context canceled after the feed ended and the pipeline drained is not a run error: the
	// stop must be something the run actually observed.
	root := t.TempDir()
	input := writeSourceFile(t, root, "source.txt", []byte("fixture"))
	item := newFixtureItem(input, filepath.Join(root, "target.txt"))
	fixture := newStreamFixture(item)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fixture.hook = func([]Result) error {
		cancel()
		return nil
	}

	if err := runFixture(ctx, fixture, []Item{item}, WithResultBuffer(1), WithResultBatch(1)); err != nil {
		t.Fatalf("run error = %v, want nil after a complete run", err)
	}
	if _, err := item.terminal(t); err != nil {
		t.Fatalf("item failed: %v", err)
	}
}

func TestRunCancellationDrainsPrefetchedItems(t *testing.T) {
	// Feed work until the pipeline holds prepared content, then cancel with buffers still queued.
	trackChunkPool(t)
	root := t.TempDir()
	input := writeSourceFile(t, root, "source.txt", []byte("fixture"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		lock     sync.Mutex
		produced []*fixtureItem
	)
	fixture := newStreamFixture()
	stream, err := NewStream(ctx, fixture.onResults, SetToDevice(LinearDevice(true)),
		WithHashPolicy(HashReadRefresh), WithResultBuffer(1), WithResultBatch(1))
	if err != nil {
		t.Fatal(err)
	}
	fixture.hook = cancelOnFirstResult(cancel)

	// The producer stops at the feed checkpoint, exactly like a caller that watches the run.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for index := 0; ; index++ {
			item := newFixtureItem(input, filepath.Join(root, "target", fmt.Sprintf("%04d", index)))
			if err := stream.Submit(item); err != nil {
				return
			}

			lock.Lock()
			produced = append(produced, item)
			lock.Unlock()
			fixture.add(item)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the feed did not stop after cancellation")
	}
	_ = stream.Close()

	// Every produced item must report exactly one outcome.
	lock.Lock()
	items := append([]*fixtureItem(nil), produced...)
	lock.Unlock()
	if len(items) == 0 {
		t.Fatal("the feed produced no items")
	}
	for _, item := range items {
		if got := item.callbackCount(); got != 1 {
			t.Fatalf("item %q received %d outcomes, want 1", item.source, got)
		}
	}
}
