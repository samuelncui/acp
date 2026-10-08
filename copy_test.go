package acp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"hash"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	mapset "github.com/deckarep/golang-set/v2"
	"github.com/samuelncui/godf"
)

type trackingReadCloser struct {
	closed int
}

func (*trackingReadCloser) Read([]byte) (int, error) {
	return 0, io.EOF
}

func (r *trackingReadCloser) Close() error {
	r.closed++
	return nil
}

type failingReadCloser struct {
	err error
}

func (r *failingReadCloser) Read([]byte) (int, error) {
	return 0, r.err
}

func (*failingReadCloser) Close() error {
	return nil
}

// failingContentReader delivers whole batches and then fails, so target writers still hold
// source buffers when the read stage stops.
type failingContentReader struct {
	err     error
	batches int
	reads   int
}

func (r *failingContentReader) Read(p []byte) (int, error) {
	r.reads++
	if r.reads <= r.batches {
		return copy(p, bytes.Repeat([]byte{'x'}, len(p))), nil
	}
	return 0, r.err
}

func (*failingContentReader) Close() error {
	return nil
}

// trackChunkPool instruments the shared read-buffer pool so a test can prove that every
// buffer returns to it, including buffers discarded by a failed target writer.
func trackChunkPool(t *testing.T) {
	t.Helper()

	var (
		lock    sync.Mutex
		buffers []*chunkBuffer
	)

	// Empty the shared pool first so every buffer this test uses is tracked here. Two
	// collections drop the buffers cached in every processor's private slot, which a drain loop
	// on this goroutine cannot reach: without them the pipeline may reuse an untracked buffer and
	// the assertions below become vacuous.
	previousNew := chunkPool.New
	chunkPool.New = nil
	runtime.GC()
	runtime.GC()
	for chunkPool.Get() != nil {
	}
	t.Cleanup(func() {
		chunkPool.New = previousNew

		lock.Lock()
		defer lock.Unlock()
		if len(buffers) == 0 {
			t.Error("the pipeline acquired no read buffer")
		}
		for index, buffer := range buffers {
			if refs := atomic.LoadInt32(&buffer.refs); refs != 0 {
				t.Errorf("read buffer %d still holds %d references after the pipeline stopped", index, refs)
			}
		}
	})

	chunkPool.New = func() interface{} {
		buffer := &chunkBuffer{data: make([]byte, batchSize)}
		lock.Lock()
		buffers = append(buffers, buffer)
		lock.Unlock()
		return buffer
	}
}

// runWriteJob drives one prepared write Job through the copy and reporting stages. The caller
// attaches the results callback it wants to observe.
func runWriteJob(copyer *StreamCopyer, job *writeJob) error {
	// Stage-level fixtures provide synthetic readers; prepare their target facts once here.
	for _, name := range job.targets {
		target, err := copyer.indexTarget(name, nil)
		if err != nil {
			job.fail(name, err)
			continue
		}
		job.outputs = append(job.outputs, target)
	}
	prepared := make(chan *writeJob, 1)
	prepared <- job
	close(prepared)

	copyed := copyer.copy(context.Background(), prepared)
	runResults(copyer, copyed)
	for range copyed {
	}
	return copyer.Wait()
}

// write drives one synthetic reader through the same transfer implementation as the stream.
func (c *StreamCopyer) write(ctx context.Context, job *writeJob, out chan<- *baseJob, cntr *counter, exhausted mapset.Set[string]) {
	t := newTransfer(c, job, newReadAhead(c), 0)
	t.reservePublications(newPublicationOrder())
	t.run(ctx, out, make(chan struct{}, 1), cntr, exhausted)
}

// acquireChunk supplies a standalone consumer fixture without a second production pool path.
func acquireChunk() *chunkBuffer {
	chunk := chunkPool.New().(*chunkBuffer)
	chunk.refs = 1
	return chunk
}

