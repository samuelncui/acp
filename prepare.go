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

func (c *Copyer) prepare(ctx context.Context, indexed <-chan *baseJob) <-chan *writeJob {
	// Disable read-ahead when a linear source must wait for each consumer.
	chanLen := 32
	if c.fromDevice.linear {
		chanLen = 0
	}

	// Close the prepared stream only after every source worker exits.
	var wg sync.WaitGroup
	ch := make(chan *writeJob, chanLen)
	defer func() {
		go wrap(ctx, func() {
			defer close(ch)
			wg.Wait()
		})
	}()

	// Prepare source readers with the configured source-device concurrency.
	for idx := 0; idx < c.fromDevice.threads; idx++ {
		wg.Add(1)
		go wrap(ctx, func() {
			defer wg.Done()

			// Consume indexed jobs until cancellation or source exhaustion.
			for {
				select {
				case <-ctx.Done():
					return
				case job, ok := <-indexed:
					if !ok {
						return
					}
					if c.linearTargetStopped() {
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
							continue
						}
					}

					// Publish the prepared reader and preserve linear-source consumption order.
					wj := newWriteJob(job, file, size, c.fromDevice.linear)
					select {
					case ch <- wj:
					case <-ctx.Done():
						wj.finishSource()
						return
					}
					if !wj.waitConsumed(ctx) {
						return
					}
				}
			}
		})
	}

	return ch
}
