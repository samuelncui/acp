package acp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestLinearSpaceSampling(t *testing.T) {
	// Exercise admission through real output creation; discarded outputs never refund estimates.
	const mib = int64(1 << 20)
	observationErr := errors.New("capacity observation failed")
	type step struct {
		size      int64
		targets   int
		mount     string
		available int64
		queryErr  error
		wantCalls int
		wantErr   error
	}
	for _, tt := range []struct {
		name  string
		steps []step
	}{
		{"logical debit and boundary refresh", []step{
			{size: mib, available: 65 * mib, wantCalls: 1},
			{size: mib, queryErr: observationErr, wantCalls: 1},
			{size: mib, available: mib - 1, wantCalls: 2, wantErr: ErrTargetNoSpace},
			{size: mib, available: mib, wantCalls: 3},
		}},
		{"mount change", []step{
			{size: 1, mount: "first", available: 128 * mib, wantCalls: 1},
			{size: 1, mount: "second", available: 128 * mib, wantCalls: 2},
			{size: 1, mount: "first", available: 128 * mib, wantCalls: 3},
		}},
		{"large target invalidates", []step{
			{size: 1, available: 128 * mib, wantCalls: 1},
			{size: mib + 1, available: 128 * mib, wantCalls: 2},
			{size: 1, available: 128 * mib, wantCalls: 3},
		}},
		{"multiple targets invalidate", []step{
			{size: 1, available: 128 * mib, wantCalls: 1},
			{size: 1, targets: 2, available: 128 * mib, wantCalls: 3},
			{size: 1, available: 128 * mib, wantCalls: 4},
		}},
		{"empty target invalidates without querying", []step{
			{size: 1, available: 128 * mib, wantCalls: 1},
			{size: 0, queryErr: observationErr, wantCalls: 1},
			{size: 1, available: 128 * mib, wantCalls: 2},
		}},
		{"observation error invalidates", []step{
			{size: 1, available: 64 * mib, wantCalls: 1},
			{size: 1, queryErr: observationErr, wantCalls: 2, wantErr: observationErr},
			{size: 1, available: 128 * mib, wantCalls: 3},
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// Keep one linear writer and inject only the filesystem capacity observation.
			root := t.TempDir()
			opt := newOption()
			opt.toDevice.linear = true
			c := &StreamCopyer{option: opt}
			calls := 0
			for index, s := range tt.steps {
				c.availableSpace = func(device string) (int64, error) {
					calls++
					if device != s.mount {
						t.Fatalf("device = %q, want %q", device, s.mount)
					}
					return s.available, s.queryErr
				}
				targets := s.targets
				if targets == 0 {
					targets = 1
				}
				job := &writeJob{baseJob: &baseJob{stat: &stat{size: s.size}, outputs: make([]targetSpec, targets)}}
				for target := range targets {
					// A rejected admission must precede directory creation; accepted outputs are owned and discarded.
					dir := filepath.Join(root, fmt.Sprintf("%d-%d", index, target))
					out, err := c.prepareTarget(job, targetSpec{path: filepath.Join(dir, "target"), device: s.mount})
					if !errors.Is(err, s.wantErr) {
						t.Fatalf("step %d: error = %v, want %v", index, err, s.wantErr)
					}
					if out != nil {
						if err := out.Discard(); err != nil {
							t.Fatal(err)
						}
					} else if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("rejected step %d created output directory: %v", index, err)
					}
				}
				if calls != s.wantCalls {
					t.Fatalf("step %d: capacity calls = %d, want %d", index, calls, s.wantCalls)
				}
			}
		})
	}
}

func TestLinearSpaceSamplingRefreshesAfterBoundedFiles(t *testing.T) {
	// Even tiny files must refresh the observation after a bounded number of admissions.
	root := t.TempDir()
	opt := newOption()
	opt.toDevice.linear = true
	calls := 0
	c := &StreamCopyer{option: opt, availableSpace: func(string) (int64, error) { calls++; return 1 << 30, nil }}
	job := &writeJob{baseJob: &baseJob{stat: &stat{size: 1}, outputs: make([]targetSpec, 1)}}
	for index := range 65 {
		// Real output cleanup does not erase admitted file accounting.
		out, err := c.prepareTarget(job, targetSpec{device: root, path: filepath.Join(root, fmt.Sprint(index))})
		if err != nil {
			t.Fatal(err)
		}
		if err := out.Discard(); err != nil {
			t.Fatal(err)
		}
		want := 1
		if index == 64 {
			want = 2
		}
		if calls != want {
			t.Fatalf("after %d files: capacity calls = %d, want %d", index+1, calls, want)
		}
	}
}

func TestLinearSpaceSamplingRetainsActualWriteFailure(t *testing.T) {
	// Populate a shared-mount estimate with a successful small output before a refusing device.
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

	// The real second Write must stop the linear stream even though its estimate was reused.
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
