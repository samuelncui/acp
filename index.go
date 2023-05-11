package acp

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/samber/lo"
)

const (
	UnexpectFileMode = os.ModeType &^ os.ModeDir
)

type counter struct {
	bytes, files int64
}

func (c *Copyer) index(ctx context.Context) (<-chan *baseJob, error) {
	jobs, err := c.walk(ctx)
	if err != nil {
		return nil, err
	}

	ch := make(chan *baseJob, 128)
	go wrap(ctx, func() {
		defer close(ch)

		for _, job := range jobs {
			select {
			case <-ctx.Done():
				return
			case ch <- job:
			}
		}
	})

	return ch, nil
}

func (c *Copyer) walk(ctx context.Context) ([]*baseJob, error) {
	done := make(chan struct{})
	defer close(done)

	cntr := new(counter)
	go wrap(ctx, func() {
		ticker := time.NewTicker(time.Second)
		for {
			select {
			case <-ticker.C:
				c.submit(&EventUpdateCount{Bytes: atomic.LoadInt64(&cntr.bytes), Files: atomic.LoadInt64(&cntr.files)})
			case <-done:
				c.submit(&EventUpdateCount{Bytes: atomic.LoadInt64(&cntr.bytes), Files: atomic.LoadInt64(&cntr.files), Finished: true})
				return
			}
		}
	})

	jobs := make([]*baseJob, 0, 64)
	appendJob := func(job *baseJob) {
		if !job.mode.IsRegular() {
			c.reportError(job.path, "", fmt.Errorf("unexpected file mode, not regular file, mode= %s", job.mode))
			return
		}

		c.submit(&EventUpdateJob{job.report()})
		jobs = append(jobs, job)
		atomic.AddInt64(&cntr.files, 1)
		atomic.AddInt64(&cntr.bytes, job.size)
	}

	var walk func(src *source, dsts []string)
	walk = func(src *source, dsts []string) {
		path := src.src()

		stat, err := os.Stat(path)
		if err != nil {
			c.reportError(path, "", fmt.Errorf("walk get stat, %w", err))
			return
		}

		mode := stat.Mode()
		if mode.IsRegular() {
			targets := make([]string, 0, len(dsts))
			for _, d := range dsts {
				targets = append(targets, src.dst(d))
			}

			appendJob(&baseJob{
				copyer: c,
				path:   path,

				size:    stat.Size(),
				mode:    stat.Mode(),
				modTime: stat.ModTime(),

				targets: targets,
			})
			return
		}
		if mode&UnexpectFileMode != 0 {
			return
		}

		files, err := os.ReadDir(path)
		if err != nil {
			c.reportError(path, "", fmt.Errorf("walk read dir, %w", err))
			return
		}
		for _, file := range files {
			walk(src.append(file.Name()), dsts)
		}
	}

	results := make([]*baseJob, 0, 64)
	for _, j := range c.wildcardJobs {
		for _, s := range j.src {
			walk(s, j.dst)
		}

		if len(jobs) == 0 {
			continue
		}

		joined, err := c.joinJobs(jobs)
		if err != nil {
			return nil, err
		}

		results = append(results, joined...)
		jobs = jobs[:0]
	}

	for _, j := range c.accurateJobs {
		stat, err := os.Stat(j.src)
		if err != nil {
			c.reportError(j.src, "", fmt.Errorf("accurate job get stat, %w", err))
			continue
		}
		if !stat.Mode().IsRegular() {
			continue
		}

		appendJob(&baseJob{
			copyer: c,
			src:    &source{base: "/", path: lo.Filter(strings.Split(j.src, "/"), func(s string, _ int) bool { return s != "" })},
			path:   j.src,

			size:    stat.Size(),
			mode:    stat.Mode(),
			modTime: stat.ModTime(),

			targets: j.dsts,
		})
	}
	results = append(results, jobs...)

	return results, nil
}

func (c *Copyer) joinJobs(jobs []*baseJob) ([]*baseJob, error) {
	sort.Slice(jobs, func(i int, j int) bool {
		return comparePath(jobs[i].src.path, jobs[j].src.path) < 0
	})

	var last *baseJob
	filtered := make([]*baseJob, 0, len(jobs))
	for _, job := range jobs {
		if last != nil && comparePath(last.src.path, job.src.path) == 0 {
			c.reportError(last.path, "", fmt.Errorf("same relative path, ignored, '%s'", job.path))
			continue
		}

		filtered = append(filtered, job)
		last = job
	}

	return filtered, nil
}
