package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"math/rand"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"sort"
	"time"

	"github.com/samuelncui/acp"
	"github.com/schollz/progressbar/v3"
	"github.com/sirupsen/logrus"
)

var (
	randSource = rand.New(rand.NewSource(time.Now().UnixNano()))
)

type rewriteEntry struct {
	Path  string   `json:"path"`
	Links []string `json:"links,omitempty"`
}

type rewriteState struct {
	Root     string         `json:"root"`
	Pending  []rewriteEntry `json:"pending,omitempty"`
	Busy     []rewriteEntry `json:"busy,omitempty"`
	TmpFiles []string       `json:"tmp_files,omitempty"`
}

func main() {
	withProgressBar := flag.Bool("p", true, "display progress bar")
	dryRun := flag.Bool("dryrun", false, "only generate task list without rewriting")
	statePath := flag.String("state", ".acp-rewrite-state.json", "state storage path")
	reportPath := flag.String("report", "", "json report storage path")
	reportIndent := flag.Bool("report-indent", false, "json report with indent")

	flag.Parse()
	if flag.NArg() == 0 {
		logrus.Fatalf("path required")
	}

	root := flag.Arg(0)
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		logrus.Fatalf("get abs root fail, %s", err)
	}
	stateAbs, err := filepath.Abs(*statePath)
	if err != nil {
		logrus.Fatalf("get abs state path fail, %s", err)
	}
	var reportAbs string
	if *reportPath != "" {
		reportAbs, err = filepath.Abs(*reportPath)
		if err != nil {
			logrus.Fatalf("get abs report path fail, %s", err)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	state, err := loadState(stateAbs)
	if err != nil {
		logrus.Fatalf("load state fail, %s", err)
	}
	if state == nil || state.Root != rootAbs {
		state = &rewriteState{Root: rootAbs}
	}

	reportJobs, reportErrors := loadReport(*reportPath)

	if len(state.TmpFiles) > 0 {
		if err := cleanupTmpFiles(state); err != nil {
			logrus.Warnf("cleanup tmp files fail, %s", err)
		}
		if err := saveState(stateAbs, state); err != nil {
			logrus.Fatalf("save state fail, %s", err)
		}
	}

	if len(state.Pending) == 0 && len(state.Busy) == 0 {
		entries, err := scanEntries(rootAbs, stateAbs, reportAbs)
		if err != nil {
			logrus.Fatalf("scan path fail, %s", err)
		}
		state.Pending = entries
		if err := saveState(stateAbs, state); err != nil {
			logrus.Fatalf("save state fail, %s", err)
		}
	}
	if *dryRun {
		logrus.Infof("dryrun tasks= %d", len(state.Pending)+len(state.Busy))
		return
	}

	queue := append([]rewriteEntry{}, state.Pending...)
	queue = append(queue, state.Busy...)
	state.Pending = nil
	state.Busy = nil
	if err := saveState(stateAbs, state); err != nil {
		logrus.Fatalf("save state fail, %s", err)
	}

	var bar *progressbar.ProgressBar
	if *withProgressBar {
		bar = progressbar.NewOptions(len(queue))
	}

	var currentTmp string
	for idx, entry := range queue {
		if ctx.Err() != nil {
			if currentTmp != "" {
				_ = os.Remove(currentTmp)
				currentTmp = ""
			}
			state.Pending = append(state.Pending, queue[idx:]...)
			if err := saveState(stateAbs, state); err != nil {
				logrus.Fatalf("save state fail, %s", err)
			}
			return
		}

		busy, err := isFileBusy(entry.Path)
		if err != nil {
			logrus.Warnf("check busy fail, path= '%s', err= %s", entry.Path, err)
			state.Pending = append(state.Pending, entry)
			if bar != nil {
				_ = bar.Add(1)
			}
			continue
		}
		if busy {
			state.Busy = append(state.Busy, entry)
			if bar != nil {
				_ = bar.Add(1)
			}
			continue
		}

		tmpPath, err := newTmpPath(entry.Path, randSource)
		if err != nil {
			logrus.Warnf("generate tmp fail, path= '%s', err= %s", entry.Path, err)
			state.Pending = append(state.Pending, entry)
			if bar != nil {
				_ = bar.Add(1)
			}
			continue
		}

		state.TmpFiles = append(state.TmpFiles, tmpPath)
		if err := saveState(stateAbs, state); err != nil {
			logrus.Fatalf("save state fail, %s", err)
		}

		currentTmp = tmpPath
		report, err := rewriteFile(ctx, entry, tmpPath)
		currentTmp = ""

		state.TmpFiles = removeTmp(state.TmpFiles, tmpPath)
		if err := saveState(stateAbs, state); err != nil {
			logrus.Fatalf("save state fail, %s", err)
		}

		if err != nil {
			logrus.Warnf("rewrite fail, path= '%s', err= %s", entry.Path, err)
			state.Pending = append(state.Pending, entry)
			if bar != nil {
				_ = bar.Add(1)
			}
			continue
		}

		if err := relink(entry); err != nil {
			logrus.Warnf("relink fail, path= '%s', err= %s", entry.Path, err)
		}

		mergeReport(reportJobs, &reportErrors, report)
		if err := saveReport(*reportPath, *reportIndent, reportJobs, reportErrors); err != nil {
			logrus.Warnf("save report fail, %s", err)
		}
		if err := saveState(stateAbs, state); err != nil {
			logrus.Fatalf("save state fail, %s", err)
		}
		if bar != nil {
			_ = bar.Add(1)
		}
	}

	if err := saveState(stateAbs, state); err != nil {
		logrus.Fatalf("save state fail, %s", err)
	}
	if err := saveReport(*reportPath, *reportIndent, reportJobs, reportErrors); err != nil {
		logrus.Warnf("save report fail, %s", err)
	}

	printDuplicates(reportJobs)
}

func scanEntries(root, statePath, reportPath string) ([]rewriteEntry, error) {
	groups := make(map[fileIdentity][]string)
	entries := make([]rewriteEntry, 0, 128)

	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == statePath || (reportPath != "" && p == reportPath) {
			return nil
		}
		if d.IsDir() {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}

		id, linked := checkFileLinked(info)
		if linked {
			groups[id] = append(groups[id], p)
			return nil
		}

		entries = append(entries, rewriteEntry{Path: p})
		return nil
	})
	if err != nil {
		return nil, err
	}

	for _, paths := range groups {
		sort.Strings(paths)
		if len(paths) == 0 {
			continue
		}
		entry := rewriteEntry{Path: paths[0]}
		if len(paths) > 1 {
			entry.Links = append(entry.Links, paths[1:]...)
		}
		entries = append(entries, entry)
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, nil
}

