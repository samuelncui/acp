package acp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunCopiesItemsToLinearTarget(t *testing.T) {
	// Build caller-owned items without constructing copy options per file.
	root := t.TempDir()
	order := new(callbackOrder)
	items := make([]Item, 0, 3)
	fixtures := make([]*fixtureItem, 0, 3)
	for index, content := range []string{"first", "second", "third"} {
		name := string(rune('a'+index)) + ".txt"
		source := writeSourceFile(t, root, filepath.Join("source", name), []byte(content))
		item := newFixtureItem(source, filepath.Join(root, "target", name))
		item.hook = func(*Result, error) { order.record(item) }
		items = append(items, item)
		fixtures = append(fixtures, item)
	}

	// Keep the target serialized while allowing source preparation to complete in any order.
	if err := runFixture(
		context.Background(),
		newStreamFixture(fixtures...),
		items,
		WithHashPolicy(HashRead),
		SetToDevice(LinearDevice(true)),
	); err != nil {
		t.Fatal(err)
	}

	// Result order is unspecified, so the test asserts every item's own outcome and that a
	// serialized target still received them all.
	if reported := order.snapshot(); len(reported) != len(items) {
		t.Fatalf("received %d callbacks, want %d", len(reported), len(items))
	}
	for _, item := range fixtures {
		result, err := item.terminal(t)
		if err != nil {
			t.Fatalf("item %q failed: %v", item.source, err)
		}
		if len(result.Targets) != 1 || result.Targets[0].Err != nil || len(result.SHA256) == 0 {
			t.Fatalf("unexpected result: %#v", result)
		}
		if result.Job != Item(item) {
			t.Fatalf("result job = %#v, want the submitted item", result.Job)
		}
	}
}

func TestRunHashesWithoutTargets(t *testing.T) {
	// Submit one targetless item through the same batch interface.
	content := []byte("hash-only fixture")
	source := writeSourceFile(t, t.TempDir(), "source.txt", content)
	item := newFixtureItem(source)

	// Verify ACP reads the source once and reports its SHA-256 without creating a target.
	if err := runFixture(context.Background(), newStreamFixture(item), []Item{item}, WithHashPolicy(HashRead)); err != nil {
		t.Fatal(err)
	}
	result, err := item.terminal(t)
	if err != nil {
		t.Fatalf("item failed: %v", err)
	}

	wantHash := sha256.Sum256(content)
	if got := hex.EncodeToString(result.SHA256); got != hex.EncodeToString(wantHash[:]) {
		t.Fatalf("SHA256 = %q, want %q", got, hex.EncodeToString(wantHash[:]))
	}
	if len(result.Targets) != 0 {
		t.Fatalf("hash-only targets = %v, want none", result.Targets)
	}
	if result.SignatureCacheHit {
		t.Fatal("hash-only item reported a signature cache hit")
	}
}

func TestRunRejectsReuseOnlyPolicyForItemsWithTargets(t *testing.T) {
	// A copy always reads its source and produces a computed hash, so a value that promises a
	// stored hash cannot describe it. The policy is rejected as an item error
	// instead of silently reading.
	for _, policy := range []HashPolicy{HashCachedOnly, HashCachedOrRead} {
		t.Run(policy.String(), func(t *testing.T) {
			root := t.TempDir()
			input := writeSourceFile(t, root, "source.txt", []byte("fixture"))
			target := filepath.Join(root, "target.txt")
			item := newFixtureItem(input, target)

			if err := runFixture(context.Background(), newStreamFixture(item), []Item{item}, WithHashPolicy(policy)); err != nil {
				t.Fatalf("run error = %v, want nil: a rejected item is an item outcome", err)
			}
			if result, err := item.terminal(t); err == nil {
				t.Fatalf("item completed with %#v, want the rejected policy", result)
			} else if !strings.Contains(err.Error(), "check hash policy failed") ||
				!strings.Contains(err.Error(), policy.String()) {
				t.Fatalf("item error = %v, want a rejected %s policy", err, policy)
			}

			// The rejected item must not have touched the target.
			if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("stat target error = %v, want %v", err, os.ErrNotExist)
			}
		})
	}
}

