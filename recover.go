package acp

import (
	"fmt"
	"runtime/debug"

	"github.com/sirupsen/logrus"
)

// wrap runs one pipeline goroutine under the fatal-panic safety net. A panic that escapes
// pipeline code means the stage can no longer keep its promise, so the panic becomes the run's
// error and ends the pipeline: a stage that stopped draining a channel would otherwise leave a
// blocked handoff behind, and Wait and WaitErr must never report success after a worker died.
// The panic value itself is kept, so a panicking error stays identifiable through errors.Is.
func (c *StreamCopyer) wrap(f func()) {
	defer func() {
		e := recover()
		if e == nil {
			return
		}

		c.fatalPanic("pipeline worker", e)
	}()

	f()
}

// recoveredPanic is a panic converted into an error. Its message names where it happened and its
// value. A panic value that is an error stays identifiable through errors.Is. The stack is logged
// once where the panic is recovered, so wrapping the error cannot lose it or repeat it.
type recoveredPanic struct {
	what  string
	value any
}

func (p *recoveredPanic) Error() string {
	if err, ok := p.value.(error); ok {
		return fmt.Sprintf("%s panicked, %v", p.what, err)
	}
	return fmt.Sprintf("%s panicked, value= %v", p.what, p.value)
}

func (p *recoveredPanic) Unwrap() error {
	err, _ := p.value.(error)
	return err
}

// logPanic logs a recovered panic with its stack through the run's logger. It must be called from
// the deferred function that recovered, so the stack still shows the panic.
func (c *StreamCopyer) logPanic(err *recoveredPanic) {
	c.logf(logrus.ErrorLevel, "%v\n%s", err, debug.Stack())
}

// fatalPanic turns a panic that escaped pipeline code into the run's error and ends the pipeline.
// The error is recorded before the panic is logged, so a waiter released by a deferred Done
// already sees it.
func (c *StreamCopyer) fatalPanic(what string, value any) error {
	err := &recoveredPanic{what: what, value: value}
	c.setError(err)
	c.stopHard()
	c.logPanic(err)
	return err
}

// protectCall runs one call into caller-implemented code: the results callback, an event handler,
// a content endpoint, an encoder or decoder, or an Item method. A panic becomes an error instead
// of unwinding a pipeline goroutine, because an unwound goroutine keeps publishing on channels it
// no longer owns and leaves the caller without a terminal outcome.
func (c *StreamCopyer) protectCall(what string, f func()) (err error) {
	defer func() {
		e := recover()
		if e == nil {
			return
		}

		panicErr := &recoveredPanic{what: what, value: e}
		c.logPanic(panicErr)
		err = panicErr
	}()

	f()
	return nil
}
