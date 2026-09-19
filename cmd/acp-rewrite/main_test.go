package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"

	"github.com/samuelncui/acp"
)

func TestScanEntriesHardlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("hardlink scan unsupported on windows")
	}

	root := t.TempDir()
	statePath := filepath.Join(root, ".acp-rewrite-state.json")
	reportPath := filepath.Join(root, "report.json")

	if err := os.WriteFile(statePath, []byte("{}"), 0o644); err != nil {
		t.Fatalf("write state: %v", err)
	}
	if err := os.WriteFile(reportPath, []byte("{}"), 0o644); err != nil {
		t.Fatalf("write report: %v", err)
	}

	a := filepath.Join(root, "a.txt")
	b := filepath.Join(root, "b.txt")
	c := filepath.Join(root, "c.txt")

	if err := os.WriteFile(a, []byte("a"), 0o644); err != nil {
		t.Fatalf("write a: %v", err)
	}
	if err := os.Link(a, b); err != nil {
		t.Fatalf("link b: %v", err)
	}
	if err := os.WriteFile(c, []byte("c"), 0o644); err != nil {
		t.Fatalf("write c: %v", err)
	}

	entries, err := scanEntries(context.Background(), root, statePath, reportPath, nil)
	if err != nil {
		t.Fatalf("scan entries: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries count = %d", len(entries))
	}

	var withLinks *rewriteEntry
	var single *rewriteEntry
	for i := range entries {
		if len(entries[i].Links) > 0 {
			withLinks = &entries[i]
		} else {
			single = &entries[i]
		}
	}
	if withLinks == nil || single == nil {
		t.Fatalf("unexpected entries: %+v", entries)
	}
	if withLinks.Path != a {
		t.Fatalf("hardlink entry path = %q", withLinks.Path)
	}
	if len(withLinks.Links) != 1 || withLinks.Links[0] != b {
		t.Fatalf("hardlink entry links = %+v", withLinks.Links)
	}
	if single.Path != c {
		t.Fatalf("single entry path = %q", single.Path)
	}
}

func TestScanEntriesCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	root := t.TempDir()
	entries, err := scanEntries(ctx, root, "", "", nil)
	if err == nil || err != context.Canceled {
		t.Fatalf("expected canceled, got %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("entries count = %d", len(entries))
	}
}

