package acp

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"github.com/schollz/progressbar/v3"
)

const (
	barUpdateInterval = time.Nanosecond * 255002861
)

func (c *Copyer) startProgressBar(ctx context.Context) {
	// progressBar := progressbar.DefaultBytes(1, "[0/0] indexing...")
	progressBar := progressbar.NewOptions64(
		1,
		progressbar.OptionSetDescription("[0/0] indexing..."),
		progressbar.OptionSetWriter(os.Stderr),
		progressbar.OptionShowBytes(true),
		progressbar.OptionSetWidth(10),
		progressbar.OptionThrottle(65*time.Millisecond),
		progressbar.OptionShowCount(),
		progressbar.OptionOnCompletion(func() {}),
		progressbar.OptionSpinnerType(14),
		progressbar.OptionFullWidth(),
		progressbar.OptionSetRenderBlankState(true),
	)

	ch := make(chan func(bar *progressbar.ProgressBar), 8)
	c.updateProgressBar = func(f func(bar *progressbar.ProgressBar)) {
		ch <- f
	}

	go func() {
		for f := range ch {
			if progressBar == nil {
				continue
			}
			f(progressBar)
		}
	}()

	go func() {
		defer close(ch)

		ticker := time.NewTicker(barUpdateInterval) // around 255ms, avoid conflict with progress bar fresh by second
		defer ticker.Stop()

		var lastCopyedBytes int64
		for {
			select {
			case <-ticker.C:
				switch atomic.LoadInt64(&c.stage) {
				case StageCopy:
					currentCopyedBytes := atomic.LoadInt64(&c.copyedBytes)
					diff := currentCopyedBytes - lastCopyedBytes
					lastCopyedBytes = currentCopyedBytes

					c.updateProgressBar(func(bar *progressbar.ProgressBar) {
						bar.Add64(diff)
					})
				}
			case <-ctx.Done():
				currentCopyedBytes := atomic.LoadInt64(&c.copyedBytes)
				diff := int(currentCopyedBytes - lastCopyedBytes)
				lastCopyedBytes = currentCopyedBytes

				copyedFiles, totalFiles := atomic.LoadInt64(&c.copyedFiles), atomic.LoadInt64(&c.totalFiles)
				c.updateProgressBar(func(bar *progressbar.ProgressBar) {
					bar.Add(diff)
					bar.Describe(fmt.Sprintf("[%d/%d] finished!", copyedFiles, totalFiles))
				})
				return
			}
		}
	}()
}
