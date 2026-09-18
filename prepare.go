package acp

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/samuelncui/acp/mmap"
)

type prepareResult struct {
	order   uint64
	job     *writeJob
	release chan struct{}
}

func (c *Copyer) prepare(ctx context.Context, indexed <-chan *baseJob) <-chan *writeJob {
	// Disable read-ahead when a linear source must wait for each consumer.
	chanLen := 32
	if c.fromDevice.linear {
		chanLen = 0
	}

	// Collect every preparation outcome so skipped jobs can still advance linear ordering.
	completed := make(chan prepareResult, chanLen)
	prepared := make(chan *writeJob, chanLen)
	var workers sync.WaitGroup

	// Prepare source readers with the configured source-device concurrency.
	for idx := 0; idx < c.fromDevice.threads; idx++ {
		workers.Add(1)
		go wrap(ctx, func() {
			defer workers.Done()

			// Consume indexed jobs until source exhaustion. A stopped pipeline drains
			// its input without reading it, so no accepted item is dropped.
			for {
				job, ok := <-indexed
				if !ok {
					return
				}

				// An item that was already rejected while indexing needs no source.
				if job.itemError != nil {
					if !c.abandonUnprepared(ctx, completed, job, job.itemError) {
						return
					}
					continue
				}
				if c.stopped(ctx) {
					if !c.abandonUnprepared(ctx, completed, job, c.abandonment(ctx)) {
						return
					}
					continue
				}

				result := prepareResult{order: job.order}
				if c.toDevice.linear {
					result.release = make(chan struct{})
				}

				// Enter preparation. A stored hash is read whenever the policy uses the
				// cache and is reused only where the policy allows it.
				job.setStatus(jobStatusPreparing)
				var file io.ReadCloser
				var size int64
				reuseEligible := len(job.targets) == 0 && c.hashPolicy.reusesCache()
				if c.signatures != nil && reuseEligible {
					if hash, ok := c.signatures.lookup(job.path, job.stat); ok {
						job.setCachedHash(hash)
						file = io.NopCloser(bytes.NewReader(nil))
						size = job.stat.size
					}
				}

				// A policy that never reads content completes without opening the source.
				if file == nil && len(job.targets) == 0 && !c.hashPolicy.readsContent() {
					// No stored hash was reused here, so the item has no hash and is not
					// reported as a cache hit.
					job.setHash(nil)
					wj := newWriteJob(job, nil, 0, c.fromDevice.linear)
					wj.skipContent = true
					result.job = wj
					if !c.sendPrepareResult(completed, result) {
						return
					}
					if !wj.waitConsumed(ctx) {
						return
					}
					continue
				}

				// Cache misses and every transfer open the real source content.
				if file == nil {
					var err error
					file, size, err = func(path string) (io.ReadCloser, int64, error) {
						// Only a caller that asks for it reads through a mapping, with an
						// empty-file fallback.
						if c.fromDevice.readMode == ReadMapped {
							readerAt, err := mmap.Open(path)
							if err != nil {
								return nil, 0, fmt.Errorf("open src file by mmap fail, %w", err)
							}
							if readerAt.Len() == 0 {
								if err := readerAt.Close(); err != nil {
									return nil, 0, fmt.Errorf("close empty src file by mmap fail, %w", err)
								}
								return io.NopCloser(bytes.NewReader(nil)), 0, nil
							}

							return mmap.NewReader(readerAt), int64(readerAt.Len()), nil
						}

						// Every other source is read buffered, without updating its access time.
						file, err := openSource(path)
						if err != nil {
							return nil, 0, fmt.Errorf("open src file fail, %w", err)
						}
						fileInfo, err := file.Stat()
						if err != nil {
							_ = file.Close()
							return nil, 0, fmt.Errorf("get src file stat fail, %w", err)
						}
						return file, fileInfo.Size(), nil
					}(job.path)
					if err != nil {
						c.reportItemError(job.path, "", err)
						if !c.abandonUnprepared(ctx, completed, job, err) {
							return
						}
						continue
					}
				}

				// Transfer the prepared reader to the ordering stage before waiting on a linear source.
				wj := newWriteJob(job, file, size, c.fromDevice.linear)
				result.job = wj
				if !c.sendPrepareResult(completed, result) {
					return
				}
				if !wj.waitConsumed(ctx) {
					return
				}
			}
		})
	}

	// Close preparation outcomes only after every source worker relinquishes ownership.
	go wrap(ctx, func() {
		workers.Wait()
		close(completed)
	})

	// Preserve request order only where one linear writer requires it.
	go wrap(ctx, func() {
		defer close(prepared)
		c.forwardPrepared(ctx, completed, prepared)
	})
	return prepared
}

// abandonUnprepared reports an accepted item that preparation will never read and keeps
// request ordering intact by publishing an empty preparation result for it.
func (c *Copyer) abandonUnprepared(
	ctx context.Context,
	completed chan<- prepareResult,
	job *baseJob,
	reason error,
) bool {
	if !c.abandon(job, reason) {
		return false
	}

	result := prepareResult{order: job.order}
	if c.toDevice.linear {
		result.release = make(chan struct{})
	}
	return c.sendPrepareResult(completed, result)
}

func (c *Copyer) sendPrepareResult(completed chan<- prepareResult, result prepareResult) bool {
	select {
	case completed <- result:
	case <-c.hardStop:
		result.finish()
		return false
	}
	if result.release == nil {
		return true
	}
	select {
	case <-result.release:
		return true
	case <-c.hardStop:
		return false
	}
}

func (result prepareResult) releaseWorker() {
	if result.release != nil {
		close(result.release)
	}
}

func (result prepareResult) finish() {
	if result.job != nil {
		result.job.finishSource()
	}
	result.releaseWorker()
}

// forwardPrepared hands prepared readers to the copy stage. A graceful stop keeps
// forwarding so the copy stage can report the items it holds.
func (c *Copyer) forwardPrepared(
	ctx context.Context,
	completed <-chan prepareResult,
	prepared chan<- *writeJob,
) {
	// Close readers that remain owned by this stage when a hard stop ends forwarding.
	defer func() {
		for result := range completed {
			result.finish()
		}
	}()

	// Random targets retain completion-order concurrency.
	if !c.toDevice.linear {
		for result := range completed {
			if result.job == nil {
				continue
			}
			select {
			case prepared <- result.job:
			case <-c.hardStop:
				result.finish()
				return
			}
		}
		return
	}

	// A bounded reorder buffer feeds the linear writer in original request order.
	next := uint64(0)
	pending := make(map[uint64]prepareResult, c.fromDevice.threads)
	defer func() {
		for _, result := range pending {
			result.finish()
		}
	}()
	for {
		select {
		case <-c.hardStop:
			return
		case result, ok := <-completed:
			if !ok {
				if len(pending) != 0 {
					c.setError(fmt.Errorf("ordered preparation ended before request %d", next))
				}
				return
			}
			pending[result.order] = result
		}

		for {
			result, ok := pending[next]
			if !ok {
				break
			}
			delete(pending, next)
			next++
			if result.job == nil {
				result.releaseWorker()
				continue
			}
			select {
			case prepared <- result.job:
				result.releaseWorker()
			case <-c.hardStop:
				result.finish()
				return
			}
		}
	}
}
