package acp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

// fixtureItem is a caller-owned item that records every terminal outcome, so a test can assert
// the result contract without a report or a sink. The engine reports results through onResults,
// and the fixture below dispatches each result to the item that produced it.
type fixtureItem struct {
	source  string
	targets []string

	// hook observes each terminal outcome before it is recorded.
	hook func(*Result, error)

	lock     sync.Mutex
	results  []*Result
	failures []error
}

func newFixtureItem(source string, targets ...string) *fixtureItem {
	return &fixtureItem{source: source, targets: targets}
}

func (i *fixtureItem) Source() string { return i.source }

func (i *fixtureItem) Targets() []string { return i.targets }

// Completed records one completed item. It is a test helper, not part of Item.
func (i *fixtureItem) Completed(result *Result) { i.record(result, nil) }

// Failed records one item ACP could not process. It is a test helper, not part of Item.
func (i *fixtureItem) Failed(err error) { i.record(nil, err) }

func (i *fixtureItem) record(result *Result, err error) {
	if i.hook != nil {
		i.hook(result, err)
	}

	i.lock.Lock()
	defer i.lock.Unlock()
	if err != nil {
		i.failures = append(i.failures, err)
		return
	}
	i.results = append(i.results, result)
}

// terminal returns the item's single terminal outcome.
func (i *fixtureItem) terminal(t *testing.T) (*Result, error) {
	t.Helper()

	i.lock.Lock()
	defer i.lock.Unlock()
	if total := len(i.results) + len(i.failures); total != 1 {
		t.Fatalf("item %q received %d terminal outcomes, want exactly 1", i.source, total)
	}
	if len(i.failures) == 1 {
		return nil, i.failures[0]
	}
	return i.results[0], nil
}

// callbackCount returns how many terminal outcomes the item received.
func (i *fixtureItem) callbackCount() int {
	i.lock.Lock()
	defer i.lock.Unlock()
	return len(i.results) + len(i.failures)
}

// callbackOrder records the terminal outcome order of several items.
type callbackOrder struct {
	lock  sync.Mutex
	items []*fixtureItem
}

func (o *callbackOrder) record(item *fixtureItem) {
	o.lock.Lock()
	defer o.lock.Unlock()
	o.items = append(o.items, item)
}

func (o *callbackOrder) snapshot() []*fixtureItem {
	o.lock.Lock()
	defer o.lock.Unlock()
	return append([]*fixtureItem(nil), o.items...)
}

// streamFixture is the results callback of one test run: it records every delivered batch and
// dispatches each result to the fixture item that produced it, so a test keeps asserting an
// item's single terminal outcome.
type streamFixture struct {
	lock    sync.Mutex
	items   map[Item]*fixtureItem
	batches [][]Result

	// hook observes every batch before it is dispatched; an error it returns is the run's stop.
	hook func([]Result) error
}

func newStreamFixture(items ...*fixtureItem) *streamFixture {
	fixture := &streamFixture{items: make(map[Item]*fixtureItem, len(items))}
	for _, item := range items {
		fixture.items[item] = item
	}
	return fixture
}

func (f *streamFixture) onResults(results []Result) error {
	f.lock.Lock()
	f.batches = append(f.batches, append([]Result(nil), results...))
	hook := f.hook
	f.lock.Unlock()

	if hook != nil {
		if err := hook(results); err != nil {
			return err
		}
	}

	for _, result := range results {
		f.lock.Lock()
		item := f.items[result.Job]
		f.lock.Unlock()

		if item == nil {
			continue
		}
		if result.Err != nil {
			item.Failed(result.Err)
			continue
		}

		completed := result
		item.Completed(&completed)
	}

	return nil
}

// add registers more fixture items, which a producer that creates items while it feeds needs.
func (f *streamFixture) add(items ...*fixtureItem) {
	f.lock.Lock()
	defer f.lock.Unlock()
	for _, item := range items {
		f.items[item] = item
	}
}

// batchesDelivered returns how many times the run called the results callback.
func (f *streamFixture) batchesDelivered() int {
	f.lock.Lock()
	defer f.lock.Unlock()
	return len(f.batches)
}

