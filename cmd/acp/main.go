package main

import (
	"context"
	"encoding/hex"
	"flag"
	"io"
	"os"
	"os/signal"
	"path/filepath"
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

	// Select the entries to copy: one exact target path, or every file under the sources
	// mapped onto the target directories.
	var entries []acp.FileEntry
	if !*noTarget && accurateTarget(sources, targetPaths) {
		src := filepath.Clean(sources[0])
		base, name := filepath.Split(src)
		entries = []acp.FileEntry{{
			Base:    base,
			Path:    name,
			Source:  src,
			Targets: []string{filepath.Clean(targetPaths[0])},
		}}
	} else {
		selected, err := acp.SelectFiles(sources, targetPaths)
		if err != nil {
			logrus.Fatalf("unexpected exit: %s", err)
		}
		entries = selected
	}

	report := newReport()
	opts := make([]acp.Option, 0, 8)
	// The command line reports hashes only when it is asked for a report.
	hashPolicy := acp.HashOff
	if *reportPath != "" {
		hashPolicy = acp.HashRead
	}
	opts = append(opts, acp.WithHashPolicy(hashPolicy))
	opts = append(opts, acp.SetToDevice(acp.Overwrite(!*notOverwrite)))

	if *withProgressBar {
		opts = append(opts, acp.WithProgressBar())
	}

	if *fromLinear {
		opts = append(opts, acp.SetFromDevice(acp.LinearDevice(true)))
	}
	if *toLinear {
		opts = append(opts, acp.SetToDevice(acp.LinearDevice(true)))
	}

	opts = append(opts, acp.WithEventHandler(report.handleEvent))
	defer func() {
		if *reportPath == "" {
			return
		}

		r, err := os.Create(*reportPath)
		if err != nil {
			logrus.Warnf("open report fail, path= '%s', err= %s", *reportPath, err)
			logrus.Infof("report= %q", report.getter().ToJSONString(false))
			return
		}
		defer r.Close()

		r.Write([]byte(report.getter().ToJSONString(*reportIndent)))
	}()

	if err := acp.Run(ctx, &fileSource{entries: entries, report: report}, opts...); err != nil {
		logrus.Errorf("copy failed, %s", err)
	}
}

// accurateTarget reports whether one source path copies to one exact target path instead
// of mapping a source tree onto a target directory.
func accurateTarget(sources, targets []string) bool {
	if len(sources) > 1 || len(targets) > 1 {
		return false
	}

	dst, src := targetPaths[0], sources[0]
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

// fileSource supplies the selected entries as caller-owned items.
type fileSource struct {
	entries []acp.FileEntry
	report  *report
}

func (s *fileSource) Next(ctx context.Context) ([]acp.Item, error) {
	if len(s.entries) == 0 {
		return nil, io.EOF
	}

	entries := s.entries
	s.entries = nil

	items := make([]acp.Item, 0, len(entries))
	for _, entry := range entries {
		items = append(items, &fileItem{entry: entry, report: s.report})
	}
	return items, nil
}

// fileItem carries one file entry through the copy pipeline.
type fileItem struct {
	entry  acp.FileEntry
	report *report
}

func (i *fileItem) Source() string { return i.entry.Source }

func (i *fileItem) Targets() []string { return i.entry.Targets }

// Completed records the item's outcome for the JSON report. The pipeline reports events
// as the item advances, so the callback itself performs no I/O.
func (i *fileItem) Completed(result *acp.Result) {
	i.report.completed(i.entry, result)
}

func (i *fileItem) Failed(err error) {
	i.report.failed(i.entry, err)
}

// report collects job rows and errors for the JSON report.
type report struct {
	lock   sync.Mutex
	jobs   []*acp.Job
	errors []*acp.Error
}

func newReport() *report {
	return new(report)
}

func (r *report) handleEvent(event acp.Event) {
	failure, ok := event.(*acp.EventReportError)
	if !ok {
		return
	}

	r.lock.Lock()
	defer r.lock.Unlock()
	r.errors = append(r.errors, failure.Error)
}

func (r *report) completed(entry acp.FileEntry, result *acp.Result) {
	r.lock.Lock()
	defer r.lock.Unlock()

	job := &acp.Job{
		FullPath: entry.Source,
		Base:     entry.Base,
		Path:     entry.Path,

		Status:    acp.JobStatusFinished,
		Size:      result.Size,
		Mode:      result.Mode,
		ModTime:   result.ModTime,
		WriteTime: result.WriteTime,
		SHA256:    hex.EncodeToString(result.SHA256),

		SignatureCacheHit: result.SignatureCacheHit,
	}
	for _, target := range result.Targets {
		if target.Err == nil {
			job.SuccessTargets = append(job.SuccessTargets, target.Path)
			continue
		}
		if job.FailTargets == nil {
			job.FailTargets = make(map[string]error, len(result.Targets))
		}
		job.FailTargets[target.Path] = target.Err
	}

	r.jobs = append(r.jobs, job)
}

func (r *report) failed(entry acp.FileEntry, err error) {
	r.lock.Lock()
	defer r.lock.Unlock()

	r.errors = append(r.errors, &acp.Error{Src: entry.Source, Err: err})
}

func (r *report) getter() *acp.Report {
	r.lock.Lock()
	defer r.lock.Unlock()

	return &acp.Report{Jobs: r.jobs, Errors: r.errors}
}
