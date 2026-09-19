package acp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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

	// -notarget is the directory index: every selected file is read and hashed, and nothing is
	// written anywhere.
	indexPath := filepath.Join(tempDir, "index.json")
	run(binary, "-p=false", "-notarget", "-report", indexPath, source)

	indexData, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatalf("read index report: %v", err)
	}
	var index acp.Report
	if err := json.Unmarshal(indexData, &index); err != nil {
		t.Fatalf("decode index report: %v", err)
	}
	if len(index.Errors) != 0 || len(index.Jobs) != len(files) {
		t.Fatalf("index report errors/files = %d/%d, want 0/%d", len(index.Errors), len(index.Jobs), len(files))
	}
	for _, job := range index.Jobs {
		if job.SHA256 == "" {
			t.Fatalf("index row %q has no SHA256", job.FullPath)
		}
		if len(job.SuccessTargets) != 0 || len(job.FailTargets) != 0 {
			t.Fatalf("index row %q targets = %v / %v, want none", job.FullPath, job.SuccessTargets, job.FailTargets)
		}
	}

	// -to-linear copies through the single ordered writer instead of the concurrent target pool.
	linearRoot := filepath.Join(tempDir, "linear-target")
	if err := os.MkdirAll(linearRoot, 0o755); err != nil {
		t.Fatalf("create linear target directory: %v", err)
	}
	run(binary, "-p=false", "-to-linear", source, linearRoot)
	for relativePath, want := range files {
		got, err := os.ReadFile(filepath.Join(linearRoot, filepath.Base(source), relativePath))
		if err != nil {
			t.Fatalf("read linear copy of %q: %v", relativePath, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("linear copy of %q = %v, want %v", relativePath, got, want)
		}
	}
}

// selectedSourceFiles returns every regular file below source, which is the set of files a
// wildcard run selects. The pre-stream API exposed that selection as data; the shell does not,
// so the test walks the tree itself instead of asking the library what it selected.
func selectedSourceFiles(t *testing.T, source string) []string {
	t.Helper()

	var files []string
	err := filepath.WalkDir(source, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == source || !entry.Type().IsRegular() {
			return nil
		}

		files = append(files, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk source %q: %v", source, err)
	}

	return files
}

// stoppedReportShape is the JSON shape the command writes after a graceful stop.
type stoppedReportShape struct {
	Files []struct {
		FullPath    string            `json:"full_path"`
		Status      string            `json:"status"`
		Success     []string          `json:"success_target"`
		FailTargets map[string]string `json:"fail_target"`
	} `json:"files"`
	Errors []struct {
		Err string `json:"error"`
	} `json:"errors"`
}

func TestACPCommandStopsGracefullyWithACompleteReport(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping end-to-end test in short mode")
	}
	if runtime.GOOS == "windows" {
		t.Skip("interrupt delivery is not portable on windows")
	}

	repoRoot, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	tempDir := t.TempDir()
	binary := filepath.Join(tempDir, "acp")

	buildCtx, buildCancel := context.WithTimeout(context.Background(), time.Minute)
	defer buildCancel()
	build := exec.CommandContext(buildCtx, "go", "build", "-o", binary, "./cmd/acp")
	build.Dir = repoRoot
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build acp: %v\n%s", err, output)
	}

	source := filepath.Join(tempDir, "source")
	target := filepath.Join(tempDir, "target")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatalf("create source directory: %v", err)
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("create target directory: %v", err)
	}

	// One file large enough that its copy is still in flight when the signal arrives, and
	// many files behind it, so the stop has to account for queued work.
	large := "0000-large.bin"
	if err := os.WriteFile(filepath.Join(source, large), bytes.Repeat([]byte{'x'}, 16<<20), 0o644); err != nil {
		t.Fatalf("write large source file: %v", err)
	}
	const small = 40
	for index := 1; index <= small; index++ {
		name := fmt.Sprintf("%04d.txt", index)
		if err := os.WriteFile(filepath.Join(source, name), []byte(name), 0o644); err != nil {
			t.Fatalf("write source file %q: %v", name, err)
		}
	}

	reportPath := filepath.Join(tempDir, "report.json")
	runCtx, runCancel := context.WithTimeout(context.Background(), time.Minute)
	defer runCancel()
	command := exec.CommandContext(runCtx, binary, "-p=false", "-from-linear", "-report", reportPath, source, target)
	command.Dir = repoRoot
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Start(); err != nil {
		t.Fatalf("start acp: %v", err)
	}
	defer func() { _ = command.Process.Kill() }()

	// The target file exists as soon as the copy stage owns the first item, so waiting for it
	// proves the run has started and the signal lands while the copy is still in flight.
	copied := filepath.Join(target, filepath.Base(source), large)
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(copied); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the command never started copying the first file:\n%s", output.String())
		}
		time.Sleep(time.Millisecond)
	}
	if err := command.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("interrupt acp: %v", err)
	}

	// The stop abandoned work, so the command must tell its caller: a non-zero status, with the
	// complete report written before it exits.
	waitErr := command.Wait()
	var exitErr *exec.ExitError
	if !errors.As(waitErr, &exitErr) {
		t.Fatalf("acp exit = %v, want a non-zero status after an interrupted run:\n%s", waitErr, output.String())
	}

	reportData, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("read report: %v\n%s", err, output.String())
	}
	var report stoppedReportShape
	if err := json.Unmarshal(reportData, &report); err != nil {
		t.Fatalf("decode report: %v", err)
	}

	// Every selected file appears exactly once: a graceful stop abandons work, it never drops
	// a file from the report.
	selected := selectedSourceFiles(t, source)
	if len(selected) != small+1 {
		t.Fatalf("selected files = %d, want %d", len(selected), small+1)
	}
	rows := make(map[string]int, len(report.Files))
	abandoned := 0
	for _, row := range report.Files {
		rows[row.FullPath]++
		if failure, ok := row.FailTargets[""]; ok && failure != "" {
			abandoned++
		}
	}
	for _, path := range selected {
		if rows[path] != 1 {
			t.Fatalf("report rows for %q = %d, want exactly 1:\n%s", path, rows[path], output.String())
		}
	}
	if abandoned == 0 {
		t.Fatalf("the interrupt abandoned no file:\n%s", output.String())
	}
}