// delivered returns every delivered result, in delivery order.
func (f *streamFixture) delivered() []Result {
	f.lock.Lock()
	defer f.lock.Unlock()

	all := make([]Result, 0, len(f.batches))
	for _, batch := range f.batches {
		all = append(all, batch...)
	}
	return all
}

// runStream submits one batch of items, closes the run and returns its terminal error.
func runStream(ctx context.Context, onResults func([]Result) error, items []Item, opts ...Option) error {
	stream, err := NewStream(ctx, onResults, opts...)
	if err != nil {
		return err
	}
	_ = stream.Submit(items...)
	_ = stream.Close()

	return stream.Wait()
}

// runFixture is runStream for a set of fixture items.
func runFixture(ctx context.Context, fixture *streamFixture, items []Item, opts ...Option) error {
	return runStream(ctx, fixture.onResults, items, opts...)
}

// runShell drives one af05f05c shell run and returns its terminal error.
func runShell(ctx context.Context, opts ...Option) error {
	copyer, err := New(ctx, opts...)
	if err != nil {
		return err
	}

	return copyer.WaitErr()
}

// feedBatches submits every batch in order and returns the first refusal, which is what a
// caller-owned feed sees when the run stops accepting work.
func feedBatches(stream *StreamCopyer, batches [][]Item) error {
	for _, batch := range batches {
		if err := stream.Submit(batch...); err != nil {
			return err
		}
	}
	return nil
}

// newTestStream builds the pipeline state a stage-level test needs without starting it.
func newTestStream(t *testing.T, opts ...Option) *StreamCopyer {
	t.Helper()

	option, err := buildOption(opts...)
	if err != nil {
		t.Fatalf("check options: %v", err)
	}

	device := t.TempDir()
	return &StreamCopyer{
		option:    option,
		ctx:       context.Background(),
		readCh:    make(chan *baseJob, option.readBuffer),
		resultCh:  make(chan Result, option.resultBuffer),
		eventCh:   make(chan Event, 256),
		hardStop:  make(chan struct{}),
		onResults: func([]Result) error { return nil },
		getDevice: func(string) (string, error) { return device, nil },
		getDiskUsageCache: func(string) *diskUsageCache {
			return newDiskUsageCache(device, defaultDiskUsageFreshInterval)
		},
	}
}

// runResults drives the reporting stage and the delivery stage of a stage-level test the way the
// pipeline does: the delivery stage drains the result buffer on its own goroutine until the
// reporting stage closed it. It returns once the caller's callback saw everything.
func runResults(copyer *StreamCopyer, copyed <-chan *baseJob) {
	delivered := make(chan struct{})
	go func() {
		defer close(delivered)
		copyer.deliver()
	}()

	copyer.report(copyed)
	close(copyer.resultCh)
	<-delivered
}

// cancelOnFirstResult stops the run from the results callback, which is where a caller observes
// that the pipeline is working.
func cancelOnFirstResult(cancel context.CancelFunc) func([]Result) error {
	var once sync.Once
	return func([]Result) error {
		once.Do(cancel)
		return nil
	}
}

// writeSourceFile writes one source file below dir and returns its path.
func writeSourceFile(t *testing.T, dir, name string, content []byte) string {
	t.Helper()

	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create source directory: %v", err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("write source file %q: %v", path, err)
	}
	return path
}

