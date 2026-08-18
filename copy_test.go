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