func TestRunCopiesEmptyFile(t *testing.T) {
	tests := []struct {
		name string
		opts []Option
	}{
		{name: "buffered source"},
		{name: "mapped source", opts: []Option{SetFromDevice(WithReadMode(ReadMapped))}},
		{name: "linear source", opts: []Option{SetFromDevice(LinearDevice(true))}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			source := writeSourceFile(t, root, "src", nil)
			target := filepath.Join(root, "dst")

			// The report machinery belongs to the af05f05c shell, so the row is asserted
			// through the shell while the read mode stays a device option.
			handler, getter := NewReportGetter()
			opts := append([]Option{AccurateJob(source, []string{target}), WithEventHandler(handler)}, tt.opts...)
			if err := runShell(context.Background(), opts...); err != nil {
				t.Fatalf("run: %v", err)
			}

			info, err := os.Stat(target)
			if err != nil {
				t.Fatalf("stat dst: %v", err)
			}
			if info.Size() != 0 {
				t.Fatalf("dst size = %d", info.Size())
			}

			report := getter()
			if len(report.Errors) != 0 {
				t.Fatalf("report errors = %v", report.Errors)
			}
			if len(report.Jobs) != 1 {
				t.Fatalf("report jobs = %d", len(report.Jobs))
			}
			job := report.Jobs[0]
			if job.Status != JobStatusFinished {
				t.Fatalf("job status = %q", job.Status)
			}
			if job.Size != 0 {
				t.Fatalf("job size = %d, want 0", job.Size)
			}
			if len(job.SuccessTargets) != 1 || job.SuccessTargets[0] != target {
				t.Fatalf("success targets = %v", job.SuccessTargets)
			}
		})
	}
}

func TestWriteReportsTargetlessReadFailureAsItemFailure(t *testing.T) {
	// Build a target-free hash item whose source fails on its first read.
	readErr := errors.New("read failed")
	copyer := newTestStream(t, WithHashPolicy(HashRead))
	item := newFixtureItem("source")
	fixture := newStreamFixture(item)
	copyer.onResults = fixture.onResults
	job := newWriteJob(&baseJob{
		copyer: copyer,
		item:   item,
		path:   "source",
		stat:   &stat{size: 1},
	}, &failingReadCloser{err: readErr})
	completed := make(chan *baseJob, 1)

	// An item with no target outcome to report could not be processed at all.
	copyer.write(context.Background(), job, completed, new(counter), mapset.NewSet[string]())
	if err := (<-completed).itemError; !errors.Is(err, readErr) {
		t.Fatalf("item error = %v, want %v", err, readErr)
	}

	// The results stage must report that item as a failure instead of completing it.
	copyed := make(chan *baseJob, 1)
	copyed <- job.baseJob
	close(copyed)
	runResults(copyer, copyed)
	if result, err := item.terminal(t); !errors.Is(err, readErr) {
		t.Fatalf("terminal outcome = %#v / %v, want failure %v", result, err, readErr)
	}
	if err := copyer.Wait(); err != nil {
		t.Fatalf("Wait() = %v, want nil: a failed item is an item outcome", err)
	}
}

