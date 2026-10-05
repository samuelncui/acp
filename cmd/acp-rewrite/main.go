package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"

	"github.com/samuelncui/acp"
	"github.com/samuelncui/acp/internal/fileio"
	"github.com/schollz/progressbar/v3"
	"github.com/sirupsen/logrus"
)

var errRewriteBusy = errors.New("rewrite source is busy")

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string) int {
	// Parse syntax separately from execution so usage failures consistently exit with status 2.
	flags := flag.NewFlagSet("acp-rewrite", flag.ContinueOnError)
	progress := flags.Bool("p", true, "display progress bar")
	dryRun := flags.Bool("dryrun", false, "only generate task list without rewriting")
	statePath := flags.String("state", ".acp-rewrite-state.json", "state storage path")
	reportPath := flags.String("report", "", "json report storage path")
	indent := flags.Bool("report-indent", false, "json report with indent")
	var ignores []string
	flags.Func("ignore", "ignore path, file or dir", func(path string) error { ignores = append(ignores, path); return nil })
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 1 {
		logrus.Error("exactly one root path required")
		return 2
	}

	// Resolve the saved documents before any scan, cleanup, or source modification.
	root, err := filepath.Abs(flags.Arg(0))
	if err != nil {
		logrus.Error(err)
		return 1
	}
	stateAbs, err := filepath.Abs(*statePath)
	if err != nil {
		logrus.Error(err)
		return 1
	}
	reportAbs := ""
	if *reportPath != "" {
		reportAbs, err = filepath.Abs(*reportPath)
		if err != nil {
			logrus.Error(err)
			return 1
		}
	}
	if err := fileio.CheckJSONPaths(root, filepath.Dir(stateAbs), filepath.Dir(reportAbs)); err != nil {
		logrus.Error(err)
		return 1
	}
	ignoreAbs, err := normalizeIgnorePaths(root, ignores)
	if err != nil {
		logrus.Error(err)
		return 1
	}
	state, err := loadState(stateAbs)
	if err != nil {
		logrus.Errorf("load state fail, %s", err)
		return 1
	}
	if state != nil && state.Root != root {
		logrus.Error("saved state belongs to a different root")
		return 1
	}
	if state == nil {
		state = &rewriteState{Root: root}
	}
	jobs, reportErrors, err := loadReport(reportAbs)
	if err != nil {
		logrus.Errorf("load report fail, %s", err)
		return 1
	}

	// Once both documents are decoded, execution failures still write the requested report.
	var startupErr error
	defer func() {
		if startupErr == nil {
			return
		}
		reportErrors = append(reportErrors, &acp.Error{Err: startupErr})
		if err := writeReport(reportAbs, *indent, jobs, reportErrors); err != nil {
			rememberCleanup(state, err)
			logrus.Errorf("save report failed, %v", err)
		}
		// A failed checkpoint or report may have retained a newly owned temporary.
		if !state.saved {
			if err := writeState(stateAbs, state); err != nil {
				logrus.Errorf("save cleanup ownership failed, %v", err)
			}
		}
	}()

	// Keep failed cleanup recorded; a failed checkpoint must not hide the cleanup failure.
	if err := cleanupTmpFiles(state); err != nil {
		startupErr = err
		if !state.saved {
			startupErr = errors.Join(startupErr, writeState(stateAbs, state))
		}
		logrus.Error(startupErr)
		return 1
	}
	if len(state.Pending) == 0 && len(state.Busy) == 0 {
		state.Pending, err = scanEntries(ctx, root, stateAbs, reportAbs, ignoreAbs)
		if err != nil {
			startupErr = err
			logrus.Errorf("scan path fail, %s", err)
			return 1
		}
		if len(state.Pending) > 0 {
			state.saved = false
		}
	}
	if !state.saved {
		if err := writeState(stateAbs, state); err != nil {
			startupErr = err
			logrus.Errorf("save state fail, %s", err)
			return 1
		}
	}
	if *dryRun {
		logrus.Infof("dryrun tasks= %d", len(state.Pending)+len(state.Busy))
		return 0
	}

	// The sequential rewrite owner drains its current copy before advancing the persisted queue.
	err = runQueue(ctx, state, stateAbs, reportAbs, *indent, *progress, jobs, &reportErrors)
	if err != nil {
		logrus.Error(err)
		return 1
	}
	printDuplicates(jobs)
	return 0
}

