package acp

import (
	"context"
	"fmt"
)

func (c *Copyer) cleanupJob(ctx context.Context, copyed <-chan *baseJob) {
	for {
		select {
		case job, ok := <-copyed:
			if !ok {
				return
			}

			for _, dst := range job.successTargets {
				if err := writeSysStat(dst, job.stat); err != nil {
					c.reportError(job.path, dst, fmt.Errorf("change info, write sys stat fail, %w", err))
				}
			}

			job.setStatus(jobStatusFinished)
		case <-ctx.Done():
			return
		}
	}
}
