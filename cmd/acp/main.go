package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"strings"

	"github.com/klauspost/cpuid/v2"
	"github.com/samuelncui/acp"
	"github.com/samuelncui/acp/internal/fileio"
	"github.com/sirupsen/logrus"
)

var (
	withProgressBar = flag.Bool("p", true, "display progress bar")
	notOverwrite    = flag.Bool("n", false, "not overwrite exist file")
	// continueReport  = flag.String("c", "", "continue with previous report, for auto fill circumstances")
	noTarget     = flag.Bool("notarget", false, "do not have target, use as dir index tool")
	reportPath   = flag.String("report", "", "json report storage path")
	reportIndent = flag.Bool("report-indent", false, "json report with indent")
	fromLinear   = flag.Bool("from-linear", false, "copy from linear device, such like tape drive")
	toLinear     = flag.Bool("to-linear", false, "copy to linear device, such like tape drive")

	targetPaths []string
)

func init() {
	flag.Func("target", "use target flag to give multi target path", func(s string) error {
		targetPaths = append(targetPaths, s)
		return nil
	})
}

func main() { os.Exit(run()) }

func run() int {
	// Parse the command and establish its cancellation context.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	cpuid.Flags()
	flag.Parse()
	cpuid.Detect()

	// An index has no destinations even when -target was also supplied.
	sources := flag.Args()
	if len(sources) == 0 {
		logrus.Error("source path required")
		return 2
	}
	if *noTarget {
		targetPaths = nil
	} else if len(targetPaths) == 0 {
		targetPaths = append(targetPaths, sources[len(sources)-1])
		sources = sources[:len(sources)-1]
	}
	if len(sources) == 0 {
		logrus.Error("source path required")
		return 2
	}

	// Collect the report and configure the requested copy or index operation.
	report := newReport()
	opts := make([]acp.Option, 0, 8)

	// One exact target path copies one file to exactly that path; every other run maps the
	// source trees onto the target directories.
	if !*noTarget && accurateTarget(sources, targetPaths) {
		opts = append(opts, acp.AccurateJob(sources[0], []string{targetPaths[0]}))
	} else {
		opts = append(opts, acp.WildcardJob(acp.Source(sources...), acp.Target(targetPaths...)))
	}

	// The command line reports hashes only when it is asked for a report.
	opts = append(opts, acp.WithHash(*reportPath != ""))
	opts = append(opts, acp.Overwrite(!*notOverwrite))

	// Linear devices serialize their own side of the transfer.
	if *fromLinear {
		opts = append(opts, acp.SetFromDevice(acp.LinearDevice(true)))
	}
	if *toLinear {
		opts = append(opts, acp.SetToDevice(acp.LinearDevice(true)))
	}

	// ACP keeps one event handler per run: a later WithEventHandler replaces the earlier one, so
	// the progress bar and the report collector are composed here instead of registered apart.
	// Without this the bar would silently receive nothing, because the report registration comes
	// last and wins.
	handler := acp.EventHandler(report.handleEvent)
	if *withProgressBar {
		bar := acp.NewProgressBar()
		collect := handler
		handler = func(event acp.Event) {
			bar(event)
			collect(event)
		}
	}
	opts = append(opts, acp.WithEventHandler(handler))

	// Drain the run before deciding its status or writing the report.
	copyer, err := acp.New(ctx, opts...)
	if err != nil {
		report.handleEvent(&acp.EventReportError{Error: &acp.Error{Err: err}})
	}
	runErr := err
	if copyer != nil {
		runErr = copyer.WaitErr()
	}
	return report.finish(runErr, *reportPath, *reportIndent)
}

// finish captures one terminal report for storage, diagnostics and exit status after the run drains.
func (r *report) finish(runErr error, path string, indent bool) int {
	// Capture the terminal rows once; a run error remains separate from item and target outcomes.
	snapshot := r.getter()
	if runErr != nil {
		logrus.Errorf("copy failed, %s", runErr)
	}

	// The report is stored even when the run failed, so a batch can be inspected after the
	// command told its caller that something went wrong.
	reportErr := storeReport(snapshot, path, indent)
	if reportErr != nil {
		logrus.Warnf("open report fail, path= '%s', err= %s", path, reportErr)
		logrus.Infof("report= %q", snapshot.ToJSONString(false))
	}

	// A copy that did not finish is not a success: an item ACP could not process, a target
	// that was not written, a pipeline failure or an unsaved requested report all exit non-zero.
	if runErr != nil || reportErr != nil || hasFailure(snapshot) {
		return 1
	}
	return 0
}

// storeReport writes the JSON report when the caller asked for one.
func storeReport(snapshot *acp.Report, path string, indent bool) error {
	if path == "" {
		return nil
	}

	return fileio.WriteJSON(path, snapshot, indent)
}

// accurateTarget reports whether one source path copies to one exact target path instead
// of mapping a source tree onto a target directory.
func accurateTarget(sources, targets []string) bool {
	if len(sources) != 1 || len(targets) != 1 {
		return false
	}

	dst, src := targets[0], sources[0]
	if strings.HasSuffix(dst, "/") {
		return false
	}

	dstStat, err := os.Stat(dst)
	if err == nil {
		return !dstStat.IsDir()
	}
	if !os.IsNotExist(err) {
		return false
	}

	srcStat, err := os.Stat(src)
	if err == nil {
		return srcStat.Mode().IsRegular()
	}

	// Let option validation report the source error and still write the requested report.
	return false
}

// report uses the shared collector and finalizes the command after the run drains.
type report struct {
	handleEvent acp.EventHandler
	getter      acp.ReportGetter
}

func newReport() *report {
	handler, getter := acp.NewReportGetter()
	return &report{handleEvent: handler, getter: getter}
}

func hasFailure(snapshot *acp.Report) bool {
	if len(snapshot.Errors) > 0 {
		return true
	}
	for _, job := range snapshot.Jobs {
		if len(job.FailTargets) > 0 {
			return true
		}
	}
	return false
}
