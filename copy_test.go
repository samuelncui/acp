package acp

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	mapset "github.com/deckarep/golang-set/v2"
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

func TestCopyEmptyFile(t *testing.T) {
	tests := []struct {
		name string
		opts []Option
	}{
		{name: "mmap"},
		{name: "linear", opts: []Option{SetFromDevice(LinearDevice(true))}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "src")
			dst := filepath.Join(dir, "dst")
			if err := os.WriteFile(src, nil, 0o644); err != nil {
				t.Fatalf("write src: %v", err)
			}

			handler, getter := NewReportGetter()
			opts := append([]Option{
				AccurateJob(src, []string{dst}),
				Overwrite(true),
				WithEventHandler(handler),
			}, tt.opts...)
			copyer, err := New(context.Background(), opts...)
			if err != nil {
				t.Fatalf("new copyer: %v", err)
			}

			done := make(chan struct{})
			go func() {
				copyer.Wait()
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatalf("copy empty file timed out")
			}

			info, err := os.Stat(dst)
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
			if len(job.SuccessTargets) != 1 || job.SuccessTargets[0] != dst {
				t.Fatalf("success targets = %v", job.SuccessTargets)
			}
		})
	}
}

func TestWritePublishesFinishingJob(t *testing.T) {
	// Build a target-free write Job so only the worker-to-cleanup handoff is exercised.
	copyer := &Copyer{option: newOption(), eventCh: make(chan Event, 8)}
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
	copyer := &Copyer{option: newOption(), eventCh: make(chan Event, 1)}
	copyer.toDevice.threads = 1
	reader := new(trackingReadCloser)
	job := newWriteJob(nil, reader, 0, true)
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
}

func TestWriteReturnsWhenCanceledBeforePublishing(t *testing.T) {
	// Use an unbuffered completion channel with no receiver to expose a blocked handoff.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	copyer := &Copyer{option: newOption(), eventCh: make(chan Event, 8)}
	reader := new(trackingReadCloser)
	job := newWriteJob(&baseJob{
		copyer: copyer,
		src:    &source{},
		stat:   &stat{},
	}, reader, 0, false)
	done := make(chan struct{})
	go func() {
		copyer.write(ctx, job, make(chan *baseJob), new(counter), mapset.NewSet[string]())
		close(done)
	}()

	// Cancellation must skip the completion handoff while retaining source cleanup.
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("write did not return after cancellation")
	}
	if reader.closed != 1 {
		t.Fatalf("reader closed %d times, want 1", reader.closed)
	}
}
