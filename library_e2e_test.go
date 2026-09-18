package acp_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/samuelncui/acp"
)

// entryItem is the caller-owned item the library end-to-end test feeds to acp.Run.
type entryItem struct {
	entry acp.FileEntry

	lock   sync.Mutex
	result *acp.Result
	err    error
	count  int
}

func (i *entryItem) Source() string { return i.entry.Source }

func (i *entryItem) Targets() []string { return i.entry.Targets }

func (i *entryItem) Completed(result *acp.Result) {
	i.lock.Lock()
	defer i.lock.Unlock()
	i.result = result
	i.count++
}

func (i *entryItem) Failed(err error) {
	i.lock.Lock()
	defer i.lock.Unlock()
	i.err = err
	i.count++
}

// outcome returns the item's single terminal outcome.
func (i *entryItem) outcome(t *testing.T) (*acp.Result, error) {
	t.Helper()

	i.lock.Lock()
	defer i.lock.Unlock()
	if i.count != 1 {
		t.Fatalf("item %q received %d terminal callbacks, want 1", i.entry.Source, i.count)
	}
	return i.result, i.err
}

// entrySource supplies the selected entries as one batch of caller-owned items.
type entrySource struct {
	entries []acp.FileEntry

	lock  sync.Mutex
	items []*entryItem
}

func (s *entrySource) Next(context.Context) ([]acp.Item, error) {
	s.lock.Lock()
	defer s.lock.Unlock()

	if len(s.entries) == 0 {
		return nil, io.EOF
	}

	entries := s.entries
	s.entries = nil

	items := make([]acp.Item, 0, len(entries))
	for _, entry := range entries {
		item := &entryItem{entry: entry}
		s.items = append(s.items, item)
		items = append(items, item)
	}
	return items, nil
}

func (s *entrySource) submitted() []*entryItem {
	s.lock.Lock()
	defer s.lock.Unlock()
	return append([]*entryItem(nil), s.items...)
}

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

	// Select the entries the public API copies, then submit them as caller-owned items.
	entries, err := acp.SelectFiles([]string{source}, targets)
	if err != nil {
		t.Fatalf("select files: %v", err)
	}
	if len(entries) != len(files) {
		t.Fatalf("selected entries = %d, want %d", len(entries), len(files))
	}
	items := &entrySource{entries: entries}

	handler, getter := acp.NewReportGetter()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := acp.Run(
		ctx,
		items,
		acp.WithHashPolicy(acp.HashRead),
		acp.SetToDevice(acp.Overwrite(true)),
		acp.WithEventHandler(handler),
	); err != nil {
		t.Fatalf("run: %v", err)
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

	// Every submitted item owns one completion with one outcome per requested target.
	submitted := items.submitted()
	if len(submitted) != len(files) {
		t.Fatalf("submitted items = %d, want %d", len(submitted), len(files))
	}
	for _, item := range submitted {
		result, err := item.outcome(t)
		if err != nil {
			t.Fatalf("item %q failed: %v", item.entry.Source, err)
		}
		if result.Source != filepath.Clean(item.entry.Source) {
			t.Fatalf("item %q result source = %q", item.entry.Source, result.Source)
		}
		if len(result.Targets) != len(targets) {
			t.Fatalf("item %q targets = %v, want %d", item.entry.Source, result.Targets, len(targets))
		}
		for index, outcome := range result.Targets {
			if outcome.Path != item.entry.Targets[index] || outcome.Err != nil {
				t.Fatalf("item %q target %d = %#v", item.entry.Source, index, outcome)
			}
		}

		data := files[item.entry.Source[len(source)+1:]]
		sum := sha256.Sum256(data)
		if !bytes.Equal(result.SHA256, sum[:]) {
			t.Fatalf("item %q SHA256 = %x, want %x", item.entry.Source, result.SHA256, sum)
		}
	}
}
