package acp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
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

	// Empty the shared pool first so every buffer this test uses is tracked here.
	previousNew := chunkPool.New
	chunkPool.New = nil
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

// runWriteJob drives one prepared write Job through the copy and cleanup stages.
func runWriteJob(copyer *Copyer, job *writeJob) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	prepared := make(chan *writeJob, 1)
	prepared <- job
	close(prepared)

	copyed := copyer.copy(ctx, prepared)
	copyer.cleanup(ctx, copyed)
	for range copyed {
	}
	return copyer.WaitErr()
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
			item := newFixtureItem(source, target)

			handler, getter := NewReportGetter()
			opts := append([]Option{WithEventHandler(handler)}, tt.opts...)
			if err := Run(context.Background(), newSliceSource(item), opts...); err != nil {
				t.Fatalf("run: %v", err)
			}

			info, err := os.Stat(target)
			if err != nil {
				t.Fatalf("stat dst: %v", err)
			}
			if info.Size() != 0 {
				t.Fatalf("dst size = %d", info.Size())
			}

			result, err := item.terminal(t)
			if err != nil {
				t.Fatalf("item failed: %v", err)
			}
			if len(result.Targets) != 1 || result.Targets[0].Err != nil || result.Targets[0].Path != target {
				t.Fatalf("targets = %v", result.Targets)
			}
			if result.Size != 0 {
				t.Fatalf("result size = %d, want 0", result.Size)
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
			if len(job.SuccessTargets) != 1 || job.SuccessTargets[0] != target {
				t.Fatalf("success targets = %v", job.SuccessTargets)
			}
		})
	}
}

func TestWritePublishesFinishingJob(t *testing.T) {
	// Build a target-free write Job so only the worker-to-cleanup handoff is exercised.
	copyer := newTestCopyer(t)
	job := newWriteJob(&baseJob{
		copyer: copyer,
		src:    &source{},
		stat:   &stat{},
	}, io.NopCloser(bytes.NewReader(nil)), 0, false)
	completed := make(chan *baseJob, 1)

	// The copy worker must finish all mutations before publishing ownership to cleanup.
	copyer.write(context.Background(), job, completed, new(counter), mapset.NewSet[string]())
	if status := (<-completed).status; status != jobStatusFinishing {
		t.Fatalf("published status = %q, want %q", status, jobStatusFinishing)
	}
}

func TestWriteReportsTargetlessReadFailureAsItemFailure(t *testing.T) {
	// Build a target-free hash item whose source fails on its first read.
	readErr := errors.New("read failed")
	copyer := newTestCopyer(t, WithHashPolicy(HashRead))
	item := newFixtureItem("source")
	job := newWriteJob(&baseJob{
		copyer: copyer,
		item:   item,
		src:    &source{},
		path:   "source",
		stat:   &stat{size: 1},
	}, &failingReadCloser{err: readErr}, 1, false)
	completed := make(chan *baseJob, 1)

	// An item with no target outcome to report could not be processed at all.
	copyer.write(context.Background(), job, completed, new(counter), mapset.NewSet[string]())
	if err := (<-completed).itemError; !errors.Is(err, readErr) {
		t.Fatalf("item error = %v, want %v", err, readErr)
	}

	// Cleanup must route that item to its failure callback instead of completing it.
	copyed := make(chan *baseJob, 1)
	copyed <- job.baseJob
	close(copyed)
	copyer.cleanup(context.Background(), copyed)
	if result, err := item.terminal(t); !errors.Is(err, readErr) {
		t.Fatalf("terminal outcome = %#v / %v, want failure %v", result, err, readErr)
	}
	if err := copyer.WaitErr(); err != nil {
		t.Fatalf("WaitErr() = %v, want nil: a failed item is an item outcome", err)
	}
}

