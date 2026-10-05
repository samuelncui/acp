package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/samuelncui/acp"
	"github.com/samuelncui/acp/internal/fileio"
	"github.com/sirupsen/logrus"
)

func TestRewriteStartupRetainsCleanupAndStateFailures(t *testing.T) {
	// Start from durable pending work with one owned temporary that cannot be cleaned.
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.WriteFile(source, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	tmp, cleaned := filepath.Join(root, ".tmp_owned"), filepath.Join(root, ".tmp_cleaned")
	for _, path := range []string{tmp, cleaned} {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	statePath, reportPath := filepath.Join(root, "state.json"), filepath.Join(root, "report.json")
	state := &rewriteState{Root: root, Pending: []rewriteEntry{{Path: source}}, TmpFiles: []string{tmp, cleaned}}
	if err := saveState(statePath, state); err != nil {
		t.Fatal(err)
	}

	// Fail cleanup and its state write together, retaining the command's visible diagnostics.
	cleanupErr, stateErr := errors.New("injected cleanup failure"), errors.New("injected state failure")
	oldRemove, oldState := removeTemporary, writeState
	t.Cleanup(func() { removeTemporary, writeState = oldRemove, oldState })
	removeTemporary = func(path string) error {
		if path == cleaned {
			return oldRemove(path)
		}
		return &fileio.CleanupError{Path: path, Err: cleanupErr}
	}
	writeState = func(string, *rewriteState) error { return stateErr }
	logger := logrus.StandardLogger()
	oldOutput := logger.Out
	t.Cleanup(func() { logger.SetOutput(oldOutput) })
	var output bytes.Buffer
	logger.SetOutput(&output)

	// Both failures must reach the exit diagnostics and the requested report before returning.
	if code := run(context.Background(), []string{"-p=false", "-state", statePath, "-report", reportPath, root}); code != 1 {
		t.Fatalf("exit code=%d want=1", code)
	}
	_, reportErrors, err := loadReport(reportPath)
	if err != nil || len(reportErrors) != 1 {
		t.Fatalf("report errors=%v load error=%v", reportErrors, err)
	}
	for _, failure := range []error{cleanupErr, stateErr} {
		if !strings.Contains(output.String(), failure.Error()) || !strings.Contains(reportErrors[0].Err.Error(), failure.Error()) {
			t.Fatalf("failure %q hidden: output=%q report=%v", failure, output.String(), reportErrors)
		}
	}

	// The failed checkpoint must preserve ownership and prevent the pending rewrite from starting.
	saved, err := loadState(statePath)
	if err != nil || !reflect.DeepEqual(saved.Pending, state.Pending) || !reflect.DeepEqual(saved.TmpFiles, []string{tmp, cleaned}) {
		t.Fatalf("saved state=%+v load error=%v", saved, err)
	}
	after, err := os.Stat(source)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("pending source changed: %v", err)
	}
	if _, err := os.Stat(tmp); err != nil {
		t.Fatalf("owned temporary lost: %v", err)
	}
}

func TestRewriteStartupPersistsCheckpointCleanupOwnership(t *testing.T) {
	// A failed checkpoint can own a new temporary that was absent from the prior state.
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.WriteFile(source, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	statePath, reportPath := filepath.Join(root, "state.json"), filepath.Join(root, "report.json")
	stale := filepath.Join(root, ".tmp_stale")
	if err := os.WriteFile(stale, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	state := &rewriteState{Root: root, Pending: []rewriteEntry{{Path: source}}, TmpFiles: []string{stale}}
	if err := saveState(statePath, state); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(root, ".tmp_checkpoint")
	if err := os.WriteFile(tmp, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	// Model the atomic writer returning both its primary failure and failed cleanup ownership.
	oldState := writeState
	t.Cleanup(func() { writeState = oldState })
	failed := false
	writeState = func(path string, next *rewriteState) error {
		if !failed {
			failed = true
			err := errors.Join(errors.New("injected checkpoint failure"),
				&fileio.CleanupError{Path: tmp, Err: errors.New("injected checkpoint cleanup failure")})
			rememberCleanup(next, err)
			return err
		}
		return saveState(path, next)
	}

	// A successful report must still be followed by saving the newly retained ownership.
	if code := run(context.Background(), []string{"-p=false", "-state", statePath, "-report", reportPath, root}); code != 1 {
		t.Fatalf("exit code=%d want=1", code)
	}
	_, reportErrors, err := loadReport(reportPath)
	if err != nil || len(reportErrors) != 1 {
		t.Fatalf("report errors=%v load error=%v", reportErrors, err)
	}
	saved, err := loadState(statePath)
	if err != nil || saved == nil || !reflect.DeepEqual(saved.Pending, state.Pending) || !reflect.DeepEqual(saved.TmpFiles, []string{tmp}) {
		t.Fatalf("cleanup ownership or pending work lost: state=%+v err=%v", saved, err)
	}
	if _, err := os.Stat(tmp); err != nil {
		t.Fatalf("owned temporary lost: %v", err)
	}
}

func TestRewritePendingSurvivesEveryCommitBoundary(t *testing.T) {
	for _, boundary := range []string{"copy commit", "relink", "report", "progress"} {
		t.Run(boundary, func(t *testing.T) {
			// Keep a hardlink group and an independent tail so premature progress is observable.
			root := t.TempDir()
			first, link, tail := filepath.Join(root, "a"), filepath.Join(root, "b"), filepath.Join(root, "c")
			for _, path := range []string{first, tail} {
				if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Link(first, link); err != nil {
				t.Skipf("hardlinks unsupported: %v", err)
			}
			queue := []rewriteEntry{{Path: first, Links: []string{link}}, {Path: tail}}
			state := &rewriteState{Root: root, Pending: append([]rewriteEntry(nil), queue...)}
			statePath, reportPath := filepath.Join(root, "state.json"), filepath.Join(root, "report.json")
			sentinel := errors.New("injected " + boundary)
			oldCopy, oldRename, oldState, oldReport := copyRewrite, renameFile, writeState, writeReport
			t.Cleanup(func() { copyRewrite, renameFile, writeState, writeReport = oldCopy, oldRename, oldState, oldReport })
			checked := false
			checkPending := func() {
				t.Helper()
				saved, err := loadState(statePath)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(saved.Pending, queue) {
					t.Fatalf("in-flight pending=%+v want=%+v", saved.Pending, queue)
				}
				checked = true
			}
			copyRewrite = func(ctx context.Context, item *fileio.RewriteItem) (*acp.Report, error) {
				if boundary == "copy commit" && item.Path == first {
					checkPending()
					return nil, sentinel
				}
				return oldCopy(ctx, item)
			}
			renameFile = func(from, to string) error {
				if boundary == "relink" && to == link {
					checkPending()
					return sentinel
				}
				return oldRename(from, to)
			}
			writeReport = func(path string, indent bool, jobs map[string]*acp.Job, errs []*acp.Error) error {
				if boundary == "report" {
					checkPending()
					return sentinel
				}
				return oldReport(path, indent, jobs, errs)
			}
			writeState = func(path string, next *rewriteState) error {
				if boundary == "progress" && len(next.Pending) == 1 && next.Pending[0].Path == tail {
					checkPending()
					return sentinel
				}
				return oldState(path, next)
			}
			before, err := os.Stat(tail)
			if err != nil {
				t.Fatal(err)
			}

			// Persistence failures stop the tail; item failures retain the full group and continue it.
			jobs := map[string]*acp.Job{}
			var reportErrors []*acp.Error
			err = runQueue(context.Background(), state, statePath, reportPath, false, false, jobs, &reportErrors)
			if !errors.Is(err, sentinel) || !checked {
				t.Fatalf("boundary=%s err=%v checked=%t", boundary, err, checked)
			}
			saved, err := loadState(statePath)
			if err != nil {
				t.Fatal(err)
			}
			if len(saved.Pending) == 0 || saved.Pending[0].Path != first || !reflect.DeepEqual(saved.Pending[0].Links, []string{link}) {
				t.Fatalf("retry group lost: %+v", saved)
			}
			after, err := os.Stat(tail)
			if err != nil {
				t.Fatal(err)
			}
			if boundary == "report" || boundary == "progress" {
				if !os.SameFile(before, after) || len(saved.Pending) != 2 {
					t.Fatal("new entry started after persistence failure")
				}
			} else {
				if os.SameFile(before, after) || len(saved.Pending) != 1 {
					t.Fatal("independent entry did not finish")
				}
			}

			// Removing the injected failure must resume the group and restore its hardlink identity.
			copyRewrite, renameFile, writeState, writeReport = oldCopy, oldRename, oldState, oldReport
			if err := runQueue(context.Background(), saved, statePath, reportPath, false, false, jobs, &reportErrors); err != nil {
				t.Fatal(err)
			}
			a, err := os.Stat(first)
			if err != nil {
				t.Fatal(err)
			}
			b, err := os.Stat(link)
			if err != nil || !os.SameFile(a, b) {
				t.Fatalf("resumed hardlinks differ: %v", err)
			}
		})
	}
}

func TestRewriteCopyFailureNeverRemovesOriginal(t *testing.T) {
	// A rejected copy keeps the original inode and bytes intact while cleaning its allocation.
	root := t.TempDir()
	path := filepath.Join(root, "original")
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	old := copyRewrite
	sentinel := errors.New("copy refused")
	copyRewrite = func(context.Context, *fileio.RewriteItem) (*acp.Report, error) { return nil, sentinel }
	t.Cleanup(func() { copyRewrite = old })
	state := &rewriteState{Root: root, Pending: []rewriteEntry{{Path: path}}}
	_, err = processEntry(context.Background(), state.Pending[0], state, filepath.Join(root, "state.json"))
	if !errors.Is(err, sentinel) {
		t.Fatalf("copy failure hidden: %v", err)
	}
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("original replaced or removed: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "original" {
		t.Fatalf("original content=%q / %v", data, err)
	}
}

func TestRewritePersistsCleanupFailureOwnership(t *testing.T) {
	// Fail both the primary copy and its cleanup; both failures and the owned path must survive.
	root := t.TempDir()
	path := filepath.Join(root, "original")
	if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(root, "state.json")
	state := &rewriteState{Root: root, Pending: []rewriteEntry{{Path: path}}}
	oldCopy := copyRewrite
	primary := errors.New("copy failed")
	copyRewrite = func(_ context.Context, item *fileio.RewriteItem) (*acp.Report, error) {
		blockOutputRemoval(t, item.Output)
		return nil, primary
	}
	t.Cleanup(func() { copyRewrite = oldCopy })
	jobs := map[string]*acp.Job{}
	var reportErrors []*acp.Error
	err := runQueue(context.Background(), state, statePath, filepath.Join(root, "report.json"), false, false, jobs, &reportErrors)
	var cleanup *fileio.CleanupError
	if !errors.Is(err, primary) || !errors.As(err, &cleanup) {
		t.Fatalf("combined failure=%v", err)
	}
	saved, err := loadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Pending) != 1 || len(saved.TmpFiles) != 1 || !strings.HasPrefix(filepath.Base(saved.TmpFiles[0]), ".tmp_") {
		t.Fatalf("ownership lost: %+v", saved)
	}

	// A later run cleans precisely the recorded allocation without scanning by filename.
	foreign := filepath.Join(root, ".tmp_acp_foreign")
	if err := os.WriteFile(foreign, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(saved.TmpFiles[0], "block")); err != nil {
		t.Fatal(err)
	}
	if err := cleanupTmpFiles(saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.TmpFiles) != 0 {
		t.Fatal(saved.TmpFiles)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatal("unowned file removed")
	}
}

func TestRewriteCancellationRetainsTail(t *testing.T) {
	// Stop after the first commit; the accepted copy drains and the independent tail stays pending.
	root := t.TempDir()
	first, tail := filepath.Join(root, "first"), filepath.Join(root, "tail")
	for _, path := range []string{first, tail} {
		if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.Stat(tail)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	old := copyRewrite
	copyRewrite = func(ctx context.Context, item *fileio.RewriteItem) (*acp.Report, error) {
		report, err := old(ctx, item)
		if item.Path == first {
			cancel()
		}
		return report, err
	}
	t.Cleanup(func() { copyRewrite = old })
	state := &rewriteState{Root: root, Pending: []rewriteEntry{{Path: first}, {Path: tail}}}
	statePath := filepath.Join(root, "state.json")
	var reportErrors []*acp.Error
	err = runQueue(ctx, state, statePath, filepath.Join(root, "report.json"), false, false, map[string]*acp.Job{}, &reportErrors)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation=%v", err)
	}
	saved, err := loadState(statePath)
	if err != nil || len(saved.Pending) != 1 || saved.Pending[0].Path != tail {
		t.Fatalf("pending=%+v / %v", saved, err)
	}
	after, err := os.Stat(tail)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("tail started after cancellation: %v", err)
	}
}

func TestRewriteCleanupFailureDoesNotFailIndependentEntry(t *testing.T) {
	// Leave the first entry's temporary behind, but let a later entry complete and leave pending.
	root := t.TempDir()
	first, second := filepath.Join(root, "a"), filepath.Join(root, "b")
	for _, path := range []string{first, second} {
		if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	oldCopy := copyRewrite
	var owned string
	primary := errors.New("copy refused")
	copyRewrite = func(ctx context.Context, item *fileio.RewriteItem) (*acp.Report, error) {
		if item.Path == first {
			owned = item.Output.Temporary
			blockOutputRemoval(t, item.Output)
			return nil, primary
		}
		return oldCopy(ctx, item)
	}
	t.Cleanup(func() { copyRewrite = oldCopy })
	state := &rewriteState{Root: root, Pending: []rewriteEntry{{Path: first}, {Path: second}}}
	statePath := filepath.Join(root, "state.json")
	jobs := map[string]*acp.Job{}
	var reportErrors []*acp.Error
	err := runQueue(context.Background(), state, statePath, filepath.Join(root, "report.json"), false, false, jobs, &reportErrors)
	var cleanup *fileio.CleanupError
	if !errors.Is(err, primary) || !errors.As(err, &cleanup) {
		t.Fatal(err)
	}

	// Only the failed entry owns the cleanup error, pending retry, and retained scratch path.
	saved, err := loadState(statePath)
	if err != nil || len(saved.Pending) != 1 || saved.Pending[0].Path != first {
		t.Fatalf("pending=%+v / %v", saved, err)
	}
	if len(saved.TmpFiles) != 1 || saved.TmpFiles[0] != owned {
		t.Fatalf("ownership=%v", saved.TmpFiles)
	}
	if job := jobs[second]; job == nil || len(job.FailTargets) != 0 || len(job.SuccessTargets) != 1 {
		t.Fatalf("independent row=%+v", job)
	}
}

// blockOutputRemoval injects a real removal failure at the sequential handoff boundary.
func blockOutputRemoval(t *testing.T, output *fileio.Output) {
	t.Helper()
	// Replace only the owned allocation with a nonempty directory; preserve the original file.
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(output.Temporary); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(output.Temporary, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(output.Temporary, "block"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}
