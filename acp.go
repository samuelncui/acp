package acp

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/sirupsen/logrus"
)

var (
	// errStreamClosed reports a submission after Close: the feed has ended for this run.
	errStreamClosed = errors.New("stream copyer is closed, it accepts no more items")
	// errStreamStopped reports a submission after a fatal pipeline failure ended the run.
	errStreamStopped = errors.New("stream copyer stopped by a pipeline failure")
)

// StreamCopyer is the push engine. The caller submits items, ACP reports every accepted item
// through one results callback, and Wait returns the run's terminal error.
//
// A run has exactly one cancellation checkpoint: Submit consults the context, the results
// callback error and an exhausted linear target before it accepts a batch. Every later stage
// runs on a context that never cancels, so it only drains and forwards what it holds; a stage
// that pulls an item it can no longer start marks it with the stopping reason, and the results
// stage is the only goroutine that invokes the caller's callback. A graceful stop therefore
// gives every accepted item exactly one result.
type StreamCopyer struct {
	*option
	ctx context.Context

	// readCh is the read buffer: Submit fills it, preparation drains it.
	readCh chan *baseJob

	// resultCh is the result buffer: the reporting stage fills it, the delivery stage drains it.
	resultCh chan Result
	// resultsDone is closed once the delivery stage returned, so the pipeline can order its own
	// shutdown after the last delivery.
	resultsDone chan struct{}

	// feedLock serializes Submit against Close, so a submit can never race a closed channel.
	feedLock sync.Mutex
	closed   bool

	running           sync.WaitGroup
	errLock           sync.Mutex
	err               error
	stopLock          sync.Mutex
	callbackStopErr   error
	getDevice         func(in string) (string, error)
	getDiskUsageCache func(mountPoint string) *diskUsageCache
	linearTargetEnded uint32
	signatures        *signatureCache

	// order numbers submitted items; a single submitter keeps it race-free.
	order uint64

	// countBytes and countFiles are the indexed totals of the feed, which is what a progress
	// consumer sizes its display from. Submit writes them while it feeds and the pipeline reads
	// them for its final event, and a caller is allowed to Close from another goroutine, so they
	// are atomic.
	countBytes atomic.Int64
	countFiles atomic.Int64

	// onResults receives one batch of results at a time, from one goroutine.
	onResults func([]Result) error

	// hardStop ends the pipeline without per-item accounting, for fatal failures.
	hardStop     chan struct{}
	hardStopOnce sync.Once

	closeOnce sync.Once
	closeErr  error

	eventCh chan Event
}

// NewStream builds a push engine. It returns creation and validation errors: a nil results
// callback, an invalid option combination, or a job option that only the New shell can honour.
//
// onResults is called from one goroutine, never concurrently. An error it returns is handled as
// a cancellation: already accepted items finish and are delivered normally, items still inside
// the read pipeline are delivered as failures with that error, the batch in hand is still
// submitted, the next Submit returns an error, and Wait returns it. A result that carries any
// error is delivered immediately as its own batch; results without an error are buffered until
// the delivery reaches WithResultBatch results, the result flush interval elapses, or Close
// flushes them.
func NewStream(ctx context.Context, onResults func([]Result) error, opts ...Option) (*StreamCopyer, error) {
	if onResults == nil {
		return nil, fmt.Errorf("new stream failed, results callback is nil")
	}

	opt, err := buildOption(opts...)
	if err != nil {
		return nil, err
	}
	if len(opt.wildcardJobs) > 0 || len(opt.accurateJobs) > 0 {
		return nil, fmt.Errorf("new stream failed, job options describe the compatibility shell, use New")
	}

	return newStream(ctx, onResults, opt)
}

// buildOption applies every option in order, so a repeated option is last-wins.
func buildOption(opts ...Option) (*option, error) {
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

	return opt, nil
}

// newStream starts the pipeline of an already validated option set.
func newStream(ctx context.Context, onResults func([]Result) error, opt *option) (*StreamCopyer, error) {
	getDevice, err := getMountpointCache()
	if err != nil {
		return nil, err
	}

	c := &StreamCopyer{
		option:      opt,
		ctx:         ctx,
		readCh:      make(chan *baseJob, opt.readBuffer),
		resultCh:    make(chan Result, opt.resultBuffer),
		resultsDone: make(chan struct{}),
		onResults:   onResults,
		eventCh:     make(chan Event, 128),
		hardStop:    make(chan struct{}),
		getDevice:   getDevice,
		getDiskUsageCache: Cache(func(mountPoint string) *diskUsageCache {
			return newDiskUsageCache(mountPoint, defaultDiskUsageFreshInterval)
		}),
	}
	if opt.hashPolicy.usesCache() {
		c.signatures = newSignatureCache()
	}

	// Account for the pipeline, event dispatch and result delivery before any of them starts.
	c.running.Add(3)
	go c.wrap(ctx, func() { defer c.running.Done(); c.eventLoop() })
	go c.wrap(ctx, func() { defer c.running.Done(); c.run(ctx) })
	go c.wrap(ctx, func() { defer c.running.Done(); defer close(c.resultsDone); c.deliver() })

	return c, nil
}

