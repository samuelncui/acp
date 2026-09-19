package acp

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// UnexpectFileMode is the file-mode mask a walk refuses to descend into.
const UnexpectFileMode = os.ModeType &^ os.ModeDir

// Copyer is the af05f05c compatibility shell: the same job options, events and JSON report as
// the commit that predates the streaming API, built on the push engine.
//
// New enumerates the requested jobs, submits every selected file to a StreamCopyer, and
// translates each result into the terminal report row event the CLI, Report and
// NewReportGetter machinery reads. Wait/WaitErr close that stream and report how the run ended.
type Copyer struct {
	*option

	stream *StreamCopyer

	// feedErr is written by the feed goroutine before it closes feedDone.
	feedErr  error
	feedDone chan struct{}
}

// New builds a shell run. Creation and validation errors, such as a target that is not a
// directory or a source that does not exist, come back from New; an enumeration error, such as
// two sources mapping to the same relative path, ends the run and comes back from WaitErr.
func New(ctx context.Context, opts ...Option) (*Copyer, error) {
	opt, err := buildOption(opts...)
	if err != nil {
		return nil, err
	}

	c := &Copyer{option: opt, feedDone: make(chan struct{})}
	stream, err := newStream(ctx, c.reportResults, opt)
	if err != nil {
		return nil, err
	}
	c.stream = stream

	// The walk and the feed run beside the pipeline, exactly like the af05f05c index stage.
	go c.feed(ctx)

	return c, nil
}

// Wait blocks until the run finished. Use WaitErr to learn how it ended.
func (c *Copyer) Wait() {
	_ = c.WaitErr()
}

// WaitErr closes the run, drains the pipeline and returns its terminal error. An item-level
// failure is not a run error: it travels in that item's report row.
func (c *Copyer) WaitErr() error {
	<-c.feedDone
	c.stream.setError(c.feedErr)
	_ = c.stream.Close()

	return c.stream.Wait()
}

// feed enumerates the job options and submits everything as one batch. One batch matters: the
// engine keeps every accepted item, so a run stopped midway still reports every selected file
// instead of losing the ones the feed had not reached.
func (c *Copyer) feed(ctx context.Context) {
	defer close(c.feedDone)
	// End the feed even when the caller never waits, so the run finishes on its own.
	defer func() { _ = c.stream.Close() }()

	items, err := c.selectItems(ctx)
	if err != nil {
		c.feedErr = err
		return
	}
	if len(items) == 0 {
		return
	}

	// The engine records why a submission stopped, so Wait reports it; ErrTargetNoSpace is an
	// item outcome, not a run error, and the items carry it.
	_ = c.stream.Submit(items...)
}

// selectItems enumerates every job option into the items of one run. Enumeration errors are run
// errors: a duplicate relative path would write one target twice, so it ends the run instead of
// being logged and skipped.
func (c *Copyer) selectItems(ctx context.Context) ([]Item, error) {
	items := make([]Item, 0, 64)

	for _, job := range c.wildcardJobs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		entries, err := c.walkWildcard(job)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			items = append(items, entry)
		}
	}

	for _, job := range c.accurateJobs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		item := c.accurateItem(job)
		if item == nil {
			continue
		}
		items = append(items, item)
	}

	return items, nil
}

