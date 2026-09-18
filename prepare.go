package acp

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
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

			// Consume indexed jobs until cancellation or source exhaustion.
			for {
				select {
				case <-ctx.Done():
					return
				case job, ok := <-indexed:
					if !ok {
						return
					}
					result := prepareResult{order: job.order}
					if c.toDevice.linear {
						result.release = make(chan struct{})
					}
					if c.linearTargetStopped() {
						if !sendPrepareResult(ctx, completed, result) {
							return
						}
						continue
					}

					// Enter preparation and let eligible targetless jobs reuse a valid cache entry.
					job.setStatus(jobStatusPreparing)
					var file io.ReadCloser
					var size int64
					cacheEligible := c.signatures != nil && !c.forceRehash && len(job.targets) == 0
					if cacheEligible {
						if hash, ok := c.signatures.lookup(job.path, job.stat); ok {
							job.setCachedHash(hash)
							file = io.NopCloser(bytes.NewReader(nil))
							size = job.stat.size
						}
					}

					// Cache misses and every transfer open the real source content.
					if file == nil {
						var err error
						file, size, err = func(path string) (io.ReadCloser, int64, error) {
							// Keep linear sources on ordinary descriptors for ordered reads.
							if c.fromDevice.linear {
								file, err := os.Open(path)
								if err != nil {
									return nil, 0, fmt.Errorf("open src file fail, %w", err)
								}
								fileInfo, err := file.Stat()
								if err != nil {
									_ = file.Close()
									return nil, 0, fmt.Errorf("get src file stat fail, %w", err)
								}
								return file, fileInfo.Size(), nil
							}

							// Use mmap-backed readers for random-access sources, with an empty-file fallback.
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
						}(job.path)
						if err != nil {
							c.reportError(job.path, "", err)
							job.fail("", err)
							job.setStatus(jobStatusFinished)
							if !sendPrepareResult(ctx, completed, result) {
								return
							}
							continue
						}
					}

					// Transfer the prepared reader to the ordering stage before waiting on a linear source.
					wj := newWriteJob(job, file, size, c.fromDevice.linear)
					result.job = wj
					if !sendPrepareResult(ctx, completed, result) {
						return
					}
					if !wj.waitConsumed(ctx) {
						return
					}
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

func sendPrepareResult(ctx context.Context, completed chan<- prepareResult, result prepareResult) bool {
	select {
	case completed <- result:
	case <-ctx.Done():
		result.finish()
		return false
	}
	if result.release == nil {
		return true
	}
	select {
	case <-result.release:
		return true
	case <-ctx.Done():
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

func (c *Copyer) forwardPrepared(
	ctx context.Context,
	completed <-chan prepareResult,
	prepared chan<- *writeJob,
) {
	// Close readers that remain owned by this stage when cancellation stops forwarding.
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
			case <-ctx.Done():
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
		case <-ctx.Done():
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
			case <-ctx.Done():
				result.finish()
				return
			}
		}
	}
}