func TestACPCommandExitsNonZeroWhenACopyFails(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping end-to-end test in short mode")
	}

	repoRoot, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	tempDir := t.TempDir()
	binary := filepath.Join(tempDir, "acp")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}

	buildCtx, buildCancel := context.WithTimeout(context.Background(), time.Minute)
	defer buildCancel()
	build := exec.CommandContext(buildCtx, "go", "build", "-o", binary, "./cmd/acp")
	build.Dir = repoRoot
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build acp: %v\n%s", err, output)
	}

	source := filepath.Join(tempDir, "source")
	target := filepath.Join(tempDir, "target")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "a.txt"), []byte("new content"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The target already exists and the run is told not to overwrite it, so the item completes
	// with a refused target.
	refused := filepath.Join(target, filepath.Base(source), "a.txt")
	if err := os.MkdirAll(filepath.Dir(refused), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(refused, []byte("existing"), 0o644); err != nil {
		t.Fatal(err)
	}

	reportPath := filepath.Join(tempDir, "report.json")
	runCtx, runCancel := context.WithTimeout(context.Background(), time.Minute)
	defer runCancel()
	command := exec.CommandContext(runCtx, binary, "-p=false", "-n", "-report", reportPath, source, target)
	command.Dir = repoRoot
	output, err := command.CombinedOutput()

	// A copy that did not write its target is not a success.
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("acp exit = %v, want a non-zero status for a refused target:\n%s", err, output)
	}

	// The report file survives the failure and explains it.
	reportData, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("read report: %v\n%s", err, output)
	}
	var report stoppedReportShape
	if err := json.Unmarshal(reportData, &report); err != nil {
		t.Fatalf("decode report: %v\n%s", err, reportData)
	}
	found := false
	for _, row := range report.Files {
		if failure := row.FailTargets[refused]; failure != "" {
			found = true
		}
	}
	if !found {
		t.Fatalf("report has no refused target %q:\n%s", refused, reportData)
	}

	// The refused target keeps its previous content.
	content, err := os.ReadFile(refused)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "existing" {
		t.Fatalf("refused target content = %q, want %q", content, "existing")
	}
}

// TestACPCommandRendersTheProgressBar pins the command's default progress bar: the bar and the
// report collector are two event handlers, and a run keeps one, so the command composes them.
// A run that only collects the report prints the bar's construction frame and nothing else.
func TestACPCommandRendersTheProgressBar(t *testing.T) {
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

	build := exec.CommandContext(ctx, "go", "build", "-o", binary, "./cmd/acp")
	build.Dir = repoRoot
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the command: %v\n%s", err, output)
	}

	source := filepath.Join(tempDir, "source")
	target := filepath.Join(tempDir, "target")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	// A few megabytes keep the run long enough to produce progress frames as well as the final
	// one, and every frame is written to stderr.
	for index := 0; index < 4; index++ {
		name := fmt.Sprintf("part-%d.bin", index)
		if err := os.WriteFile(filepath.Join(source, name), bytes.Repeat([]byte{byte(index)}, 2<<20), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	command := exec.CommandContext(ctx, binary, "-report", filepath.Join(tempDir, "report.json"), source, target)
	command.Dir = repoRoot
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("run the command: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}

	// A bar that receives nothing prints its construction frame alone, which reads
	// "[0/0] indexing... 0% ... ( 0/ 1 B)". The run's own frames carry the indexed item count and
	// the byte totals, so both markers must appear. Later frames may be cleared once the bar is
	// full, which is why this asserts on content rather than on the last line.
	rendered := stderr.String()
	if !strings.Contains(rendered, "[0/4] indexing...") {
		t.Fatalf("the progress bar never received the indexed totals, stderr:\n%s", rendered)
	}
	if !strings.Contains(rendered, "MB") {
		t.Fatalf("the progress bar never received a progress event, stderr:\n%s", rendered)
	}
}
