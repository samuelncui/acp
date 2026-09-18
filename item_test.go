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
)

// fixtureItem is a caller-owned item that records every terminal callback, so a test can
// assert the item contract without a report or a sink.
type fixtureItem struct {
	source  string
	targets []string

	// hook observes each terminal callback before it is recorded.
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

func (i *fixtureItem) Completed(result *Result) { i.record(result, nil) }

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
		t.Fatalf("item %q received %d terminal callbacks, want exactly 1", i.source, total)
	}
	if len(i.failures) == 1 {
		return nil, i.failures[0]
	}
	return i.results[0], nil
}

// callbackCount returns how many terminal callbacks the item received.
func (i *fixtureItem) callbackCount() int {
	i.lock.Lock()
	defer i.lock.Unlock()
	return len(i.results) + len(i.failures)
}

// callbackOrder records the terminal callback order of several items.
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

// sliceSource replays prepared batches and then ends the input.
type sliceSource struct {
	batches [][]Item
	err     error

	lock  sync.Mutex
	index int
	nexts int
}

func newSliceSource(items ...Item) *sliceSource {
	return &sliceSource{batches: [][]Item{items}}
}

func (s *sliceSource) Next(context.Context) ([]Item, error) {
	s.lock.Lock()
	defer s.lock.Unlock()

	s.nexts++
	if s.index < len(s.batches) {
		batch := s.batches[s.index]
		s.index++
		return batch, nil
	}
	if s.err != nil {
		return nil, s.err
	}
	return nil, io.EOF
}

func (s *sliceSource) calls() int {
	s.lock.Lock()
	defer s.lock.Unlock()
	return s.nexts
}

// blockingSource delivers one batch and then blocks until the context ends, which models a
// caller that stops feeding items when it stops the pipeline.
type blockingSource struct {
	batch []Item
	// endErr is returned once the context ends and defaults to the context error.
	endErr error

	lock  sync.Mutex
	nexts int
}

func (s *blockingSource) Next(ctx context.Context) ([]Item, error) {
	s.lock.Lock()
	s.nexts++
	first := s.nexts == 1
	s.lock.Unlock()

	if first {
		return s.batch, nil
	}

	<-ctx.Done()
	if s.endErr != nil {
		return nil, s.endErr
	}
	return nil, ctx.Err()
}

func (s *blockingSource) calls() int {
	s.lock.Lock()
	defer s.lock.Unlock()
	return s.nexts
}

// endlessSource produces one item per call until the caller cancels the context.
type endlessSource struct {
	input  string
	target string

	lock     sync.Mutex
	produced []*fixtureItem
}

func (s *endlessSource) Next(ctx context.Context) ([]Item, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	s.lock.Lock()
	defer s.lock.Unlock()

	index := len(s.produced) + 1
	item := newFixtureItem(s.input, filepath.Join(s.target, fmt.Sprintf("%04d", index)))
	s.produced = append(s.produced, item)
	return []Item{item}, nil
}