func TestScanEntriesIgnorePaths(t *testing.T) {
	root := t.TempDir()
	keep := filepath.Join(root, "keep.txt")
	skipDir := filepath.Join(root, "skip")
	skipFile := filepath.Join(root, "skip.txt")

	if err := os.MkdirAll(skipDir, 0o755); err != nil {
		t.Fatalf("mkdir skip: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skipDir, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatalf("write skip a: %v", err)
	}
	if err := os.WriteFile(skipFile, []byte("skip"), 0o644); err != nil {
		t.Fatalf("write skip file: %v", err)
	}
	if err := os.WriteFile(keep, []byte("keep"), 0o644); err != nil {
		t.Fatalf("write keep: %v", err)
	}

	ignorePaths, err := normalizeIgnorePaths(root, []string{"skip", "skip.txt"})
	if err != nil {
		t.Fatalf("normalize ignore: %v", err)
	}
	entries, err := scanEntries(context.Background(), root, "", "", ignorePaths)
	if err != nil {
		t.Fatalf("scan entries: %v", err)
	}
	if len(entries) != 1 || entries[0].Path != keep {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestRelinkOnePreserveLinkAttrs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("hardlink attrs unsupported on windows")
	}

	root := t.TempDir()
	src := filepath.Join(root, "src.txt")
	link := filepath.Join(root, "link.txt")
	tmp := filepath.Join(root, "tmp.txt")

	if err := os.WriteFile(src, []byte("old"), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	if err := os.Link(src, link); err != nil {
		t.Fatalf("link: %v", err)
	}

	oldTime := time.Unix(1700000000, 0)
	newTime := time.Unix(1700001000, 0)

	if err := os.Chmod(link, 0o600); err != nil {
		t.Fatalf("chmod link: %v", err)
	}
	if err := os.Chtimes(link, oldTime, oldTime); err != nil {
		t.Fatalf("chtimes link: %v", err)
	}

	if err := os.WriteFile(tmp, []byte("new"), 0o644); err != nil {
		t.Fatalf("write tmp: %v", err)
	}
	if err := os.Chtimes(tmp, newTime, newTime); err != nil {
		t.Fatalf("chtimes tmp: %v", err)
	}
	if err := os.Rename(tmp, src); err != nil {
		t.Fatalf("rename tmp to src: %v", err)
	}

	if err := relinkOne(src, link); err != nil {
		t.Fatalf("relinkOne: %v", err)
	}

	srcInfo, err := os.Stat(src)
	if err != nil {
		t.Fatalf("stat src: %v", err)
	}
	linkInfo, err := os.Stat(link)
	if err != nil {
		t.Fatalf("stat link: %v", err)
	}
	if !os.SameFile(srcInfo, linkInfo) {
		t.Fatalf("src and link not same file")
	}
	if srcInfo.Mode().Perm() != 0o600 || linkInfo.Mode().Perm() != 0o600 {
		t.Fatalf("mode mismatch src=%v link=%v", srcInfo.Mode().Perm(), linkInfo.Mode().Perm())
	}
	if srcInfo.ModTime().Unix() != oldTime.Unix() || linkInfo.ModTime().Unix() != oldTime.Unix() {
		t.Fatalf("modtime mismatch src=%v link=%v", srcInfo.ModTime(), linkInfo.ModTime())
	}
	data, err := os.ReadFile(link)
	if err != nil {
		t.Fatalf("read link: %v", err)
	}
	if string(data) != "new" {
		t.Fatalf("link content = %q", string(data))
	}
}

func TestRelinkOrRetry(t *testing.T) {
	dir := t.TempDir()
	entry := rewriteEntry{
		Path:  filepath.Join(dir, "missing-src"),
		Links: []string{filepath.Join(dir, "link")},
	}
	state := new(rewriteState)

	if err := relinkOrRetry(state, entry); err == nil {
		t.Fatalf("expected relink error")
	}
	if len(state.Pending) != 1 {
		t.Fatalf("pending entries = %d", len(state.Pending))
	}
	if state.Pending[0].Path != entry.Path {
		t.Fatalf("pending path = %q", state.Pending[0].Path)
	}
}

// TestRewriteFileCommitsTheFinalPath pins one rewrite: the content survives, the scratch file
// is gone, and the report row names the final path rather than the temporary one.
func TestRewriteFileCommitsTheFinalPath(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "a.txt")
	content := []byte("rewrite fixture")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	modified := time.Unix(1700000000, 0)
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}

	tmpPath, err := newTmpPath(path, randSource)
	if err != nil {
		t.Fatal(err)
	}
	report, err := rewriteFile(context.Background(), rewriteEntry{Path: path}, tmpPath)
	if err != nil {
		t.Fatalf("rewriteFile: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(content) {
		t.Fatalf("rewritten content = %q, want %q", got, content)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.ModTime().Unix() != modified.Unix() {
		t.Fatalf("rewritten mtime = %v, want %v", info.ModTime(), modified)
	}
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Fatalf("scratch file %q still exists: %v", tmpPath, err)
	}

	job, ok := findJob(report, path)
	if !ok {
		t.Fatalf("report has no row for %q: %#v", path, report.Jobs)
	}
	if len(job.FailTargets) != 0 {
		t.Fatalf("row failures = %v", job.FailTargets)
	}
	if len(job.SuccessTargets) != 1 || job.SuccessTargets[0] != path {
		t.Fatalf("row success targets = %v, want %q", job.SuccessTargets, path)
	}
	if job.SHA256 == "" {
		t.Fatal("row has no SHA256")
	}
}

// TestRewriteFileRejectsAChangedSource pins the failure path of one rewrite: a source that
// cannot be read leaves the original in place.
func TestRewriteFileRejectsAMissingSource(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "missing.txt")
	tmpPath, err := newTmpPath(path, randSource)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := rewriteFile(context.Background(), rewriteEntry{Path: path}, tmpPath); err == nil {
		t.Fatal("rewriteFile() error = nil, want the missing source reported")
	}
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Fatalf("scratch file %q exists after a failed rewrite: %v", tmpPath, err)
	}
}

