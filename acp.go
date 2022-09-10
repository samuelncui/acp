package acp

import (
	"context"
	"os"
	"sync"

	"github.com/schollz/progressbar/v3"
	"github.com/sirupsen/logrus"
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

	updateProgressBar func(func(bar *progressbar.ProgressBar))
	updateCopying     func(func(set map[int64]struct{}))
	logf              func(l logrus.Level, format string, args ...any)

	jobsLock sync.Mutex
	jobs     []*baseJob

	errsLock sync.Mutex
	errors   []*Error

	badDstsLock sync.Mutex
	badDsts     map[string]error

	readingFiles chan struct{}
	writePipe    chan *writeJob
	postPipe     chan *baseJob

	running sync.WaitGroup
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
		option: opt,
		stage:  StageIndex,

		updateProgressBar: func(f func(bar *progressbar.ProgressBar)) {},
		updateCopying:     func(f func(set map[int64]struct{})) {},
		logf: func(l logrus.Level, format string, args ...any) {
			logrus.StandardLogger().Logf(l, format, args...)
		},

		badDsts:   make(map[string]error),
		writePipe: make(chan *writeJob, 32),
		postPipe:  make(chan *baseJob, 8),
	}

	c.createFlag = os.O_WRONLY | os.O_CREATE
	if c.overwrite {
		c.createFlag |= os.O_TRUNC
	} else {
		c.createFlag |= os.O_EXCL
	}

	if c.fromDevice.linear {
		c.readingFiles = make(chan struct{}, 1)
	}

	c.running.Add(1)
	go c.run(ctx)
	return c, nil
}

func (c *Copyer) Wait() *Report {
	c.running.Wait()
	return c.Report()
}

func (c *Copyer) run(ctx context.Context) {
	defer c.running.Done()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if c.withProgressBar {
		c.startProgressBar(ctx)
	}

	c.index(ctx)
	if !c.checkJobs() {
		return
	}

	c.copy(ctx)
}

func (c *Copyer) reportError(file string, err error) {
	e := &Error{Path: file, Err: err}
	c.logf(logrus.ErrorLevel, e.Error())

	c.errsLock.Lock()
	defer c.errsLock.Unlock()
	c.errors = append(c.errors, e)
}

func (c *Copyer) getErrors() []*Error {
	c.errsLock.Lock()
	defer c.errsLock.Unlock()
	return c.errors
}

func (c *Copyer) addBadDsts(dst string, err error) {
	c.badDstsLock.Lock()
	defer c.badDstsLock.Unlock()
	c.badDsts[dst] = err
}

func (c *Copyer) getBadDsts() map[string]error {
	c.errsLock.Lock()
	defer c.errsLock.Unlock()
	return c.badDsts
}
