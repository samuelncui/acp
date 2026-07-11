package acp

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
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
