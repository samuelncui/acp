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
		// Recover a worker's string panic under the stage-level safety net.
		captureLogs(t)
		copyer := newTestStream(t)
		copyer.wrap(context.Background(), func() { panic("worker fixture") })

		// The run must retain the panic value as its terminal error.
		err := copyer.Wait()
		if err == nil {
			t.Fatal("Wait() = nil, want the panic reported")
		}
		if !strings.Contains(err.Error(), "worker fixture") {
			t.Fatalf("Wait() = %v, want the panic value", err)
		}
	})

	t.Run("error value keeps its identity", func(t *testing.T) {
		// Recover an error value without losing the caller's error identity.
		captureLogs(t)
		copyer := newTestStream(t)
		panicErr := errors.New("worker fixture")
		copyer.wrap(context.Background(), func() { panic(panicErr) })

		// Callers must still be able to classify the error with errors.Is.
		if err := copyer.Wait(); !errors.Is(err, panicErr) {
			t.Fatalf("Wait() = %v, want %v", err, panicErr)
		}
	})

	t.Run("first error wins", func(t *testing.T) {
		// A later worker panic must preserve the run's first recorded failure.
		captureLogs(t)
		copyer := newTestStream(t)
		copyer.setError(errors.New("earlier failure"))
		copyer.wrap(context.Background(), func() { panic("worker fixture") })

		// Wait remains the authoritative outlet for the original failure.
		if err := copyer.Wait(); err == nil || err.Error() != "earlier failure" {
			t.Fatalf("Wait() = %v, want the first failure", err)
		}
	})
}
