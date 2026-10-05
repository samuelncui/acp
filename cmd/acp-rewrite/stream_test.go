package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/samuelncui/acp"
	"github.com/samuelncui/acp/internal/fileio"
)

func TestRewriteUsesScanMetadataAndOnlyChangedCheckpoints(t *testing.T) {
	// Start from scanned work whose metadata has not been lost through a resume document.
	root := t.TempDir()
	path := filepath.Join(root, "source")
	if err := os.WriteFile(path, []byte("fixture"), 0o640); err != nil {
		t.Fatal(err)
	}
	statePath, reportPath := filepath.Join(root, "state.json"), filepath.Join(root, "report.json")
	entries, err := scanEntries(context.Background(), root, statePath, reportPath, nil)
	if err != nil || len(entries) != 1 || entries[0].Info == nil {
		t.Fatalf("scan = %+v / %v", entries, err)
	}
	info := entries[0].Info
	state := &rewriteState{Root: root, Pending: entries}
	if err := saveState(statePath, state); err != nil {
		t.Fatal(err)
	}

	// The sequential handoff must use that exact observation and an already persisted allocation.
	oldCopy, oldState, oldReport := copyRewrite, writeState, writeReport
	t.Cleanup(func() { copyRewrite, writeState, writeReport = oldCopy, oldState, oldReport })
	var output *fileio.Output
	copyRewrite = func(ctx context.Context, item *fileio.RewriteItem) (*acp.Report, error) {
		output = item.Output
		if item.Info != info || output.File == nil || output.File.Name() != output.Temporary {
			t.Fatalf("handoff = %+v / %+v", item, output)
		}
		saved, err := loadState(statePath)
		if err != nil || len(saved.Pending) != 1 || !reflect.DeepEqual(saved.TmpFiles, []string{output.Temporary}) {
			t.Fatalf("handoff ownership = %+v / %v", saved, err)
		}
		return oldCopy(ctx, item)
	}
	states, reports := 0, 0
	writeState = func(path string, state *rewriteState) error {
		states++
		return oldState(path, state)
	}
	writeReport = func(path string, indent bool, jobs map[string]*acp.Job, errs []*acp.Error) error {
		reports++
		return oldReport(path, indent, jobs, errs)
	}
	jobs := map[string]*acp.Job{}
	var reportErrors []*acp.Error
	if err := runQueue(context.Background(), state, statePath, reportPath, false, false, jobs, &reportErrors); err != nil {
		t.Fatal(err)
	}

	// One ownership checkpoint and one durable progress checkpoint suffice for this file.
	if states != 2 || reports != 1 {
		t.Fatalf("checkpoint writes = %d states / %d reports", states, reports)
	}
	if output == nil || output.File != nil || output.Temporary != "" {
		t.Fatalf("unsettled output = %+v", output)
	}
	if len(state.Pending) != 0 || len(state.TmpFiles) != 0 || len(jobs) != 1 {
		t.Fatalf("completion = %+v / %+v", state, jobs)
	}
	if err := runQueue(context.Background(), state, statePath, reportPath, false, false, jobs, &reportErrors); err != nil {
		t.Fatal(err)
	}
	if states != 2 || reports != 1 {
		t.Fatalf("unchanged documents rewritten: %d states / %d reports", states, reports)
	}
}

