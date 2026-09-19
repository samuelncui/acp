package acp

import (
	"context"
	"fmt"
	"runtime/debug"

	"github.com/sirupsen/logrus"
)

// wrap runs one pipeline goroutine under the fatal-panic safety net. A panic that escapes
// pipeline code means the stage can no longer keep its promise, so the panic becomes the run's
// error and ends the pipeline: a stage that stopped draining a channel would otherwise leave a
// blocked handoff behind, and Wait and WaitErr must never report success after a worker died.
// The panic value itself is kept, so a panicking error stays identifiable through errors.Is.
func (c *StreamCopyer) wrap(ctx context.Context, f func()) {
	defer func() {
		e := recover()
		if e == nil {
			return
		}

		err := panicError("pipeline worker", e)
		c.setError(err)
		c.stopHard()
		logrus.WithContext(ctx).WithError(err).Errorf("panic: %s", debug.Stack())
	}()

	f()
}

// panicError converts a recovered panic value into an error without losing it.
func panicError(what string, value any) error {
	if err, ok := value.(error); ok {
		return fmt.Errorf("%s panicked, %w", what, err)
	}
	return fmt.Errorf("%s panicked, value= %v", what, value)
}

// protectCall runs one call into caller-implemented code: the results callback, an event handler,
// or Item.Source/Item.Targets. A panic becomes an error instead of unwinding a pipeline goroutine,
// because an unwound goroutine keeps publishing on channels it no longer owns and leaves the caller
// without a terminal outcome.
func protectCall(what string, f func()) (err error) {
	defer func() {
		e := recover()
		if e == nil {
			return
		}

		err = panicError(what, e)
		logrus.WithField("stack", string(debug.Stack())).Error(err)
	}()

	f()
	return nil
}
