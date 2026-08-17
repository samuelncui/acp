package acp

import (
	"context"
	"fmt"
)

// StreamRequest describes one exact source and its destinations.
type StreamRequest struct {
	ID      int64
	Source  string
	Targets []string
}

// StreamResult identifies the final report for one streamed request.
type StreamResult struct {
	ID  int64
	Job *Job
}

// StreamSource supplies requests serially. It returns io.EOF when exhausted.
type StreamSource interface {
	Next(context.Context) (*StreamRequest, error)
}

// StreamSink consumes final results serially and flushes after the pipeline drains.
type StreamSink interface {
	Write(context.Context, *StreamResult) error
	Flush(context.Context) error
}

// RunStream copies a bounded stream without retaining a whole-job report.
func RunStream(ctx context.Context, source StreamSource, sink StreamSink, opts ...Option) error {
	if source == nil {
		return fmt.Errorf("run stream failed, source is nil")
	}
	if sink == nil {
		return fmt.Errorf("run stream failed, sink is nil")
	}

	streamOption := func(option *option) *option {
		option.streamSource = source
		option.streamSink = sink
		return option
	}
	copyer, err := New(ctx, append(opts, streamOption)...)
	if err != nil {
		return fmt.Errorf("run stream failed, %w", err)
	}
	if err := copyer.WaitErr(); err != nil {
		return fmt.Errorf("run stream failed, %w", err)
	}
	return nil
}
