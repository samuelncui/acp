package acp

import (
	"context"
	"encoding/hex"
	"fmt"
	"hash"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/abc950309/acp/mmap"
	"github.com/hashicorp/go-multierror"
	"github.com/minio/sha256-simd"
	"github.com/schollz/progressbar/v3"
	"github.com/sirupsen/logrus"
)

const (
	unexpectFileMode = os.ModeType &^ os.ModeDir
	batchSize        = 1024 * 1024
)

var (
	sha256Pool = &sync.Pool{New: func() interface{} { return sha256.New() }}
)

const (
	StageIndex = iota
	StageCopy
	StageFinished
)

type Copyer struct {
	*option

	createFlag  int
	stage       int64
	copyedBytes int64
	totalBytes  int64
	copyedFiles int64
	totalFiles  int64

	progressBarLock   sync.Mutex
	updateProgressBar func(func(bar *progressbar.ProgressBar))

	jobs      []*Job
	writePipe chan *writeJob
	metaPipe  chan *metaJob

	wg         sync.WaitGroup
	reportLock sync.Mutex
	errors     []*Error
	files      []*File
}

func New(ctx context.Context, opts ...Option) (*Copyer, error) {
	opt := newOption()
	for _, o := range opts {
		if o == nil {
			continue
		}
		opt = o(opt)
	}
	if err := opt.check(); err != nil {
		return nil, err
	}

	c := &Copyer{
		option:            opt,
		writePipe:         make(chan *writeJob, 32),
		metaPipe:          make(chan *metaJob, 8),
		updateProgressBar: func(f func(bar *progressbar.ProgressBar)) {},
	}

	c.createFlag = os.O_WRONLY | os.O_CREATE
	if c.overwrite {
		c.createFlag |= os.O_TRUNC
	} else {
		c.createFlag |= os.O_EXCL
	}

	c.wg.Add(1)
	go c.run(ctx)
	return c, nil
}

func (c *Copyer) Wait() *Report {
	c.wg.Wait()
	return c.Report()
}

func (c *Copyer) reportError(file string, err error) {
	e := &Error{Path: file, Err: err}
	logrus.Errorf(e.Error())

	c.reportLock.Lock()
	defer c.reportLock.Unlock()
	c.errors = append(c.errors, e)
}

func (c *Copyer) run(ctx context.Context) {
	defer c.wg.Done()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if c.withProgressBar {
		c.startProgressBar(ctx)
	}

	c.index(ctx)
	c.copy(ctx)
}

func (c *Copyer) index(ctx context.Context) {
	for _, s := range c.src {
		c.walk(s.base, s.path)
	}
}

func (c *Copyer) copy(ctx context.Context) {
	atomic.StoreInt64(&c.stage, StageCopy)
	defer atomic.StoreInt64(&c.stage, StageFinished)

	go func() {
		defer close(c.writePipe)

		for _, job := range c.jobs {
			c.prepare(ctx, job)

			select {
			case <-ctx.Done():
				return
			default:
			}
		}
	}()

	go func() {
		defer close(c.metaPipe)

		for job := range c.writePipe {
			c.write(ctx, job)

			select {
			case <-ctx.Done():
				return
			default:
			}
		}
	}()

	for file := range c.metaPipe {
		c.meta(file)
	}
}

func (c *Copyer) walk(base, path string) {
	stat, err := os.Stat(path)
	if err != nil {
		c.reportError(path, fmt.Errorf("walk get stat, %w", err))
		return
	}

	job, err := newJobFromFileInfo(base, path, stat)
	if err != nil {
		c.reportError(path, fmt.Errorf("make job fail, %w", err))
		return
	}
	if job.Mode&unexpectFileMode != 0 {
		return
	}
	if !job.Mode.IsDir() {
		atomic.AddInt64(&c.totalFiles, 1)
		atomic.AddInt64(&c.totalBytes, job.Size)
		c.jobs = append(c.jobs, job)
		return
	}

	enterJob := new(Job)
	*enterJob = *job
	enterJob.Type = JobTypeEnterDir
	c.jobs = append(c.jobs, enterJob)

	files, err := os.ReadDir(path)
	if err != nil {
		c.reportError(path, fmt.Errorf("walk read dir, %w", err))
		return
	}

	for _, file := range files {
		c.walk(base, path+"/"+file.Name())
	}

	exitJob := new(Job)
	*exitJob = *job
	exitJob.Type = JobTypeExitDir
	c.jobs = append(c.jobs, exitJob)
}