func TestWriteJobWaitConsumedReturnsOnCancellation(t *testing.T) {
	// Model a linear source whose reader has already moved to the Copy stage.
	job := newWriteJob(nil, new(trackingReadCloser), 0, true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Cancellation must release Prepare without waiting for Copy to consume the reader.
	if job.waitConsumed(ctx) {
		t.Fatal("waitConsumed() = true after cancellation, want false")
	}
}

func TestCopyClosesPreparedSourcesAfterCancellation(t *testing.T) {
	// Queue one prefetched reader before starting an already-canceled Copy stage.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	copyer := newTestCopyer(t)
	copyer.toDevice.threads = 1
	reader := new(trackingReadCloser)
	job := newWriteJob(&baseJob{copyer: copyer, item: newFixtureItem("source")}, reader, 0, true)
	prepared := make(chan *writeJob, 1)
	prepared <- job
	close(prepared)

	// Copy owns accepted readers and must drain and close them during cancellation.
	for range copyer.copy(ctx, prepared) {
	}
	if reader.closed != 1 {
		t.Fatalf("reader closed %d times, want 1", reader.closed)
	}
	if !job.waitConsumed(context.Background()) {
		t.Fatal("linear source was not notified that the reader was consumed")
	}

	// The abandoned item is handed to cleanup with the stopping error.
	select {
	case abandoned := <-copyer.abandoned:
		if abandoned != job.baseJob {
			t.Fatalf("abandoned job = %p, want %p", abandoned, job.baseJob)
		}
		if !errors.Is(abandoned.itemError, context.Canceled) {
			t.Fatalf("abandoned item error = %v, want %v", abandoned.itemError, context.Canceled)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("copy did not report the prefetched item")
	}
}

func TestWriteReturnsAfterHardStopWithoutPublishing(t *testing.T) {
	// Use an unbuffered completion channel with no receiver to expose a blocked handoff.
	copyer := newTestCopyer(t)
	copyer.stopHard()
	reader := new(trackingReadCloser)
	job := newWriteJob(&baseJob{
		copyer: copyer,
		item:   newFixtureItem("source"),
		src:    &source{},
		stat:   &stat{},
	}, reader, 0, false)
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
	copyer := newTestCopyer(t)
	item := newFixtureItem("source", "target")
	job := &baseJob{
		copyer: copyer, item: item, src: &source{}, stat: &stat{},
		targets: []string{"target"}, successTargets: []string{"target"},
	}

	// A late target failure must remove the target from the successful result.
	job.fail("target", syscall.ENOSPC)
	result := job.result()
	if len(result.Targets) != 1 || result.Targets[0].Err == nil {
		t.Fatalf("targets = %#v, want one failed outcome", result.Targets)
	}
	if report := job.report(); len(report.SuccessTargets) != 0 {
		t.Fatalf("success targets = %v, want none", report.SuccessTargets)
	}
	if !errors.Is(result.Targets[0].Err, syscall.ENOSPC) {
		t.Fatalf("target failure = %v, want %v", result.Targets[0].Err, syscall.ENOSPC)
	}
}

func TestTargetFailureKeepsItsIdentityAsItemOutcome(t *testing.T) {
	// Record a mapped write failure before a later, unrelated pipeline error.
	copyer := newTestCopyer(t)
	item := newFixtureItem("source", "target")
	job := &baseJob{
		copyer: copyer, item: item, src: &source{}, stat: &stat{}, targets: []string{"target"},
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
	copyer := newTestCopyer(t, WithHashPolicy(HashRead))
	job := newWriteJob(&baseJob{
		copyer:  copyer,
		item:    item,
		src:     &source{base: root, path: "source"},
		path:    filepath.Join(root, "source"),
		stat:    &stat{size: batchSize, mode: 0o644},
		targets: targets,
	}, &failingContentReader{err: readErr, batches: 1}, batchSize, false)

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

func TestLinearTargetStopsWhenDiskUsageEstimateIsInsufficient(t *testing.T) {
	// Size the Job beyond the filesystem estimate without allocating the source payload.
	root := t.TempDir()
	target := filepath.Join(root, "target")
	usage, err := godf.NewDiskUsage(root)
	if err != nil {
		t.Fatalf("read disk usage: %v", err)
	}
	if usage.Available() > math.MaxInt64-defaultDiskUsageFreshInterval {
		t.Fatal("available disk space cannot be represented by the test Job size")
	}
	size := usage.Available() + defaultDiskUsageFreshInterval
	option := newOption()
	if err := option.check(); err != nil {
		t.Fatal(err)
	}
	copyer := &Copyer{
		option: option, eventCh: make(chan Event, 8), hardStop: make(chan struct{}),
		getDevice:         func(string) string { return root },
		getDiskUsageCache: func(string) *diskUsageCache { return newDiskUsageCache(root, defaultDiskUsageFreshInterval) },
	}
	copyer.toDevice.linear = true
	item := newFixtureItem("source", target)
	job := newWriteJob(&baseJob{
		copyer: copyer, item: item, src: &source{}, path: "source",
		stat: &stat{size: size}, targets: []string{target},
	}, io.NopCloser(bytes.NewReader(nil)), size, false)
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

func TestStoppedLinearTargetDoesNotReadBatchSource(t *testing.T) {
	// Mark a linear target exhausted before its batch indexer requests more work.
	source := newSliceSource()
	copyer := newTestCopyer(t, SetToDevice(LinearDevice(true)))
	copyer.batch = source
	copyer.endLinearTarget(ErrTargetNoSpace)

	// A stopped target closes the index without consuming another batch.
	indexed, err := copyer.index(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for range indexed {
	}
	if calls := source.calls(); calls != 0 {
		t.Fatalf("source calls = %d, want 0", calls)
	}
}

func TestIndexReportsBatchSourceFailureAndYieldsAcceptedItems(t *testing.T) {
	// A source error is a pipeline failure, and items accepted before it stay owned.
	sourceErr := errors.New("source failed")
	copyer := newTestCopyer(t)
	item := newFixtureItem("source")
	copyer.batch = &sliceSource{batches: [][]Item{{item}}, err: sourceErr}

	indexed, err := copyer.index(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	accepted := 0
	for job := range indexed {
		accepted++
		if job.item != item {
			t.Fatalf("indexed item = %v, want the submitted item", job.item)
		}
	}
	if accepted != 1 {
		t.Fatalf("indexed items = %d, want 1", accepted)
	}
	if err := copyer.WaitErr(); !errors.Is(err, sourceErr) {
		t.Fatalf("WaitErr() = %v, want %v", err, sourceErr)
	}
}