func newTmpPath(path string, rnd *rand.Rand) (string, error) {
	suffix := func() string {
		const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
		buf := make([]byte, 4)
		for i := range buf {
			buf[i] = letters[rnd.Intn(len(letters))]
		}
		return string(buf)
	}
	for i := 0; i < 10; i++ {
		tmp := fmt.Sprintf("%s.tmp%s", path, suffix())
		if _, err := os.Stat(tmp); err != nil {
			if os.IsNotExist(err) {
				return tmp, nil
			}
			return "", err
		}
	}
	return "", fmt.Errorf("tmp path collide")
}

func rewriteFile(ctx context.Context, entry rewriteEntry, tmpPath string) (*acp.Report, error) {
	stat, err := os.Stat(entry.Path)
	if err != nil {
		return nil, err
	}
	if !stat.Mode().IsRegular() {
		return nil, nil
	}

	opts := []acp.Option{
		acp.AccurateJob(entry.Path, []string{tmpPath}),
		acp.WithHash(true),
		acp.Overwrite(true),
	}

	handler, getter := acp.NewReportGetter()
	opts = append(opts, acp.WithEventHandler(handler))

	c, err := acp.New(ctx, opts...)
	if err != nil {
		return nil, err
	}
	c.Wait()

	report := getter()
	if report == nil {
		return nil, fmt.Errorf("report nil")
	}
	if len(report.Errors) > 0 {
		return report, report.Errors[0]
	}

	job, ok := findJob(report, entry.Path)
	if !ok {
		return report, fmt.Errorf("job not found")
	}
	if len(job.FailTargets) > 0 {
		return report, fmt.Errorf("copy fail")
	}
	if len(job.SuccessTargets) == 0 {
		return report, fmt.Errorf("copy not finished")
	}

	if err := os.Rename(tmpPath, entry.Path); err != nil {
		if remErr := os.Remove(entry.Path); remErr != nil {
			return report, err
		}
		if err2 := os.Rename(tmpPath, entry.Path); err2 != nil {
			return report, err2
		}
	}

	return report, nil
}