func runQueue(ctx context.Context, state *rewriteState, statePath, reportPath string,
	indent, progress bool, jobs map[string]*acp.Job, reportErrors *[]*acp.Error) (runErr error) {
	// Fold the prior busy queue into pending once; each item keeps the remaining slice in place.
	if len(state.Busy) > 0 {
		state.Pending = append(state.Pending, state.Busy...)
		state.Busy = nil
		state.saved = false
	}
	// An empty run only creates a requested report when no previous document exists.
	reportDirty := false
	if len(state.Pending) == 0 && reportPath != "" {
		if _, err := os.Stat(reportPath); err != nil {
			if !os.IsNotExist(err) {
				return fmt.Errorf("stat report failed, %w", err)
			}
			reportDirty = true
		}
	}
	defer func() {
		// Retry only unpublished changes; failed removal keeps its durable ownership.
		runErr = errors.Join(runErr, cleanupTmpFiles(state))
		if reportDirty {
			if err := writeReport(reportPath, indent, jobs, *reportErrors); err != nil {
				rememberCleanup(state, err)
				runErr = errors.Join(runErr, fmt.Errorf("save report failed, %w", err))
			}
		}
		if !state.saved {
			if err := writeState(statePath, state); err != nil {
				runErr = errors.Join(runErr, fmt.Errorf("save state failed, %w", err))
			}
		}
	}()
	if !state.saved {
		if err := writeState(statePath, state); err != nil {
			return fmt.Errorf("save state failed, %w", err)
		}
	}
	var bar *progressbar.ProgressBar
	if progress {
		bar = progressbar.NewOptions(len(state.Pending))
	}

	// Retained failures stay ahead of the unprocessed tail in the existing queue order.
	retries := 0
	for remaining := len(state.Pending); remaining > 0; remaining-- {
		if err := ctx.Err(); err != nil {
			return errors.Join(runErr, err)
		}
		entry := state.Pending[retries]
		report, itemErr := processEntry(ctx, entry, state, statePath)
		if itemErr != nil {
			logrus.Warnf("rewrite failed, path=%q, %v", entry.Path, itemErr)
			runErr = errors.Join(runErr, itemErr)
			report = failedReport(report, entry, itemErr)
		}
		mergeReport(jobs, reportErrors, report)
		reportDirty = true

		// Report and relinks must be durable before the current entry can leave pending work.
		if err := writeReport(reportPath, indent, jobs, *reportErrors); err != nil {
			rememberCleanup(state, err)
			return errors.Join(runErr, fmt.Errorf("save report failed, %w", err))
		}
		reportDirty = false
		var persistence *persistenceError
		if errors.As(itemErr, &persistence) {
			return runErr
		}

		// Advance past completed work by moving only the retained retry prefix, never the tail.
		if itemErr != nil && !errors.Is(itemErr, errRewriteBusy) {
			retries++
		} else {
			if itemErr != nil {
				state.Busy = append(state.Busy, entry)
			}
			copy(state.Pending[1:retries+1], state.Pending[:retries])
			state.Pending[0] = rewriteEntry{}
			state.Pending = state.Pending[1:]
			state.saved = false
		}
		if !state.saved {
			if err := writeState(statePath, state); err != nil {
				return errors.Join(runErr, fmt.Errorf("save state failed, %w", err))
			}
		}
		if bar != nil {
			_ = bar.Add(1)
		}
	}
	return runErr
}

func processEntry(ctx context.Context, entry rewriteEntry, state *rewriteState, statePath string) (report *acp.Report, err error) {
	// Reuse scan metadata during this run; resumed entries observe their source once.
	missing := len(state.Missing)
	state.Missing = removeEntry(state.Missing, entry.Path)
	if len(state.Missing) != missing {
		state.saved = false
	}
	info := entry.Info
	if info == nil {
		info, err = os.Stat(entry.Path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				state.Missing = append(state.Missing, entry)
				state.saved = false
			}
			return nil, fmt.Errorf("stat rewrite source failed, %w", err)
		}
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("rewrite source is not a regular file: %q", entry.Path)
	}
	busy, err := isFileBusy(entry.Path)
	if err != nil {
		return nil, fmt.Errorf("check busy failed, %w", err)
	}
	if busy {
		return nil, fmt.Errorf("%w: %q", errRewriteBusy, entry.Path)
	}

	// Persist ownership before submission and settle it only after all core users have ended.
	output, err := fileio.NewOutput(entry.Path)
	if err != nil {
		return nil, err
	}
	tmp := output.Temporary
	defer func() {
		err = errors.Join(err, output.Discard())
		if output.Temporary == "" {
			state.TmpFiles = removeTmp(state.TmpFiles, tmp)
			state.saved = false
		}
	}()
	state.TmpFiles = append(state.TmpFiles, tmp)
	state.saved = false
	if err := writeState(statePath, state); err != nil {
		return nil, &persistenceError{err}
	}
	report, err = copyRewrite(ctx, &fileio.RewriteItem{Path: entry.Path, Info: info, Output: output})
	if err != nil {
		return report, err
	}

	// A retry may redo the current file; every original hardlink stays recorded until all succeed.
	if err := relink(entry, state, statePath); err != nil {
		return report, err
	}
	return report, nil
}

func failedReport(report *acp.Report, entry rewriteEntry, err error) *acp.Report {
	// Early failures still get a terminal row, and commit/relink failures cannot look successful.
	if report == nil {
		report = new(acp.Report)
	}
	job, found := findJob(report, entry.Path)
	if !found {
		base, name := filepath.Split(entry.Path)
		job = &acp.Job{Base: base, Path: []string{name}, FullPath: entry.Path, Status: acp.JobStatusFinished}
		report.Jobs = append(report.Jobs, job)
	}
	job.SuccessTargets = nil
	job.FailTargets = map[string]error{entry.Path: err}
	return report
}
