package acp

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/schollz/progressbar/v3"
)

const (
	barUpdateInterval = time.Nanosecond * 255002861
)

func (c *Copyer) startProgressBar(ctx context.Context) {
	var lock sync.Mutex

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

	c.updateProgressBar = func(f func(bar *progressbar.ProgressBar)) {
		lock.Lock()
		defer lock.Unlock()

		if progressBar == nil {
			return
		}
		f(progressBar)
	}

	go func() {
		ticker := time.NewTicker(barUpdateInterval) // around 255ms, avoid conflict with progress bar fresh by second
		defer ticker.Stop()

		var lastCopyedBytes int64
		for range ticker.C {
			switch atomic.LoadInt64(&c.stage) {
			case StageIndex:
				go c.updateProgressBar(func(bar *progressbar.ProgressBar) {
					bar.ChangeMax64(atomic.LoadInt64(&c.totalBytes))
					bar.Describe(fmt.Sprintf("[0/%d] indexing...", atomic.LoadInt64(&c.totalFiles)))
				})
			case StageCopy:
				currentCopyedBytes := atomic.LoadInt64(&c.copyedBytes)
				diff := currentCopyedBytes - lastCopyedBytes
				lastCopyedBytes = currentCopyedBytes

				go c.updateProgressBar(func(bar *progressbar.ProgressBar) {
					bar.Add64(diff)
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
				time.Sleep(barUpdateInterval)
				c.updateProgressBar(func(bar *progressbar.ProgressBar) {
					bar.Close()
					progressBar = nil
				})
				return
			default:
			}
		}
	}()
}
