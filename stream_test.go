package acp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// collectResults returns a results callback that records every delivered batch in order.
func collectResults() (func([]Result) error, func() [][]Result) {
	var (
		lock    sync.Mutex
		batches [][]Result
	)
	callback := func(results []Result) error {
		lock.Lock()
		defer lock.Unlock()
		batches = append(batches, append([]Result(nil), results...))
		return nil
	}
	snapshot := func() [][]Result {
		lock.Lock()
		defer lock.Unlock()
		return append([][]Result(nil), batches...)
	}
	return callback, snapshot
}

// TestFailedResultBypassesTheResultBuffer pins error rule 1: a result that carries an error is
// delivered immediately instead of waiting for a batch or the flush interval, so a caller can
// persist it before it submits the next batch. Both error outlets count: the item's own error and
// a requested target that was not written.
func TestFailedResultBypassesTheResultBuffer(t *testing.T) {
	root := t.TempDir()
	good := newFixtureItem(
		writeSourceFile(t, root, "good.txt", []byte("fixture")),
		filepath.Join(root, "good-target.txt"),
	)
	missing := newFixtureItem(filepath.Join(root, "missing.txt"), filepath.Join(root, "missing-target.txt"))
	failedTarget := newFixtureItem(
		writeSourceFile(t, root, "failed-target.txt", []byte("fixture")),
		// A regular file in the way of the target directory, so the item completes with a
		// failed target and no item-level error.
		filepath.Join(writeSourceFile(t, root, "not-a-directory", []byte("fixture")), "failed-target.txt"),
	)

	delivered := make(chan []Result, 4)
	// An interval long enough that only the rules under test can deliver a success.
	stream, err := NewStream(context.Background(), func(results []Result) error {
		delivered <- append([]Result(nil), results...)
		return nil
	}, WithResultFlushInterval(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Submit(good, missing, failedTarget); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}

	// Both failures arrive as their own batch, in whatever order the pipeline completed them.
	for _, want := range []Item{missing, failedTarget} {
		select {
		case results := <-delivered:
			if len(results) != 1 || results[0].Job != want {
				t.Fatalf("delivered %#v, want %#v alone", results, want)
			}
			if want == Item(missing) && results[0].Err == nil {
				t.Fatalf("delivered %#v, want the item error", results)
			}
			if want == Item(failedTarget) {
				if results[0].Err != nil {
					t.Fatalf("delivered %#v, want a completed item with a failed target", results)
				}
				if len(results[0].Targets) != 1 || results[0].Targets[0].Err == nil {
					t.Fatalf("delivered %#v, want the failed target outcome", results)
				}
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("a failed result (%#v) waited for the result buffer", want)
		}
	}

	// The successful result is still buffered at this point.
	select {
	case results := <-delivered:
		t.Fatalf("a successful result was delivered before the buffer filled: %#v", results)
	case <-time.After(50 * time.Millisecond):
	}

	if err := stream.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	select {
	case results := <-delivered:
		if len(results) != 1 || results[0].Job != Item(good) || results[0].Err != nil {
			t.Fatalf("delivered %#v, want the successful item", results)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not flush the buffered success")
	}
}

// TestSuccessResultsAreBatchedAndFlushedOnClose pins error rule 2 for successes: they are
// buffered into batches, delivered when a batch fills, and flushed by Close.
func TestSuccessResultsAreBatchedAndFlushedOnClose(t *testing.T) {
	t.Run("buffered until close", func(t *testing.T) {
		root := t.TempDir()
		const total = 8

		items := make([]Item, 0, total)
		for index := 0; index < total; index++ {
			name := fmt.Sprintf("%02d.txt", index)
			source := writeSourceFile(t, root, name, []byte(name))
			items = append(items, newFixtureItem(source, filepath.Join(root, "target", name)))
		}

		callback, batches := collectResults()
		stream, err := NewStream(
			context.Background(),
			callback,
			// One slot and one batch slot more than the run produces, so only Close can
			// deliver a success here.
			WithResultBuffer(total+1),
			WithResultBatch(total+1),
			WithResultFlushInterval(time.Hour),
			Overwrite(true),
		)
		if err != nil {
			t.Fatal(err)
		}
		if err := stream.Submit(items...); err != nil {
			t.Fatalf("Submit() error = %v", err)
		}
		time.Sleep(100 * time.Millisecond)
		if got := batches(); len(got) != 0 {
			t.Fatalf("delivered %d batches before Close, want none", len(got))
		}

		// Close is the only delivery point a run of successful items has here.
		if err := stream.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
		got := batches()
		if len(got) != 1 {
			t.Fatalf("delivered %d batches, want one", len(got))
		}
		if len(got[0]) != total {
			t.Fatalf("delivered %d results, want %d", len(got[0]), total)
		}
	})

	t.Run("delivered when the batch fills", func(t *testing.T) {
		root := t.TempDir()
		const (
			total = 8
			// The batch is what decides delivery, while the result buffer is only the depth
			// of the queue behind it.
			batch        = 2
			resultBuffer = 1
		)

		items := make([]Item, 0, total)
		for index := 0; index < total; index++ {
			name := fmt.Sprintf("%02d.txt", index)
			source := writeSourceFile(t, root, name, []byte(name))
			items = append(items, newFixtureItem(source, filepath.Join(root, "target", name)))
		}

		delivered := make(chan []Result, total)
		stream, err := NewStream(context.Background(), func(results []Result) error {
			delivered <- append([]Result(nil), results...)
			return nil
		}, WithResultBuffer(resultBuffer), WithResultBatch(batch), WithResultFlushInterval(time.Hour), Overwrite(true))
		if err != nil {
			t.Fatal(err)
		}
		if err := stream.Submit(items...); err != nil {
			t.Fatalf("Submit() error = %v", err)
		}

		select {
		case results := <-delivered:
			if len(results) != batch {
				t.Fatalf("first batch = %d results, want %d", len(results), batch)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the result batch never filled")
		}
		if err := stream.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})
}

// TestResultsCallbackErrorActsAsACancellation pins error rule 3: an error returned by the
// results callback stops the feed, reports the items it had not started with that error, keeps
// the items already past the read stage, and is the run's terminal error.
func TestResultsCallbackErrorActsAsACancellation(t *testing.T) {
	root := t.TempDir()
	const total = 24

	items := make([]Item, 0, total)
	for index := 0; index < total; index++ {
		name := fmt.Sprintf("%02d.txt", index)
		source := writeSourceFile(t, root, name, []byte(strings.Repeat(name, 64)))
		items = append(items, newFixtureItem(source, filepath.Join(root, "target", name)))
	}

	stopErr := errors.New("results sink failed")
	var (
		lock      sync.Mutex
		delivered []Result
		calls     int
	)
	onResults := func(results []Result) error {
		lock.Lock()
		delivered = append(delivered, results...)
		calls++
		fail := calls >= 2
		lock.Unlock()

		if fail {
			return stopErr
		}
		return nil
	}

	// One writer serializes the items, so a stop still finds work inside the read pipeline, and
	// one result per delivery lets the callback fail on its second call.
	stream, err := NewStream(context.Background(), onResults, WithResultBuffer(1), WithResultBatch(1), SetToDevice(DeviceThreads(1)))
	if err != nil {
		t.Fatal(err)
	}

	// The batch in hand is submitted completely; the next submission is refused once the
	// callback error has become the run's stop.
	if err := stream.Submit(items...); err != nil {
		t.Fatalf("Submit() error = %v, want the batch in hand accepted", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for stream.callbackStop() == nil {
		if time.Now().After(deadline) {
			t.Fatal("the callback error never became the run's stop")
		}
		time.Sleep(time.Millisecond)
	}
	if err := stream.Submit(newFixtureItem(items[0].(*fixtureItem).source)); err == nil {
		t.Fatal("Submit() error = nil, want a refusal after the callback failed")
	}
	// Close reports the flush error, which is the callback failure that stopped the run.
	if err := stream.Close(); !errors.Is(err, stopErr) {
		t.Fatalf("Close() error = %v, want %v", err, stopErr)
	}
	if err := stream.Wait(); !errors.Is(err, stopErr) {
		t.Fatalf("Wait() error = %v, want %v", err, stopErr)
	}

	// Every accepted item is reported exactly once, either with the facts of a finished item
	// or with the callback error that stopped the run.
	lock.Lock()
	defer lock.Unlock()
	outcomes := make(map[Item]Result, len(delivered))
	for _, result := range delivered {
		if _, reported := outcomes[result.Job]; reported {
			t.Fatalf("item %v was reported more than once", result.Job)
		}
		outcomes[result.Job] = result
	}
	if len(outcomes) != total {
		t.Fatalf("reported items = %d, want %d", len(outcomes), total)
	}

	completed, cancelled := 0, 0
	for _, item := range items {
		result, ok := outcomes[item]
		if !ok {
			t.Fatalf("item %q received no result", item.(*fixtureItem).source)
		}
		if result.Err != nil {
			if !errors.Is(result.Err, stopErr) {
				t.Fatalf("cancelled item %v error = %v, want %v", item, result.Err, stopErr)
			}
			cancelled++
			continue
		}
		if len(result.Targets) != 1 || result.Targets[0].Err != nil {
			t.Fatalf("finished item %v targets = %v", item, result.Targets)
		}
		completed++
	}
	if completed == 0 {
		t.Fatal("no item past the read stage delivered its result")
	}
	if cancelled == 0 {
		t.Fatal("no item inside the read pipeline was reported as cancelled")
	}
}

// TestSubmitBlocksWhileTheReadBufferIsFull pins the feed's backpressure: Submit blocks until the
// read buffer has room instead of growing without bound.
func TestSubmitBlocksWhileTheReadBufferIsFull(t *testing.T) {
	root := t.TempDir()
	input := writeSourceFile(t, root, "source.txt", []byte("fixture"))
	copyer := newTestStream(t, WithReadBuffer(1))

	if err := copyer.Submit(newFixtureItem(input)); err != nil {
		t.Fatalf("Submit() error = %v, want the first item accepted", err)
	}

	blocked := make(chan error, 1)
	go func() { blocked <- copyer.Submit(newFixtureItem(input, filepath.Join(root, "target.txt"))) }()
	select {
	case err := <-blocked:
		t.Fatalf("Submit() returned %v while the read buffer was full", err)
	case <-time.After(50 * time.Millisecond):
	}

	// Draining one item releases the blocked submission.
	<-copyer.readCh
	select {
	case err := <-blocked:
		if err != nil {
			t.Fatalf("Submit() error = %v after the read buffer drained", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Submit stayed blocked after the read buffer drained")
	}
}

// TestWrapStopsAPanickingPipeline pins the fatal-panic boundary: a panic that escapes pipeline
// code ends the run, so a blocked handoff can never leave Wait waiting for a stage that died.
func TestWrapStopsAPanickingPipeline(t *testing.T) {
	logs := captureLogs(t)
	root := t.TempDir()
	input := writeSourceFile(t, root, "source.txt", []byte("fixture"))
	copyer := newTestStream(t, WithReadBuffer(1))

	// Fill the read buffer, so the next submission blocks with no consumer to release it.
	if err := copyer.Submit(newFixtureItem(input)); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	blocked := make(chan error, 1)
	go func() { blocked <- copyer.Submit(newFixtureItem(input)) }()

	copyer.wrap(context.Background(), func() { panic("worker fixture") })

	select {
	case err := <-blocked:
		if !errors.Is(err, errStreamStopped) {
			t.Fatalf("Submit() error = %v, want %v", err, errStreamStopped)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Submit stayed blocked after a pipeline panic")
	}
	if err := copyer.Wait(); err == nil || !strings.Contains(err.Error(), "worker fixture") {
		t.Fatalf("Wait() error = %v, want the pipeline panic", err)
	}
	if logs.String() == "" {
		t.Fatal("the panic was not logged")
	}
}

// TestShellCopiesAnAccurateJob pins the af05f05c shell end to end: New plus AccurateJob copies
// one exact source to one exact target and publishes a terminal report row for it.
func TestShellCopiesAnAccurateJob(t *testing.T) {
	root := t.TempDir()
	content := []byte("accurate job fixture")
	source := writeSourceFile(t, root, "source.txt", content)

	t.Run("written target", func(t *testing.T) {
		target := filepath.Join(root, "renamed.txt")
		handler, getter := NewReportGetter()

		copyer, err := New(
			context.Background(),
			AccurateJob(source, []string{target}),
			WithHash(true),
			WithEventHandler(handler),
		)
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		if err := copyer.WaitErr(); err != nil {
			t.Fatalf("WaitErr() error = %v, want nil", err)
		}

		got, err := os.ReadFile(target)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, content) {
			t.Fatalf("target content = %q, want %q", got, content)
		}

		report := getter()
		if len(report.Errors) != 0 {
			t.Fatalf("report errors = %v, want none", report.Errors)
		}
		if len(report.Jobs) != 1 {
			t.Fatalf("report rows = %d, want 1", len(report.Jobs))
		}

		row := report.Jobs[0]
		// An exact job keeps the af05f05c row: the source split into path segments under the
		// file-system root, with the additive whole path next to it.
		if row.FullPath != source || row.Base != "/" || !reflect.DeepEqual(row.Path, pathSegments(source)) {
			t.Fatalf("row identity = %#v, want %q as segments below %q", row, source, "/")
		}
		if row.Status != JobStatusFinished || len(row.FailTargets) != 0 {
			t.Fatalf("row = %#v, want a finished row without failures", row)
		}
		if len(row.SuccessTargets) != 1 || row.SuccessTargets[0] != target {
			t.Fatalf("row success targets = %v, want %q", row.SuccessTargets, target)
		}

		wantHash := sha256.Sum256(content)
		if row.SHA256 != hex.EncodeToString(wantHash[:]) {
			t.Fatalf("row SHA256 = %q, want %x", row.SHA256, wantHash)
		}
	})

	t.Run("refused target", func(t *testing.T) {
		// An existing target without Overwrite is refused, which is an item outcome: the row
		// keeps the failure and the run is not an error.
		target := writeSourceFile(t, root, "existing.txt", []byte("existing"))
		handler, getter := NewReportGetter()

		copyer, err := New(context.Background(), AccurateJob(source, []string{target}), WithEventHandler(handler))
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		if err := copyer.WaitErr(); err != nil {
			t.Fatalf("WaitErr() error = %v, want nil: a failed target is an item outcome", err)
		}

		report := getter()
		if len(report.Errors) != 0 {
			t.Fatalf("report errors = %v, want none", report.Errors)
		}
		if len(report.Jobs) != 1 {
			t.Fatalf("report rows = %d, want 1", len(report.Jobs))
		}

		row := report.Jobs[0]
		if row.FullPath != source || row.Status != JobStatusFinished {
			t.Fatalf("row = %#v", row)
		}
		if len(row.FailTargets) != 1 || row.FailTargets[target] == nil {
			t.Fatalf("row fail targets = %v, want %q", row.FailTargets, target)
		}
		if len(row.SuccessTargets) != 0 {
			t.Fatalf("row success targets = %v, want none", row.SuccessTargets)
		}
	})
}

// TestDeliveredBatchesAreFreshSlices pins that a caller may keep what it received: every delivery
// owns its slice, so a later flush never overwrites a batch the caller still holds. Result order is
// unspecified, so the callback records the content of each batch as it arrives and the test
// compares that record with the slices it kept.
func TestDeliveredBatchesAreFreshSlices(t *testing.T) {
	root := t.TempDir()
	const total = 4

	items := make([]Item, 0, total)
	for index := 0; index < total; index++ {
		name := fmt.Sprintf("%02d.txt", index)
		source := writeSourceFile(t, root, name, []byte(name))
		items = append(items, newFixtureItem(source, filepath.Join(root, "target", name)))
	}

	var (
		lock     sync.Mutex
		kept     [][]Result
		observed []string
	)
	callback := func(results []Result) error {
		lock.Lock()
		defer lock.Unlock()
		// Keep the exact slice the callback received, plus what it held at that moment.
		kept = append(kept, results)
		observed = append(observed, results[0].Targets[0].Path)
		return nil
	}
	snapshot := func() ([][]Result, []string) {
		lock.Lock()
		defer lock.Unlock()
		return append([][]Result(nil), kept...), append([]string(nil), observed...)
	}

	stream, err := NewStream(context.Background(), callback,
		WithResultBuffer(1), WithResultBatch(1), WithResultFlushInterval(time.Hour), Overwrite(true))
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Submit(items...); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	kept, observed = snapshot()
	if len(kept) != total {
		t.Fatalf("delivered %d batches, want %d", len(kept), total)
	}
	seen := make(map[string]bool, total)
	for index, batch := range kept {
		if len(batch) != 1 {
			t.Fatalf("batch %d = %d results, want 1", index, len(batch))
		}
		if got := batch[0].Targets[0].Path; got != observed[index] {
			t.Fatalf("batch %d changed after later deliveries: %q, want %q", index, got, observed[index])
		}
		if seen[observed[index]] {
			t.Fatalf("target %q was delivered twice", observed[index])
		}
		seen[observed[index]] = true
	}
}

// TestEventHandlerSeesExactlyOneFinishedEvent pins the event contract: every registration receives
// one EventFinished, after the count and progress events that carry the finished flag.
func TestEventHandlerSeesExactlyOneFinishedEvent(t *testing.T) {
	root := t.TempDir()
	item := newFixtureItem(
		writeSourceFile(t, root, "source.txt", []byte("fixture")),
		filepath.Join(root, "target.txt"),
	)

	var (
		lock   sync.Mutex
		events []Event
	)
	handler := func(event Event) {
		lock.Lock()
		defer lock.Unlock()
		events = append(events, event)
	}

	if err := runFixture(context.Background(), newStreamFixture(item), []Item{item}, WithEventHandler(handler)); err != nil {
		t.Fatalf("run: %v", err)
	}

	lock.Lock()
	defer lock.Unlock()

	finished, countFinished, progressFinished := 0, 0, 0
	for _, event := range events {
		switch e := event.(type) {
		case *EventFinished:
			finished++
		case *EventUpdateCount:
			if e.Finished {
				countFinished++
			}
		case *EventUpdateProgress:
			if e.Finished {
				progressFinished++
			}
		}
	}
	if finished != 1 {
		t.Fatalf("EventFinished delivered %d times, want exactly 1", finished)
	}
	if countFinished != 1 || progressFinished != 1 {
		t.Fatalf("finished count/progress events = %d/%d, want 1/1", countFinished, progressFinished)
	}
}

// panickingItem fails when ACP describes it, which is the caller-code boundary the engine wraps.
type panickingItem struct{}

func (*panickingItem) Source() string    { panic("item source fixture") }
func (*panickingItem) Targets() []string { return nil }

// TestItemPanicBecomesThatItemsResultError pins the outlet of a panic in caller-owned item code:
// the item is reported exactly once with the panic as its error, so the run continues and Wait
// reports no run-level failure.
func TestItemPanicBecomesThatItemsResultError(t *testing.T) {
	callback, batches := collectResults()
	stream, err := NewStream(context.Background(), callback, WithResultFlushInterval(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	item := &panickingItem{}
	if err := stream.Submit(item); err != nil {
		t.Fatalf("Submit() error = %v, want the item accepted", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := stream.Wait(); err != nil {
		t.Fatalf("Wait() error = %v, want nil: a panicking item is an item outcome", err)
	}

	got := batches()
	if len(got) != 1 || len(got[0]) != 1 {
		t.Fatalf("delivered %#v, want one batch of one result", got)
	}
	if got[0][0].Job != Item(item) {
		t.Fatalf("result job = %#v, want the submitted item", got[0][0].Job)
	}
	if got[0][0].Err == nil || !strings.Contains(got[0][0].Err.Error(), "Item.Source panicked") {
		t.Fatalf("result error = %v, want the wrapped item panic", got[0][0].Err)
	}
}
