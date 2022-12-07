package acp

import (
	"context"
	"fmt"
	"os"
)

func (c *Copyer) cleanupJob(ctx context.Context, copyed <-chan *baseJob) {
	for {
		select {
		case job, ok := <-copyed:
			if !ok {
				return
			}
			for _, name := range job.successTargets {
				if err := os.Chtimes(name, job.modTime, job.modTime); err != nil {
					c.reportError(job.source.src(), name, fmt.Errorf("change info, chtimes fail, %w", err))
				}
			}

			job.setStatus(jobStatusFinished)
		case <-ctx.Done():
			return
		}
	}
}