func TestRunKeepsEveryOtherPolicyForItemsWithTargets(t *testing.T) {
	// The remaining values read content or need none, so a transfer can use all of them.
	for _, policy := range []HashPolicy{HashOff, HashCachedOrReadRefresh, HashRead, HashReadRefresh} {
		t.Run(policy.String(), func(t *testing.T) {
			root := t.TempDir()
			content := []byte("policy fixture")
			input := writeSourceFile(t, root, "source.txt", content)
			target := filepath.Join(root, "target.txt")
			item := newFixtureItem(input, target)

			if err := runFixture(context.Background(), newStreamFixture(item), []Item{item}, WithHashPolicy(policy)); err != nil {
				t.Fatalf("run error = %v", err)
			}
			result, err := item.terminal(t)
			if err != nil {
				t.Fatalf("item failed: %v", err)
			}
			if len(result.Targets) != 1 || result.Targets[0].Err != nil || result.Targets[0].Path != target {
				t.Fatalf("targets = %#v, want %q written", result.Targets, target)
			}

			got, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, content) {
				t.Fatalf("target content = %q, want %q", got, content)
			}

			// Only a reading policy produces the computed hash of the copied bytes.
			wantHash := sha256.Sum256(content)
			if policy.producesHash() {
				if !bytes.Equal(result.SHA256, wantHash[:]) {
					t.Fatalf("SHA256 = %x, want %x", result.SHA256, wantHash)
				}
			} else if len(result.SHA256) != 0 {
				t.Fatalf("SHA256 = %x, want none", result.SHA256)
			}
		})
	}
}

func TestRunReportsTargetFailureAsItemOutcome(t *testing.T) {
	// Force target creation to fail while the item still completes.
	root := t.TempDir()
	input := writeSourceFile(t, root, "source.txt", []byte("fixture"))
	target := writeSourceFile(t, root, "target.txt", []byte("existing"))
	item := newFixtureItem(input, target)

	// A failed target is an item outcome with its own error identity, not a run error.
	if err := runFixture(context.Background(), newStreamFixture(item), []Item{item}); err != nil {
		t.Fatalf("run error = %v, want nil", err)
	}
	result, err := item.terminal(t)
	if err != nil {
		t.Fatalf("item failed: %v", err)
	}
	if len(result.Targets) != 1 {
		t.Fatalf("targets = %v, want one outcome", result.Targets)
	}
	if !errors.Is(result.Targets[0].Err, fs.ErrExist) {
		t.Fatalf("target error = %v, want %v", result.Targets[0].Err, fs.ErrExist)
	}

	// The refused target must keep its previous content.
	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(content, []byte("existing")) {
		t.Fatalf("target content = %q, want %q", content, "existing")
	}
}

func TestRunReportsOneOutcomePerRequestedTargetInOrder(t *testing.T) {
	// Request two targets where only the second one can be written.
	root := t.TempDir()
	input := writeSourceFile(t, root, "source.txt", []byte("fixture"))
	refused := writeSourceFile(t, root, "refused.txt", []byte("existing"))
	written := filepath.Join(root, "written.txt")
	item := newFixtureItem(input, refused, written)

	if err := runFixture(context.Background(), newStreamFixture(item), []Item{item}); err != nil {
		t.Fatalf("run error = %v, want nil", err)
	}
	result, err := item.terminal(t)
	if err != nil {
		t.Fatalf("an item with a failed target must still complete: %v", err)
	}
	if len(result.Targets) != 2 {
		t.Fatalf("targets = %v, want one outcome per requested target", result.Targets)
	}
	if got := result.Targets[0]; got.Path != refused || !errors.Is(got.Err, fs.ErrExist) {
		t.Fatalf("target 0 = %#v, want %q refused with %v", got, refused, fs.ErrExist)
	}
	if got := result.Targets[1]; got.Path != written || got.Err != nil {
		t.Fatalf("target 1 = %#v, want %q written", got, written)
	}

	// The successful target must hold the source content.
	content, err := os.ReadFile(written)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(content, []byte("fixture")) {
		t.Fatalf("written content = %q, want %q", content, "fixture")
	}
}

