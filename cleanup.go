package acp

import (
	"context"
	"fmt"
)

func (c *Copyer) cleanupJob(ctx context.Context, copyed <-chan *baseJob) bool {
	streamSinkFailed := false
	for {
		select {
		case job, ok := <-copyed:
			if !ok {
				return streamSinkFailed
			}

			for _, dst := range job.successTargets {
				if err := writeSysStat(dst, job.stat); err != nil {
					c.reportError(job.path, dst, fmt.Errorf("change info, write sys stat fail, %w", err))
				}
			}

			job.setStatus(jobStatusFinished)
			if c.streamSink != nil && !streamSinkFailed {
				if err := c.streamSink.Write(ctx, &StreamResult{ID: job.streamID, Job: job.report()}); err != nil {
					c.setError(fmt.Errorf("write stream result failed, id=%d, %w", job.streamID, err))
					streamSinkFailed = true
				}
			}
		case <-ctx.Done():
			c.setError(ctx.Err())
			return streamSinkFailed
		}
	}
}
