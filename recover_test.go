package acp

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestWrapRecordsAFatalPanic pins the fatal-panic safety net: a panicking pipeline worker must
// leave the run with an error instead of a silent success, must end the pipeline so a blocked
// handoff cannot hang it, and must keep the panic value identifiable.
func TestWrapRecordsAFatalPanic(t *testing.T) {
	t.Run("string value", func(t *testing.T) {
		captureLogs(t)
		copyer := newTestStream(t)

		copyer.wrap(context.Background(), func() { panic("worker fixture") })

		err := copyer.Wait()
		if err == nil {
			t.Fatal("WaitErr() = nil, want the panic reported")
		}
		if !strings.Contains(err.Error(), "worker fixture") {
			t.Fatalf("WaitErr() = %v, want the panic value", err)
		}
	})

	t.Run("error value keeps its identity", func(t *testing.T) {
		captureLogs(t)
		copyer := newTestStream(t)
		panicErr := errors.New("worker fixture")

		copyer.wrap(context.Background(), func() { panic(panicErr) })

		if err := copyer.Wait(); !errors.Is(err, panicErr) {
			t.Fatalf("WaitErr() = %v, want %v", err, panicErr)
		}
	})

	t.Run("first error wins", func(t *testing.T) {
		captureLogs(t)
		copyer := newTestStream(t)
		copyer.setError(errors.New("earlier failure"))

		copyer.wrap(context.Background(), func() { panic("worker fixture") })

		if err := copyer.Wait(); err == nil || err.Error() != "earlier failure" {
			t.Fatalf("WaitErr() = %v, want the first failure", err)
		}
	})
}
