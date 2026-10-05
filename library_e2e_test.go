package acp_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/samuelncui/acp"
)

// streamRun is the results callback of the caller-owned variant: it records every result the
// push engine delivers, so the test can assert the result contract instead of a report row.
type streamRun struct {
	lock    sync.Mutex
	results []acp.Result
}

func (r *streamRun) onResults(results []acp.Result) error {
	r.lock.Lock()
	defer r.lock.Unlock()

	r.results = append(r.results, results...)
	return nil
}

// delivered returns every result the run reported, in delivery order.
func (r *streamRun) delivered() []acp.Result {
	r.lock.Lock()
	defer r.lock.Unlock()

	return append([]acp.Result(nil), r.results...)
}

func TestACPLibraryE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping end-to-end test in short mode")
	}

	// Prepare two target directories and a tree with non-default metadata.
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
	modified := time.Unix(1700000000, 0)
	metadata := make(map[string]os.FileInfo, len(files))
	for relativePath, data := range files {
		path := filepath.Join(source, relativePath)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create source directory: %v", err)
		}
		if err := os.WriteFile(path, data, 0o640); err != nil {
			t.Fatalf("write source file %q: %v", relativePath, err)
		}
		if err := os.Chtimes(path, modified, modified); err != nil {
			t.Fatalf("set source times: %v", err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat source: %v", err)
		}
		metadata[relativePath] = info
	}

	// The shell maps the source tree onto both targets and reports one terminal row per file.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	handler, getter := acp.NewReportGetter()
	copyer, err := acp.New(
		ctx,
		acp.WildcardJob(acp.Source(source), acp.Target(targets...)),
		acp.WithHashPolicy(acp.HashRead),
		acp.Overwrite(true),
		acp.WithEventHandler(handler),
	)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := copyer.WaitErr(); err != nil {
		t.Fatalf("run: %v", err)
	}

	// Verify the shell actually restores content and metadata at both targets.
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
			info, err := os.Stat(filepath.Join(copyRoot, relativePath))
			if err != nil {
				t.Fatalf("stat target: %v", err)
			}
			if want := metadata[relativePath]; info.Mode() != want.Mode() || !info.ModTime().Equal(want.ModTime()) {
				t.Fatalf("target %q metadata = %v / %v, want %v / %v",
					relativePath, info.Mode(), info.ModTime(), want.Mode(), want.ModTime())
			}
		}
	}

	// Report rows must match the files the shell selected and copied.
	report := getter()
	if len(report.Errors) != 0 {
		t.Fatalf("report errors = %v", report.Errors)
	}
	if len(report.Jobs) != len(files) {
		t.Fatalf("report jobs = %d, want %d", len(report.Jobs), len(files))
	}

	// Locate each selected file's row independently of delivery order.
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
		if want := metadata[relativePath]; job.Mode != want.Mode() || !job.ModTime.Equal(want.ModTime()) {
			t.Fatalf("job %q metadata = %v / %v, want %v / %v",
				sourcePath, job.Mode, job.ModTime, want.Mode(), want.ModTime())
		}

		// Each requested target must appear exactly once in the report row.
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
			delete(wantTargets, target)
		}

		// A hash must describe the selected file's exact content, including an empty file.
		sum := sha256.Sum256(data)
		if want := hex.EncodeToString(sum[:]); job.SHA256 != want {
			t.Fatalf("job %q SHA256 = %q, want %q", sourcePath, job.SHA256, want)
		}
	}

	// Submit caller-owned items to fresh targets so the shell's copies cannot hide a failed write.
	items := make([]acp.Item, 0, len(files))
	submitted := make([]*acp.SimpleJob, 0, len(files))
	for relativePath := range files {
		dsts := make([]string, 0, len(targets))
		for _, target := range targets {
			dsts = append(dsts, filepath.Join(target, "stream", relativePath))
		}
		job := &acp.SimpleJob{Path: filepath.Join(source, relativePath), Dsts: dsts}
		items = append(items, job)
		submitted = append(submitted, job)
	}

	// Exercise the public stream lifecycle without the compatibility shell.
	run := new(streamRun)
	stream, err := acp.NewStream(
		ctx,
		run.onResults,
		acp.WithHashPolicy(acp.HashRead),
		acp.Overwrite(true),
	)
	if err != nil {
		t.Fatalf("new stream: %v", err)
	}
	if err := stream.Submit(items...); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := stream.Wait(); err != nil {
		t.Fatalf("wait: %v", err)
	}

	// Every submitted item owns exactly one result, and that result reports the item's own
	// content facts plus one outcome per requested target, in request order.
	results := run.delivered()
	if len(results) != len(submitted) {
		t.Fatalf("results = %d, want %d", len(results), len(submitted))
	}
	counted := make(map[*acp.SimpleJob]int, len(submitted))
	for _, result := range results {
		job, ok := result.Job.(*acp.SimpleJob)
		if !ok {
			t.Fatalf("result job = %#v, want the submitted *acp.SimpleJob", result.Job)
		}
		counted[job]++

		// Validate the source facts associated with this exact submitted instance.
		relativePath, err := filepath.Rel(source, job.Path)
		if err != nil {
			t.Fatalf("relative path of %q: %v", job.Path, err)
		}
		data, ok := files[relativePath]
		if !ok {
			t.Fatalf("result for an unexpected source %q", job.Path)
		}
		if result.Err != nil {
			t.Fatalf("job %q failed: %v", job.Path, result.Err)
		}
		if result.Size != int64(len(data)) {
			t.Fatalf("job %q size = %d, want %d", job.Path, result.Size, len(data))
		}
		if want := metadata[relativePath]; result.Mode != want.Mode() || !result.ModTime.Equal(want.ModTime()) {
			t.Fatalf("job %q metadata = %v / %v, want %v / %v",
				job.Path, result.Mode, result.ModTime, want.Mode(), want.ModTime())
		}

		// Verify the computed hash and the requested target outcomes against the source fixture.
		sum := sha256.Sum256(data)
		if !bytes.Equal(result.SHA256, sum[:]) {
			t.Fatalf("job %q SHA256 = %x, want %x", job.Path, result.SHA256, sum)
		}

		// Fresh target files must hold the content and metadata reported by this item.
		if len(result.Targets) != len(job.Dsts) {
			t.Fatalf("job %q targets = %v, want %d", job.Path, result.Targets, len(job.Dsts))
		}
		for index, outcome := range result.Targets {
			if outcome.Path != job.Dsts[index] || outcome.Err != nil {
				t.Fatalf("job %q target %d = %#v", job.Path, index, outcome)
			}
			if outcome.Size != result.Size || outcome.WriteTime.IsZero() || !outcome.WriteTime.Equal(result.WriteTime) {
				t.Fatalf("target %q facts = %#v, want the item's size and write time", outcome.Path, outcome)
			}
			got, err := os.ReadFile(outcome.Path)
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("target %q content = %v / %v, want %v", outcome.Path, got, err, data)
			}
			info, err := os.Stat(outcome.Path)
			if err != nil {
				t.Fatalf("stat target: %v", err)
			}
			if want := metadata[relativePath]; info.Mode() != want.Mode() || !info.ModTime().Equal(want.ModTime()) {
				t.Fatalf("target %q metadata = %v / %v, want %v / %v",
					outcome.Path, info.Mode(), info.ModTime(), want.Mode(), want.ModTime())
			}
		}
	}

	// No submitted instance may be lost or reported twice.
	for _, job := range submitted {
		if counted[job] != 1 {
			t.Fatalf("job %q received %d results, want exactly 1", job.Path, counted[job])
		}
	}
}
