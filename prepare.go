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
	chanLen := 32
	if c.fromDevice.linear {
		chanLen = 0
	}

	var wg sync.WaitGroup
	ch := make(chan *writeJob, chanLen)
	defer func() {
		go wrap(ctx, func() {
			defer close(ch)
			wg.Wait()
		})
	}()

	for idx := 0; idx < c.fromDevice.threads; idx++ {
		wg.Add(1)
		go wrap(ctx, func() {
			defer wg.Done()

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

					job.setStatus(jobStatusPreparing)

					file, size, err := func(path string) (io.ReadCloser, int64, error) {
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
