package acp

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

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

// newTestStream builds the pipeline state a stage-level test needs without starting it.
func newTestStream(t *testing.T, opts ...Option) *StreamCopyer {
	t.Helper()

	option, err := buildOption(opts...)
	if err != nil {
		t.Fatalf("check options: %v", err)
	}

	device := t.TempDir()
	return &StreamCopyer{
		option:         option,
		ctx:            context.Background(),
		readCh:         make(chan *baseJob, option.readBuffer),
		resultCh:       make(chan Result, option.resultBuffer),
		eventCh:        make(chan Event, 256),
		hardStop:       make(chan struct{}),
		onResults:      func([]Result) error { return nil },
		getDevice:      func(string) (string, error) { return device, nil },
		availableSpace: availableSpace,
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

// contentOfFixture returns the source content one fixture item was built from.
func contentOfFixture(fixtures []*fixtureItem, job Item) []byte {
	for _, item := range fixtures {
		if Item(item) == job {
			return []byte(strings.Repeat(filepath.Base(item.source), 32))
		}
	}
	return nil
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