// Submit accepts items. It blocks while the read buffer is full, so a caller that produces work
// faster than the pipeline reads it is slowed down instead of growing memory, and it returns an
// error once the run accepts no more work: after Close, after the context ended, after the
// results callback returned an error, or after a fatal pipeline failure.
//
// A single submitter is required, and Items must be submitted before Close. A nil item is a
// submission error and ends the run.
func (c *StreamCopyer) Submit(items ...Item) error {
	if err := c.feedStop(); err != nil {
		// An exhausted linear target stays an item outcome: the items that reached it carry
		// the error, and the run itself is not a failure.
		if !errors.Is(err, ErrTargetNoSpace) {
			c.setError(err)
		}
		return err
	}

	for _, item := range items {
		if item == nil {
			err := fmt.Errorf("submit failed, item is nil")
			c.setError(err)
			return err
		}

		// Orders are zero-based: the linear reorder buffer forwards from request zero.
		job := c.buildJob(item, c.order)
		c.order++
		if job.itemError == nil && job.stat != nil {
			c.countFiles.Add(1)
			c.countBytes.Add(job.stat.size)
		}
		if err := c.push(job); err != nil {
			c.setError(err)
			return err
		}
	}
	c.submit(&EventUpdateCount{Bytes: c.countBytes.Load(), Files: c.countFiles.Load()})

	return nil
}

// Close ends the feed and drains the run: every item already submitted finishes, its cache entry
// is published before its descriptor closes, the remaining results are flushed, and the run's
// cache summary is reported. It returns the flush error, and it is safe to call more than once.
func (c *StreamCopyer) Close() error {
	c.closeOnce.Do(func() {
		c.feedLock.Lock()
		if !c.closed {
			c.closed = true
			close(c.readCh)
		}
		c.feedLock.Unlock()

		c.running.Wait()
	})

	return c.closeErr
}

// Wait drains the run and returns its terminal error: a pipeline failure, a submission error, an
// error the results callback returned, or a flush failure. It is the authoritative outlet for
// run-time errors, and a caller that stopped the run gracefully reads the stopping error here.
func (c *StreamCopyer) Wait() error {
	_ = c.Close()

	c.errLock.Lock()
	defer c.errLock.Unlock()
	return c.err
}

// push hands one job to the read buffer. It holds the feed lock, so Close cannot close the
// channel between the check and the send.
func (c *StreamCopyer) push(job *baseJob) error {
	c.feedLock.Lock()
	defer c.feedLock.Unlock()

	if c.closed {
		return errStreamClosed
	}

	select {
	case c.readCh <- job:
		return nil
	case <-c.hardStop:
		return errStreamStopped
	}
}

// stopHard ends the pipeline immediately. Per-item accounting is not promised afterwards.
func (c *StreamCopyer) stopHard() {
	c.hardStopOnce.Do(func() {
		close(c.hardStop)
	})
}

// setCallbackStop records a results-callback error as the run's soft stop: the feed stops, and
// items that have not started are reported with this error.
func (c *StreamCopyer) setCallbackStop(err error) {
	c.stopLock.Lock()
	defer c.stopLock.Unlock()

	if c.callbackStopErr == nil {
		c.callbackStopErr = err
	}
}

// callbackStop reports the error the results callback returned, or nil.
func (c *StreamCopyer) callbackStop() error {
	c.stopLock.Lock()
	defer c.stopLock.Unlock()
	return c.callbackStopErr
}

