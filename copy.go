package acp

import (
	"context"
	"errors"
	"fmt"
	"hash"
	"os"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/abc950309/acp/mmap"
	"github.com/hashicorp/go-multierror"
	"github.com/minio/sha256-simd"
	"github.com/sirupsen/logrus"
)

const (
	batchSize = 1024 * 1024
)

var (
	sha256Pool = &sync.Pool{New: func() interface{} { return sha256.New() }}
)

func (c *Copyer) copy(ctx context.Context) {
	atomic.StoreInt64(&c.stage, StageCopy)
	defer atomic.StoreInt64(&c.stage, StageFinished)
	wg := new(sync.WaitGroup)

	wg.Add(1)
	go wrap(ctx, func() {
		defer wg.Done()
		defer close(c.writePipe)

		for _, job := range c.getJobs() {
			c.prepare(ctx, job)

			select {
			case <-ctx.Done():
				return
			default:
			}
		}
	})

	for i := 0; i < c.threads; i++ {
		wg.Add(1)
		go wrap(ctx, func() {
			defer wg.Done()

			for {
				select {
				case job, ok := <-c.writePipe:
					if !ok {
						return
					}

					wg.Add(1)
					c.write(ctx, job)
				case <-ctx.Done():
					return
				}
			}
		})
	}

	go wrap(ctx, func() {
		for job := range c.postPipe {
			c.post(wg, job)
		}
	})

	finished := make(chan struct{}, 1)
	go wrap(ctx, func() {
		wg.Wait()
		finished <- struct{}{}
	})

	select {
	case <-finished:
	case <-ctx.Done():
	}
}

func (c *Copyer) prepare(ctx context.Context, job *baseJob) {
	job.setStatus(jobStatusPreparing)

	switch job.typ {
	case jobTypeDir:
		for _, d := range c.dst {
			target := job.source.target(d)
			if err := os.MkdirAll(target, job.mode&os.ModePerm); err != nil && !os.IsExist(err) {
				c.reportError(target, fmt.Errorf("mkdir fail, %w", err))
				job.fail(target, fmt.Errorf("mkdir fail, %w", err))
				continue
			}
			job.succes(target)
		}

		c.writePipe <- &writeJob{baseJob: job}
		return
	}

	if c.readingFiles != nil {
		c.readingFiles <- struct{}{}
	}

	file, err := mmap.Open(job.source.path())
	if err != nil {
		c.reportError(job.source.path(), fmt.Errorf("open src file fail, %w", err))
		return
	}

	c.writePipe <- &writeJob{baseJob: job, src: file}
}

func (c *Copyer) write(ctx context.Context, job *writeJob) {
	job.setStatus(jobStatusCopying)
	if job.typ != jobTypeNormal {
		return
	}

	var wg sync.WaitGroup
	defer func() {
		wg.Wait()

		job.src.Close()
		if c.readingFiles != nil {
			<-c.readingFiles
		}

		c.postPipe <- job.baseJob
	}()

	num := atomic.AddInt64(&c.copyedFiles, 1)
	c.logf(logrus.InfoLevel, "[%d/%d] copying: %s", num, c.totalFiles, job.source.relativePath)
	c.updateCopying(func(set map[int64]struct{}) { set[num] = struct{}{} })
	defer c.updateCopying(func(set map[int64]struct{}) { delete(set, num) })

	chans := make([]chan []byte, 0, len(c.dst)+1)
	defer func() {
		for _, ch := range chans {
			close(ch)
		}
	}()

	if c.withHash {
		sha := sha256Pool.Get().(hash.Hash)
		sha.Reset()

		ch := make(chan []byte, 4)
		chans = append(chans, ch)

		wg.Add(1)
		go wrap(ctx, func() {
			defer wg.Done()
			defer sha256Pool.Put(sha)

			for buf := range ch {
				sha.Write(buf)
			}

			job.setHash(sha.Sum(nil))
		})
	}

	var readErr error
	badDsts := c.getBadDsts()
	for _, d := range c.dst {
		dst := d

		name := job.source.target(dst)
		if e, has := badDsts[dst]; has && e != nil {
			job.fail(name, fmt.Errorf("bad target path, %w", e))
		}

		file, err := os.OpenFile(name, c.createFlag, job.mode)
		if err != nil {
			c.reportError(name, fmt.Errorf("open dst file fail, %w", err))
			job.fail(name, fmt.Errorf("open dst file fail, %w", err))
			continue
		}

		ch := make(chan []byte, 4)
		chans = append(chans, ch)

		wg.Add(1)
		go wrap(ctx, func() {
			defer wg.Done()

			var rerr error
			defer func() {
				if rerr == nil {
					job.succes(name)
					return
				}

				// avoid block channel
				for range ch {
				}

				if re := os.Remove(name); re != nil {
					rerr = multierror.Append(rerr, re)
				}

				// if no space
				if errors.Is(err, syscall.ENOSPC) {
					c.addBadDsts(dst, err)
				}

				c.reportError(name, rerr)
				job.fail(name, rerr)
			}()

			defer file.Close()
			for buf := range ch {
				n, err := file.Write(buf)
				if err != nil {
					rerr = fmt.Errorf("write fail, %w", err)
					return
				}
				if len(buf) != n {
					rerr = fmt.Errorf("write fail, unexpected writen bytes return, read= %d write= %d", len(buf), n)
					return
				}
			}

			if readErr != nil {
				rerr = readErr
			}
		})
	}

	if len(chans) == 0 {
		return
	}
	readErr = c.streamCopy(ctx, chans, job.src)
}

func (c *Copyer) streamCopy(ctx context.Context, dsts []chan []byte, src *mmap.ReaderAt) error {
	if src.Len() == 0 {
		return nil
	}

	for idx := int64(0); ; idx += batchSize {
		buf, err := src.Slice(idx, batchSize)
		if err != nil {
			return fmt.Errorf("slice mmap fail, %w", err)
		}

		for _, ch := range dsts {
			ch <- buf
		}

		nr := len(buf)
		atomic.AddInt64(&c.copyedBytes, int64(nr))
		if nr < batchSize {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}
}

func (c *Copyer) post(wg *sync.WaitGroup, job *baseJob) {
	defer wg.Done()

	job.setStatus(jobStatusFinishing)
	for _, name := range job.successTargets {
		if err := os.Chtimes(name, job.modTime, job.modTime); err != nil {
			c.reportError(name, fmt.Errorf("change info, chtimes fail, %w", err))
		}
	}

	job.setStatus(jobStatusFinished)
	if job.parent == nil {
		return
	}

	left := job.parent.done(job)
	if left == 0 {
		c.postPipe <- job.parent
	}
}
