package acp

import (
	"context"
	"fmt"
	"os"
	"sort"
	"sync/atomic"

	"github.com/schollz/progressbar/v3"
)

const (
	unexpectFileMode = os.ModeType &^ os.ModeDir
)

func (c *Copyer) index(ctx context.Context) {
	for _, s := range c.src {
		c.walk(nil, s)
	}

	c.updateProgressBar(func(bar *progressbar.ProgressBar) {
		bar.ChangeMax64(atomic.LoadInt64(&c.totalBytes))
		bar.Describe(fmt.Sprintf("[0/%d] index finished...", atomic.LoadInt64(&c.totalFiles)))
	})
}

func (c *Copyer) walk(parent *baseJob, src *source) *baseJob {
	path := src.path()

	stat, err := os.Stat(path)
	if err != nil {
		c.reportError(path, fmt.Errorf("walk get stat, %w", err))
		return nil
	}
	if stat.Mode()&unexpectFileMode != 0 {
		return nil
	}

	job, err := newJobFromFileInfo(parent, src, stat)
	if err != nil {
		c.reportError(path, fmt.Errorf("make job fail, %w", err))
		return nil
	}

	c.appendJobs(job)
	if job.typ == jobTypeNormal {
		totalBytes := atomic.AddInt64(&c.totalBytes, job.size)
		totalFiles := atomic.AddInt64(&c.totalFiles, 1)

		c.updateProgressBar(func(bar *progressbar.ProgressBar) {
			bar.ChangeMax64(totalBytes)
			bar.Describe(fmt.Sprintf("[0/%d] indexing...", totalFiles))
		})

		return job
	}

	files, err := os.ReadDir(path)
	if err != nil {
		c.reportError(path, fmt.Errorf("walk read dir, %w", err))
		return nil
	}

	job.children = make(map[*baseJob]struct{}, len(files))
	for _, file := range files {
		id := c.walk(job, &source{base: src.base, relativePath: src.relativePath + "/" + file.Name()})
		if id == nil {
			continue
		}
		job.children[id] = struct{}{}
	}

	return job
}

func (c *Copyer) checkJobs() bool {
	c.jobsLock.Lock()
	defer c.jobsLock.Unlock()

	if len(c.jobs) == 0 {
		c.reportError("", fmt.Errorf("cannot found available jobs"))
		return false
	}

	sort.Slice(c.jobs, func(i int, j int) bool {
		return c.jobs[i].comparableRelativePath < c.jobs[j].comparableRelativePath
	})

	var last *baseJob
	filtered := make([]*baseJob, 0, len(c.jobs))
	for _, job := range c.jobs {
		if last == nil {
			filtered = append(filtered, job)
			last = job
			continue
		}
		if last.source.relativePath != job.source.relativePath {
			filtered = append(filtered, job)
			last = job
			continue
		}

		if last.typ != job.typ {
			c.reportError(job.source.path(), fmt.Errorf("same relative path with different type, '%s' and '%s'", job.source.path(), last.source.path()))
			return false
		}
		if last.typ == jobTypeNormal {
			c.reportError(job.source.path(), fmt.Errorf("same relative path as normal file, ignored, '%s'", job.source.path()))
			continue
		}

		func() {
			last.lock.Lock()
			defer last.lock.Unlock()

			for n := range job.children {
				last.children[n] = struct{}{}
				n.parent = last
			}
		}()
	}

	c.jobs = filtered
	return true
}

func (c *Copyer) appendJobs(jobs ...*baseJob) {
	c.jobsLock.Lock()
	defer c.jobsLock.Unlock()
	c.jobs = append(c.jobs, jobs...)
}

func (c *Copyer) getJobs() []*baseJob {
	c.jobsLock.Lock()
	defer c.jobsLock.Unlock()
	return c.jobs
}

func (c *Copyer) getJobsAndNoSpaceSource() ([]*baseJob, []*source) {
	c.jobsLock.Lock()
	defer c.jobsLock.Unlock()
	return c.jobs, c.noSpaceSource
}
