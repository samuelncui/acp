package acp

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"
)

func TestLinearSpaceRetainsActualWriteFailure(t *testing.T) {
	// Copy a successful small output before a refusing device, regardless of polling completion.
	if _, err := os.Stat("/dev/full"); err != nil {
		t.Skip("requires /dev/full")
	}
	root := t.TempDir()
	content := bytes.Repeat([]byte("x"), 256<<10)
	source := writeSourceFile(t, root, "source", content)
	good := newFixtureItem(source, filepath.Join(root, "good"))
	bad := newFixtureItem(source, "/dev/full")
	later := newFixtureItem(source, filepath.Join(root, "later"))
	fixture := newStreamFixture(good, bad, later)
	c, err := NewStream(context.Background(), fixture.onResults, Overwrite(true), SetToDevice(LinearDevice(true)))
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	c.getDevice = func(string) (string, error) { return root, nil }
	c.availableSpace = func(string) (int64, error) { calls++; return 1 << 30, nil }

	// The real second Write must stop the linear stream even though the observation is optimistic.
	if err := c.Submit(good, bad, later); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Wait(); err != nil {
		t.Fatal(err)
	}
	result, err := good.terminal(t)
	if err != nil || len(result.Targets) != 1 || result.Targets[0].Err != nil {
		t.Fatalf("first output: %+v / %v", result, err)
	}
	if data, err := os.ReadFile(good.targets[0]); err != nil || !bytes.Equal(data, content) {
		t.Fatalf("first content: %v", err)
	}
	result, err = bad.terminal(t)
	if err != nil || len(result.Targets) != 1 || !errors.Is(result.Targets[0].Err, syscall.ENOSPC) || !errors.Is(result.Targets[0].Err, ErrTargetNoSpace) {
		t.Fatalf("refusing output: %+v / %v", result, err)
	}
	if _, err := later.terminal(t); !errors.Is(err, ErrTargetNoSpace) {
		t.Fatalf("later item: %v", err)
	}
	if calls != 1 || !c.linearTargetStopped() {
		t.Fatalf("queries=%d stopped=%t", calls, c.linearTargetStopped())
	}
	if _, err := os.Stat(later.targets[0]); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("later output started: %v", err)
	}
}

func TestSpaceEstimateRefreshKeepsConcurrentDebits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Hold the observation while admissions consume the previous cached capacity.
		sample := &spaceEstimate{known: true, available: 100}
		entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
		go func() {
			defer close(done)
			_ = sample.refresh(func() (int64, error) {
				close(entered)
				<-release
				return 80, nil
			})
		}()
		<-entered
		if err := sample.admit(30); err != nil {
			t.Fatal(err)
		}
		close(release)
		<-done

		// A completed query must not overwrite logical charges accepted while it was blocked.
		if err := sample.admit(51); !errors.Is(err, ErrTargetNoSpace) {
			t.Fatalf("overlapping admission lost its debit: %v", err)
		}
		if err := sample.admit(50); err != nil {
			t.Fatalf("exact remaining capacity refused: %v", err)
		}

		// A failed observation keeps its identity; a later successful sample can recover.
		sentinel := errors.New("capacity query failed")
		_ = sample.refresh(func() (int64, error) { return 0, sentinel })
		if err := sample.admit(1); !errors.Is(err, sentinel) {
			t.Fatalf("capacity failure identity: %v", err)
		}
		_ = sample.refresh(func() (int64, error) { return 10, nil })
		if err := sample.admit(10); err != nil {
			t.Fatalf("capacity refresh did not recover: %v", err)
		}
	})
}