// TestRunCopiesAChunkedSourceToEveryTarget pins a successful multi-target copy larger than one
// read buffer: several chunks of the shared buffer pool travel to every target, and each target
// must hold the whole source. The content is a fixed byte pattern instead of a random source, so
// the test is reproducible, and every target hash is computed from the bytes read back from disk.
func TestRunCopiesAChunkedSourceToEveryTarget(t *testing.T) {
	// Three full chunks plus a short final one, so the read loop, the buffer handoff and the
	// final partial write are all exercised.
	content := make([]byte, 3*batchSize+12345)
	if len(content) <= batchSize {
		t.Fatalf("test content = %d bytes, want more than one %d-byte chunk", len(content), batchSize)
	}
	for index := range content {
		content[index] = byte(index*7 + index/251)
	}

	// Track the shared pool as well: several buffers pass through it, and every reference must
	// come back by the time the run ends.
	trackChunkPool(t)

	root := t.TempDir()
	input := writeSourceFile(t, root, "source.bin", content)
	targets := []string{
		filepath.Join(root, "target-a.bin"),
		filepath.Join(root, "target-b.bin"),
		filepath.Join(root, "target-c.bin"),
	}
	item := newFixtureItem(input, targets...)

	if err := runFixture(context.Background(), newStreamFixture(item), []Item{item}, WithHashPolicy(HashRead)); err != nil {
		t.Fatalf("run error = %v, want nil", err)
	}
	result, err := item.terminal(t)
	if err != nil {
		t.Fatalf("item failed: %v", err)
	}

	wantHash := sha256.Sum256(content)
	if result.Size != int64(len(content)) {
		t.Fatalf("result size = %d, want %d", result.Size, len(content))
	}
	if !bytes.Equal(result.SHA256, wantHash[:]) {
		t.Fatalf("result SHA256 = %x, want %x", result.SHA256, wantHash)
	}
	if len(result.Targets) != len(targets) {
		t.Fatalf("targets = %#v, want one outcome per requested target", result.Targets)
	}
	for index, target := range targets {
		outcome := result.Targets[index]
		if outcome.Path != target || outcome.Err != nil {
			t.Fatalf("target %d = %#v, want %q written", index, outcome, target)
		}

		got, err := os.ReadFile(target)
		if err != nil {
			t.Fatalf("read target %q: %v", target, err)
		}
		if !bytes.Equal(got, content) {
			t.Fatalf("target %q holds %d bytes that differ from the source content, want %d matching bytes", target, len(got), len(content))
		}
		if hash := sha256.Sum256(got); !bytes.Equal(hash[:], wantHash[:]) {
			t.Fatalf("target %q SHA256 = %x, want %x", target, hash, wantHash)
		}
	}
}

func TestRunReportsUnprocessableItemsAsFailures(t *testing.T) {
	root := t.TempDir()
	input := writeSourceFile(t, root, "source.txt", []byte("fixture"))
	directory := filepath.Join(root, "directory")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}

	// A source that cannot be described, opened, or read never completes.
	tests := []struct {
		name    string
		source  string
		target  []string
		wantErr error
		anyErr  bool
	}{
		{name: "missing source", source: filepath.Join(root, "missing"), wantErr: fs.ErrNotExist},
		{name: "directory source", source: directory, anyErr: true},
		{name: "successful item", source: input, target: []string{filepath.Join(root, "target.txt")}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			item := newFixtureItem(test.source, test.target...)
			if err := runFixture(context.Background(), newStreamFixture(item), []Item{item}); err != nil {
				t.Fatalf("run error = %v, want nil", err)
			}

			result, err := item.terminal(t)
			if test.wantErr != nil || test.anyErr {
				if err == nil {
					t.Fatalf("item completed with %#v, want a failure", result)
				}
				if test.wantErr != nil && !errors.Is(err, test.wantErr) {
					t.Fatalf("item error = %v, want %v", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("item failed: %v", err)
			}
			if submitted, ok := result.Job.(*fixtureItem); !ok || submitted.source != filepath.Clean(test.source) {
				t.Fatalf("result job = %#v, want the submitted item", result.Job)
			}
		})
	}
}
