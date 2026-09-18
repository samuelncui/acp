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
	signatures        *signatureCache

	// hardStop ends the pipeline without per-item accounting, for internal failures.
	hardStop     chan struct{}
	hardStopOnce sync.Once
	// abandoned carries items that will never be processed so the reporting goroutine
	// can still report them.
	abandoned chan *baseJob
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
		hardStop:  make(chan struct{}),
		abandoned: make(chan *baseJob),
		getDevice: getDevice,
		getDiskUsageCache: Cache(func(mountPoint string) *diskUsageCache {
			return newDiskUsageCache(mountPoint, defaultDiskUsageFreshInterval)
		}),
	}
	if opt.hashPolicy.usesCache() {
		c.signatures = newSignatureCache(signatureWorkers(opt.fromDevice, opt.toDevice))
	}

	// Account for both pipeline and event dispatch before either goroutine starts.
	c.running.Add(2)
	go wrap(ctx, func() { c.run(ctx) })

	return c, nil
}

// stopHard ends the pipeline immediately. Per-item accounting is not promised afterwards.
func (c *Copyer) stopHard() {
	c.hardStopOnce.Do(func() {
		close(c.hardStop)
	})
}

// stopped reports whether the caller stopped the pipeline gracefully.
func (c *Copyer) stopped(ctx context.Context) bool {
	if ctx.Err() != nil || c.linearTargetStopped() {
		return true
	}
	select {
	case <-c.hardStop:
		return true
	default:
		return false
	}
}

// abandonment describes why an accepted item never entered processing.
func (c *Copyer) abandonment(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.linearTargetStopped() {
		return ErrTargetNoSpace
	}
	return context.Canceled
}

// abandon hands an accepted item to the reporting goroutine, because a stage stops
// reading its input but must not drop a job it already owns.
func (c *Copyer) abandon(job *baseJob, reason error) bool {
	if job == nil || job.item == nil {
		return false
	}
	job.itemError = reason
	select {
	case c.abandoned <- job:
		return true
	case <-c.hardStop:
		return false
	}
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
	defer c.finishSignatureCache()

	// Keep event dispatch alive until every pipeline stage stops publishing.
	go wrap(ctx, func() { c.eventLoop(ctx) })

	// Start the bounded index before connecting downstream stages.
	indexed, err := c.index(ctx)
	if err != nil {
		c.setError(err)
		c.stopHard()
		return err
	}

	// Run preparation, copying, and result reporting as one pipeline. Every stage drains
	// what it holds, so a graceful stop still reports every accepted item.
	prepared := c.prepare(ctx, indexed)
	copyed := c.copy(ctx, prepared)
	c.cleanup(ctx, copyed)

	// Drain remaining stages in dependency order. A hard stop leaves jobs behind.
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

// reportItemError reports one item's failure without failing the pipeline: the caller
// receives the failure as an item outcome and decides what it means.
func (c *Copyer) reportItemError(src, dst string, err error) {
	e := &Error{Src: src, Dst: dst, Err: err}
	c.logf(logrus.ErrorLevel, e.Error())
	c.submit(&EventReportError{Error: e})
}
