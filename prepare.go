package acp

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/abc950309/acp/mmap"
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

					job.setStatus(jobStatusPreparing)

					file, err := func(path string) (io.ReadCloser, error) {
						if c.fromDevice.linear {
							file, err := os.Open(path)
							if err != nil {
								return nil, fmt.Errorf("open src file fail, %w", err)
							}

							fileInfo, err := file.Stat()
							if err != nil {
								return nil, fmt.Errorf("get src file stat fail, %w", err)
							}
							if fileInfo.Size() == 0 {
								return nil, fmt.Errorf("get src file, size is zero")
							}

							return file, nil
						}

						readerAt, err := mmap.Open(path)
						if err != nil {
							return nil, fmt.Errorf("open src file by mmap fail, %w", err)
						}
						if readerAt.Len() == 0 {
							return nil, fmt.Errorf("get src file by mmap, size is zero")
						}

						return mmap.NewReader(readerAt), nil
					}(job.path)
					if err != nil {
						c.reportError(job.path, "", err)
					}

					wj := newWriteJob(job, file, c.fromDevice.linear)
					ch <- wj
					wj.wait()
				}
			}
		})
	}

	return ch
}