// TestStateRoundTrip pins the resumable state: it is persisted and read back unchanged.
func TestStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "state.json")
	want := &rewriteState{
		Root:     "/root",
		Pending:  []rewriteEntry{{Path: "/root/a.txt"}},
		Busy:     []rewriteEntry{{Path: "/root/b.txt", Links: []string{"/root/c.txt"}}},
		Missing:  []rewriteEntry{{Path: "/root/d.txt"}},
		TmpFiles: []string{"/root/a.txt.tmp1234"},
	}
	if err := saveState(path, want); err != nil {
		t.Fatalf("saveState: %v", err)
	}

	got, err := loadState(path)
	if err != nil {
		t.Fatalf("loadState: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("state = %#v, want %#v", got, want)
	}

	// A state file that does not exist yet is a fresh start.
	if missing, err := loadState(filepath.Join(t.TempDir(), "missing.json")); err != nil || missing != nil {
		t.Fatalf("loadState() = %#v / %v, want a fresh state", missing, err)
	}

	// A state file that cannot be decoded is an error, never a silent restart.
	broken := filepath.Join(t.TempDir(), "broken.json")
	if err := os.WriteFile(broken, []byte("{ not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadState(broken); err == nil {
		t.Fatal("loadState() error = nil, want a decode failure")
	}
}

// TestReportRoundTripKeepsHistory pins the accumulated report: previous rows and their failures
// survive a save and a load, and a report that cannot be decoded is an error.
func TestReportRoundTripKeepsHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.json")
	jobs := map[string]*acp.Job{
		"/root/a.txt": {
			FullPath: "/root/a.txt",
			Status:   acp.JobStatusFinished,
			SHA256:   "aa",
			FailTargets: map[string]error{
				"/root/a.txt.tmp1234": errors.New("write failed"),
			},
		},
	}
	reportErrors := []*acp.Error{{Src: "/root/a.txt", Err: errors.New("pipeline failed")}}
	if err := saveReport(path, true, jobs, reportErrors); err != nil {
		t.Fatalf("saveReport: %v", err)
	}

	loadedJobs, loadedErrors, err := loadReport(path)
	if err != nil {
		t.Fatalf("loadReport: %v", err)
	}
	if len(loadedJobs) != 1 {
		t.Fatalf("loaded jobs = %d, want 1", len(loadedJobs))
	}
	job := loadedJobs["/root/a.txt"]
	if job == nil || job.SHA256 != "aa" {
		t.Fatalf("loaded job = %#v", job)
	}
	if err := job.FailTargets["/root/a.txt.tmp1234"]; err == nil || err.Error() != "write failed" {
		t.Fatalf("loaded failure = %v, want the recorded message", job.FailTargets)
	}
	if len(loadedErrors) != 1 || loadedErrors[0].Err == nil || loadedErrors[0].Err.Error() != "pipeline failed" {
		t.Fatalf("loaded errors = %#v", loadedErrors)
	}

	// A report that does not exist yet is an empty history.
	missingJobs, missingErrors, err := loadReport(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil || len(missingJobs) != 0 || len(missingErrors) != 0 {
		t.Fatalf("loadReport() = %d jobs / %d errors / %v, want an empty history", len(missingJobs), len(missingErrors), err)
	}

	// A report that exists but cannot be decoded must not be replaced by an empty one.
	broken := filepath.Join(t.TempDir(), "broken.json")
	if err := os.WriteFile(broken, []byte("{ not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if jobs, errors, err := loadReport(broken); err == nil {
		t.Fatalf("loadReport() = %v / %v, want a decode failure", jobs, errors)
	}
}

// TestCleanupTmpFilesRemovesOnlyScratchFiles pins the interrupted-run cleanup: it removes every
// recorded scratch file and leaves the data alone.
func TestCleanupTmpFilesRemovesOnlyScratchFiles(t *testing.T) {
	root := t.TempDir()
	kept := filepath.Join(root, "kept.txt")
	first := filepath.Join(root, "kept.txt.tmpabcd")
	second := filepath.Join(root, "kept.txt.tmpefgh")
	for _, path := range []string{kept, first, second} {
		if err := os.WriteFile(path, []byte("fixture"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	state := &rewriteState{Root: root, TmpFiles: []string{first, second}}
	if err := cleanupTmpFiles(state); err != nil {
		t.Fatalf("cleanupTmpFiles: %v", err)
	}
	for _, path := range []string{first, second} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("scratch file %q still exists: %v", path, err)
		}
	}
	if _, err := os.Stat(kept); err != nil {
		t.Fatalf("cleanup removed a data file: %v", err)
	}
	if len(state.TmpFiles) != 0 {
		t.Fatalf("state tmp files = %v, want none", state.TmpFiles)
	}
}

// TestRewriteCommandResumesAfterDryRun drives the command end to end: a dry run only records
// tasks, and a resumed run cleans the stale scratch file, rewrites the file, drains the state,
// and reports the final path.
func TestRewriteCommandResumesAfterDryRun(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping end-to-end test in short mode")
	}

	repoRoot, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	tempDir := t.TempDir()
	binary := filepath.Join(tempDir, "acp-rewrite")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}

	buildCtx, buildCancel := context.WithTimeout(context.Background(), time.Minute)
	defer buildCancel()
	build := exec.CommandContext(buildCtx, "go", "build", "-o", binary, ".")
	build.Dir = repoRoot
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build acp-rewrite: %v\n%s", err, output)
	}

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

	run(ctx, "-p=false", "-state", statePath, "-report", reportPath, dataDir)

	// The resumed run removed the stale scratch file, rewrote the file, and drained the state.
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
