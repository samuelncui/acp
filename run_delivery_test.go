package acp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestRunReportsEveryAcceptedItemExactlyOnce(t *testing.T) {
	tests := []struct {
		name string
		opts []Option
	}{
		{name: "buffered target"},
		{name: "linear target", opts: []Option{SetToDevice(LinearDevice(true))}},
		// A linear source hands one reader to the copy stage at a time, so a stop that
		// releases its wait instead of draining its input is where items used to disappear.
		{name: "linear source", opts: []Option{SetFromDevice(LinearDevice(true))}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			const total = 16

			items := make([]Item, 0, total)
			fixtures := make([]*fixtureItem, 0, total)
			for index := 0; index < total; index++ {
				name := fmt.Sprintf("%02d.txt", index)
				source := writeSourceFile(t, root, name, []byte(strings.Repeat(name, 64)))
				item := newFixtureItem(source, filepath.Join(root, "target", name))
				items = append(items, item)
				fixtures = append(fixtures, item)
			}

			fixture := newStreamFixture(fixtures...)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			// The read buffer is much smaller than the batch in hand, so the stop lands while
			// the pipeline is still working through accepted items.
			opts := []Option{WithReadBuffer(4), WithResultBuffer(1), WithResultBatch(1)}
			opts = append(opts, test.opts...)
			stream, err := NewStream(ctx, fixture.onResults, opts...)
			if err != nil {
				t.Fatal(err)
			}
			fixture.hook = cancelOnFirstResult(cancel)

			// One batch is submitted completely even though the callback stopped the feed, and
			// the run reports what it can still start.
			if err := stream.Submit(items...); err != nil {
				t.Fatalf("Submit() error = %v, want the batch in hand accepted", err)
			}
			_ = stream.Close()
			// A stop the run observed is the run's error, while a pipeline that drained before
			// the callback cancelled reports success, exactly as
			// TestRunDoesNotReportAStopAfterACompleteRun pins. Which of the two happens depends
			// on how far the pipeline got, so only the stopping reason is asserted here.
			if err := stream.Wait(); err != nil && !errors.Is(err, context.Canceled) {
				t.Fatalf("Wait() error = %v, want nil or %v", err, context.Canceled)
			}

			// The stop must not break the accounting: every accepted item owns exactly one
			// terminal outcome, none of them is duplicated, and an item the stop reached is
			// reported with the stopping reason instead of a completion. How many items are
			// abandoned depends on how far the pipeline got before the callback cancelled, so
			// the test asserts the accounting rather than a count: a fast run may finish every
			// item it accepted.
			completed := 0
			for _, item := range fixtures {
				if got := item.callbackCount(); got != 1 {
					t.Fatalf("item %q received %d outcomes, want 1", item.source, got)
				}
				result, itemErr := item.terminal(t)
				if itemErr != nil {
					if !errors.Is(itemErr, context.Canceled) {
						t.Fatalf("abandoned item %q error = %v, want %v", item.source, itemErr, context.Canceled)
					}
					continue
				}
				if len(result.Targets) != 1 || result.Targets[0].Err != nil {
					t.Fatalf("completed item %q targets = %v", item.source, result.Targets)
				}
				if _, err := os.Stat(result.Targets[0].Path); err != nil {
					t.Fatalf("completed item %q target %q: %v", item.source, result.Targets[0].Path, err)
				}
				completed++
			}

			// The delivery that cancelled the run carries the first result the pipeline produced,
			// and a result produced before the stop cannot carry the stopping reason, so the run
			// always completes at least the item whose delivery stopped it.
			if completed == 0 {
				t.Fatal("no item finished before the stop")
			}
		})
	}
}

func TestRunDeliversResultsFromOneGoroutine(t *testing.T) {
	// ACP delivers results one batch at a time and from a single goroutine, so a caller may
	// keep unguarded state in its callback.
	root := t.TempDir()
	const total = 32

	var (
		lock        sync.Mutex
		goroutines  = map[string]struct{}{}
		callbackErr []error
		overlapped  int32
		inFlight    int32
	)

	items := make([]Item, 0, total)
	fixtures := make([]*fixtureItem, 0, total)
	for index := 0; index < total; index++ {
		name := fmt.Sprintf("%02d.txt", index)
		content := []byte(strings.Repeat(name, 32))
		source := writeSourceFile(t, root, name, content)
		item := newFixtureItem(source, filepath.Join(root, "target", name))
		items = append(items, item)
		fixtures = append(fixtures, item)
	}

	fixture := newStreamFixture(fixtures...)
	fixture.hook = func(results []Result) error {
		if atomic.AddInt32(&inFlight, 1) != 1 {
			atomic.AddInt32(&overlapped, 1)
		}
		id := currentGoroutineID()
		lock.Lock()
		goroutines[id] = struct{}{}
		lock.Unlock()

		// The callback runs after its item is final, so this test reads a small file to prove
		// it. A callback that does I/O only slows the pipeline down.
		for _, result := range results {
			for _, outcome := range result.Targets {
				if outcome.Err != nil {
					continue
				}
				got, readErr := os.ReadFile(outcome.Path)
				if readErr != nil {
					lock.Lock()
					callbackErr = append(callbackErr, readErr)
					lock.Unlock()
					continue
				}
				if !bytes.Equal(got, contentOfFixture(fixtures, result.Job)) {
					lock.Lock()
					callbackErr = append(callbackErr, fmt.Errorf("target %q content = %q", outcome.Path, got))
					lock.Unlock()
				}
			}
		}

		runtime.Gosched()
		atomic.AddInt32(&inFlight, -1)
		return nil
	}

	if err := runFixture(context.Background(), fixture, items, WithHashPolicy(HashRead), WithResultBuffer(1), WithResultBatch(1)); err != nil {
		t.Fatal(err)
	}

	if overlapped != 0 {
		t.Fatalf("%d callbacks overlapped another callback", overlapped)
	}
	lock.Lock()
	defer lock.Unlock()
	if len(goroutines) != 1 {
		t.Fatalf("callbacks arrived from %d goroutines, want 1", len(goroutines))
	}
	if len(callbackErr) != 0 {
		t.Fatalf("callback I/O failed: %v", callbackErr)
	}
}