func relink(entry rewriteEntry) error {
	if len(entry.Links) == 0 {
		return nil
	}
	for _, link := range entry.Links {
		if link == entry.Path {
			continue
		}
		if err := relinkOne(entry.Path, link); err != nil {
			return err
		}
	}
	return nil
}

func relinkOne(src, link string) error {
	tmpPath, err := newTmpPath(link, randSource)
	if err != nil {
		return fmt.Errorf("new tmp path fail, path= %q, %w", link, err)
	}
	if err := os.Link(src, tmpPath); err != nil {
		return fmt.Errorf("create link fail, src= %q dst= %q, %w", src, tmpPath, err)
	}

	if err := acp.CopyAttrs(tmpPath, link); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("copy attrs fail, dst= %q src= %q, %w", tmpPath, link, err)
	}

	if err := os.Rename(tmpPath, link); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename tmp fail, from= %q to= %q, %w", tmpPath, link, err)
	}

	return nil
}

func loadState(path string) (*rewriteState, error) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var state rewriteState
	if err := json.NewDecoder(f).Decode(&state); err != nil {
		return nil, err
	}
	return &state, nil
}

func saveState(path string, state *rewriteState) error {
	if state == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "\t")
	if err := enc.Encode(state); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func cleanupTmpFiles(state *rewriteState) error {
	for _, tmp := range state.TmpFiles {
		_ = os.Remove(tmp)
	}
	state.TmpFiles = nil
	return nil
}

func removeTmp(tmpFiles []string, tmp string) []string {
	next := make([]string, 0, len(tmpFiles))
	for _, t := range tmpFiles {
		if t == tmp {
			continue
		}
		next = append(next, t)
	}
	return next
}

func findJob(report *acp.Report, filePath string) (*acp.Job, bool) {
	for _, job := range report.Jobs {
		full := job.Base + path.Join(job.Path...)
		if full == filePath {
			return job, true
		}
	}
	return nil, false
}

func loadReport(path string) (map[string]*acp.Job, []*acp.Error) {
	jobs := make(map[string]*acp.Job, 128)
	errors := make([]*acp.Error, 0)
	if path == "" {
		return jobs, errors
	}
	if _, err := os.Stat(path); err != nil {
		return jobs, errors
	}

	f, err := os.Open(path)
	if err != nil {
		return jobs, errors
	}
	defer f.Close()

	var report acp.Report
	if err := json.NewDecoder(f).Decode(&report); err != nil {
		return jobs, errors
	}

	mergeReport(jobs, &errors, &report)
	return jobs, errors
}

func mergeReport(jobs map[string]*acp.Job, errors *[]*acp.Error, report *acp.Report) {
	if report == nil {
		return
	}
	for _, job := range report.Jobs {
		full := job.Base + path.Join(job.Path...)
		jobs[full] = job
	}
	if len(report.Errors) > 0 {
		*errors = append(*errors, report.Errors...)
	}
}

func saveReport(path string, indent bool, jobs map[string]*acp.Job, errors []*acp.Error) error {
	if path == "" {
		return nil
	}
	report := &acp.Report{
		Jobs:   make([]*acp.Job, 0, len(jobs)),
		Errors: errors,
	}
	for _, job := range jobs {
		report.Jobs = append(report.Jobs, job)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	if indent {
		enc.SetIndent("", "\t")
	}
	return enc.Encode(report)
}

func printDuplicates(jobs map[string]*acp.Job) {
	type dupKey struct {
		size int64
		hash string
	}
	dups := make(map[dupKey][]string)
	for _, job := range jobs {
		if job == nil || job.SHA256 == "" || job.Size == 0 {
			continue
		}
		full := job.Base + path.Join(job.Path...)
		key := dupKey{size: job.Size, hash: job.SHA256}
		dups[key] = append(dups[key], full)
	}

	keys := make([]dupKey, 0, len(dups))
	for key := range dups {
		if len(dups[key]) > 1 {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].size != keys[j].size {
			return keys[i].size < keys[j].size
		}
		return keys[i].hash < keys[j].hash
	})

	for _, key := range keys {
		paths := dups[key]
		sort.Strings(paths)
		logrus.Infof("duplicate size= %d sha256= %s files= %v", key.size, key.hash, paths)
	}
}