func TestRunCopiesItemsToLinearTarget(t *testing.T) {
	// Build caller-owned items without constructing copy options per file.
	root := t.TempDir()
	order := new(callbackOrder)
	items := make([]Item, 0, 3)
	fixtures := make([]*fixtureItem, 0, 3)
	for index, content := range []string{"first", "second", "third"} {
		name := string(rune('a'+index)) + ".txt"
		source := writeSourceFile(t, root, filepath.Join("source", name), []byte(content))
		item := newFixtureItem(source, filepath.Join(root, "target", name))
		item.hook = func(*Result, error) { order.record(item) }
		items = append(items, item)
		fixtures = append(fixtures, item)
	}

	// Keep the target serialized while allowing source preparation to complete in any order.
	if err := runFixture(
		context.Background(),
		newStreamFixture(fixtures...),
		items,
		WithHashPolicy(HashRead),
		SetToDevice(LinearDevice(true)),
	); err != nil {
		t.Fatal(err)
	}

	// Result order is unspecified, so the test asserts every item's own outcome and that a
	// serialized target still received them all.
	if reported := order.snapshot(); len(reported) != len(items) {
		t.Fatalf("received %d callbacks, want %d", len(reported), len(items))
	}
	for _, item := range fixtures {
		result, err := item.terminal(t)
		if err != nil {
			t.Fatalf("item %q failed: %v", item.source, err)
		}
		if len(result.Targets) != 1 || result.Targets[0].Err != nil || len(result.SHA256) == 0 {
			t.Fatalf("unexpected result: %#v", result)
		}
		if result.Job != Item(item) {
			t.Fatalf("result job = %#v, want the submitted item", result.Job)
		}
	}
}

func TestForwardPreparedOrdersOnlyLinearTargets(t *testing.T) {
	newJob := func(order uint64, path string) *writeJob {
		return newWriteJob(
			&baseJob{order: order, path: path},
			io.NopCloser(strings.NewReader("fixture")),
			int64(len("fixture")),
			false,
		)
	}
	tests := []struct {
		name    string
		linear  bool
		wantIDs []string
	}{
		{name: "linear request order", linear: true, wantIDs: []string{"1", "3"}},
		{name: "random completion order", wantIDs: []string{"3", "1"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Deliver later preparation results first; a failed or skipped middle request has no write Job.
			copyer := &StreamCopyer{option: &option{
				fromDevice: &deviceOption{threads: 3},
				toDevice:   &deviceOption{linear: test.linear},
			}}
			completed := make(chan prepareResult, 3)
			completed <- prepareResult{order: 2, job: newJob(2, "3")}
			completed <- prepareResult{order: 0, job: newJob(0, "1")}
			completed <- prepareResult{order: 1}
			close(completed)
			prepared := make(chan *writeJob, 3)

			// Forwarding must reorder only the serialized target path.
			copyer.forwardPrepared(completed, prepared)
			close(prepared)
			var ids []string
			for job := range prepared {
				ids = append(ids, job.path)
				job.finishSource()
			}
			if len(ids) != len(test.wantIDs) {
				t.Fatalf("prepared paths = %v, want %v", ids, test.wantIDs)
			}
			for index := range ids {
				if ids[index] != test.wantIDs[index] {
					t.Fatalf("prepared paths = %v, want %v", ids, test.wantIDs)
				}
			}
		})
	}
}

func TestRunHashesWithoutTargets(t *testing.T) {
	// Submit one targetless item through the same batch interface.
	content := []byte("hash-only fixture")
	source := writeSourceFile(t, t.TempDir(), "source.txt", content)
	item := newFixtureItem(source)

	// Verify ACP reads the source once and reports its SHA-256 without creating a target.
	if err := runFixture(context.Background(), newStreamFixture(item), []Item{item}, WithHashPolicy(HashRead)); err != nil {
		t.Fatal(err)
	}
	result, err := item.terminal(t)
	if err != nil {
		t.Fatalf("item failed: %v", err)
	}

	wantHash := sha256.Sum256(content)
	if got := hex.EncodeToString(result.SHA256); got != hex.EncodeToString(wantHash[:]) {
		t.Fatalf("SHA256 = %q, want %q", got, hex.EncodeToString(wantHash[:]))
	}
	if len(result.Targets) != 0 {
		t.Fatalf("hash-only targets = %v, want none", result.Targets)
	}
	if result.SignatureCacheHit {
		t.Fatal("hash-only item reported a signature cache hit")
	}
}

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