// stopReason reports why the pipeline stopped taking work, or nil while it still takes items.
// The results callback, the caller's context and the exhausted linear target are the whole stop
// condition: the item feed checks it to end the feed, and a consuming stage checks it only to
// mark an item it can no longer start, which is how an accepted item keeps its single result.
func (c *StreamCopyer) stopReason(ctx context.Context) error {
	if err := c.callbackStop(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.linearTargetStopped() {
		return ErrTargetNoSpace
	}
	return nil
}

// feedStop is stopReason plus the closed state, and it is what Submit checks before accepting a
// batch. A closed copyer still drains the items it accepted, so Close itself is not a stopping
// reason for an item that is already in the pipeline.
func (c *StreamCopyer) feedStop() error {
	if err := c.stopReason(c.ctx); err != nil {
		return err
	}
	c.feedLock.Lock()
	defer c.feedLock.Unlock()
	if c.closed {
		return errStreamClosed
	}
	return nil
}

// markUnstarted marks an item the pipeline stopped before starting, so the results stage reports
// it instead of dropping it. It reports whether the item must not be started.
func (c *StreamCopyer) markUnstarted(ctx context.Context, job *baseJob) bool {
	if job.itemError != nil {
		return true
	}
	reason := c.stopReason(ctx)
	if reason == nil {
		return false
	}
	job.itemError = reason

	// An abandoned item is a run-level outcome too: an exhausted linear target is the one
	// stopping reason that stays an item outcome only, because the target already reports it.
	if !errors.Is(reason, ErrTargetNoSpace) {
		c.setError(reason)
	}
	return true
}

// publish hands one finished job to the results stage. A hard stop drops it, because a fatal
// failure promises no per-item accounting.
func (c *StreamCopyer) publish(ch chan<- *baseJob, job *baseJob) {
	select {
	case ch <- job:
	case <-c.hardStop:
	}
}

// publishResult hands one finished result to the result buffer, which blocks while the delivery
// stage is behind. A hard stop drops it, because a fatal failure promises no per-item accounting.
func (c *StreamCopyer) publishResult(result Result) {
	select {
	case c.resultCh <- result:
	case <-c.hardStop:
	}
}

// recordClose keeps the first flush error of the run, so Close can report it and Wait returns it.
func (c *StreamCopyer) recordClose(err error) {
	if err == nil {
		return
	}

	c.errLock.Lock()
	defer c.errLock.Unlock()
	if c.closeErr == nil {
		c.closeErr = err
	}
}

func (c *StreamCopyer) setError(err error) {
	if err == nil {
		return
	}
	c.errLock.Lock()
	defer c.errLock.Unlock()
	if c.err == nil {
		c.err = err
	}
}

func (c *StreamCopyer) endLinearTarget(err error) {
	if !c.toDevice.linear || !checkErrorAbort(err) {
		return
	}
	atomic.StoreUint32(&c.linearTargetEnded, 1)
}

func (c *StreamCopyer) linearTargetStopped() bool {
	return atomic.LoadUint32(&c.linearTargetEnded) != 0
}

// run owns the pipeline. It drains in dependency order, so a hard stop leaves jobs behind and
// closes every stage that is still connected. It closes the result buffer on every path,
// including a fatal panic, and waits for the delivery stage before it closes the event channel:
// the caller has seen every result by the time EventFinished is delivered. The intermediate
// finished flags describe their own counter, not the results, so they may arrive before the
// final flush.
func (c *StreamCopyer) run(ctx context.Context) error {
	defer close(c.eventCh)
	defer c.finishSignatureCache()
	defer func() {
		close(c.resultCh)
		<-c.resultsDone
	}()

	// The feed is Submit: preparation consumes the read buffer directly.
	prepared := c.prepare(ctx, c.readCh)
	copyed := c.copy(ctx, prepared)
	c.report(copyed)

	for range c.readCh {
	}
	for job := range prepared {
		job.finishSource()
	}
	for range copyed {
	}

	// The feed ended when the read buffer closed, so the indexed totals are final.
	c.submit(&EventUpdateCount{Bytes: c.countBytes.Load(), Files: c.countFiles.Load(), Finished: true})

	return nil
}

func (c *StreamCopyer) eventLoop() {
	chans := make([]chan Event, len(c.eventHandlers))
	for idx := range chans {
		chans[idx] = make(chan Event, 128)
	}

	var handlers sync.WaitGroup
	for idx, ch := range chans {
		handler := c.eventHandlers[idx]
		events := ch

		handlers.Add(1)
		go c.wrap(c.ctx, func() {
			defer handlers.Done()

			for {
				e, ok := <-events
				if !ok {
					c.deliverEvent(handler, &EventFinished{})
					return
				}
				c.deliverEvent(handler, e)
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

func (c *StreamCopyer) logf(l logrus.Level, format string, args ...any) {
	c.logger.Logf(l, format, args...)
}

// submit hands one event to the dispatch stage. A hard stop drops it: an event describes work in
// progress, and a fatal failure gives no promise that a stage still drains its handoff.
func (c *StreamCopyer) submit(e Event) {
	select {
	case c.eventCh <- e:
	case <-c.hardStop:
	}
}

func (c *StreamCopyer) reportError(src, dst string, err error) {
	e := &Error{Src: src, Dst: dst, Err: err}
	c.setError(fmt.Errorf("copy failed, source=%q target=%q, %w", src, dst, err))
	c.logf(logrus.ErrorLevel, "%s", e.Error())
	c.submit(&EventReportError{Error: e})
}

// deliverEvent hands one event to a caller-owned handler. A panic in the handler becomes a
// pipeline error instead of unwinding the dispatch goroutine, which owns channel closing.
func (c *StreamCopyer) deliverEvent(handler EventHandler, e Event) {
	if err := protectCall("EventHandler", func() { handler(e) }); err != nil {
		c.setError(fmt.Errorf("event handler failed, %w", err))
	}
}
