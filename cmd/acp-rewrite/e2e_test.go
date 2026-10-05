package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestRewriteCommandResumesAfterDryRun drives the command end to end: a dry run only records
// tasks, and a resumed run cleans the stale scratch file, rewrites the file, drains the state,
// and reports the final path.
func TestRewriteCommandResumesAfterDryRun(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping end-to-end test in short mode")
	}

	// Build the real rewrite command for both the dry run and resumed run.
	repoRoot, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	tempDir := t.TempDir()
	binary := filepath.Join(tempDir, "acp-rewrite")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}

	// Bound compilation and each command invocation.
	buildCtx, buildCancel := context.WithTimeout(context.Background(), time.Minute)
	defer buildCancel()
	build := exec.CommandContext(buildCtx, "go", "build", "-o", binary, ".")
	build.Dir = repoRoot
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build acp-rewrite: %v\n%s", err, output)
	}

	// A successful invocation must complete rather than only persist part of its work.
	run := func(ctx context.Context, args ...string) string {
		t.Helper()
		command := exec.CommandContext(ctx, binary, args...)
		command.Dir = repoRoot
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("run acp-rewrite %v: %v\n%s", args, err, output)
		}
		return string(output)
	}

	// Keep saved state and report files outside the data tree.
	dataDir := filepath.Join(tempDir, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dataDir, "a.txt")
	content := []byte("resume fixture")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tempDir, "state.json")
	reportPath := filepath.Join(tempDir, "report.json")

	// A dry run records the task list without touching the file or writing a report.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	run(ctx, "-p=false", "-dryrun", "-state", statePath, "-report", reportPath, dataDir)
	state, err := loadState(statePath)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if state == nil || len(state.Pending) != 1 || state.Pending[0].Path != path {
		t.Fatalf("dry-run state = %#v, want one pending task for %q", state, path)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != string(content) {
		t.Fatalf("dry run changed the file: %q / %v", got, err)
	}
	if _, err := os.Stat(reportPath); !os.IsNotExist(err) {
		t.Fatalf("dry run wrote a report: %v", err)
	}

	// Resume the same task list with a stale scratch file left by the interrupted run.
	staleTmp := path + ".tmpstale"
	if err := os.WriteFile(staleTmp, []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := saveState(statePath, &rewriteState{
		Root:     dataDir,
		Pending:  []rewriteEntry{{Path: path}},
		TmpFiles: []string{staleTmp},
	}); err != nil {
		t.Fatalf("save state: %v", err)
	}

	// The resumed run removed the stale scratch file, rewrote the file, and drained the state.
	run(ctx, "-p=false", "-state", statePath, "-report", reportPath, dataDir)
	if _, err := os.Stat(staleTmp); !os.IsNotExist(err) {
		t.Fatalf("stale scratch file still exists: %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != string(content) {
		t.Fatalf("resumed run changed the content: %q / %v", got, err)
	}
	state, err = loadState(statePath)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if len(state.Pending) != 0 || len(state.Busy) != 0 || len(state.TmpFiles) != 0 {
		t.Fatalf("state after the resumed run = %#v, want a drained queue", state)
	}

	// The report names the rewritten file and the path it now occupies.
	jobs, _, err := loadReport(reportPath)
	if err != nil {
		t.Fatalf("load report: %v", err)
	}
	job := jobs[path]
	if job == nil {
		t.Fatalf("report has no row for %q: %#v", path, jobs)
	}
	if len(job.SuccessTargets) != 1 || job.SuccessTargets[0] != path {
		t.Fatalf("row success targets = %v, want %q", job.SuccessTargets, path)
	}
	if job.SHA256 == "" {
		t.Fatal("row has no SHA256")
	}
}

func TestRewriteCommandRejectsInvalidSavedFiles(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping end-to-end test in short mode")
	}

	// Build once; both cases exercise startup before cleanup or rewriting can occur.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	binary := filepath.Join(t.TempDir(), "acp-rewrite")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build acp-rewrite: %v\n%s", err, output)
	}

	// Either saved document failing to decode must leave the source and saved files intact.
	for _, saved := range []string{"state", "report"} {
		for name, document := range map[string]string{
			"malformed": "{ not json", "null": "null", "trailing": "{} {}",
			"invalid UTF-8": "{\"root\":\"/bad-\xff\"}",
			"null row":      `{"pending":[null],"files":[null]}`,
		} {
			t.Run(saved+"/"+name, func(t *testing.T) {
				// A recorded scratch file makes an early cleanup observable as well.
				root := t.TempDir()
				source := filepath.Join(root, "source.txt")
				scratch := source + ".tmpstale"
				statePath := filepath.Join(root, "state.json")
				reportPath := filepath.Join(root, "report.json")
				if err := saveState(statePath, &rewriteState{
					Root:     root,
					Pending:  []rewriteEntry{{Path: source}},
					TmpFiles: []string{scratch},
				}); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(reportPath, []byte("{}"), 0o644); err != nil {
					t.Fatal(err)
				}
				broken := statePath
				if saved == "report" {
					broken = reportPath
				}
				if err := os.WriteFile(broken, []byte(document), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(source, []byte("original content"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(scratch, []byte("partial content"), 0o644); err != nil {
					t.Fatal(err)
				}
				before, err := os.Stat(source)
				if err != nil {
					t.Fatal(err)
				}
				snapshots := make(map[string][]byte)
				for _, path := range []string{source, scratch, statePath, reportPath} {
					content, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					snapshots[path] = content
				}

				// Startup must fail specifically at the invalid saved document.
				command := exec.CommandContext(ctx, binary, "-p=false", "-state", statePath, "-report", reportPath, root)
				output, err := command.CombinedOutput()
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || !strings.Contains(string(output), "load "+saved+" fail") {
					t.Fatalf("command exit = %v, want a %s decode failure:\n%s", err, saved, output)
				}

				// Checking identity catches a rewrite even if it reproduced the same content.
				after, err := os.Stat(source)
				if err != nil || !os.SameFile(before, after) {
					t.Fatalf("startup replaced the source: %v", err)
				}
				for path, want := range snapshots {
					got, err := os.ReadFile(path)
					if err != nil || !bytes.Equal(got, want) {
						t.Errorf("saved file %q = %q / %v, want %q", path, got, err, want)
					}
				}
			})
		}
	}
}

func TestRewriteCommandPartialFailureAndExitCodes(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping end-to-end test in short mode")
	}

	// Persist a missing entry before a valid entry, then drive the actual executable through a retry.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	root := t.TempDir()
	binary := filepath.Join(t.TempDir(), "acp-rewrite")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	if output, err := exec.CommandContext(ctx, "go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	code := func(want int, args ...string) {
		t.Helper()
		output, err := exec.CommandContext(ctx, binary, args...).CombinedOutput()
		got := 0
		if err != nil {
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatal(err)
			}
			got = exit.ExitCode()
		}
		if got != want {
			t.Fatalf("exit=%d want=%d args=%v\n%s", got, want, args, output)
		}
	}
	code(2)
	code(2, "-unknown")
	code(2, root, root)
	missing, good := filepath.Join(root, "missing"), filepath.Join(root, "good")
	if err := os.WriteFile(good, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(good)
	if err != nil {
		t.Fatal(err)
	}
	statePath, reportPath := filepath.Join(root, "state.json"), filepath.Join(root, "report.json")
	if err := saveState(statePath, &rewriteState{Root: root, Pending: []rewriteEntry{{Path: missing}, {Path: good}}}); err != nil {
		t.Fatal(err)
	}
	args := []string{"-p=false", "-state", statePath, "-report", reportPath, root}
	code(1, args...)

	// A partial run records both rows and keeps only unfinished work; its successful inode changed.
	after, err := os.Stat(good)
	if err != nil || os.SameFile(before, after) {
		t.Fatalf("independent rewrite did not complete: %v", err)
	}
	state, err := loadState(statePath)
	if err != nil || len(state.Pending) != 1 || state.Pending[0].Path != missing {
		t.Fatalf("pending=%+v / %v", state, err)
	}
	jobs, _, err := loadReport(reportPath)
	if err != nil || len(jobs) != 2 || len(jobs[missing].FailTargets) == 0 || len(jobs[good].SuccessTargets) != 1 {
		t.Fatalf("partial report=%+v / %v", jobs, err)
	}
	if err := os.WriteFile(missing, []byte("recovered"), 0o600); err != nil {
		t.Fatal(err)
	}
	code(0, args...)
	jobs, _, err = loadReport(reportPath)
	if err != nil || len(jobs) != 2 || len(jobs[missing].FailTargets) != 0 {
		t.Fatalf("resumed report=%+v / %v", jobs, err)
	}
}