// TestWriteKeepsTargetOutcomesWhenTheReadAlsoFails pins one Result carrying both levels of
// failure: an item whose every target fails to open and whose source read then fails reports the
// item error and the target outcomes it did establish.
func TestWriteKeepsTargetOutcomesWhenTheReadAlsoFails(t *testing.T) {
	root := t.TempDir()
	readErr := errors.New("read failed")

	// A target that already exists as a directory can never be opened for writing.
	refused := filepath.Join(root, "refused")
	if err := os.MkdirAll(refused, 0o755); err != nil {
		t.Fatal(err)
	}

	copyer := newTestStream(t, WithHashPolicy(HashRead))
	item := newFixtureItem(filepath.Join(root, "source"), refused)
	fixture := newStreamFixture(item)
	copyer.onResults = fixture.onResults
	job := newWriteJob(&baseJob{
		copyer:  copyer,
		item:    item,
		path:    filepath.Join(root, "source"),
		stat:    &stat{size: 1, mode: 0o644},
		targets: []string{refused},
	}, &failingReadCloser{err: readErr})

	if err := runWriteJob(copyer, job); err != nil {
		t.Fatalf("pipeline failed: %v", err)
	}
	if _, err := item.terminal(t); !errors.Is(err, readErr) {
		t.Fatalf("terminal outcome = %v, want failure %v", err, readErr)
	}

	// The result carries the item-level failure and the target outcome it established.
	result := fixture.delivered()
	if len(result) != 1 {
		t.Fatalf("delivered results = %d, want 1", len(result))
	}
	if !errors.Is(result[0].Err, readErr) {
		t.Fatalf("result error = %v, want %v", result[0].Err, readErr)
	}
	if len(result[0].Targets) != 1 || !errors.Is(result[0].Targets[0].Err, os.ErrExist) {
		t.Fatalf("result targets = %#v, want the refused target %q", result[0].Targets, refused)
	}
}

func TestCopyReportsPrefetchedItemsAfterCancellation(t *testing.T) {
	// Queue one prefetched reader before starting an already-canceled Copy stage.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	copyer := newTestStream(t)
	copyer.toDevice.threads = 1
	reader := new(trackingReadCloser)
	item := newFixtureItem("source")
	fixture := newStreamFixture(item)
	copyer.onResults = fixture.onResults
	job := newWriteJob(&baseJob{copyer: copyer, item: item, path: "source"}, reader)
	prepared := make(chan *writeJob, 1)
	prepared <- job
	close(prepared)

	// Copy owns accepted readers and must drain and close them during cancellation, then hand
	// the item to the results stage, which reports it exactly once.
	copyed := copyer.copy(ctx, prepared)
	runResults(copyer, copyed)
	for range copyed {
	}
	if reader.closed != 1 {
		t.Fatalf("reader closed %d times, want 1", reader.closed)
	}
	if result, err := item.terminal(t); !errors.Is(err, context.Canceled) {
		t.Fatalf("terminal outcome = %#v / %v, want %v", result, err, context.Canceled)
	}
}

func TestWriteReturnsAfterHardStopWithoutPublishing(t *testing.T) {
	// Use an unbuffered completion channel with no receiver to expose a blocked handoff.
	copyer := newTestStream(t)
	copyer.stopHard()
	reader := new(trackingReadCloser)
	job := newWriteJob(&baseJob{
		copyer: copyer,
		item:   newFixtureItem("source"),
		stat:   &stat{},
	}, reader)
	done := make(chan struct{})
	go func() {
		copyer.write(context.Background(), job, make(chan *baseJob), new(counter), mapset.NewSet[string]())
		close(done)
	}()

	// A pipeline failure skips the completion handoff while retaining source cleanup.
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("write did not return after the pipeline stopped")
	}
	if reader.closed != 1 {
		t.Fatalf("reader closed %d times, want 1", reader.closed)
	}
}

func TestBaseJobFailureMovesSuccessfulTarget(t *testing.T) {
	// Seed a completed target before applying a metadata-stage failure.
	copyer := newTestStream(t)
	item := newFixtureItem("source", "target")
	job := &baseJob{
		copyer: copyer, item: item, stat: &stat{},
		targets: []string{"target"}, successTargets: []string{"target"},
	}

	// A late target failure must remove the target from the successful result.
	job.fail("target", syscall.ENOSPC)
	result := job.result()
	if len(result.Targets) != 1 || result.Targets[0].Err == nil {
		t.Fatalf("targets = %#v, want one failed outcome", result.Targets)
	}
	if len(job.successTargets) != 0 {
		t.Fatalf("success targets = %v, want none", job.successTargets)
	}
	if !errors.Is(result.Targets[0].Err, syscall.ENOSPC) {
		t.Fatalf("target failure = %v, want %v", result.Targets[0].Err, syscall.ENOSPC)
	}
}