// TestRunCallsOneEventGoroutineAtATime pins the contract a handler may rely on: one registered
// handler receives events from one goroutine and never concurrently, which is what lets the
// progress bar keep its total count without a lock.
func TestRunCallsOneEventGoroutineAtATime(t *testing.T) {
	// The contract a handler may rely on: one registered handler receives events from one
	// goroutine and never concurrently, which is what lets the progress bar keep its total
	// count without a lock.
	root := t.TempDir()
	const total = 8

	items := make([]Item, 0, total)
	fixtures := make([]*fixtureItem, 0, total)
	for index := 0; index < total; index++ {
		name := fmt.Sprintf("%02d.txt", index)
		source := writeSourceFile(t, root, name, []byte(name))
		item := newFixtureItem(source, filepath.Join(root, "target", name))
		items = append(items, item)
		fixtures = append(fixtures, item)
	}

	var (
		inFlight  int32
		seen      int
		goroutine string
	)
	handler := func(Event) {
		if !atomic.CompareAndSwapInt32(&inFlight, 0, 1) {
			t.Error("the event handler was called concurrently")
		}
		// Unguarded handler state is the point: the contract is one event at a time.
		seen++
		if id := currentGoroutineID(); goroutine == "" {
			goroutine = id
		} else if id != goroutine {
			t.Errorf("the event handler ran on goroutine %s, want %s", id, goroutine)
		}
		atomic.StoreInt32(&inFlight, 0)
	}

	if err := runFixture(
		context.Background(),
		newStreamFixture(fixtures...),
		items,
		WithHashPolicy(HashRead),
		WithEventHandler(handler),
	); err != nil {
		t.Fatal(err)
	}
	if seen == 0 {
		t.Fatal("the event handler received no event")
	}
}

func TestRunReturnsErrorWhenTheResultsCallbackPanics(t *testing.T) {
	// A panic in caller-owned callback code must become a run error instead of unwinding the
	// reporting goroutine, which would silently drop the accounting of every other item.
	tests := []struct {
		name string
		// broken marks the panicking item as one ACP cannot process, so its failure is the
		// result that panics.
		broken bool
	}{
		{name: "completed item"},
		{name: "failed item", broken: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			logs := captureLogs(t)
			root := t.TempDir()
			const total = 4

			items := make([]Item, 0, total)
			all := make([]*fixtureItem, 0, total)
			fixtures := make([]*fixtureItem, 0, total)
			var panicking *fixtureItem
			for index := 0; index < total; index++ {
				name := fmt.Sprintf("%02d.txt", index)
				source := writeSourceFile(t, root, name, []byte(name))
				if test.broken && index == 1 {
					source = filepath.Join(root, "removed.txt")
				}
				item := newFixtureItem(source, filepath.Join(root, "target", name))
				if index == 1 {
					panicking = item
				} else {
					fixtures = append(fixtures, item)
				}
				all = append(all, item)
				items = append(items, item)
			}

			fixture := newStreamFixture(all...)
			fixture.hook = func(results []Result) error {
				for _, result := range results {
					if result.Job == Item(panicking) {
						panic("callback fixture")
					}
				}
				return nil
			}

			// One result per batch, so a panicking callback only loses the item it panicked on.
			err := runFixture(context.Background(), fixture, items, WithResultBuffer(1), WithResultBatch(1))
			if err == nil {
				t.Fatal("run error = nil, want the callback panic reported")
			}
			if !strings.Contains(err.Error(), "callback fixture") {
				t.Fatalf("run error = %v, want the panicking callback", err)
			}

			// The panic must not take the accounting of the other items with it.
			for _, item := range fixtures {
				item.terminal(t)
			}
			if panicking.callbackCount() != 0 {
				t.Fatalf("panicking item recorded %d outcomes, want none", panicking.callbackCount())
			}
			if strings.Contains(logs.String(), "send on closed channel") {
				t.Fatalf("the pipeline published on a closed channel:\n%s", logs.String())
			}
		})
	}
}

func TestRunReturnsErrorWhenAnEventHandlerPanics(t *testing.T) {
	logs := captureLogs(t)
	root := t.TempDir()
	const total = 4

	items := make([]Item, 0, total)
	fixtures := make([]*fixtureItem, 0, total)
	for index := 0; index < total; index++ {
		name := fmt.Sprintf("%02d.txt", index)
		source := writeSourceFile(t, root, name, []byte(name))
		item := newFixtureItem(source, filepath.Join(root, "target", name))
		items = append(items, item)
		fixtures = append(fixtures, item)
	}

	// A broken handler must fail the run and must not stop the remaining items.
	handler := func(event Event) {
		if _, ok := event.(*EventUpdateCount); ok {
			panic("handler fixture")
		}
	}
	err := runFixture(context.Background(), newStreamFixture(fixtures...), items, WithEventHandler(handler))
	if err == nil {
		t.Fatal("run error = nil, want the handler panic reported")
	}
	if !strings.Contains(err.Error(), "handler fixture") {
		t.Fatalf("run error = %v, want the panicking handler", err)
	}

	// Every item still reports exactly one outcome.
	for _, item := range fixtures {
		item.terminal(t)
	}
	if strings.Contains(logs.String(), "send on closed channel") {
		t.Fatalf("the pipeline published on a closed channel:\n%s", logs.String())
	}
}