// walkWildcard walks every source of one wildcard job, keeps regular files, maps each
// source-relative path onto every target directory, and returns the entries in physical order.
// A source that cannot be read is reported and skipped, which is what af05f05c did.
func (c *Copyer) walkWildcard(job *wildcardJob) ([]*compatItem, error) {
	entries := make([]*compatItem, 0, 64)

	var walk func(src *source)
	walk = func(src *source) {
		path := src.src()

		info, err := os.Stat(path)
		if err != nil {
			c.stream.reportError(path, "", fmt.Errorf("walk get stat, %w", err))
			return
		}

		mode := info.Mode()
		if mode.IsRegular() {
			targets := make([]string, 0, len(job.dst))
			for _, dst := range job.dst {
				targets = append(targets, src.dst(dst))
			}
			entries = append(entries, compatItemOf(src, path, targets))
			return
		}
		if mode&UnexpectFileMode != 0 {
			return
		}

		files, err := os.ReadDir(path)
		if err != nil {
			c.stream.reportError(path, "", fmt.Errorf("walk read dir, %w", err))
			return
		}
		for _, file := range files {
			walk(src.append(file.Name()))
		}
	}
	for _, src := range job.src {
		walk(src)
	}

	// One physical order drives a linear target, so the request order is the platform's path
	// order rather than the order the file system returned.
	sort.Slice(entries, func(i, j int) bool {
		return comparePath(entries[i].relative(), entries[j].relative()) < 0
	})

	// A repeated relative path would write one target twice, so the run fails before it copies
	// anything instead of keeping the first and skipping the rest.
	for index := 1; index < len(entries); index++ {
		if entries[index].relative() != entries[index-1].relative() {
			continue
		}
		return nil, fmt.Errorf(
			"same relative path, sources= '%s' and '%s'",
			entries[index-1].source, entries[index].source,
		)
	}

	return entries, nil
}

// accurateItem resolves one exact source and target pair, which copies a file to the path it was
// given instead of mapping a relative path onto a target directory.
func (c *Copyer) accurateItem(job *accurateJob) *compatItem {
	path := filepath.Clean(job.src)

	info, err := os.Stat(path)
	if err != nil {
		c.stream.reportError(path, "", fmt.Errorf("accurate job get stat, %w", err))
		return nil
	}
	if !info.Mode().IsRegular() {
		return nil
	}

	targets := make([]string, 0, len(job.dsts))
	for _, dst := range job.dsts {
		targets = append(targets, filepath.Clean(dst))
	}

	// af05f05c reported an exact source against the file-system root, with the source split into
	// path segments, so the old row keeps that base and those segments; FullPath names the source
	// directly for a consumer that needs one path.
	return compatItemOf(&source{base: "/", path: path}, path, targets)
}

// compatItemOf builds the report coordinates of one enumerated file. The shell's walk knows the
// source base and the source-relative path, which are exactly the af05f05c report row fields.
func compatItemOf(src *source, sourcePath string, targets []string) *compatItem {
	return &compatItem{
		source:  sourcePath,
		targets: targets,
		base:    src.base,
		path:    pathSegments(src.path),
	}
}

// pathSegments splits a path into the slash-delimited segments a report row carries. Empty
// segments are dropped, which is how af05f05c turned a path into a report row.
func pathSegments(name string) []string {
	segments := strings.Split(filepath.ToSlash(name), "/")
	filtered := make([]string, 0, len(segments))
	for _, segment := range segments {
		if segment == "" {
			continue
		}
		filtered = append(filtered, segment)
	}
	return filtered
}

// compatItem carries the report coordinates of one enumerated file, so a result can be
// translated back into the row this surface promises: the whole source path plus the base and
// the source-relative path segments the af05f05c report row is made of.
type compatItem struct {
	source  string
	targets []string
	base    string
	path    []string
}

func (i *compatItem) Source() string { return i.source }

func (i *compatItem) Targets() []string { return i.targets }

// relative is the source-relative path the row's segments join to, which is what orders and
// de-duplicates one enumeration.
func (i *compatItem) relative() string { return path.Join(i.path...) }

// reportResults translates every result into the terminal report row event the report machinery
// consumes. An item-level failure travels under the empty target key, which is the slot no
// requested target can occupy, so a failed item still has a row.
func (c *Copyer) reportResults(results []Result) error {
	for _, result := range results {
		c.stream.submit(&EventUpdateJob{Job: reportRow(result)})
	}

	return nil
}