func (c *Copyer) prepare(ctx context.Context, job *Job) {
	switch job.Type {
	case JobTypeEnterDir:
		for _, d := range c.dst {
			name := d + job.RelativePath
			err := os.Mkdir(name, job.Mode&os.ModePerm)
			if err != nil && !os.IsExist(err) {
				c.reportError(name, fmt.Errorf("mkdir fail, %w", err))
			}
		}
		return
	case JobTypeExitDir:
		c.writePipe <- &writeJob{Job: job}
		return
	}

	name := job.Source
	file, err := mmap.Open(name)
	if err != nil {
		c.reportError(name, fmt.Errorf("open src file fail, %w", err))
		return
	}

	c.writePipe <- &writeJob{Job: job, src: file}
}

func (c *Copyer) write(ctx context.Context, job *writeJob) {
	if job.src == nil {
		c.metaPipe <- &metaJob{Job: job.Job}
		return
	}
	defer job.src.Close()

	num := atomic.AddInt64(&c.copyedFiles, 1)
	go c.updateProgressBar(func(bar *progressbar.ProgressBar) {
		bar.Describe(fmt.Sprintf("[%d/%d] %s", num, c.totalFiles, job.RelativePath))
	})

	var wg sync.WaitGroup
	var lock sync.Mutex
	var readErr error
	chans := make([]chan []byte, 0, len(c.dst)+1)
	next := &metaJob{Job: job.Job, failTarget: make(map[string]string)}

	if c.withHash {
		sha := sha256Pool.Get().(hash.Hash)
		sha.Reset()

		ch := make(chan []byte, 4)
		chans = append(chans, ch)

		wg.Add(1)
		go func() {
			defer wg.Done()
			defer sha256Pool.Put(sha)

			for buf := range ch {
				sha.Write(buf)
			}

			lock.Lock()
			defer lock.Unlock()
			next.hash = sha.Sum(nil)
		}()
	}

	for _, d := range c.dst {
		name := d + job.RelativePath
		file, err := os.OpenFile(name, c.createFlag, job.Mode)
		if err != nil {
			c.reportError(name, fmt.Errorf("open dst file fail, %w", err))

			lock.Lock()
			defer lock.Unlock()
			next.failTarget[name] = fmt.Errorf("open dst file fail, %w", err).Error()
			continue
		}

		ch := make(chan []byte, 4)
		chans = append(chans, ch)

		wg.Add(1)
		go func() {
			defer wg.Done()

			var rerr error
			defer func() {
				if rerr == nil {
					lock.Lock()
					defer lock.Unlock()
					next.successTarget = append(next.successTarget, name)
					return
				}

				// avoid block channel
				for range ch {
				}

				if re := os.Remove(name); re != nil {
					rerr = multierror.Append(rerr, re)
				}

				c.reportError(name, rerr)

				lock.Lock()
				defer lock.Unlock()
				next.failTarget[name] = rerr.Error()
			}()

			defer file.Close()
			for buf := range ch {
				nr := len(buf)

				n, err := file.Write(buf)
				if n < 0 || nr < n {
					if err == nil {
						rerr = fmt.Errorf("write fail, unexpected return, byte_num= %d", n)
						return
					}

					rerr = fmt.Errorf("write fail, %w", err)
					return
				}
				if nr != n {
					rerr = fmt.Errorf("write fail, write and read bytes not equal, read= %d write= %d", nr, n)
					return
				}
			}

			if readErr != nil {
				rerr = readErr
			}
		}()
	}

	defer func() {
		for _, ch := range chans {
			close(ch)
		}

		wg.Wait()
		c.metaPipe <- next
	}()

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

func (c *Copyer) meta(job *metaJob) {
	if job.Mode.IsDir() {
		for _, d := range c.dst {
			if err := os.Chtimes(d+job.RelativePath, job.ModTime, job.ModTime); err != nil {
				c.reportError(d+job.RelativePath, fmt.Errorf("change info, chtimes fail, %w", err))
			}
		}
		return
	}

	c.files = append(c.files, &File{
		Source:        job.Source,
		SuccessTarget: job.successTarget,
		FailTarget:    job.failTarget,
		RelativePath:  job.RelativePath,
		Size:          job.Size,
		Mode:          job.Mode,
		ModTime:       job.ModTime,
		WriteTime:     time.Now(),
		SHA256:        hex.EncodeToString(job.hash),
	})

	for _, name := range job.successTarget {
		if err := os.Chtimes(name, job.ModTime, job.ModTime); err != nil {
			c.reportError(name, fmt.Errorf("change info, chtimes fail, %w", err))
		}
	}
}
