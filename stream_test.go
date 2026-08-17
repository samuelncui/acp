package acp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
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
	results []*StreamResult
	writes  int
	flushes int
	err     error
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
	return nil
}

func TestRunStreamCopiesRequestsInLinearOrder(t *testing.T) {
	// Create an ordered source stream without building ACP options per file.
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

	// Run the linear pipeline and verify every final result is emitted once.
	if err := RunStream(context.Background(), source, sink, WithHash(true), SetToDevice(LinearDevice(true))); err != nil {
		t.Fatal(err)
	}
	if len(sink.results) != len(source.requests) {
		t.Fatalf("received %d results, want %d", len(sink.results), len(source.requests))
	}
	for index, result := range sink.results {
		if result.ID != int64(index+1) {
			t.Fatalf("result ID = %d, want %d", result.ID, index+1)
		}
		if result.Job.Status != JobStatusFinished || len(result.Job.SuccessTargets) != 1 || result.Job.SHA256 == "" {
			t.Fatalf("unexpected result: %#v", result.Job)
		}
	}
	if sink.flushes != 1 {
		t.Fatalf("sink flushed %d times, want 1", sink.flushes)
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
