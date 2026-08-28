package acp

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/sirupsen/logrus"
)

type Copyer struct {
	*option
	running           sync.WaitGroup
	errLock           sync.Mutex
	err               error
	eventCh           chan Event
	getDevice         func(in string) string
	getDiskUsageCache func(mountPoint string) *diskUsageCache
	linearTargetEnded uint32
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

	getDevice, err := getMountpointCache()
	if err != nil {
		return nil, err
	}

	c := &Copyer{
		option:    opt,
		eventCh:   make(chan Event, 128),
		getDevice: getDevice,
		getDiskUsageCache: Cache(func(mountPoint string) *diskUsageCache {
			return newDiskUsageCache(mountPoint, defaultDiskUsageFreshInterval)
		}),
	}

	// Account for both pipeline and event dispatch before either goroutine starts.
	c.running.Add(2)
	go wrap(ctx, func() { c.run(ctx) })

	return c, nil
}

func (c *Copyer) Wait() {
	c.running.Wait()
}

// WaitErr waits for the copy pipeline and returns its first error.
func (c *Copyer) WaitErr() error {
	c.Wait()
	c.errLock.Lock()
	defer c.errLock.Unlock()
	return c.err
}

func (c *Copyer) setError(err error) {
	if err == nil {
		return
	}
	c.errLock.Lock()
	defer c.errLock.Unlock()
	if c.err == nil {
		c.err = err
	}
}

func (c *Copyer) endLinearTarget(err error) {
	if !c.toDevice.linear || !checkErrorAbort(err) {
		return
	}
	atomic.StoreUint32(&c.linearTargetEnded, 1)
}

func (c *Copyer) linearTargetStopped() bool {
	return atomic.LoadUint32(&c.linearTargetEnded) != 0
}

func (c *Copyer) run(ctx context.Context) error {
	// Give internal failures one cancellation boundary without canceling the caller.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer c.running.Done()
	defer close(c.eventCh)

	// Keep event dispatch alive until every pipeline stage stops publishing.
	go wrap(ctx, func() { c.eventLoop(ctx) })

	// Start the bounded index before connecting downstream stages.
	indexed, err := c.index(ctx)
	if err != nil {
		c.setError(err)
		return err
	}

	// Run preparation, copying, and result persistence as one pipeline.
	prepared := c.prepare(ctx, indexed)
	copyed := c.copy(ctx, prepared)
	sinkFailed := c.cleanupJob(ctx, cancel, copyed)

	// Flush persisted results unless a Sink write already failed.
	if c.streamSink != nil && !sinkFailed {
		if err := c.streamSink.Flush(ctx); err != nil {
			c.setError(fmt.Errorf("flush stream sink failed, %w", err))
		}
	}

	// Drain remaining stages in dependency order. Prepared jobs retain open sources.
	for range indexed {
	}
	for job := range prepared {
		job.finishSource()
	}
	for range copyed {
	}

	return nil
}

func (c *Copyer) eventLoop(ctx context.Context) {
	defer c.running.Done()

	chans := make([]chan Event, len(c.eventHanders))
	for idx := range chans {
		chans[idx] = make(chan Event, 128)
	}

	var handlers sync.WaitGroup
	for idx, ch := range chans {
		handler := c.eventHanders[idx]
		events := ch

		handlers.Add(1)
		go wrap(ctx, func() {
			defer handlers.Done()

			for {
				e, ok := <-events
				if !ok {
					handler(&EventFinished{})
					return
				}
				handler(e)
			}
		})
	}

	defer func() {
		for _, ch := range chans {
			close(ch)
		}
		handlers.Wait()
	}()
	for e := range c.eventCh {
		for _, ch := range chans {
			ch <- e
		}
	}
}

func (c *Copyer) logf(l logrus.Level, format string, args ...any) {
	c.logger.Logf(l, format, args...)
}

func (c *Copyer) submit(e Event) {
	c.eventCh <- e
}

func (c *Copyer) reportError(src, dst string, err error) {
	e := &Error{Src: src, Dst: dst, Err: err}
	c.setError(fmt.Errorf("copy failed, source=%q target=%q, %w", src, dst, err))
	c.logf(logrus.ErrorLevel, e.Error())
	c.submit(&EventReportError{Error: e})
}