func TestRunRejectsReuseOnlyPolicyForItemsWithTargets(t *testing.T) {
	// A copy always reads its source and produces a computed hash, so a value that promises a
	// stored hash cannot describe it. The policy is rejected as an option error for the item
	// instead of silently reading.
	for _, policy := range []HashPolicy{HashCachedOnly, HashCachedOrRead} {
		t.Run(policy.String(), func(t *testing.T) {
			root := t.TempDir()
			input := writeSourceFile(t, root, "source.txt", []byte("fixture"))
			target := filepath.Join(root, "target.txt")
			item := newFixtureItem(input, target)

			if err := runFixture(context.Background(), newStreamFixture(item), []Item{item}, WithHashPolicy(policy)); err != nil {
				t.Fatalf("run error = %v, want nil: a rejected item is an item outcome", err)
			}
			if result, err := item.terminal(t); err == nil {
				t.Fatalf("item completed with %#v, want the rejected policy", result)
			} else if !strings.Contains(err.Error(), "check hash policy failed") ||
				!strings.Contains(err.Error(), policy.String()) {
				t.Fatalf("item error = %v, want a rejected %s policy", err, policy)
			}

			// The rejected item must not have touched the target.
			if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("stat target error = %v, want %v", err, os.ErrNotExist)
			}
		})
	}
}

func TestRunKeepsEveryOtherPolicyForItemsWithTargets(t *testing.T) {
	// The remaining values read content or need none, so a transfer can use all of them.
	for _, policy := range []HashPolicy{HashOff, HashCachedOrReadRefresh, HashRead, HashReadRefresh} {
		t.Run(policy.String(), func(t *testing.T) {
			root := t.TempDir()
			content := []byte("policy fixture")
			input := writeSourceFile(t, root, "source.txt", content)
			target := filepath.Join(root, "target.txt")
			item := newFixtureItem(input, target)

			if err := runFixture(context.Background(), newStreamFixture(item), []Item{item}, WithHashPolicy(policy)); err != nil {
				t.Fatalf("run error = %v", err)
			}
			result, err := item.terminal(t)
			if err != nil {
				t.Fatalf("item failed: %v", err)
			}
			if len(result.Targets) != 1 || result.Targets[0].Err != nil || result.Targets[0].Path != target {
				t.Fatalf("targets = %#v, want %q written", result.Targets, target)
			}

			got, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, content) {
				t.Fatalf("target content = %q, want %q", got, content)
			}

			// Only a reading policy produces the computed hash of the copied bytes.
			wantHash := sha256.Sum256(content)
			if policy.producesHash() {
				if !bytes.Equal(result.SHA256, wantHash[:]) {
					t.Fatalf("SHA256 = %x, want %x", result.SHA256, wantHash)
				}
			} else if len(result.SHA256) != 0 {
				t.Fatalf("SHA256 = %x, want none", result.SHA256)
			}
		})
	}
}

func TestRunReportsTargetFailureAsItemOutcome(t *testing.T) {
	// Force target creation to fail while the item still completes.
	root := t.TempDir()
	input := writeSourceFile(t, root, "source.txt", []byte("fixture"))
	target := writeSourceFile(t, root, "target.txt", []byte("existing"))
	item := newFixtureItem(input, target)

	// A failed target is an item outcome with its own error identity, not a run error.
	if err := runFixture(context.Background(), newStreamFixture(item), []Item{item}); err != nil {
		t.Fatalf("run error = %v, want nil", err)
	}
	result, err := item.terminal(t)
	if err != nil {
		t.Fatalf("item failed: %v", err)
	}
	if len(result.Targets) != 1 {
		t.Fatalf("targets = %v, want one outcome", result.Targets)
	}
	if !errors.Is(result.Targets[0].Err, fs.ErrExist) {
		t.Fatalf("target error = %v, want %v", result.Targets[0].Err, fs.ErrExist)
	}

	// The refused target must keep its previous content.
	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(content, []byte("existing")) {
		t.Fatalf("target content = %q, want %q", content, "existing")
	}
}