func TestTargetFailureKeepsItsIdentityAsItemOutcome(t *testing.T) {
	// Record a mapped write failure before a later, unrelated pipeline error.
	copyer := newTestStream(t)
	item := newFixtureItem("source", "target")
	job := &baseJob{
		copyer: copyer, item: item, stat: &stat{}, targets: []string{"target"},
	}
	job.fail("target", mappingError(syscall.ENOSPC))
	copyer.setError(errors.New("remove failed"))

	// The item outcome keeps the no-space classification the caller classifies on.
	result := job.result()
	if len(result.Targets) != 1 || !errors.Is(result.Targets[0].Err, ErrTargetNoSpace) {
		t.Fatalf("target outcome = %#v, want %v", result.Targets, ErrTargetNoSpace)
	}
}

func TestWriteFailureDrainsBuffersAndTargets(t *testing.T) {
	// Fail the source read after a whole batch, so every target writer runs its failure
	// drain while the pipeline still holds the failed item's buffers.
	trackChunkPool(t)

	root := t.TempDir()
	readErr := errors.New("read failed")
	targets := []string{filepath.Join(root, "target-1"), filepath.Join(root, "target-2")}
	item := newFixtureItem(filepath.Join(root, "source"), targets...)
	copyer := newTestStream(t, WithHashPolicy(HashRead))
	fixture := newStreamFixture(item)
	copyer.onResults = fixture.onResults
	job := newWriteJob(&baseJob{
		copyer:  copyer,
		item:    item,
		path:    filepath.Join(root, "source"),
		stat:    &stat{size: batchSize, mode: 0o644},
		targets: targets,
	}, &failingContentReader{err: readErr, batches: 1})

	// The pipeline must drain every writer, report exactly one outcome, and return.
	done := make(chan error, 1)
	go func() {
		done <- runWriteJob(copyer, job)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("pipeline failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the pipeline deadlocked while draining failed target writers")
	}

	result, err := item.terminal(t)
	if err != nil {
		t.Fatalf("an item whose targets failed must still complete: %v", err)
	}
	if len(result.SHA256) != 0 {
		t.Fatalf("SHA256 = %x, want none: the read stopped before the source ended", result.SHA256)
	}
	if len(result.Targets) != 2 {
		t.Fatalf("targets = %#v, want one outcome per requested target", result.Targets)
	}
	for _, outcome := range result.Targets {
		if !errors.Is(outcome.Err, readErr) {
			t.Fatalf("target %q error = %v, want %v", outcome.Path, outcome.Err, readErr)
		}
		if _, statErr := os.Stat(outcome.Path); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("failed target %q still exists: %v", outcome.Path, statErr)
		}
	}
}

// panickingHash panics on its first write, which is how a broken hash implementation dies inside
// the pipeline.
type panickingHash struct{ hash.Hash }

var errPanickingHash = errors.New("hash fixture panicked")

func (panickingHash) Write([]byte) (int, error) {
	panic(errPanickingHash)
}

