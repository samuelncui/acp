package acp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

type sliceStreamSource struct {
	requests []*StreamRequest
	index    int
	err      error
}

func (s *sliceStreamSource) Next(context.Context) (*StreamRequest, error) {
	if s.index < len(s.requests) {
		request := s.requests[s.index]
		s.index++
		return request, nil
	}
	if s.err != nil {
		return nil, s.err
	}
	return nil, io.EOF
}

type collectingStreamSink struct {
	results  []*StreamResult
	writes   int
	flushes  int
	err      error
	flushErr error
}

func (s *collectingStreamSink) Write(_ context.Context, result *StreamResult) error {
	s.writes++
	if s.err != nil {
		return s.err
	}
	s.results = append(s.results, result)
	return nil
}

func (s *collectingStreamSink) Flush(context.Context) error {
	s.flushes++
	return s.flushErr
}

func TestRunStreamCopiesRequestsToLinearTarget(t *testing.T) {
	// Create a source stream without building ACP options per file.
	root := t.TempDir()
	source := new(sliceStreamSource)
	for index, content := range []string{"first", "second", "third"} {
		input := filepath.Join(root, "source", string(rune('a'+index))+".txt")
		output := filepath.Join(root, "target", string(rune('a'+index))+".txt")
		if err := os.MkdirAll(filepath.Dir(input), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(input, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		source.requests = append(source.requests, &StreamRequest{ID: int64(index + 1), Source: input, Targets: []string{output}})
	}
	sink := new(collectingStreamSink)

	// Keep the target serialized while allowing source preparation to complete in any order.
	if err := RunStream(context.Background(), source, sink, WithHash(true), SetToDevice(LinearDevice(true))); err != nil {
		t.Fatal(err)
	}
	if len(sink.results) != len(source.requests) {
		t.Fatalf("received %d results, want %d", len(sink.results), len(source.requests))
	}
	seen := make(map[int64]struct{}, len(sink.results))
	for _, result := range sink.results {
		if result.ID < 1 || result.ID > int64(len(source.requests)) {
			t.Fatalf("unexpected result ID: %d", result.ID)
		}
		if _, exists := seen[result.ID]; exists {
			t.Fatalf("duplicate result ID: %d", result.ID)
		}
		seen[result.ID] = struct{}{}
		if result.Job.Status != JobStatusFinished || len(result.Job.SuccessTargets) != 1 || result.Job.SHA256 == "" {
			t.Fatalf("unexpected result: %#v", result.Job)
		}
	}
	if sink.flushes != 1 {
		t.Fatalf("sink flushed %d times, want 1", sink.flushes)
	}
}

func TestRunStreamHashesWithoutTargets(t *testing.T) {
	// Submit one targetless request through the same bounded stream interface.
	content := []byte("hash-only fixture")
	input := filepath.Join(t.TempDir(), "source.txt")
	if err := os.WriteFile(input, content, 0o644); err != nil {
		t.Fatal(err)
	}
	source := &sliceStreamSource{requests: []*StreamRequest{{ID: 1, Source: input}}}
	sink := new(collectingStreamSink)

	// Verify ACP reads the source once and reports its SHA-256 without creating a target.
	if err := RunStream(context.Background(), source, sink, WithHash(true)); err != nil {
		t.Fatal(err)
	}
	if len(sink.results) != 1 {
		t.Fatalf("received %d results, want 1", len(sink.results))
	}
	job := sink.results[0].Job
	wantHash := sha256.Sum256(content)
	if job.Status != JobStatusFinished || job.SHA256 != hex.EncodeToString(wantHash[:]) {
		t.Fatalf("unexpected hash-only result: %#v", job)
	}
	if len(job.SuccessTargets) != 0 || len(job.FailTargets) != 0 {
		t.Fatalf("hash-only targets = success:%v fail:%v", job.SuccessTargets, job.FailTargets)
	}
}

func TestRunStreamReturnsSourceAndSinkErrors(t *testing.T) {
	// Verify that source failures cross the synchronous stream interface.
	sourceErr := errors.New("source failed")
	err := RunStream(context.Background(), &sliceStreamSource{err: sourceErr}, new(collectingStreamSink))
	if !errors.Is(err, sourceErr) {
		t.Fatalf("RunStream() error = %v, want %v", err, sourceErr)
	}

	// Verify that persistence failures are returned after the pipeline drains.
	root := t.TempDir()
	input := filepath.Join(root, "source.txt")
	if err := os.WriteFile(input, []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	sinkErr := errors.New("sink failed")
	sink := &collectingStreamSink{err: sinkErr}
	err = RunStream(context.Background(), &sliceStreamSource{requests: []*StreamRequest{
		{ID: 1, Source: input, Targets: []string{filepath.Join(root, "target-1.txt")}},
		{ID: 2, Source: input, Targets: []string{filepath.Join(root, "target-2.txt")}},
		{ID: 3, Source: input, Targets: []string{filepath.Join(root, "target-3.txt")}},
	}}, sink, Overwrite(true))
	if !errors.Is(err, sinkErr) {
		t.Fatalf("RunStream() error = %v, want %v", err, sinkErr)
	}
	if sink.writes != 1 || sink.flushes != 0 {
		t.Fatalf("sink calls = writes:%d flushes:%d, want writes:1 flushes:0", sink.writes, sink.flushes)
	}

	// Verify that final persistence failures cross the synchronous stream interface.
	flushErr := errors.New("flush failed")
	sink = &collectingStreamSink{flushErr: flushErr}
	err = RunStream(context.Background(), &sliceStreamSource{requests: []*StreamRequest{{
		ID: 4, Source: input,
	}}}, sink, WithHash(true))
	if !errors.Is(err, flushErr) {
		t.Fatalf("RunStream() error = %v, want %v", err, flushErr)
	}
	if sink.writes != 1 || sink.flushes != 1 {
		t.Fatalf("sink calls = writes:%d flushes:%d, want writes:1 flushes:1", sink.writes, sink.flushes)
	}
}

func TestRunStreamReturnsTargetFailure(t *testing.T) {
	// Force target creation to fail while allowing the Sink to accept the final Job result.
	root := t.TempDir()
	input := filepath.Join(root, "source.txt")
	target := filepath.Join(root, "target.txt")
	if err := os.WriteFile(input, []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("existing"), 0o644); err != nil {
		t.Fatal(err)
	}
	sink := new(collectingStreamSink)

	// A failed copy is a RunStream error even when the Sink records the failed result.
	err := RunStream(context.Background(), &sliceStreamSource{requests: []*StreamRequest{{
		ID: 1, Source: input, Targets: []string{target},
	}}}, sink)
	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("RunStream() error = %v, want %v", err, os.ErrExist)
	}
	if sink.writes != 1 || len(sink.results) != 1 {
		t.Fatalf("sink results = writes:%d results:%d, want writes:1 results:1", sink.writes, len(sink.results))
	}
	if len(sink.results[0].Job.FailTargets) != 1 {
		t.Fatalf("failed targets = %v, want one target", sink.results[0].Job.FailTargets)
	}
}

type boundedStreamSource struct {
	input    string
	target   string
	total    int64
	produced int64
	consumed *int64
	maximum  int64
}

func (s *boundedStreamSource) Next(context.Context) (*StreamRequest, error) {
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
	return &StreamRequest{
		ID: id, Source: s.input, Targets: []string{filepath.Join(s.target, fmt.Sprintf("%04d", id))},
	}, nil
}

type countingStreamSink struct {
	consumed int64
}

func (s *countingStreamSink) Write(context.Context, *StreamResult) error {
	atomic.AddInt64(&s.consumed, 1)
	return nil
}

func (*countingStreamSink) Flush(context.Context) error {
	return nil
}

func TestRunStreamAppliesBoundedBackpressure(t *testing.T) {
	root := t.TempDir()
	input := filepath.Join(root, "source.txt")
	if err := os.WriteFile(input, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	const total = int64(1024)
	sink := new(countingStreamSink)
	source := &boundedStreamSource{
		input: input, target: filepath.Join(root, "target"), total: total, consumed: &sink.consumed,
	}
	if err := RunStream(context.Background(), source, sink, SetToDevice(LinearDevice(true))); err != nil {
		t.Fatal(err)
	}
	if consumed := atomic.LoadInt64(&sink.consumed); consumed != total {
		t.Fatalf("consumed %d results, want %d", consumed, total)
	}
	if maximum := atomic.LoadInt64(&source.maximum); maximum >= total/2 {
		t.Fatalf("maximum outstanding requests = %d, want less than %d", maximum, total/2)
	}
}

type untilCanceledStreamSource struct {
	input  string
	target string
	nextID int64
}

func (s *untilCanceledStreamSource) Next(ctx context.Context) (*StreamRequest, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	s.nextID++
	return &StreamRequest{
		ID:      s.nextID,
		Source:  s.input,
		Targets: []string{filepath.Join(s.target, fmt.Sprintf("%04d", s.nextID))},
	}, nil
}

func TestRunStreamCancellationDrainsPrefetchedJobs(t *testing.T) {
	// Feed work until Prepare starts so cancellation occurs with an active pipeline.
	root := t.TempDir()
	input := filepath.Join(root, "source.txt")
	if err := os.WriteFile(input, []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	source := &untilCanceledStreamSource{input: input, target: filepath.Join(root, "target")}
	cancelWhenPreparing := func(event Event) {
		update, ok := event.(*EventUpdateJob)
		if !ok {
			return
		}
		if update.Job.Status != JobStatusPreparing {
			return
		}
		cancel()
	}

	// The canceled pipeline must drain its queues and return the context error.
	done := make(chan error, 1)
	go func() {
		done <- RunStream(
			ctx,
			source,
			new(collectingStreamSink),
			SetToDevice(LinearDevice(true)),
			WithEventHandler(cancelWhenPreparing),
		)
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("RunStream() error = %v, want %v", err, context.Canceled)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunStream did not return after cancellation")
	}
}