func TestRunReportsOneOutcomePerRequestedTargetInOrder(t *testing.T) {
	// Request two targets where only the second one can be written.
	root := t.TempDir()
	input := writeSourceFile(t, root, "source.txt", []byte("fixture"))
	refused := writeSourceFile(t, root, "refused.txt", []byte("existing"))
	written := filepath.Join(root, "written.txt")
	item := newFixtureItem(input, refused, written)

	if err := runFixture(context.Background(), newStreamFixture(item), []Item{item}); err != nil {
		t.Fatalf("run error = %v, want nil", err)
	}
	result, err := item.terminal(t)
	if err != nil {
		t.Fatalf("an item with a failed target must still complete: %v", err)
	}
	if len(result.Targets) != 2 {
		t.Fatalf("targets = %v, want one outcome per requested target", result.Targets)
	}
	if got := result.Targets[0]; got.Path != refused || !errors.Is(got.Err, fs.ErrExist) {
		t.Fatalf("target 0 = %#v, want %q refused with %v", got, refused, fs.ErrExist)
	}
	if got := result.Targets[1]; got.Path != written || got.Err != nil {
		t.Fatalf("target 1 = %#v, want %q written", got, written)
	}

	// The successful target must hold the source content.
	content, err := os.ReadFile(written)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(content, []byte("fixture")) {
		t.Fatalf("written content = %q, want %q", content, "fixture")
	}
}

// TestRunCopiesAChunkedSourceToEveryTarget pins a successful multi-target copy larger than one
// read buffer: several chunks of the shared buffer pool travel to every target, and each target
// must hold the whole source. The content is a fixed byte pattern instead of a random source, so
// the test is reproducible, and every target hash is computed from the bytes read back from disk.
func TestRunCopiesAChunkedSourceToEveryTarget(t *testing.T) {
	// Three full chunks plus a short final one, so the read loop, the buffer handoff and the
	// final partial write are all exercised.
	content := make([]byte, 3*batchSize+12345)
	if len(content) <= batchSize {
		t.Fatalf("test content = %d bytes, want more than one %d-byte chunk", len(content), batchSize)
	}
	for index := range content {
		content[index] = byte(index*7 + index/251)
	}

	// Track the shared pool as well: several buffers pass through it, and every reference must
	// come back by the time the run ends.
	trackChunkPool(t)

	root := t.TempDir()
	input := writeSourceFile(t, root, "source.bin", content)
	targets := []string{
		filepath.Join(root, "target-a.bin"),
		filepath.Join(root, "target-b.bin"),
		filepath.Join(root, "target-c.bin"),
	}
	item := newFixtureItem(input, targets...)

	if err := runFixture(context.Background(), newStreamFixture(item), []Item{item}, WithHashPolicy(HashRead)); err != nil {
		t.Fatalf("run error = %v, want nil", err)
	}
	result, err := item.terminal(t)
	if err != nil {
		t.Fatalf("item failed: %v", err)
	}

	wantHash := sha256.Sum256(content)
	if result.Size != int64(len(content)) {
		t.Fatalf("result size = %d, want %d", result.Size, len(content))
	}
	if !bytes.Equal(result.SHA256, wantHash[:]) {
		t.Fatalf("result SHA256 = %x, want %x", result.SHA256, wantHash)
	}
	if len(result.Targets) != len(targets) {
		t.Fatalf("targets = %#v, want one outcome per requested target", result.Targets)
	}
	for index, target := range targets {
		outcome := result.Targets[index]
		if outcome.Path != target || outcome.Err != nil {
			t.Fatalf("target %d = %#v, want %q written", index, outcome, target)
		}

		got, err := os.ReadFile(target)
		if err != nil {
			t.Fatalf("read target %q: %v", target, err)
		}
		if !bytes.Equal(got, content) {
			t.Fatalf("target %q holds %d bytes that differ from the source content, want %d matching bytes", target, len(got), len(content))
		}
		if hash := sha256.Sum256(got); !bytes.Equal(hash[:], wantHash[:]) {
			t.Fatalf("target %q SHA256 = %x, want %x", target, hash, wantHash)
		}
	}
}

