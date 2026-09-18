package acp

import (
	"context"
	"errors"
	"fmt"
	"os"
)

// cleanup reports every accepted item exactly once. It is the only goroutine that
// invokes item callbacks, so callbacks arrive one at a time and a slow callback cannot
// stall the read pipeline.
func (c *Copyer) cleanup(ctx context.Context, copyed <-chan *baseJob) {
	for {
		select {
		case job, ok := <-copyed:
			if !ok {
				return
			}

			// An item with no target outcome to report could not be processed at all, so
			// it fails instead of completing with content facts it never established.
			if job.itemError != nil {
				c.failedJob(job)
				continue
			}
			c.finishJob(job)
		case job := <-c.abandoned:
			c.failedJob(job)
		case <-c.hardStop:
			return
		}
	}
}

// failedJob reports an accepted item that ACP could not process at all.
func (c *Copyer) failedJob(job *baseJob) {
	job.item.Failed(job.itemError)
}

// refreshSignature publishes a computed signature unless the target already stores it.
func (c *Copyer) refreshSignature(path string, signature CachedSignature) {
	stored, ok, err := ReadCachedSignature(path)
	if err == nil && ok && stored == signature {
		return
	}

	c.signatures.enqueue(path, signature)
}

// finishJob publishes one finished job after restoring its metadata and refreshing the
// content cache.
func (c *Copyer) finishJob(job *baseJob) {
	// An item ACP could not process at all is reported as a failure rather than as a
	// completion with failed target outcomes.
	if job.itemError != nil {
		job.setStatus(jobStatusFinished)
		job.item.Failed(job.itemError)
		return
	}

	// Restore metadata before publishing the final result.
	for _, dst := range append([]string(nil), job.successTargets...) {
		if err := mappingError(writeSysStat(dst, job.stat)); err != nil {
			c.endLinearTarget(err)
			job.fail(dst, fmt.Errorf("change info, write sys stat fail, %w", err))

			// Remove the failed target so the same operation can retry it.
			if err := os.Remove(dst); err != nil && !errors.Is(err, os.ErrNotExist) {
				c.reportItemError(job.path, dst, fmt.Errorf("delete target after metadata failure failed, %w", err))
			}
		}
	}

	// Refresh signatures backed by a complete source hash, and only where the stored
	// value differs: an identical rewrite costs a physical xattr block on copy-on-write
	// filesystems and leaves the cache in the same state.
	shouldRefreshSignature := c.signatures != nil && job.hashValid && !job.cacheHit && c.hashPolicy.refreshesCache()
	if shouldRefreshSignature {
		signature, err := newCachedSignature(job.hash, job.stat)
		if err != nil {
			c.signatures.recordFailure(job.path, err)
		} else {
			c.refreshSignature(job.path, signature)
			for _, dst := range job.successTargets {
				c.refreshSignature(dst, signature)
			}
		}
	}

	// Publish only results whose data and metadata lifecycle has finished.
	job.setStatus(jobStatusFinished)
	job.item.Completed(job.result())
}