func (s *endlessSource) items() []*fixtureItem {
	s.lock.Lock()
	defer s.lock.Unlock()
	return append([]*fixtureItem(nil), s.produced...)
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

// newTestCopyer builds the pipeline state a stage-level test needs without starting it.
func newTestCopyer(t *testing.T, opts ...Option) *Copyer {
	t.Helper()

	option := newOption()
	for _, opt := range opts {
		if opt == nil {
			continue
		}
		option = opt(option)
	}
	if err := option.check(); err != nil {
		t.Fatalf("check options: %v", err)
	}

	device := t.TempDir()
	return &Copyer{
		option:    option,
		eventCh:   make(chan Event, 256),
		hardStop:  make(chan struct{}),
		abandoned: make(chan *baseJob, 16),
		getDevice: func(string) string { return device },
		getDiskUsageCache: func(string) *diskUsageCache {
			return newDiskUsageCache(device, defaultDiskUsageFreshInterval)
		},
	}
}

// cancelOnFirstCopy stops the pipeline when the first item starts copying.
func cancelOnFirstCopy(cancel context.CancelFunc) EventHandler {
	var once sync.Once
	return func(event Event) {
		update, ok := event.(*EventUpdateJob)
		if !ok || update.Job.Status != JobStatusCopying {
			return
		}
		once.Do(cancel)
	}
}

func TestRunCopiesItemsToLinearTarget(t *testing.T) {
	// Build caller-owned items without constructing copy options per file.
	root := t.TempDir()
	order := new(callbackOrder)
	items := make([]Item, 0, 3)
	for index, content := range []string{"first", "second", "third"} {
		name := string(rune('a'+index)) + ".txt"
		source := writeSourceFile(t, root, filepath.Join("source", name), []byte(content))
		item := newFixtureItem(source, filepath.Join(root, "target", name))
		item.hook = func(*Result, error) { order.record(item) }
		items = append(items, item)
	}

	// Keep the target serialized while allowing source preparation to complete in any order.
	if err := Run(
		context.Background(),
		newSliceSource(items...),
		WithHashPolicy(HashRead),
		SetToDevice(LinearDevice(true)),
	); err != nil {
		t.Fatal(err)
	}

	// A linear target reports items in request order.
	reported := order.snapshot()
	if len(reported) != len(items) {
		t.Fatalf("received %d callbacks, want %d", len(reported), len(items))
	}
	for index, item := range reported {
		if want := items[index].(*fixtureItem); item != want {
			t.Fatalf("callback %d reported %q, want %q", index, item.source, want.source)
		}

		result, err := item.terminal(t)
		if err != nil {
			t.Fatalf("item %q failed: %v", item.source, err)
		}
		if len(result.Targets) != 1 || result.Targets[0].Err != nil || len(result.SHA256) == 0 {
			t.Fatalf("unexpected result: %#v", result)
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
			copyer := &Copyer{option: &option{
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
			copyer.forwardPrepared(context.Background(), completed, prepared)
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
	if err := Run(context.Background(), newSliceSource(item), WithHashPolicy(HashRead)); err != nil {
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

func TestRunReturnsBatchSourceError(t *testing.T) {
	// Verify that a batch source failure crosses the synchronous Run boundary.
	sourceErr := errors.New("source failed")
	err := Run(context.Background(), &sliceSource{err: sourceErr}, WithHashPolicy(HashOff))
	if !errors.Is(err, sourceErr) {
		t.Fatalf("Run() error = %v, want %v", err, sourceErr)
	}

	// Verify that items accepted before the failure still receive their outcome.
	root := t.TempDir()
	input := writeSourceFile(t, root, "source.txt", []byte("fixture"))
	items := []*fixtureItem{
		newFixtureItem(input, filepath.Join(root, "target-1.txt")),
		newFixtureItem(input, filepath.Join(root, "target-2.txt")),
	}
	err = Run(
		context.Background(),
		&sliceSource{batches: [][]Item{{items[0]}, {items[1]}}, err: sourceErr},
		SetToDevice(Overwrite(true)),
	)
	if !errors.Is(err, sourceErr) {
		t.Fatalf("Run() error = %v, want %v", err, sourceErr)
	}
	for _, item := range items {
		result, terminalErr := item.terminal(t)
		if terminalErr != nil {
			t.Fatalf("item %q failed: %v", item.source, terminalErr)
		}
		if len(result.Targets) != 1 || result.Targets[0].Err != nil {
			t.Fatalf("item %q targets = %v", item.source, result.Targets)
		}
	}
}

func TestRunReportsTargetFailureAsItemOutcome(t *testing.T) {
	// Force target creation to fail while the item still completes.
	root := t.TempDir()
	input := writeSourceFile(t, root, "source.txt", []byte("fixture"))
	target := writeSourceFile(t, root, "target.txt", []byte("existing"))
	item := newFixtureItem(input, target)

	// A failed target is an item outcome with its own error identity, not a Run error.
	if err := Run(context.Background(), newSliceSource(item)); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
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

	if err := Run(context.Background(), newSliceSource(item)); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
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
			if err := Run(context.Background(), newSliceSource(item)); err != nil {
				t.Fatalf("Run() error = %v, want nil", err)
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
			if result.Source != filepath.Clean(test.source) {
				t.Fatalf("result source = %q, want %q", result.Source, filepath.Clean(test.source))
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

			copyer := newTestCopyer(t)
			// The source does not exist, so only a pipeline that still starts a read can
			// report anything other than the failure this stage owes the caller.
			item := newFixtureItem(filepath.Join(t.TempDir(), "removed"), filepath.Join(t.TempDir(), "target"))
			job := &baseJob{
				copyer:    copyer,
				item:      item,
				src:       &source{base: filepath.Dir(item.source), path: filepath.Base(item.source)},
				path:      item.source,
				order:     0,
				itemError: test.itemError,
			}

			indexed := make(chan *baseJob, 1)
			indexed <- job
			close(indexed)

			done := make(chan struct{})
			go func() {
				defer close(done)
				for range copyer.prepare(ctx, indexed) {
					t.Error("prepare handed out a reader for an item that never started")
				}
			}()

			select {
			case abandoned := <-copyer.abandoned:
				if abandoned != job {
					t.Fatalf("abandoned job = %p, want %p", abandoned, job)
				}
				// Cleanup owns terminal callbacks, so deliver the one this item is owed.
				copyer.failedJob(abandoned)
			case <-time.After(5 * time.Second):
				t.Fatal("prepare did not report the unstarted item")
			}

			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("prepare did not finish after the stop")
			}

			if _, err := item.terminal(t); !errors.Is(err, test.want) {
				t.Fatalf("item error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestPrepareCompletesWithoutOpeningTheSourceWhenPolicyNeedsNoContent(t *testing.T) {
	tests := []struct {
		name   string
		policy HashPolicy
	}{
		{name: "off", policy: HashOff},
		{name: "cached only without entry", policy: HashCachedOnly},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			copyer := newTestCopyer(t, WithHashPolicy(test.policy))
			if test.policy.usesCache() {
				copyer.signatures = newSignatureCache(1)
				t.Cleanup(func() { copyer.signatures.closeAndWait() })
			}

			// The source only has to be described: a policy that needs no content must not
			// open it, so a missing path cannot turn the item into a failure.
			item := newFixtureItem(filepath.Join(t.TempDir(), "removed"))
			job := &baseJob{
				copyer: copyer,
				item:   item,
				src:    &source{base: filepath.Dir(item.source), path: filepath.Base(item.source)},
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
					t.Fatal("prepare opened a source whose policy needs no content")
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

	if err := Run(
		context.Background(),
		newSliceSource(missing, good),
		SetToDevice(LinearDevice(true)),
	); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
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
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			const total = 16

			items := make([]Item, 0, total)
			for index := 0; index < total; index++ {
				name := fmt.Sprintf("%02d.txt", index)
				source := writeSourceFile(t, root, name, []byte(strings.Repeat(name, 64)))
				items = append(items, newFixtureItem(source, filepath.Join(root, "target", name)))
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			opts := []Option{WithReadBuffer(4), WithEventHandler(cancelOnFirstCopy(cancel))}
			opts = append(opts, test.opts...)
			err := Run(ctx, newSliceSource(items...), opts...)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Run() error = %v, want %v", err, context.Canceled)
			}

			// Every accepted item owns exactly one terminal outcome, and a stop abandons
			// the rest with the stopping error instead of dropping them.
			completed, abandoned := 0, 0
			for _, submitted := range items {
				item := submitted.(*fixtureItem)
				if got := item.callbackCount(); got != 1 {
					t.Fatalf("item %q received %d callbacks, want 1", item.source, got)
				}
				result, itemErr := item.terminal(t)
				if itemErr != nil {
					if !errors.Is(itemErr, context.Canceled) {
						t.Fatalf("abandoned item %q error = %v, want %v", item.source, itemErr, context.Canceled)
					}
					abandoned++
					continue
				}
				if len(result.Targets) != 1 || result.Targets[0].Err != nil {
					t.Fatalf("completed item %q targets = %v", item.source, result.Targets)
				}
				completed++
			}
			if completed == 0 {
				t.Fatal("no item finished after the stop")
			}
			if abandoned == 0 {
				t.Fatal("no item was abandoned after the stop")
			}
		})
	}
}

func TestRunDeliversTerminalCallbacksFromOneGoroutine(t *testing.T) {
	// ACP delivers terminal callbacks one at a time and from a single goroutine, so a
	// caller may perform its own I/O there without excluding the pipeline.
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
	for index := 0; index < total; index++ {
		name := fmt.Sprintf("%02d.txt", index)
		content := []byte(strings.Repeat(name, 32))
		source := writeSourceFile(t, root, name, content)
		target := filepath.Join(root, "target", name)
		item := newFixtureItem(source, target)
		item.hook = func(result *Result, err error) {
			if atomic.AddInt32(&inFlight, 1) != 1 {
				atomic.AddInt32(&overlapped, 1)
			}
			id := currentGoroutineID()
			lock.Lock()
			goroutines[id] = struct{}{}
			lock.Unlock()

			// The callback owns its own I/O, and every target it sees is already final.
			if result != nil {
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
					if !bytes.Equal(got, content) {
						lock.Lock()
						callbackErr = append(callbackErr, fmt.Errorf("target %q content = %q, want %q", outcome.Path, got, content))
						lock.Unlock()
					}
				}
			}

			runtime.Gosched()
			atomic.AddInt32(&inFlight, -1)
		}
		items = append(items, item)
	}

	if err := Run(context.Background(), newSliceSource(items...), WithHashPolicy(HashRead)); err != nil {
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

func TestRunAppliesBoundedBackpressure(t *testing.T) {
	root := t.TempDir()
	input := writeSourceFile(t, root, "source.txt", nil)

	const (
		total       = int64(256)
		readBuffer  = 8
		consumedMax = total / 2
	)
	var consumed int64
	source := &boundedItemSource{
		input: input, target: filepath.Join(root, "target"), total: total, consumed: &consumed,
	}

	// A bounded read buffer must stop the source from producing the whole input up front.
	if err := Run(
		context.Background(),
		source,
		WithReadBuffer(readBuffer),
		SetToDevice(LinearDevice(true)),
	); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt64(&consumed); got != total {
		t.Fatalf("consumed %d items, want %d", got, total)
	}
	if maximum := atomic.LoadInt64(&source.maximum); maximum >= consumedMax {
		t.Fatalf("maximum outstanding items = %d, want less than %d", maximum, consumedMax)
	}
}

// boundedItemSource produces one item per call and records how far production ran ahead of
// the terminal callbacks.
type boundedItemSource struct {
	input    string
	target   string
	total    int64
	consumed *int64
	maximum  int64
	produced int64
}

func (s *boundedItemSource) Next(context.Context) ([]Item, error) {
	id := atomic.AddInt64(&s.produced, 1)
	if id > s.total {
		return nil, io.EOF
	}

	outstanding := id - atomic.LoadInt64(s.consumed)
	for {
		maximum := atomic.LoadInt64(&s.maximum)
		if outstanding <= maximum || atomic.CompareAndSwapInt64(&s.maximum, maximum, outstanding) {
			break
		}
	}

	item := newFixtureItem(s.input, filepath.Join(s.target, fmt.Sprintf("%04d", id)))
	item.hook = func(*Result, error) { atomic.AddInt64(s.consumed, 1) }
	return []Item{item}, nil
}

func TestRunStopsFeedingItemsAfterGracefulStop(t *testing.T) {
	// Let the source block on its next batch after producing one copy item.
	root := t.TempDir()
	input := writeSourceFile(t, root, "source.txt", []byte("fixture"))
	item := newFixtureItem(input, filepath.Join(root, "target.txt"))
	source := &blockingSource{batch: []Item{item}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The first completed item stops the pipeline while the source waits for work.
	item.hook = func(*Result, error) { cancel() }

	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, source)
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() error = %v, want %v", err, context.Canceled)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not release the batch source after the caller cancelled")
	}
	if calls := source.calls(); calls != 2 {
		t.Fatalf("source calls = %d, want 2", calls)
	}
	if _, err := item.terminal(t); err != nil {
		t.Fatalf("item failed: %v", err)
	}
}

func TestRunReturnsStoppingErrorWhenBatchSourceEndsFirst(t *testing.T) {
	// A source that ends its input after the caller cancels must not hide the stop.
	root := t.TempDir()
	input := writeSourceFile(t, root, "source.txt", []byte("fixture"))
	item := newFixtureItem(input, filepath.Join(root, "target.txt"))
	source := &blockingSource{batch: []Item{item}, endErr: io.EOF}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, source, WithEventHandler(cancelOnFirstCopy(cancel)))
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() error = %v, want %v", err, context.Canceled)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop after the caller cancelled")
	}
	if calls := source.calls(); calls != 2 {
		t.Fatalf("source calls = %d, want 2", calls)
	}
}

func TestRunCancellationDrainsPrefetchedItems(t *testing.T) {
	// Feed work until preparation starts so cancellation occurs with an active pipeline.
	root := t.TempDir()
	input := writeSourceFile(t, root, "source.txt", []byte("fixture"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	source := &endlessSource{input: input, target: filepath.Join(root, "target")}
	cancelWhenPreparing := func(event Event) {
		update, ok := event.(*EventUpdateJob)
		if !ok || update.Job.Status != JobStatusPreparing {
			return
		}
		cancel()
	}

	// The canceled pipeline must drain its queues and return the context error.
	done := make(chan error, 1)
	go func() {
		done <- Run(
			ctx,
			source,
			SetToDevice(LinearDevice(true)),
			WithEventHandler(cancelWhenPreparing),
		)
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() error = %v, want %v", err, context.Canceled)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}

	// Every produced item must still report exactly one terminal outcome.
	items := source.items()
	if len(items) == 0 {
		t.Fatal("the source produced no items")
	}
	for _, item := range items {
		if got := item.callbackCount(); got != 1 {
			t.Fatalf("item %q received %d callbacks, want 1", item.source, got)
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
