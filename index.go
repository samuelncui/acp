package acp

import (
	"context"
	"fmt"
	"os"
	"sort"
	"sync/atomic"
	"time"
)

const (
	unexpectFileMode = os.ModeType &^ os.ModeDir
)

type counter struct {
	bytes, files int64
}

func (c *Copyer) index(ctx context.Context) (<-chan *baseJob, error) {
	jobs := c.walk(ctx)
	filtered, err := c.joinJobs(jobs)
	if err != nil {
		return nil, err
	}

	ch := make(chan *baseJob, 128)
	go wrap(ctx, func() {
		defer close(ch)

		for _, job := range filtered {
			select {
			case <-ctx.Done():
				return
			case ch <- job:
			}
		}
	})

	return ch, nil
}

func (c *Copyer) walk(ctx context.Context) []*baseJob {
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
		jobs = append(jobs, job)
		atomic.AddInt64(&cntr.files, 1)
		atomic.AddInt64(&cntr.bytes, job.size)
	}

	var walk func(src *source)
	walk = func(src *source) {
		path := src.src()

		stat, err := os.Stat(path)
		if err != nil {
			c.reportError(path, "", fmt.Errorf("walk get stat, %w", err))
			return
		}

		mode := stat.Mode()
		if mode&unexpectFileMode != 0 {
			return
		}
		if !mode.IsDir() {
			job, err := c.newJobFromFileInfo(src, stat)
			if err != nil {
				c.reportError(path, "", fmt.Errorf("make job fail, %w", err))
				return
			}

			appendJob(job)
			return
		}

		files, err := os.ReadDir(path)
		if err != nil {
			c.reportError(path, "", fmt.Errorf("walk read dir, %w", err))
			return
		}
		for _, file := range files {
			walk(src.append(file.Name()))
		}

		return
	}
	for _, s := range c.src {
		walk(s)
	}
	return jobs
}

func (c *Copyer) joinJobs(jobs []*baseJob) ([]*baseJob, error) {
	sort.Slice(jobs, func(i int, j int) bool {
		return comparePath(jobs[i].source.path, jobs[j].source.path) < 0
	})

	var last *baseJob
	filtered := make([]*baseJob, 0, len(jobs))
	for _, job := range jobs {
		if last != nil && comparePath(last.source.path, job.source.path) == 0 {
			c.reportError(last.source.src(), "", fmt.Errorf("same relative path, ignored, '%s'", job.source.src()))
			continue
		}

		filtered = append(filtered, job)
		last = job
	}

	return filtered, nil
}
