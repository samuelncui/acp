package acp

import (
	"context"
	"errors"
	"fmt"
	"os"
)

func (c *Copyer) cleanupJob(ctx context.Context, cancel context.CancelFunc, copyed <-chan *baseJob) bool {
	for {
		select {
		case job, ok := <-copyed:
			if !ok {
				return false
			}

			// Restore metadata before publishing the final result.
			for _, dst := range append([]string(nil), job.successTargets...) {
				if err := mappingError(writeSysStat(dst, job.stat)); err != nil {
					c.endLinearTarget(err)
					job.fail(dst, fmt.Errorf("change info, write sys stat fail, %w", err))
					c.reportError(job.path, dst, fmt.Errorf("change info, write sys stat fail, %w", err))

					// Remove the failed target so the same operation can retry it.
					if err := os.Remove(dst); err != nil && !errors.Is(err, os.ErrNotExist) {
						c.reportError(job.path, dst, fmt.Errorf("delete target after metadata failure failed, %w", err))
					}
				}
			}

			// Refresh only signatures backed by a complete source hash.
			shouldRefreshSignature := c.signatures != nil && job.hashValid && !job.cacheHit
			if shouldRefreshSignature {
				signature, err := newCachedSignature(job.hash, job.stat)
				if err != nil {
					c.signatures.recordFailure(job.path, err)
				} else {
					c.signatures.enqueue(job.path, signature)
					for _, dst := range job.successTargets {
						c.signatures.enqueue(dst, signature)
					}
				}
			}

			// Publish only results whose data and metadata lifecycle has finished.
			job.setStatus(jobStatusFinished)
			if c.streamSink == nil {
				continue
			}
			if err := c.streamSink.Write(ctx, &StreamResult{ID: job.streamID, Job: job.report()}); err != nil {
				// Stop upstream writes once final results can no longer be persisted.
				c.setError(fmt.Errorf("write stream result failed, id=%d, %w", job.streamID, err))
				cancel()
				return true
			}
		case <-ctx.Done():
			c.setError(ctx.Err())
			return false
		}
	}
}
