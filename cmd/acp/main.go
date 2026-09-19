package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"strings"
	"sync"

	"github.com/klauspost/cpuid/v2"
	"github.com/samuelncui/acp"
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

func main() {
	ctx, cancel := context.WithCancel(context.Background())

	cpuid.Flags()
	flag.Parse()
	cpuid.Detect()

	sources := flag.Args()
	if len(sources) == 0 {
		logrus.Fatalf("cannot found source path")
	}

	if !*noTarget && len(targetPaths) == 0 {
		targetPaths = append(targetPaths, sources[len(sources)-1])
		sources = sources[:len(sources)-1]
	}
	if len(sources) == 0 {
		logrus.Fatalf("cannot found source path")
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt)
	go func() {
		for sig := range signals {
			if sig != os.Interrupt {
				continue
			}
			cancel()
		}
	}()

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

	copyer, err := acp.New(ctx, opts...)
	if err != nil {
		logrus.Fatalf("unexpected exit: %s", err)
	}
	runErr := copyer.WaitErr()
	if runErr != nil {
		logrus.Errorf("copy failed, %s", runErr)
	}

	// The report is stored even when the run failed, so a batch can be inspected after the
	// command told its caller that something went wrong.
	if err := storeReport(report, *reportPath, *reportIndent); err != nil {
		logrus.Warnf("open report fail, path= '%s', err= %s", *reportPath, err)
		logrus.Infof("report= %q", report.getter().ToJSONString(false))
	}

	// A copy that did not finish is not a success: an item ACP could not process, a target
	// that was not written, or a pipeline failure all make the command exit non-zero.
	if runErr != nil || report.hasFailure() {
		os.Exit(1)
	}
}

// storeReport writes the JSON report when the caller asked for one.
func storeReport(collector *report, path string, indent bool) error {
	if path == "" {
		return nil
	}

	file, err := os.Create(path)
	if err != nil {
		return err
	}
	if _, err := file.WriteString(collector.getter().ToJSONString(indent)); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
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
		logrus.Fatalf("stat dst path fail, %s", err)
	}

	srcStat, err := os.Stat(src)
	if err == nil {
		return srcStat.Mode().IsRegular()
	}

	logrus.Fatalf("stat src path fail, %s", err)
	return false
}

// report collects the terminal row of every item and the pipeline-level errors for the JSON
// report. Item and target failures travel inside the item's own row, so a single failure is
// never counted twice.
type report struct {
	lock   sync.Mutex
	jobs   []*acp.Job
	errors []*acp.Error
}

func newReport() *report {
	return new(report)
}

func (r *report) handleEvent(event acp.Event) {
	r.lock.Lock()
	defer r.lock.Unlock()

	switch e := event.(type) {
	case *acp.EventUpdateJob:
		r.jobs = append(r.jobs, e.Job)
	case *acp.EventReportError:
		r.errors = append(r.errors, e.Error)
	}
}

// hasFailure reports whether the run left a failure behind: a pipeline problem, an item ACP
// could not process, or a target that was not written.
func (r *report) hasFailure() bool {
	r.lock.Lock()
	defer r.lock.Unlock()

	if len(r.errors) > 0 {
		return true
	}
	for _, job := range r.jobs {
		if len(job.FailTargets) > 0 {
			return true
		}
	}
	return false
}

func (r *report) getter() *acp.Report {
	r.lock.Lock()
	defer r.lock.Unlock()

	return &acp.Report{Jobs: r.jobs, Errors: r.errors}
}
