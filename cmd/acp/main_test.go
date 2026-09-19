package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/samuelncui/acp"
)

// TestAccurateTargetUsesItsTargets pins the exact-target decision to the targets the function
// is given, not to the package-level flag state.
func TestAccurateTargetUsesItsTargets(t *testing.T) {
	root := t.TempDir()
	source := writeEntryFile(t, root, "source.txt", []byte("fixture"))
	directory := filepath.Join(root, "target-dir")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "target.txt")

	previous := targetPaths
	t.Cleanup(func() { targetPaths = previous })

	// The flag state names a directory while the argument names an exact target file.
	targetPaths = []string{directory}
	if !accurateTarget([]string{source}, []string{file}) {
		t.Fatal("accurateTarget() = false, want the exact target named by its argument")
	}

	// The other way around: the argument is a directory, so the source tree is mapped onto it.
	targetPaths = []string{file}
	if accurateTarget([]string{source}, []string{directory}) {
		t.Fatal("accurateTarget() = true, want a mapped target directory")
	}

	// Anything but one source and one target is never a single exact copy.
	if accurateTarget([]string{source, source}, []string{file}) {
		t.Fatal("accurateTarget() = true for two sources")
	}
	if accurateTarget([]string{source}, nil) {
		t.Fatal("accurateTarget() = true without a target")
	}
}

// TestReportFailureDecision pins the exit-status decision: a run with any item, target or
// pipeline failure is a failure, and a clean run is not. The collector is event-driven, so each
// case feeds it the terminal row event the shell publishes.
func TestReportFailureDecision(t *testing.T) {
	clean := newReport()
	clean.handleEvent(&acp.EventUpdateJob{Job: &acp.Job{
		FullPath:       "/source/a.txt",
		Status:         acp.JobStatusFinished,
		SuccessTargets: []string{"/target/a.txt"},
	}})
	if clean.hasFailure() {
		t.Fatal("hasFailure() = true for a completed target")
	}

	refused := newReport()
	refused.handleEvent(&acp.EventUpdateJob{Job: &acp.Job{
		FullPath:    "/source/a.txt",
		Status:      acp.JobStatusFinished,
		FailTargets: map[string]error{"/target/a.txt": errors.New("file exists")},
	}})
	if !refused.hasFailure() {
		t.Fatal("hasFailure() = false for a failed target")
	}

	failed := newReport()
	failed.handleEvent(&acp.EventUpdateJob{Job: &acp.Job{
		FullPath:    "/source/a.txt",
		Status:      acp.JobStatusFinished,
		FailTargets: map[string]error{"": errors.New("source missing")},
	}})
	if !failed.hasFailure() {
		t.Fatal("hasFailure() = false for an item failure")
	}

	pipeline := newReport()
	pipeline.errors = append(pipeline.errors, &acp.Error{Src: "src", Err: errors.New("pipeline")})
	if !pipeline.hasFailure() {
		t.Fatal("hasFailure() = false for a pipeline error")
	}
}

// TestStoreReportWritesTheFailureDocument pins the report file: it is written for a failed run,
// under a two-space indent, and stays readable by the standard library.
func TestStoreReportWritesTheFailureDocument(t *testing.T) {
	collector := newReport()
	collector.handleEvent(&acp.EventUpdateJob{Job: &acp.Job{
		FullPath:    "/source/a.txt",
		Status:      acp.JobStatusFinished,
		FailTargets: map[string]error{"": errors.New("source missing")},
	}})

	path := filepath.Join(t.TempDir(), "report.json")
	if err := storeReport(collector, path, true); err != nil {
		t.Fatalf("storeReport: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	if bytes.Contains(data, []byte("\t")) {
		t.Fatalf("indented report contains a tab:\n%s", data)
	}

	var shape reportShape
	if err := json.Unmarshal(data, &shape); err != nil {
		t.Fatalf("decode stored report: %v\n%s", err, data)
	}
	if len(shape.Files) != 1 {
		t.Fatalf("report files = %d, want 1", len(shape.Files))
	}
	if failure := shape.Files[0].FailTargets[""]; failure != "source missing" {
		t.Fatalf("report failure = %q, want the item failure under the empty key", failure)
	}

	// A run without a report path stores nothing.
	if err := storeReport(collector, "", false); err != nil {
		t.Fatalf("storeReport without a path: %v", err)
	}
}
