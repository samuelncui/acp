package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/samuelncui/acp"
	"github.com/samuelncui/acp/internal/fileio"
	"github.com/sirupsen/logrus"
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

func TestPrintDuplicatesGroupsOnlyMatchingNonEmptyContent(t *testing.T) {
	// Capture duplicate output and restore the shared logger after this test.
	logger := logrus.StandardLogger()
	previousOutput, previousLevel := logger.Out, logger.GetLevel()
	var output bytes.Buffer
	logger.SetOutput(&output)
	logger.SetLevel(logrus.InfoLevel)
	t.Cleanup(func() {
		logger.SetOutput(previousOutput)
		logger.SetLevel(previousLevel)
	})

	// Only equal size and hash identify duplicates; missing hashes and empty files are omitted.
	jobs := map[string]*acp.Job{
		"/b":           {FullPath: "/b", Size: 7, SHA256: "same"},
		"/a":           {FullPath: "/a", Size: 7, SHA256: "same"},
		"/other-size":  {FullPath: "/other-size", Size: 8, SHA256: "same"},
		"/other-hash":  {FullPath: "/other-hash", Size: 7, SHA256: "different"},
		"/unhashed-a":  {FullPath: "/unhashed-a", Size: 7},
		"/unhashed-b":  {FullPath: "/unhashed-b", Size: 7},
		"/empty-a":     {FullPath: "/empty-a", SHA256: "empty"},
		"/empty-b":     {FullPath: "/empty-b", SHA256: "empty"},
		"/missing-row": nil,
	}
	printDuplicates(jobs)

	// Exactly one group is reported, with both matching paths in deterministic order.
	rendered := output.String()
	if strings.Count(rendered, "duplicate size=") != 1 ||
		!strings.Contains(rendered, "duplicate size= 7 sha256= same files= [/a /b]") {
		t.Fatalf("duplicate report = %q, want only the matching non-empty files", rendered)
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

func TestRewriteHardlinkGroupPreservesMetadata(t *testing.T) {
	// Scan recognizes hardlink groups on the platforms that provide file identities.
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("hardlink scan unsupported on this platform")
	}

	// Every original path shares the same content and metadata before the rewrite.
	root := t.TempDir()
	src := filepath.Join(root, "a.txt")
	links := []string{filepath.Join(root, "b.txt"), filepath.Join(root, "c.txt")}
	content := []byte("hardlink group fixture")
	modified := time.Unix(1700000000, 0)
	if err := os.WriteFile(src, content, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(src, modified, modified); err != nil {
		t.Fatal(err)
	}
	for _, link := range links {
		if err := os.Link(src, link); err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}

	// One scanned entry rewrites the source once, then relinks both remaining names.
	statePath := filepath.Join(root, "state.json")
	entries, err := scanEntries(context.Background(), root, statePath, "", nil)
	if err != nil || len(entries) != 1 || entries[0].Path != src || !reflect.DeepEqual(entries[0].Links, links) {
		t.Fatalf("scanned group = %+v / %v", entries, err)
	}
	state := &rewriteState{Root: root, Pending: entries}
	report, err := processEntry(context.Background(), entries[0], state, statePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Jobs) != 1 || len(report.Jobs[0].FailTargets) != 0 || len(state.TmpFiles) != 0 {
		t.Fatalf("rewrite result = %+v / %+v", report, state)
	}

	// All paths share the new inode and the metadata already restored by the core copy.
	after, err := os.Stat(src)
	if err != nil || os.SameFile(before, after) {
		t.Fatalf("source was not rewritten: %v", err)
	}
	for _, path := range append([]string{src}, links...) {
		info, err := os.Stat(path)
		if err != nil || !os.SameFile(after, info) {
			t.Fatalf("group member %q does not share the rewritten inode: %v", path, err)
		}
		if info.Mode() != before.Mode() || !info.ModTime().Equal(before.ModTime()) {
			t.Fatalf("metadata for %q = %v / %v, want %v / %v", path,
				info.Mode(), info.ModTime(), before.Mode(), before.ModTime())
		}
		if data, err := os.ReadFile(path); err != nil || !bytes.Equal(data, content) {
			t.Fatalf("content for %q = %q / %v", path, data, err)
		}
	}
}

// TestRewriteFileCommitsTheFinalPath pins one rewrite: the content survives, the scratch file
// is gone, and the report row names the final path rather than the temporary one.
func TestRewriteFileCommitsTheFinalPath(t *testing.T) {
	// Keep content and metadata distinguishable from the temporary allocation.
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

	// Pass an open output and the source observation through the internal stream seam.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	output, err := fileio.NewOutput(path)
	if err != nil {
		t.Fatal(err)
	}
	defer output.Discard()
	tmpPath := output.Temporary
	report, err := rewriteFile(context.Background(), &fileio.RewriteItem{Path: path, Info: info, Output: output})
	if err != nil {
		t.Fatalf("rewriteFile: %v", err)
	}

	// The core commits the same output, preserving content and metadata before returning.
	if output.File != nil || output.Temporary != "" {
		t.Fatalf("unsettled output: %+v", output)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(content) {
		t.Fatalf("rewritten content = %q, want %q", got, content)
	}
	info, err = os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.ModTime().Unix() != modified.Unix() {
		t.Fatalf("rewritten mtime = %v, want %v", info.ModTime(), modified)
	}
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Fatalf("scratch file %q still exists: %v", tmpPath, err)
	}

	// One terminal row names the final destination and the computed content hash.
	if len(report.Jobs) != 1 {
		t.Fatalf("report rows = %d", len(report.Jobs))
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

// TestRewriteFileRejectsAMissingSource pins the failure path of one rewrite: a missing source
// reports an error and creates no scratch file.
func TestRewriteFileRejectsAMissingSource(t *testing.T) {
	// The command checks its source before allocating or persisting a scratch file.
	root := t.TempDir()
	state := &rewriteState{Root: root}
	if _, err := processEntry(context.Background(), rewriteEntry{Path: filepath.Join(root, "missing.txt")},
		state, filepath.Join(root, "state.json")); err == nil {
		t.Fatal("missing source accepted")
	}
	paths, err := filepath.Glob(filepath.Join(root, ".tmp_*"))
	if err != nil || len(paths) != 0 {
		t.Fatalf("scratch after failure: %v / %v", paths, err)
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