func TestRewriteQueuePreservesInterleavedRetryOrder(t *testing.T) {
	// Missing files interleave with successes; a final tail must remain untouched after cancellation.
	root := t.TempDir()
	state := &rewriteState{Root: root}
	for _, name := range []string{"a-missing", "b-good", "c-missing", "d-good", "e-missing", "f-good", "g-tail"} {
		state.Pending = append(state.Pending, rewriteEntry{Path: filepath.Join(root, name)})
	}
	for _, index := range []int{1, 3, 5, 6} {
		if err := os.WriteFile(state.Pending[index].Path, []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want := []rewriteEntry{state.Pending[0], state.Pending[2], state.Pending[4], state.Pending[6]}
	lastGood, tail := state.Pending[5].Path, state.Pending[6].Path
	before, err := os.Stat(tail)
	if err != nil {
		t.Fatal(err)
	}

	// Cancel after the third success has drained; the three retry entries must precede the tail.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	old := copyRewrite
	t.Cleanup(func() { copyRewrite = old })
	copyRewrite = func(ctx context.Context, item *fileio.RewriteItem) (*acp.Report, error) {
		report, err := old(ctx, item)
		if item.Path == lastGood {
			cancel()
		}
		return report, err
	}
	statePath, reportPath := filepath.Join(root, "state.json"), filepath.Join(root, "report.json")
	jobs := map[string]*acp.Job{}
	var reportErrors []*acp.Error
	err = runQueue(ctx, state, statePath, reportPath, false, false, jobs, &reportErrors)
	if !errors.Is(err, os.ErrNotExist) || !errors.Is(err, context.Canceled) {
		t.Fatalf("run errors = %v", err)
	}

	// Persisted work, missing classification, and rows retain their original per-file meanings.
	saved, err := loadState(statePath)
	if err != nil || !reflect.DeepEqual(saved.Pending, want) || !reflect.DeepEqual(saved.Missing, want[:3]) {
		t.Fatalf("saved retries = %+v / %v", saved, err)
	}
	if len(jobs) != 6 || jobs[tail] != nil {
		t.Fatalf("rows = %+v", jobs)
	}
	for _, entry := range want[:3] {
		job := jobs[entry.Path]
		if job == nil || len(job.FailTargets) != 1 || !errors.Is(job.FailTargets[entry.Path], os.ErrNotExist) {
			t.Fatalf("failure row = %+v", job)
		}
	}
	if after, err := os.Stat(tail); err != nil || !os.SameFile(before, after) {
		t.Fatalf("unaccepted tail changed: %v", err)
	}
}

func TestRewriteSubmissionFailureSettlesOutputAndRetainsEntry(t *testing.T) {
	// A canceled submission still owns a persisted output until the stream has fully stopped.
	root := t.TempDir()
	path := filepath.Join(root, "source")
	if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	state := &rewriteState{Root: root, Pending: []rewriteEntry{{Path: path}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := processEntry(ctx, state.Pending[0], state, filepath.Join(root, "state.json"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("submission failure = %v", err)
	}

	// The current entry stays pending, while caller cleanup has removed its allocation.
	paths, err := filepath.Glob(filepath.Join(root, ".tmp_*"))
	if err != nil || len(paths) != 0 || len(state.TmpFiles) != 0 || len(state.Pending) != 1 {
		t.Fatalf("post-submission state = %+v / %v / %v", state, paths, err)
	}
}

func TestRewriteTargetFailureHasOnlyTheFinalPath(t *testing.T) {
	// A closed output forces a real core target failure before replacing the original.
	root := t.TempDir()
	path := filepath.Join(root, "source")
	if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	old := copyRewrite
	t.Cleanup(func() { copyRewrite = old })
	var output *fileio.Output
	copyRewrite = func(ctx context.Context, item *fileio.RewriteItem) (*acp.Report, error) {
		output = item.Output
		if err := output.File.Close(); err != nil {
			t.Fatal(err)
		}
		return old(ctx, item)
	}

	// The one result keeps its failure identity without a second row or temporary target key.
	state := &rewriteState{Root: root, Pending: []rewriteEntry{{Path: path}}}
	jobs := map[string]*acp.Job{}
	var reportErrors []*acp.Error
	err = runQueue(context.Background(), state, filepath.Join(root, "state.json"), "", false, false, jobs, &reportErrors)
	if err == nil || len(jobs) != 1 {
		t.Fatalf("failed result = %+v / %v", jobs, err)
	}
	job := jobs[path]
	if job == nil || job.FullPath != path || len(job.FailTargets) != 1 || job.FailTargets[path] == nil || len(job.SuccessTargets) != 0 {
		t.Fatalf("failure row = %+v", job)
	}
	if output.File != nil || output.Temporary != "" {
		t.Fatalf("failed target remains owned: %+v", output)
	}
	if after, err := os.Stat(path); err != nil || !os.SameFile(before, after) {
		t.Fatalf("failed rewrite replaced original: %v", err)
	}
}
