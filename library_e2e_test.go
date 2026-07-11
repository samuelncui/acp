package acp_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/samuelncui/acp"
)

func TestACPLibraryE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping end-to-end test in short mode")
	}

	tempDir := t.TempDir()
	source := filepath.Join(tempDir, "source")
	targets := []string{
		filepath.Join(tempDir, "target-a"),
		filepath.Join(tempDir, "target-b"),
	}
	for _, target := range targets {
		if err := os.MkdirAll(target, 0o755); err != nil {
			t.Fatalf("create target directory %q: %v", target, err)
		}
	}

	files := map[string][]byte{
		"plain.txt":                     []byte("library copy\n"),
		"empty.txt":                     nil,
		filepath.Join("nested", "data"): {5, 4, 3, 2, 1},
	}
	for relativePath, data := range files {
		path := filepath.Join(source, relativePath)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create source directory: %v", err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatalf("write source file %q: %v", relativePath, err)
		}
	}

	handler, getter := acp.NewReportGetter()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	copyer, err := acp.New(
		ctx,
		acp.WildcardJob(acp.Source(source), acp.Target(targets...)),
		acp.WithHash(true),
		acp.Overwrite(true),
		acp.WithEventHandler(handler),
	)
	if err != nil {
		t.Fatalf("create copyer: %v", err)
	}

	done := make(chan struct{})
	go func() {
		copyer.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatalf("wait for copyer: %v", ctx.Err())
	}

	for _, target := range targets {
		copyRoot := filepath.Join(target, filepath.Base(source))
		for relativePath, want := range files {
			got, err := os.ReadFile(filepath.Join(copyRoot, relativePath))
			if err != nil {
				t.Fatalf("read copied file %q from %q: %v", relativePath, target, err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("copied file %q from %q = %v, want %v", relativePath, target, got, want)
			}
		}
	}

	report := getter()
	if len(report.Errors) != 0 {
		t.Fatalf("report errors = %v", report.Errors)
	}
	if len(report.Jobs) != len(files) {
		t.Fatalf("report jobs = %d, want %d", len(report.Jobs), len(files))
	}

	jobs := make(map[string]*acp.Job, len(report.Jobs))
	for _, job := range report.Jobs {
		jobs[job.FullPath] = job
	}
	for relativePath, data := range files {
		sourcePath := filepath.Join(source, relativePath)
		job, ok := jobs[sourcePath]
		if !ok {
			t.Fatalf("report job not found for %q", sourcePath)
		}
		if job.Status != acp.JobStatusFinished {
			t.Fatalf("job %q status = %q", sourcePath, job.Status)
		}
		if len(job.FailTargets) != 0 {
			t.Fatalf("job %q failures = %v", sourcePath, job.FailTargets)
		}
		if job.Size != int64(len(data)) {
			t.Fatalf("job %q size = %d, want %d", sourcePath, job.Size, len(data))
		}

		wantTargets := make(map[string]struct{}, len(targets))
		for _, target := range targets {
			wantTargets[filepath.Join(target, filepath.Base(source), relativePath)] = struct{}{}
		}
		if len(job.SuccessTargets) != len(wantTargets) {
			t.Fatalf("job %q success targets = %v", sourcePath, job.SuccessTargets)
		}
		for _, target := range job.SuccessTargets {
			if _, ok := wantTargets[target]; !ok {
				t.Fatalf("job %q unexpected success target %q", sourcePath, target)
			}
		}

		sum := sha256.Sum256(data)
		if want := hex.EncodeToString(sum[:]); job.SHA256 != want {
			t.Fatalf("job %q SHA256 = %q, want %q", sourcePath, job.SHA256, want)
		}
	}
}