func TestRunReportsUnprocessableItemsAsFailures(t *testing.T) {
	root := t.TempDir()
	input := writeSourceFile(t, root, "source.txt", []byte("fixture"))
	directory := filepath.Join(root, "directory")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}

	// A source that cannot be described, opened, or read never completes.
	tests := []struct {
		name    string
		source  string
		target  []string
		wantErr error
		anyErr  bool
	}{
		{name: "missing source", source: filepath.Join(root, "missing"), wantErr: fs.ErrNotExist},
		{name: "directory source", source: directory, anyErr: true},
		{name: "successful item", source: input, target: []string{filepath.Join(root, "target.txt")}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			item := newFixtureItem(test.source, test.target...)
			if err := runFixture(context.Background(), newStreamFixture(item), []Item{item}); err != nil {
				t.Fatalf("run error = %v, want nil", err)
			}

			result, err := item.terminal(t)
			if test.wantErr != nil || test.anyErr {
				if err == nil {
					t.Fatalf("item completed with %#v, want a failure", result)
				}
				if test.wantErr != nil && !errors.Is(err, test.wantErr) {
					t.Fatalf("item error = %v, want %v", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("item failed: %v", err)
			}
			if submitted, ok := result.Job.(*fixtureItem); !ok || submitted.source != filepath.Clean(test.source) {
				t.Fatalf("result job = %#v, want the submitted item", result.Job)
			}
		})
	}
}