func TestLinearSpacePollingDoesNotBlockCopy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Keep both the first observation and a later periodic refresh blocked behind explicit gates.
		root := t.TempDir()
		content := bytes.Repeat([]byte("x"), 2*batchSize)
		source := writeSourceFile(t, root, "source", content)
		first := newFixtureItem(source, filepath.Join(root, "first"))
		second := newFixtureItem(source, filepath.Join(root, "second"))
		fixture := newStreamFixture(first, second)
		reported := make(chan struct{}, 2)
		c, err := NewStream(context.Background(), func(results []Result) error {
			// Retain real public results and notify the test without making Close the completion signal.
			if err := fixture.onResults(results); err != nil {
				return err
			}
			for range results {
				reported <- struct{}{}
			}
			return nil
		}, WithResultBatch(1), SetToDevice(LinearDevice(true)))
		if err != nil {
			t.Fatal(err)
		}
		gates := []chan struct{}{make(chan struct{}), make(chan struct{})}
		defer func() {
			// Unblock injected calls on assertion failure before joining the real stream.
			for _, gate := range gates {
				select {
				case <-gate:
				default:
					close(gate)
				}
			}
			_ = c.Close()
		}()
		started := make(chan int, 2)
		var calls atomic.Int32
		c.availableSpace = func(string) (int64, error) {
			// A third invocation would mean shutdown or one-query-at-a-time ownership was broken.
			index := int(calls.Add(1) - 1)
			started <- index
			if index < len(gates) {
				<-gates[index]
			}
			return 1 << 30, nil
		}

		// The first file must complete while capacity is still unknown.
		if err := c.Submit(first); err != nil {
			t.Fatal(err)
		}
		<-started
		select {
		case <-reported:
		case <-time.After(time.Second):
			t.Fatal("copy remained blocked behind the capacity query")
		}
		synctest.Wait()
		if calls.Load() != 1 {
			t.Fatalf("initial calls = %d, want 1", calls.Load())
		}
		close(gates[0])
		synctest.Wait()
		time.Sleep(4 * time.Second)
		synctest.Wait()
		if calls.Load() != 1 {
			t.Fatalf("capacity refreshed before five seconds: %d", calls.Load())
		}

		// A large next file completes against the cache while the five-second refresh is blocked.
		time.Sleep(time.Second)
		<-started
		if err := c.Submit(second); err != nil {
			t.Fatal(err)
		}
		select {
		case <-reported:
		case <-time.After(time.Second):
			t.Fatal("copy remained blocked behind the capacity query")
		}
		time.Sleep(10 * time.Second)
		synctest.Wait()
		if calls.Load() != 2 {
			t.Fatalf("overlapping capacity queries = %d, want 2 total", calls.Load())
		}
		for _, item := range []*fixtureItem{first, second} {
			result, err := item.terminal(t)
			if err != nil || len(result.Targets) != 1 || result.Targets[0].Err != nil {
				t.Fatalf("copy did not finish while statfs was blocked: %+v / %v", result, err)
			}
			if data, err := os.ReadFile(item.targets[0]); err != nil || !bytes.Equal(data, content) {
				t.Fatalf("copied content differs: %v", err)
			}
		}

		// Shutdown joins an outstanding syscall, suppresses queued ticks, and leaves no poller behind.
		done := make(chan error, 1)
		go func() { done <- c.Wait() }()
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("Wait returned before the query finished: %v", err)
		default:
		}
		close(gates[1])
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Second)
		if calls.Load() != 2 {
			t.Fatalf("capacity queried after shutdown: %d", calls.Load())
		}
	})
}

