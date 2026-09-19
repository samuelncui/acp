package acp

import (
	"errors"
	"fmt"
	"os"
	"time"
)

// resultSink owns the pending batch of one run. Only the delivery goroutine touches it, so it
// needs no lock, and it is the only place that hands results to the caller.
type resultSink struct {
	copyer  *StreamCopyer
	pending []Result
}

// deliver hands one batch to the caller's callback. A panic in the callback becomes the run's
// error instead of unwinding the delivery goroutine, which owns the accounting of every other
// item; an error the callback returns is a cancellation.
func (s *resultSink) deliver(results []Result) {
	if len(results) == 0 {
		return
	}

	copyer := s.copyer
	var callbackErr error
	if panicErr := protectCall("onResults", func() { callbackErr = copyer.onResults(results) }); panicErr != nil {
		// A panicking callback is a fatal failure of the delivery path: it is reported from
		// Wait and from Close, and the remaining items keep their accounting.
		stopErr := fmt.Errorf("results callback failed, %w", panicErr)
		copyer.setError(stopErr)
		copyer.recordClose(stopErr)
		return
	}
	if callbackErr == nil {
		return
	}

	// The callback error stops the feed and is the run's terminal error; items that have not
	// started are reported with it, and items already past the read stage still finish.
	stopErr := fmt.Errorf("results callback failed, %w", callbackErr)
	copyer.setError(stopErr)
	copyer.recordClose(stopErr)
	copyer.setCallbackStop(stopErr)
}

// flush delivers everything the buffer holds and starts a fresh batch, so the slice handed to
// the caller is never reused by the next flush.
func (s *resultSink) flush() {
	if len(s.pending) == 0 {
		return
	}

	batch := s.pending
	s.pending = make([]Result, 0, s.copyer.resultBatch)
	s.deliver(batch)
}

// report finalizes every finished job and hands its result to the result buffer, which the
// delivery stage drains. It runs on the pipeline goroutine that owns the channel, so it needs no
// synchronization, and it is never the goroutine that invokes the caller's callback.
func (c *StreamCopyer) report(copyed <-chan *baseJob) {
	for {
		select {
		case job, ok := <-copyed:
			if !ok {
				return
			}
			c.publishResult(c.finalizeJob(job))
		case <-c.hardStop:
			return
		}
	}
}

// deliver is the only goroutine that invokes the results callback. It turns the finished results
// the pipeline produced into deliveries of at most resultBatch results: a result that carries any
// error is delivered as its own batch as soon as it is read, while results without an error are
// buffered until the batch fills, the flush interval elapses, or the run ended.
//
// A slow callback therefore stalls the result buffer, and a full result buffer is what applies
// backpressure to the pipeline that produces results. The stage ends when the result buffer
// closes, which is how it flushes what it still holds.
func (c *StreamCopyer) deliver() {
	sink := &resultSink{copyer: c, pending: make([]Result, 0, c.resultBatch)}
	defer sink.flush()

	timer := time.NewTimer(c.resultFlushInterval)
	defer timer.Stop()

	for {
		select {
		case result, ok := <-c.resultCh:
			if !ok {
				return
			}

			if result.Err != nil || hasFailedTarget(result) {
				sink.deliver([]Result{result})
				continue
			}

			sink.pending = append(sink.pending, result)
			if len(sink.pending) >= c.resultBatch {
				sink.flush()
			}
		case <-timer.C:
			sink.flush()
			timer.Reset(c.resultFlushInterval)
		}
	}
}

// hasFailedTarget reports whether any requested target of one result failed.
func hasFailedTarget(result Result) bool {
	for _, target := range result.Targets {
		if target.Err != nil {
			return true
		}
	}
	return false
}

// finalizeJob turns one finished job into its result. An item ACP could not process has no data
// and metadata lifecycle to finish, so it is reported as it is.
func (c *StreamCopyer) finalizeJob(job *baseJob) Result {
	if job.itemError == nil {
		c.completeJob(job)
	}
	return job.result()
}

// completeJob finishes one item after restoring its metadata. The content cache is already
// published: the copy stage wrote it through the descriptors that read the source and wrote each
// target, while those descriptors were still open.
func (c *StreamCopyer) completeJob(job *baseJob) {
	// Restore metadata before publishing the final result.
	for _, dst := range append([]string(nil), job.successTargets...) {
		if err := mappingError(writeSysStat(dst, job.stat)); err != nil {
			c.endLinearTarget(err)
			job.fail(dst, fmt.Errorf("change info, write sys stat fail, %w", err))

			// Remove the failed target so the same operation can retry it.
			if err := os.Remove(dst); err != nil && !errors.Is(err, os.ErrNotExist) {
				c.reportError(job.path, dst, fmt.Errorf("delete target after metadata failure failed, %w", err))
			}
		}
	}
}
