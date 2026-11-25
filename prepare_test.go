package acp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

func TestForwardPreparedPreservesRequiredOrder(t *testing.T) {
	// Deliver preparation completions in a different order from their source requests.
	newJob := func(order uint64, path string) *writeJob {
		return newWriteJob(
			&baseJob{order: order, path: path},
			io.NopCloser(strings.NewReader("fixture")),
		)
	}
	tests := []struct {
		name                   string
		fromLinear, toLinear   bool
		fromThreads, toThreads int
		wantIDs                []string
	}{
		{name: "linear target", toLinear: true, fromThreads: 3, toThreads: 1, wantIDs: []string{"1", "2", "3"}},
		{name: "linear source", fromLinear: true, fromThreads: 1, toThreads: 3, wantIDs: []string{"1", "2", "3"}},
		{name: "serial random devices", fromThreads: 1, toThreads: 1, wantIDs: []string{"1", "2", "3"}},
		{name: "parallel source serial target", fromThreads: 3, toThreads: 1, wantIDs: []string{"3", "1", "2"}},
		{name: "serial source parallel target", fromThreads: 1, toThreads: 3, wantIDs: []string{"3", "1", "2"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Deliver later preparation results first.
			copyer := &StreamCopyer{option: &option{
				fromDevice: &deviceOption{threads: test.fromThreads, linear: test.fromLinear},
				toDevice:   &deviceOption{threads: test.toThreads, linear: test.toLinear},
			}}
			completed := make(chan prepareResult, 3)
			prepare := func(order uint64, path string) prepareResult {
				return prepareResult{order: order, job: newJob(order, path), release: make(chan struct{})}
			}
			completed <- prepare(2, "3")
			completed <- prepare(0, "1")
			completed <- prepare(1, "2")
			close(completed)
			prepared := make(chan *writeJob, 3)

			// Preserve former serial behavior while concurrent random preparation may complete out of order.
			copyer.forwardPrepared(completed, prepared)
			close(prepared)
			var ids []string
			for job := range prepared {
				ids = append(ids, job.path)
				job.finishSource()
			}
			if len(ids) != len(test.wantIDs) {
				t.Fatalf("prepared paths = %v, want %v", ids, test.wantIDs)
			}
			for index := range ids {
				if ids[index] != test.wantIDs[index] {
					t.Fatalf("prepared paths = %v, want %v", ids, test.wantIDs)
				}
			}
		})
	}
}

func TestPrepareReportsUnstartedItemsWithoutReadingThem(t *testing.T) {
	tests := []struct {
		name      string
		itemError error
		want      error
	}{
		{
			name:      "source rejected while indexing",
			itemError: fmt.Errorf("get source stat failed, %w", fs.ErrNotExist),
			want:      fs.ErrNotExist,
		},
		{name: "pipeline stopped", want: context.Canceled},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			if test.itemError == nil {
				cancel()
			}
			defer cancel()

			// The source does not exist, so only a pipeline that still starts a read can
			// report anything other than the failure this stage owes the caller.
			item := newFixtureItem(filepath.Join(t.TempDir(), "removed"), filepath.Join(t.TempDir(), "target"))
			copyer := newTestStream(t)
			fixture := newStreamFixture(item)
			copyer.onResults = fixture.onResults
			job := &baseJob{
				copyer:    copyer,
				item:      item,
				path:      item.source,
				order:     0,
				itemError: test.itemError,
			}

			indexed := make(chan *baseJob, 1)
			indexed <- job
			close(indexed)

			// Preparation forwards an item it must not read to the results stage, and that
			// stage reports it exactly once.
			prepared := copyer.prepare(ctx, indexed)
			copyed := copyer.copy(ctx, prepared)
			runResults(copyer, copyed)
			for range copyed {
			}

			if _, err := item.terminal(t); !errors.Is(err, test.want) {
				t.Fatalf("item error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestPrepareReadsNoContentWhenThePolicyNeedsNone(t *testing.T) {
	tests := []struct {
		name   string
		policy HashPolicy

		// A policy that uses the cache reads its stored hash through the item's descriptor, so a
		// source it cannot open is a cache miss with a warning instead of an item failure.
		wantMisses   int64
		wantFailures int64
	}{
		{name: "off", policy: HashOff},
		{name: "cached only without entry", policy: HashCachedOnly, wantMisses: 1, wantFailures: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			copyer := newTestStream(t, WithHashPolicy(test.policy))
			if test.policy.producesHash() {
				copyer.cacheStats = &cacheStats{}
			}

			// The source only has to be described: a policy that needs no content reads nothing
			// from it, so a missing path cannot turn the item into a failure.
			item := newFixtureItem(filepath.Join(t.TempDir(), "removed"))
			fixture := newStreamFixture(item)
			copyer.onResults = fixture.onResults
			job := &baseJob{
				copyer: copyer,
				item:   item,
				path:   item.source,
				stat:   &stat{size: 7, mode: 0o644},
				order:  0,
			}
			indexed := make(chan *baseJob, 1)
			indexed <- job
			close(indexed)

			var prepared []*writeJob
			for write := range copyer.prepare(context.Background(), indexed) {
				if !write.skipContent {
					t.Fatal("prepare kept a content read for a policy that needs no content")
				}
				prepared = append(prepared, write)
			}
			if len(prepared) != 1 {
				t.Fatalf("prepared jobs = %d, want 1", len(prepared))
			}

			if err := runWriteJob(copyer, prepared[0]); err != nil {
				t.Fatalf("pipeline failed: %v", err)
			}
			result, err := item.terminal(t)
			if err != nil {
				t.Fatalf("item failed: %v", err)
			}
			if len(result.SHA256) != 0 || result.SignatureCacheHit {
				t.Fatalf("result = %#v, want no hash and no cache hit", result)
			}
			if result.Size != 7 {
				t.Fatalf("result size = %d, want the indexed size", result.Size)
			}

			// A cache read that cannot open its descriptor is a miss with a warning, and the item
			// still completes without a hash.
			if copyer.cacheStats == nil {
				return
			}
			summary := copyer.cacheStats.snapshot()
			if summary.Misses != test.wantMisses || summary.Failures != test.wantFailures {
				t.Fatalf("cache summary = %#v, want misses=%d failures=%d", summary, test.wantMisses, test.wantFailures)
			}
		})
	}
}

func TestRunKeepsLinearOrderingPastAnUnprocessedItem(t *testing.T) {
	// A linear writer advances per request order, so an item that preparation never reads
	// must still advance that order: otherwise the items behind it lose their callback.
	root := t.TempDir()
	missing := newFixtureItem(filepath.Join(root, "missing"))
	goodSource := writeSourceFile(t, root, "good.txt", []byte("good"))
	goodTarget := filepath.Join(root, "target", "good.txt")
	good := newFixtureItem(goodSource, goodTarget)

	if err := runFixture(
		context.Background(),
		newStreamFixture(missing, good),
		[]Item{missing, good},
		SetToDevice(LinearDevice(true)),
	); err != nil {
		t.Fatalf("run error = %v, want nil", err)
	}

	if _, err := missing.terminal(t); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("rejected item error = %v, want %v", err, fs.ErrNotExist)
	}
	result, err := good.terminal(t)
	if err != nil {
		t.Fatalf("item behind the rejected one failed: %v", err)
	}
	if len(result.Targets) != 1 || result.Targets[0].Err != nil || result.Targets[0].Path != goodTarget {
		t.Fatalf("targets = %#v, want %q written", result.Targets, goodTarget)
	}
}
