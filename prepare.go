package acp

import (
	"context"
	"fmt"
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

					file, err := mmap.Open(job.source.src())
					if err != nil {
						c.reportError(job.source.src(), "", fmt.Errorf("open src file fail, %w", err))
						return
					}

					wj := &writeJob{baseJob: job, src: file, ch: make(chan struct{})}
					ch <- wj
					<-wj.ch
				}
			}
		})
	}

	return ch
}