func TestLinearSpaceMountsAndDebits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// A stalled mount must not prevent another mount from acquiring its own observation.
		c := newTestStream(t, SetToDevice(LinearDevice(true)))
		blocked := make(chan struct{})
		calls := make(chan string, 4)
		c.availableSpace = func(device string) (int64, error) {
			calls <- device
			if device == "slow" {
				<-blocked
			}
			return 1000, nil
		}
		job := &writeJob{baseJob: &baseJob{stat: &stat{size: 1}}}
		for _, mount := range []string{"slow", "fast"} {
			if err := c.checkLinearSpace(job, targetSpec{device: mount}); err != nil {
				t.Fatal(err)
			}
		}
		<-calls
		<-calls
		synctest.Wait()

		// More than 64 files reuse a mount's cache; empty files and mount switches retain charges.
		for range 65 {
			if err := c.checkLinearSpace(job, targetSpec{device: "fast"}); err != nil {
				t.Fatal(err)
			}
		}
		job.stat.size = 0
		if err := c.checkLinearSpace(job, targetSpec{device: "empty"}); err != nil {
			t.Fatal(err)
		}
		job.stat.size = 936
		if err := c.checkLinearSpace(job, targetSpec{device: "fast"}); !errors.Is(err, ErrTargetNoSpace) {
			t.Fatalf("logical admission did not consume cached space: %v", err)
		}
		select {
		case mount := <-calls:
			t.Fatalf("per-file capacity refresh on %q", mount)
		default:
		}
		close(blocked)
		c.stopSpaceMonitors()
		if err := c.Wait(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestLinearSpacePollingPanicIsRunFailure(t *testing.T) {
	// The worker must join only after its panic has become the stream's terminal error.
	captureLogs(t)
	c := newTestStream(t, SetToDevice(LinearDevice(true)))
	sentinel := errors.New("capacity query panic")
	c.availableSpace = func(string) (int64, error) { panic(sentinel) }
	job := &writeJob{baseJob: &baseJob{stat: &stat{size: 1}}}
	_ = c.checkLinearSpace(job, targetSpec{device: "mount"})

	// Stage-level callers must explicitly join their owned capacity workers.
	c.stopSpaceMonitors()
	if err := c.Wait(); !errors.Is(err, sentinel) {
		t.Fatalf("Wait lost capacity panic: %v", err)
	}
}

func TestLinearSpacePollingFailureRecovers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Finish one file before a blocked first query publishes its failure.
		root := t.TempDir()
		content := []byte("capacity recovery")
		source := writeSourceFile(t, root, "source", content)
		first := newFixtureItem(source, filepath.Join(root, "first"))
		failed := newFixtureItem(source, filepath.Join(root, "rejected", "target"))
		recovered := newFixtureItem(source, filepath.Join(root, "recovered"))
		fixture := newStreamFixture(first, failed, recovered)
		c, err := NewStream(context.Background(), fixture.onResults,
			WithResultBatch(1), SetToDevice(LinearDevice(true)))
		if err != nil {
			t.Fatal(err)
		}
		gate := make(chan struct{})
		defer func() {
			// Failed assertions still release the injected syscall and drain the stream.
			select {
			case <-gate:
			default:
				close(gate)
			}
			_ = c.Close()
		}()
		sentinel := errors.New("capacity observation failed")
		calls := 0
		c.availableSpace = func(string) (int64, error) {
			// Only the polling goroutine owns this counter; subsequent ticks recover normally.
			calls++
			if calls == 1 {
				<-gate
				return 0, sentinel
			}
			return 1 << 30, nil
		}
		if err := c.Submit(first); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		result, err := first.terminal(t)
		if err != nil || len(result.Targets) != 1 || result.Targets[0].Err != nil {
			t.Fatalf("unknown capacity blocked the first file: %+v / %v", result, err)
		}

		// A completed observation error belongs to the next target, without exhausting the device.
		close(gate)
		synctest.Wait()
		if err := c.Submit(failed); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		result, err = failed.terminal(t)
		if err != nil || len(result.Targets) != 1 || !errors.Is(result.Targets[0].Err, sentinel) {
			t.Fatalf("cached observation error lost its identity: %+v / %v", result, err)
		}
		if c.linearTargetStopped() {
			t.Fatal("observation failure exhausted the target")
		}
		if _, err := os.Stat(filepath.Dir(failed.targets[0])); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("failed admission created output: %v", err)
		}

		// The next periodic observation restores admission on the same stream and mount.
		time.Sleep(5 * time.Second)
		synctest.Wait()
		if err := c.Submit(recovered); err != nil {
			t.Fatal(err)
		}
		if err := c.Wait(); err != nil {
			t.Fatal(err)
		}
		result, err = recovered.terminal(t)
		if err != nil || len(result.Targets) != 1 || result.Targets[0].Err != nil {
			t.Fatalf("successful refresh did not recover: %+v / %v", result, err)
		}
		if data, err := os.ReadFile(recovered.targets[0]); err != nil || !bytes.Equal(data, content) {
			t.Fatalf("recovered content differs: %v", err)
		}
	})
}
