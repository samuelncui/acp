package acp

import (
	"context"
	"fmt"
	"io/fs"
	"time"
)

// Item is one unit of work owned by the caller.
//
// The caller implements Item over whatever state it owns, so the item is the handle for
// its own result and no id lookup or payload plumbing is needed.
type Item interface {
	// Source returns the exact path to read.
	Source() string

	// Targets returns the destinations of a copy. An empty slice hashes the source
	// without writing it anywhere.
	Targets() []string

	// Completed reports the item's terminal outcome. ACP calls it exactly once for
	// every accepted item, from a single goroutine, and never waits for I/O on it.
	Completed(*Result)

	// Failed reports an item ACP could not process at all. It also reports items that
	// were accepted but abandoned by a graceful stop, with the stopping error.
	Failed(error)
}

// BatchSource supplies items serially. Next returns io.EOF when the input ends, and
// must be cheap: ACP calls it from the loop that feeds the read buffer.
type BatchSource interface {
	Next(context.Context) ([]Item, error)
}

// Result reports the content facts of one completed item and one outcome per requested
// target. An item whose every target failed is still a completed item, which is how
// "not written" is distinguished from "could not be processed".
type Result struct {
	Source string

	Size      int64
	Mode      fs.FileMode
	ModTime   time.Time
	WriteTime time.Time

	SHA256            []byte
	SignatureCacheHit bool

	// Targets holds one outcome per requested target, in request order.
	Targets []TargetResult
}

// TargetResult is the outcome of one requested target.
type TargetResult struct {
	Path      string
	Size      int64
	WriteTime time.Time

	// Err is nil when the target was written and verified. Target failures keep their
	// error identity, so errors.Is(err, ErrTargetNoSpace) identifies an exhausted
	// medium.
	Err error
}

// Run copies caller-owned items until the batch source ends or the pipeline fails.
//
// A graceful stop (the caller cancels the context) stops feeding items, finishes the
// items already in flight, reports every accepted item through its terminal callback,
// and returns the stopping error.
func Run(ctx context.Context, source BatchSource, opts ...Option) error {
	if source == nil {
		return fmt.Errorf("run failed, batch source is nil")
	}

	copyer, err := New(ctx, append(opts, withBatchSource(source))...)
	if err != nil {
		return fmt.Errorf("run failed, %w", err)
	}
	if err := copyer.WaitErr(); err != nil {
		return fmt.Errorf("run failed, %w", err)
	}

	// The pipeline drained, so a cancelled caller learns why it stopped even when the
	// batch source ended its input instead of reporting the cancelation itself.
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("run stopped, %w", err)
	}
	return nil
}