// TestRunReleasesTheCacheGateWhenTheHashPanics pins the fatal-panic guarantee for the cache gate.
// The gate is released by a deferred close, so a hash consumer that dies cannot leave a target
// writer blocked on it: a close at the end of the goroutine's body is skipped by the panic, and the
// writer, the copy stage and therefore Close and Wait stay parked forever even though the panic was
// already recovered and recorded as the run error.
func TestRunReleasesTheCacheGateWhenTheHashPanics(t *testing.T) {
	captureLogs(t)
	requireSignatureXattrSupport(t)

	root := t.TempDir()
	content := bytes.Repeat([]byte{'c'}, 4096)
	input := writeSourceFile(t, root, "source.txt", content)
	target := filepath.Join(root, "target.txt")

	previous := sha256Pool
	sha256Pool = &sync.Pool{New: func() interface{} { return panickingHash{Hash: sha256.New()} }}
	t.Cleanup(func() { sha256Pool = previous })

	item := newFixtureItem(input, target)
	done := make(chan error, 1)
	go func() {
		done <- runFixture(context.Background(), newStreamFixture(item), []Item{item}, WithHashPolicy(HashReadRefresh))
	}()

	select {
	case err := <-done:
		if !errors.Is(err, errPanickingHash) {
			t.Fatalf("run error = %v, want the recovered panic %v", err, errPanickingHash)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the run never returned: the hash consumer panicked and a target writer is still blocked on the cache gate")
	}
}

func TestLinearTargetStopsWhenDiskUsageEstimateIsInsufficient(t *testing.T) {
	// Size the Job beyond the filesystem estimate without allocating the source payload.
	root := t.TempDir()
	target := filepath.Join(root, "target")
	usage, err := godf.NewDiskUsage(root)
	if err != nil {
		t.Fatalf("read disk usage: %v", err)
	}
	if usage.Available() > math.MaxInt64-(2<<30) {
		t.Fatal("available disk space cannot be represented by the test Job size")
	}
	size := usage.Available() + (2 << 30)
	option, err := buildOption()
	if err != nil {
		t.Fatal(err)
	}
	copyer := &StreamCopyer{
		option: option, ctx: context.Background(), readCh: make(chan *baseJob, option.readBuffer),
		eventCh: make(chan Event, 8), hardStop: make(chan struct{}),
		getDevice:      func(string) (string, error) { return root, nil },
		availableSpace: availableSpace,
	}
	copyer.toDevice.linear = true
	item := newFixtureItem("source", target)
	job := newWriteJob(&baseJob{
		copyer: copyer, item: item, path: "source",
		stat: &stat{size: size}, targets: []string{target},
		outputs: []targetSpec{{name: target, path: target, device: root}},
	}, io.NopCloser(bytes.NewReader(nil)))
	completed := make(chan *baseJob, 1)

	// The hardware-backed estimate must stop a linear target before the oversized write starts.
	copyer.write(context.Background(), job, completed, new(counter), mapset.NewSet[string]())
	result := (<-completed).result()
	if len(result.Targets) != 1 || !errors.Is(result.Targets[0].Err, ErrTargetNoSpace) {
		t.Fatalf("target outcome = %#v, want %v", result.Targets, ErrTargetNoSpace)
	}
	if !copyer.linearTargetStopped() {
		t.Fatal("linear target continued after the capacity estimate was exhausted")
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat target error = %v, want %v", err, os.ErrNotExist)
	}
}

// TestStoppedLinearTargetRefusesSubmission pins the feed checkpoint of an exhausted linear
// target: the run accepts no more items, and an exhausted target stays an item outcome instead
// of a run error.
func TestStoppedLinearTargetRefusesSubmission(t *testing.T) {
	copyer := newTestStream(t, SetToDevice(LinearDevice(true)))
	copyer.endLinearTarget(ErrTargetNoSpace)

	if err := copyer.Submit(newFixtureItem("source")); !errors.Is(err, ErrTargetNoSpace) {
		t.Fatalf("Submit() error = %v, want %v", err, ErrTargetNoSpace)
	}
	if err := copyer.Wait(); err != nil {
		t.Fatalf("Wait() error = %v, want nil: an exhausted target is an item outcome", err)
	}
}

// TestSubmitRejectsANilItem pins the feed's input validation: a nil item is a submission error that
// is recorded as the run error without ending the feed, and every accepted item keeps its outcome.
func TestSubmitRejectsANilItem(t *testing.T) {
	root := t.TempDir()
	input := writeSourceFile(t, root, "source.txt", []byte("fixture"))
	item := newFixtureItem(input, filepath.Join(root, "target.txt"))
	after := newFixtureItem(input, filepath.Join(root, "target-after.txt"))
	fixture := newStreamFixture(item, after)

	stream, err := NewStream(context.Background(), fixture.onResults)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Submit(item); err != nil {
		t.Fatalf("Submit() error = %v, want the item accepted", err)
	}
	if err := stream.Submit(nil); err == nil {
		t.Fatal("Submit(nil) error = nil, want a rejected item")
	}
	// A rejected submission is that submission's error, not a stopping reason: the run still
	// accepts what the caller submits next.
	if err := stream.Submit(after); err != nil {
		t.Fatalf("Submit() after a rejected item error = %v, want the item accepted", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := stream.Wait(); err == nil {
		t.Fatal("Wait() error = nil, want the rejected item")
	}
	for _, submitted := range []*fixtureItem{item, after} {
		if _, err := submitted.terminal(t); err != nil {
			t.Fatalf("item failed: %v", err)
		}
	}
}

// TestStreamCopyReleasesAChunkHandoffAfterHardStop pins the fatal-failure rule for the read
// handoff: a stage that stopped consuming cannot leave the reader blocked on a chunk.
func TestStreamCopyReleasesAChunkHandoffAfterHardStop(t *testing.T) {
	// A real transfer producer must relinquish its borrowed buffer when its receiver stops.
	trackChunkPool(t)
	copyer := newTestStream(t)
	job := newWriteJob(&baseJob{copyer: copyer, stat: &stat{size: 7}}, io.NopCloser(strings.NewReader("fixture")))
	transfer := newTransfer(copyer, job, newReadAhead(copyer), 0)
	transfer.chunks = make(chan *chunkBuffer)
	go copyer.wrap(context.Background(), transfer.readContent)
	time.Sleep(50 * time.Millisecond)
	copyer.stopHard()

	// Both content completion and asynchronous source closure must finish before inspection.
	select {
	case <-transfer.readDone:
	case <-time.After(5 * time.Second):
		t.Fatal("source stayed blocked on a chunk handoff")
	}
	<-transfer.sourceDone
	if transfer.readErr == nil {
		t.Fatal("missing hard-stop read error")
	}
}

// TestSubmitDropsAnEventAfterHardStop pins the same rule for the event handoff: a fatal failure
// ends the run instead of blocking the stage that reports through it.
func TestSubmitDropsAnEventAfterHardStop(t *testing.T) {
	copyer := newTestStream(t)
	// No dispatch stage drains this channel, so only the hard stop can release the send.
	copyer.eventCh = make(chan Event)
	copyer.stopHard()

	done := make(chan struct{})
	go func() {
		defer close(done)
		copyer.submit(&EventUpdateCount{})
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("submit stayed blocked on the event handoff after a hard stop")
	}
}

// TestFailAllReportsEveryTargetAsFailed pins the exhausted-device path: when no requested target
// can be written, the item still completes and every target carries the failure, which is what
// keeps the caller from reading a phantom success.
func TestFailAllReportsEveryTargetAsFailed(t *testing.T) {
	copyer := newTestStream(t)
	job := &baseJob{
		copyer:  copyer,
		item:    newFixtureItem("source"),
		path:    "source",
		targets: []string{"first", "second"},
	}

	job.failAll(ErrTargetNoSpace)
	result := job.result()
	if result.Err != nil {
		t.Fatalf("item error = %v, want nil: an exhausted target is a target outcome", result.Err)
	}
	if len(result.Targets) != 2 {
		t.Fatalf("target outcomes = %#v, want one per requested target", result.Targets)
	}
	for index, target := range result.Targets {
		if target.Path != job.targets[index] || !errors.Is(target.Err, ErrTargetNoSpace) {
			t.Fatalf("target outcome %d = %#v, want %v on %q", index, target, ErrTargetNoSpace, job.targets[index])
		}
	}
}
