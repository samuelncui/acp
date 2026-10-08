package acp_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/samuelncui/acp"
)

func TestACPCommandE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping end-to-end test in short mode")
	}

	// Build the real command for the copy, index and linear-target cases.
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

	// Select a tree with regular, empty and nested files and two target flags.
	source := filepath.Join(tempDir, "source")
	targets := []string{filepath.Join(tempDir, "target-a"), filepath.Join(tempDir, "target-b")}
	reportPath := filepath.Join(tempDir, "report.json")
	for _, target := range targets {
		if err := os.MkdirAll(target, 0o755); err != nil {
			t.Fatalf("create target directory: %v", err)
		}
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

	// Repeated target flags must write every file to both target directories.
	run(binary, "-p=false", "-report", reportPath, "-report-indent", "-target", targets[0], "-target", targets[1], source)
	for _, target := range targets {
		for relativePath, want := range files {
			got, err := os.ReadFile(filepath.Join(target, filepath.Base(source), relativePath))
			if err != nil {
				t.Fatalf("read copied file %q: %v", relativePath, err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("copied file %q = %v, want %v", relativePath, got, want)
			}
		}
	}

	// The CLI's indentation flag must reach the report writer, with exact hashes and target sets.
	reportData, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	if !bytes.Contains(reportData, []byte("\n  \"files\"")) || bytes.Contains(reportData, []byte("\t")) {
		t.Fatalf("report does not use two-space indentation:\n%s", reportData)
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

	// Every selected source needs its own row regardless of result delivery order.
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
		if len(job.SuccessTargets) != len(targets) {
			t.Fatalf("job %q success targets = %v", sourcePath, job.SuccessTargets)
		}
		for _, target := range targets {
			want := filepath.Join(target, filepath.Base(source), relativePath)
			if !slices.Contains(job.SuccessTargets, want) {
				t.Fatalf("job %q does not report target %q: %v", sourcePath, want, job.SuccessTargets)
			}
		}
		sum := sha256.Sum256(data)
		if job.SHA256 != hex.EncodeToString(sum[:]) || job.Size != int64(len(data)) {
			t.Fatalf("job %q content facts = %d / %q, want %d / %x",
				sourcePath, job.Size, job.SHA256, len(data), sum)
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
	indexed := make(map[string]bool, len(index.Jobs))
	for _, job := range index.Jobs {
		relative, err := filepath.Rel(source, job.FullPath)
		if err != nil {
			t.Fatal(err)
		}
		data, ok := files[relative]
		if !ok || indexed[relative] {
			t.Fatalf("unexpected or repeated index row %q", job.FullPath)
		}
		indexed[relative] = true
		sum := sha256.Sum256(data)
		if job.SHA256 != hex.EncodeToString(sum[:]) || job.Size != int64(len(data)) {
			t.Fatalf("index row %q content facts = %d / %q, want %d / %x",
				job.FullPath, job.Size, job.SHA256, len(data), sum)
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

func TestACPCommandHonorsIndexAndReportFlags(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping end-to-end test in short mode")
	}

	// Build the command once so both cases exercise its actual flag parsing and exit status.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	repoRoot, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "acp")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, "./cmd/acp")
	build.Dir = repoRoot
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the command: %v\n%s", err, output)
	}

	t.Run("notarget overrides explicit targets", func(t *testing.T) {
		// An explicit target already contains data that an index operation must leave alone.
		root := t.TempDir()
		source := filepath.Join(root, "source.txt")
		target := filepath.Join(root, "target")
		if err := os.MkdirAll(target, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(source, []byte("source content"), 0o644); err != nil {
			t.Fatal(err)
		}
		existing := filepath.Join(target, "source.txt")
		if err := os.WriteFile(existing, []byte("existing content"), 0o644); err != nil {
			t.Fatal(err)
		}

		// Request the index together with -target: it must still have no target outcomes.
		reportPath := filepath.Join(root, "index.json")
		command := exec.CommandContext(ctx, binary, "-p=false", "-notarget", "-target", target, "-report", reportPath, source)
		command.Dir = repoRoot
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("run the index: %v\n%s", err, output)
		}
		data, err := os.ReadFile(reportPath)
		if err != nil {
			t.Fatal(err)
		}
		var report acp.Report
		if err := json.Unmarshal(data, &report); err != nil {
			t.Fatal(err)
		}
		if len(report.Errors) != 0 || len(report.Jobs) != 1 {
			t.Fatalf("index errors/rows = %d/%d, want 0/1", len(report.Errors), len(report.Jobs))
		}
		row := report.Jobs[0]
		if row.SHA256 == "" || len(row.SuccessTargets) != 0 || len(row.FailTargets) != 0 {
			t.Errorf("index row = %#v, want a hash and no targets", row)
		}
		content, err := os.ReadFile(existing)
		if err != nil {
			t.Fatal(err)
		}
		if string(content) != "existing content" {
			t.Errorf("existing target = %q, want unchanged content", content)
		}
	})

	t.Run("report failure exits nonzero", func(t *testing.T) {
		// A directory cannot be replaced by a report file, regardless of host permissions.
		root := t.TempDir()
		source := filepath.Join(root, "source.txt")
		target := filepath.Join(root, "target.txt")
		reportPath := filepath.Join(root, "report-directory")
		if err := os.WriteFile(source, []byte("fixture"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(reportPath, 0o755); err != nil {
			t.Fatal(err)
		}

		// A successful copy with an unsaved requested report must tell its caller it failed.
		command := exec.CommandContext(ctx, binary, "-p=false", "-report", reportPath, source, target)
		command.Dir = repoRoot
		output, err := command.CombinedOutput()
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || !strings.Contains(string(output), "open report fail") {
			t.Errorf("command exit = %v, want a report-write failure:\n%s", err, output)
		}
		content, err := os.ReadFile(target)
		if err != nil {
			t.Fatal(err)
		}
		if string(content) != "fixture" {
			t.Errorf("target = %q, want a completed copy", content)
		}
	})
}

// selectedSourceFiles independently enumerates the regular files expected in a wildcard report.
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
	if err := os.WriteFile(filepath.Join(source, large), bytes.Repeat([]byte{'x'}, 64<<20), 0o644); err != nil {
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

	// A private staging file proves copying started before publication and completion.
	copied := filepath.Join(target, filepath.Base(source), ".tmp_*")
	deadline := time.Now().Add(30 * time.Second)
	for {
		if staged, _ := filepath.Glob(copied); len(staged) > 0 {
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

func TestACPCommandExitCodesAndSameFilePartialFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping end-to-end test in short mode")
	}

	// Exercise exit codes from a compiled command, including a same-file error alongside a valid target.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	root := t.TempDir()
	binary := filepath.Join(root, "acp")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	if output, err := exec.CommandContext(ctx, "go", "build", "-o", binary, "./cmd/acp").CombinedOutput(); err != nil {
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
	source := filepath.Join(root, "source")
	good := filepath.Join(root, "good")
	if err := os.WriteFile(source, []byte("source survives"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(good, 0o700); err != nil {
		t.Fatal(err)
	}
	reportPath := filepath.Join(root, "report.json")
	code(1, "-p=false", "-report", reportPath, "-target", root, "-target", good, source)

	// A partial failure writes its report and completes the independent target before exit 1.
	data, err := os.ReadFile(filepath.Join(good, "source"))
	if err != nil || string(data) != "source survives" {
		t.Fatalf("independent copy=%q / %v", data, err)
	}
	data, err = os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	var report acp.Report
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Jobs) != 1 || len(report.Jobs[0].FailTargets) != 1 || len(report.Jobs[0].SuccessTargets) != 1 {
		t.Fatalf("partial report=%+v", report)
	}
	code(0, "-p=false", source, filepath.Join(root, "exact-copy"))
}