// reportRow renders one result as a report row.
func reportRow(result Result) *Job {
	job := &Job{
		Status:            JobStatusFinished,
		Size:              result.Size,
		Mode:              result.Mode,
		ModTime:           result.ModTime,
		WriteTime:         result.WriteTime,
		SHA256:            sha256Hex(result.SHA256),
		SignatureCacheHit: result.SignatureCacheHit,
	}
	if item, ok := result.Job.(*compatItem); ok {
		job.FullPath, job.Base, job.Path = item.source, item.base, item.path
	}

	// An item ACP could not process has no target outcome to report: the failure travels under
	// the empty key, which is the shape the report has always used for it.
	if result.Err != nil {
		job.FailTargets = map[string]error{"": result.Err}
		return job
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

	return job
}

type accurateJob struct {
	src  string
	dsts []string
}

// AccurateJob copies one exact source to the given destination paths, without mapping a
// relative path onto a target directory.
func AccurateJob(src string, dsts []string) Option {
	return func(o *option) *option {
		o.accurateJobs = append(o.accurateJobs, &accurateJob{src: src, dsts: dsts})
		return o
	}
}

type wildcardJob struct {
	src []*source
	dst []string
}

// check validates the job before a run starts: every target must be an existing directory, at
// least one source must be given, and every source must exist.
func (job *wildcardJob) check() error {
	filteredDst := make([]string, 0, len(job.dst))
	for _, p := range job.dst {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		p = filepath.Clean(p)

		dstStat, err := os.Stat(p)
		if err != nil {
			return fmt.Errorf("check dst path '%s', %w", p, err)
		}
		if !dstStat.IsDir() {
			return fmt.Errorf("dst path is not a dir, path= '%s'", p)
		}

		filteredDst = append(filteredDst, p)
	}
	job.dst = filteredDst

	if len(job.src) == 0 {
		return fmt.Errorf("source path not found")
	}
	sort.Slice(job.src, func(i, j int) bool {
		return comparePath(job.src[i].path, job.src[j].path) < 0
	})
	for _, s := range job.src {
		src := s.src()
		if _, err := os.Stat(src); err != nil {
			return fmt.Errorf("check src path '%s', %w", src, err)
		}
	}

	return nil
}

// WildcardJob copies every regular file below the given sources into every target directory,
// keeping each file's source-relative path.
func WildcardJob(opts ...WildcardJobOption) Option {
	return func(o *option) *option {
		j := new(wildcardJob)
		for _, opt := range opts {
			j = opt(j)
		}

		if len(j.src) == 0 {
			return o
		}

		o.wildcardJobs = append(o.wildcardJobs, j)
		return o
	}
}

// WildcardJobOption configures one WildcardJob.
type WildcardJobOption func(*wildcardJob) *wildcardJob

// Source adds source paths to a WildcardJob. A source may be a directory or a regular file.
func Source(paths ...string) WildcardJobOption {
	return func(j *wildcardJob) *wildcardJob {
		for _, p := range paths {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			p = filepath.Clean(p)

			base, name := filepath.Split(p)
			j.src = append(j.src, &source{base: base, path: name})
		}
		return j
	}
}

// AccurateSource adds sources as base plus relative path segments to a WildcardJob.
func AccurateSource(base string, paths ...[]string) WildcardJobOption {
	return func(j *wildcardJob) *wildcardJob {
		for _, path := range paths {
			j.src = append(j.src, &source{base: base, path: filepath.Join(path...)})
		}
		return j
	}
}

// Target adds target directories to a WildcardJob. Every one of them must exist.
func Target(paths ...string) WildcardJobOption {
	return func(j *wildcardJob) *wildcardJob {
		j.dst = append(j.dst, paths...)
		return j
	}
}

// WithHash selects whether the run computes a content hash and refreshes the stored hash cache.
// It is the af05f05c form of WithHashPolicy: true maps to HashReadRefresh and false to HashOff,
// and a repeated hash option is last-wins.
func WithHash(b bool) Option {
	if !b {
		return WithHashPolicy(HashOff)
	}

	return WithHashPolicy(HashReadRefresh)
}
