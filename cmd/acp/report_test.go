package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/samuelncui/acp"
)

// reportShape is the JSON shape the command promises: one row per file, item and target
// failures inside fail_target, and a top-level errors array that carries pipeline problems.
type reportShape struct {
	Files []struct {
		FullPath    string            `json:"full_path"`
		Status      string            `json:"status"`
		Success     []string          `json:"success_target"`
		FailTargets map[string]string `json:"fail_target"`
	} `json:"files"`
	Errors []struct {
		Src string `json:"src"`
		Dst string `json:"dst"`
		Err string `json:"error"`
	} `json:"errors"`
}

// runJobs drives one shell run through the command's own report handling. watcher, when it is
// not nil, observes every event before the collector does: the shell publishes one terminal row
// per item, so a test that stops a run early triggers on the first of those rows.
func runJobs(
	t *testing.T,
	ctx context.Context,
	watcher func(acp.Event),
	opts ...acp.Option,
) (*report, error) {
	t.Helper()

	collector := newReport()
	handler := acp.EventHandler(collector.handleEvent)
	if watcher != nil {
		handler = func(event acp.Event) {
			watcher(event)
			collector.handleEvent(event)
		}
	}

	options := append([]acp.Option{acp.WithEventHandler(handler)}, opts...)
	copyer, err := acp.New(ctx, options...)
	if err != nil {
		return collector, err
	}
	return collector, copyer.WaitErr()
}

func decodeReport(t *testing.T, collector *report) reportShape {
	t.Helper()

	var shape reportShape
	if err := json.Unmarshal([]byte(collector.getter().ToJSONString(false)), &shape); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	return shape
}

func writeEntryFile(t *testing.T, dir, name string, content []byte) string {
	t.Helper()

	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create directory: %v", err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("write file %q: %v", path, err)
	}
	return path
}

func TestReportKeepsFailuresInsideTheFileRow(t *testing.T) {
	root := t.TempDir()
	sourceDir := filepath.Join(root, "source")
	targetDir := filepath.Join(root, "target")
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Three files: one ACP copies, one whose target refuses the write, and one ACP cannot open
	// at all. A wildcard run maps a source tree under base(sourceDir) inside each target.
	readable := writeEntryFile(t, sourceDir, "readable.txt", []byte("fixture"))
	refusedSource := writeEntryFile(t, sourceDir, "refused.txt", []byte("fixture"))
	unreadable := writeEntryFile(t, sourceDir, "unreadable.txt", []byte("fixture"))
	if err := os.Chmod(unreadable, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o644) })

	// A host that can open a mode-000 file cannot produce the unreadable case at all.
	if file, err := os.Open(unreadable); err == nil {
		_ = file.Close()
		t.Skip("the host can open a mode-000 file, so the unreadable source cannot be tested")
	}

	// The target refuses the write because it already exists, while rows are keyed by source.
	refused := filepath.Join(targetDir, filepath.Base(sourceDir), "refused.txt")
	writeEntryFile(t, filepath.Dir(refused), filepath.Base(refused), []byte("existing"))

	collector, runErr := runJobs(
		t,
		context.Background(),
		nil,
		acp.WildcardJob(acp.Source(sourceDir), acp.Target(targetDir)),
	)
	if runErr != nil {
		t.Fatalf("run error = %v, want nil: an item failure is an item outcome", runErr)
	}

	// Both failures stay inside the file row, and neither adds a top-level error.
	shape := decodeReport(t, collector)
	if len(shape.Errors) != 0 {
		t.Fatalf("report errors = %#v, want none", shape.Errors)
	}
	if len(shape.Files) != 3 {
		t.Fatalf("report files = %d, want 3", len(shape.Files))
	}

	rows := make(map[string]int, len(shape.Files))
	for index, row := range shape.Files {
		rows[row.FullPath] = index
		if row.Status != acp.JobStatusFinished {
			t.Fatalf("row %d status = %q, want %q", index, row.Status, acp.JobStatusFinished)
		}
	}

	unreadableRow := shape.Files[rows[unreadable]]
	if len(unreadableRow.FailTargets) != 1 || unreadableRow.FailTargets[""] == "" {
		t.Fatalf(
			"unreadable source row fail_target = %v, want one empty-key failure",
			unreadableRow.FailTargets,
		)
	}
	if len(unreadableRow.Success) != 0 {
		t.Fatalf("unreadable source row success_target = %v, want none", unreadableRow.Success)
	}

	failedTarget := shape.Files[rows[refusedSource]]
	if len(failedTarget.FailTargets) != 1 || failedTarget.FailTargets[refused] == "" {
		t.Fatalf("refused target row fail_target = %v, want %q", failedTarget.FailTargets, refused)
	}
	if len(failedTarget.Success) != 0 {
		t.Fatalf("refused target row success_target = %v, want none", failedTarget.Success)
	}

	// The one file that could be copied really was copied.
	copied := shape.Files[rows[readable]]
	if len(copied.Success) != 1 || len(copied.FailTargets) != 0 {
		t.Fatalf(
			"readable source row success_target = %v, fail_target = %v",
			copied.Success, copied.FailTargets,
		)
	}
	if _, err := os.Stat(filepath.Join(targetDir, filepath.Base(sourceDir), "readable.txt")); err != nil {
		t.Fatalf("copied file: %v", err)
	}
}

