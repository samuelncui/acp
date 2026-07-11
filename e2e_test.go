package acp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/samuelncui/acp"
)

func TestACPCommandE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping end-to-end test in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	repoRoot, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	tempDir := t.TempDir()
	binary := filepath.Join(tempDir, "acp")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}

	run := func(name string, args ...string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Dir = repoRoot
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("run %s: %v\n%s", name, err, output)
		}
	}
	run("go", "build", "-o", binary, "./cmd/acp")

	source := filepath.Join(tempDir, "source")
	target := filepath.Join(tempDir, "target")
	reportPath := filepath.Join(tempDir, "report.json")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("create target directory: %v", err)
	}

	files := map[string][]byte{
		"plain.txt":                     []byte("plain text\n"),
		"empty.txt":                     nil,
		filepath.Join("nested", "data"): {0, 1, 2, 3, 4},
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

	run(binary, "-p=false", "-report", reportPath, source, target)

	copyRoot := filepath.Join(target, filepath.Base(source))
	for relativePath, want := range files {
		got, err := os.ReadFile(filepath.Join(copyRoot, relativePath))
		if err != nil {
			t.Fatalf("read copied file %q: %v", relativePath, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("copied file %q = %v, want %v", relativePath, got, want)
		}
	}

	reportData, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	var report acp.Report
	if err := json.Unmarshal(reportData, &report); err != nil {
		t.Fatalf("decode report: %v", err)
	}
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
	for relativePath := range files {
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
		wantTarget := filepath.Join(copyRoot, relativePath)
		if len(job.SuccessTargets) != 1 || job.SuccessTargets[0] != wantTarget {
			t.Fatalf("job %q success targets = %v", sourcePath, job.SuccessTargets)
		}
		if job.SHA256 == "" {
			t.Fatalf("job %q has no SHA256", sourcePath)
		}
	}
}
