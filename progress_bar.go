package acp

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/schollz/progressbar/v3"
)

func (c *Copyer) startProgressBar(ctx context.Context) {
	progressBar := progressbar.DefaultBytes(0, "[0/0] indexing...")
	c.updateProgressBar = func(f func(bar *progressbar.ProgressBar)) {
		c.progressBarLock.Lock()
		defer c.progressBarLock.Unlock()

		if progressBar == nil {
			return
		}
		f(progressBar)
	}

	go func() {
		ticker := time.NewTicker(time.Nanosecond * 255002861) // around 255ms, avoid conflict with progress bar fresh by second
		defer ticker.Stop()

		var lastCopyedBytes int64
		for range ticker.C {
			c.progressBarLock.Lock()

			switch c.stage {
			case StageIndex:
				go c.updateProgressBar(func(bar *progressbar.ProgressBar) {
					bar.ChangeMax64(atomic.LoadInt64(&c.totalBytes))
					bar.Describe(fmt.Sprintf("[0/%d] indexing...", atomic.LoadInt64(&c.totalFiles)))
				})
			case StageCopy:
				currentCopyedBytes := atomic.LoadInt64(&c.copyedBytes)
				diff := int(currentCopyedBytes - lastCopyedBytes)
				lastCopyedBytes = currentCopyedBytes

				go c.updateProgressBar(func(bar *progressbar.ProgressBar) {
					bar.Add(diff)
				})
			case StageFinished:
				currentCopyedBytes := atomic.LoadInt64(&c.copyedBytes)
				diff := int(currentCopyedBytes - lastCopyedBytes)
				lastCopyedBytes = currentCopyedBytes

				copyedFiles, totalFiles := atomic.LoadInt64(&c.copyedFiles), atomic.LoadInt64(&c.totalFiles)

				c.updateProgressBar(func(bar *progressbar.ProgressBar) {
					bar.Add(diff)
					bar.Describe(fmt.Sprintf("[%d/%d] finished!", copyedFiles, totalFiles))
				})
			}

			select {
			case <-ctx.Done():
				c.updateProgressBar(func(bar *progressbar.ProgressBar) {
					bar.Close()
					progressBar = nil
				})
				return
			default:
			}

			c.progressBarLock.Unlock()
		}
	}()
}