func TestReportAccountsForEveryEntryAfterAGracefulStop(t *testing.T) {
	root := t.TempDir()
	sourceDir := filepath.Join(root, "source")
	targetDir := filepath.Join(root, "target")
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		t.Fatal(err)
	}

	const total = 24
	sources := make([]string, 0, total)
	for index := 0; index < total; index++ {
		name := fmt.Sprintf("%02d.txt", index)
		sources = append(sources, writeEntryFile(t, sourceDir, name, []byte(strings.Repeat(name, 32))))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The shell emits terminal rows only, so the stop trigger is the first row: one completed
	// file has its row published while later files are still queued in the pipeline.
	var once sync.Once
	stopOnFirstRow := func(event acp.Event) {
		if _, ok := event.(*acp.EventUpdateJob); ok {
			once.Do(cancel)
		}
	}

	// One result per delivery publishes every completed row immediately, and one writer
	// serializes the copies, so the stop lands while the run is still working.
	collector, runErr := runJobs(
		t,
		ctx,
		stopOnFirstRow,
		acp.WithResultBuffer(1),
		acp.WithResultBatch(1),
		acp.SetToDevice(acp.DeviceThreads(1)),
		acp.WildcardJob(acp.Source(sourceDir), acp.Target(targetDir)),
	)
	if !errors.Is(runErr, context.Canceled) {
		t.Fatalf("run error = %v, want %v", runErr, context.Canceled)
	}

	// The report accounts for every selected file exactly once: a stop abandons work, it does
	// not drop rows.
	shape := decodeReport(t, collector)
	if len(shape.Errors) != 0 {
		t.Fatalf("report errors = %#v, want none", shape.Errors)
	}
	if len(shape.Files) != len(sources) {
		t.Fatalf("report files = %d, want %d", len(shape.Files), len(sources))
	}

	stopped, completed := 0, 0
	rows := make(map[string]int, len(shape.Files))
	for _, row := range shape.Files {
		rows[row.FullPath]++
		if len(row.FailTargets) != 0 {
			if row.FailTargets[""] == "" {
				t.Fatalf("abandoned row for %q = %v, want the stopping error under the empty target key", row.FullPath, row.FailTargets)
			}
			stopped++
			continue
		}
		if len(row.Success) != 1 {
			t.Fatalf("completed row for %q success_target = %v, want one", row.FullPath, row.Success)
		}
		completed++
	}
	for _, source := range sources {
		if rows[source] != 1 {
			t.Fatalf("report rows for %q = %d, want exactly 1", source, rows[source])
		}
	}
	if completed == 0 {
		t.Fatal("the file in flight at the stop did not complete")
	}
	if stopped == 0 {
		t.Fatal("the stop abandoned no file")
	}
}