func TestPrepareReportsUnstartedItemsWithoutReadingThem(t *testing.T) {
	tests := []struct {
		name      string
		itemError error
		want      error
	}{
		{
			name:      "source rejected while indexing",
			itemError: fmt.Errorf("get source stat failed, %w", fs.ErrNotExist),
			want:      fs.ErrNotExist,
		},
		{name: "pipeline stopped", want: context.Canceled},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			if test.itemError == nil {
				cancel()
			}
			defer cancel()

			// The source does not exist, so only a pipeline that still starts a read can
			// report anything other than the failure this stage owes the caller.
			item := newFixtureItem(filepath.Join(t.TempDir(), "removed"), filepath.Join(t.TempDir(), "target"))
			copyer := newTestStream(t)
			fixture := newStreamFixture(item)
			copyer.onResults = fixture.onResults
			job := &baseJob{
				copyer:    copyer,
				item:      item,
				path:      item.source,
				order:     0,
				itemError: test.itemError,
			}

			indexed := make(chan *baseJob, 1)
			indexed <- job
			close(indexed)

			// Preparation forwards an item it must not read to the results stage, and that
			// stage reports it exactly once.
			prepared := copyer.prepare(ctx, indexed)
			copyed := copyer.copy(ctx, prepared)
			runResults(copyer, copyed)
			for range copyed {
			}

			if _, err := item.terminal(t); !errors.Is(err, test.want) {
				t.Fatalf("item error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestPrepareReadsNoContentWhenThePolicyNeedsNone(t *testing.T) {
	tests := []struct {
		name   string
		policy HashPolicy

		// A policy that uses the cache reads its stored hash through the item's descriptor, so a
		// source it cannot open is a cache miss with a warning instead of an item failure.
		wantMisses   int64
		wantFailures int64
	}{
		{name: "off", policy: HashOff},
		{name: "cached only without entry", policy: HashCachedOnly, wantMisses: 1, wantFailures: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			copyer := newTestStream(t, WithHashPolicy(test.policy))
			if test.policy.usesCache() {
				copyer.signatures = newSignatureCache()
			}

			// The source only has to be described: a policy that needs no content reads nothing
			// from it, so a missing path cannot turn the item into a failure.
			item := newFixtureItem(filepath.Join(t.TempDir(), "removed"))
			fixture := newStreamFixture(item)
			copyer.onResults = fixture.onResults
			job := &baseJob{
				copyer: copyer,
				item:   item,
				path:   item.source,
				stat:   &stat{size: 7, mode: 0o644},
				order:  0,
			}
			indexed := make(chan *baseJob, 1)
			indexed <- job
			close(indexed)

			var prepared []*writeJob
			for write := range copyer.prepare(context.Background(), indexed) {
				if !write.skipContent {
					t.Fatal("prepare kept a content read for a policy that needs no content")
				}
				prepared = append(prepared, write)
			}
			if len(prepared) != 1 {
				t.Fatalf("prepared jobs = %d, want 1", len(prepared))
			}

			if err := runWriteJob(copyer, prepared[0]); err != nil {
				t.Fatalf("pipeline failed: %v", err)
			}
			result, err := item.terminal(t)
			if err != nil {
				t.Fatalf("item failed: %v", err)
			}
			if len(result.SHA256) != 0 || result.SignatureCacheHit {
				t.Fatalf("result = %#v, want no hash and no cache hit", result)
			}
			if result.Size != 7 {
				t.Fatalf("result size = %d, want the indexed size", result.Size)
			}

			// A cache read that cannot open its descriptor is a miss with a warning, and the item
			// still completes without a hash.
			if copyer.signatures == nil {
				return
			}
			summary := copyer.signatures.snapshot()
			if summary.Misses != test.wantMisses || summary.Failures != test.wantFailures {
				t.Fatalf("cache summary = %#v, want misses=%d failures=%d", summary, test.wantMisses, test.wantFailures)
			}
		})
	}
}

func TestRunKeepsLinearOrderingPastAnUnprocessedItem(t *testing.T) {
	// A linear writer advances per request order, so an item that preparation never reads
	// must still advance that order: otherwise the items behind it lose their callback.
	root := t.TempDir()
	missing := newFixtureItem(filepath.Join(root, "missing"))
	goodSource := writeSourceFile(t, root, "good.txt", []byte("good"))
	goodTarget := filepath.Join(root, "target", "good.txt")
	good := newFixtureItem(goodSource, goodTarget)

	if err := runFixture(
		context.Background(),
		newStreamFixture(missing, good),
		[]Item{missing, good},
		SetToDevice(LinearDevice(true)),
	); err != nil {
		t.Fatalf("run error = %v, want nil", err)
	}

	if _, err := missing.terminal(t); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("rejected item error = %v, want %v", err, fs.ErrNotExist)
	}
	result, err := good.terminal(t)
	if err != nil {
		t.Fatalf("item behind the rejected one failed: %v", err)
	}
	if len(result.Targets) != 1 || result.Targets[0].Err != nil || result.Targets[0].Path != goodTarget {
		t.Fatalf("targets = %#v, want %q written", result.Targets, goodTarget)
	}
}

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

// contentOfFixture returns the source content one fixture item was built from.
func contentOfFixture(fixtures []*fixtureItem, job Item) []byte {
	for _, item := range fixtures {
		if Item(item) == job {
			return []byte(strings.Repeat(filepath.Base(item.source), 32))
		}
	}
	return nil
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

func TestRunAppliesBoundedBackpressure(t *testing.T) {
	root := t.TempDir()
	input := writeSourceFile(t, root, "source.txt", nil)

	const (
		total       = int64(256)
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
	// Feed work until the pipeline is busy, so cancellation occurs with an active pipeline.
	root := t.TempDir()
	input := writeSourceFile(t, root, "source.txt", []byte("fixture"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		lock     sync.Mutex
		produced []*fixtureItem
	)
	fixture := newStreamFixture()
	stream, err := NewStream(ctx, fixture.onResults, SetToDevice(LinearDevice(true)), WithResultBuffer(1), WithResultBatch(1))
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

// currentGoroutineID reads the runtime's goroutine identifier so a test can prove that
// callbacks arrive from one goroutine. Production code must never depend on it.
func currentGoroutineID() string {
	buf := make([]byte, 64)
	n := runtime.Stack(buf, false)
	fields := strings.Fields(string(buf[:n]))
	if len(fields) < 2 {
		return ""
	}
	return fields[1]
}

// lockedBuffer collects log output from several goroutines.
type lockedBuffer struct {
	lock sync.Mutex
	buf  bytes.Buffer
}

func (b *lockedBuffer) Write(data []byte) (int, error) {
	b.lock.Lock()
	defer b.lock.Unlock()
	return b.buf.Write(data)
}

func (b *lockedBuffer) String() string {
	b.lock.Lock()
	defer b.lock.Unlock()
	return b.buf.String()
}

// captureLogs redirects the standard logger into the returned buffer.
func captureLogs(t *testing.T) *lockedBuffer {
	t.Helper()

	logs := new(lockedBuffer)
	previous := logrus.StandardLogger().Out
	logrus.SetOutput(logs)
	t.Cleanup(func() { logrus.SetOutput(previous) })
	return logs
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
