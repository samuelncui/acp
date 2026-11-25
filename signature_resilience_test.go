package acp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/samuelncui/acp/signature"
)

func TestTargetSignaturePrecedesReadOnlyMetadata(t *testing.T) {
	// The source's final mode must not prevent publishing a fresh target's cache entry.
	requireSignatureXattrSupport(t)
	root := t.TempDir()
	source := writeSourceFile(t, root, "source", []byte("content"))
	if err := os.Chmod(source, 0o444); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "target")
	requireWritable := withFS(fsHooks{writeEntry: func(file *os.File, entry signature.Entry) error {
		// Linux user attributes require current write permission even through an already-open fd.
		if file.Name() != source {
			info, err := file.Stat()
			if err != nil {
				return err
			}
			if info.Mode().Perm()&0o200 == 0 {
				return os.ErrPermission
			}
		}
		return signature.WriteFile(file, entry)
	}})

	// Final content, permissions and the cache must all survive the ordinary completion path.
	item := newFixtureItem(source, target)
	if err := runFixture(context.Background(), newStreamFixture(item), []Item{item}, WithHashPolicy(HashReadRefresh), requireWritable); err != nil {
		t.Fatal(err)
	}
	result, err := item.terminal(t)
	if err != nil || len(result.Targets) != 1 || result.Targets[0].Err != nil {
		t.Fatalf("copy outcome=%+v / %v", result, err)
	}
	if _, found, err := signature.ReadCached(target); err != nil || !found {
		t.Fatalf("target signature found=%v / %v", found, err)
	}
	if info, err := os.Stat(target); err != nil || info.Mode().Perm() != 0o444 {
		t.Fatalf("target mode=%v / %v", info, err)
	}
}

func TestRunWritesEveryItemCacheEntry(t *testing.T) {
	// Build more sources than one item at a time, so every item publishes its own entry.
	requireSignatureXattrSupport(t)
	root := t.TempDir()
	items := make([]Item, 0, 32)
	fixtures := make([]*fixtureItem, 0, 32)
	want := make(map[string][32]byte)
	for idx := 0; idx < 32; idx++ {
		path := writeSourceFile(t, root, fmt.Sprintf("%02d.bin", idx), []byte(fmt.Sprintf("concurrent signature %d", idx)))
		item := newFixtureItem(path)
		items = append(items, item)
		fixtures = append(fixtures, item)
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		want[path] = sha256.Sum256(content)
	}

	// Run concurrent hashing and require every item to have published its own entry.
	if err := runFixture(
		context.Background(),
		newStreamFixture(fixtures...),
		items,
		WithHashPolicy(HashReadRefresh),
		SetFromDevice(DeviceThreads(4)),
	); err != nil {
		t.Fatal(err)
	}

	// Confirm every item published before the run returned: no write outlives its item.
	for path, expected := range want {
		signature, valid := readStoredSignature(t, path)
		if !valid {
			t.Fatalf("item %q published no valid signature", path)
		}
		if !bytes.Equal(signature.SHA256, expected[:]) {
			t.Fatalf("signature for %q = %x, want %x", path, signature.SHA256, expected)
		}
	}
}

func TestRunSignatureCacheSurvivesCancellation(t *testing.T) {
	// Cancel only after the first completed item has left the pipeline, which is where a caller
	// observes that the run is working.
	requireSignatureXattrSupport(t)
	content := []byte("cancellation fixture")
	path := writeSourceFile(t, t.TempDir(), "cancellation.bin", content)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	item := newFixtureItem(path)
	fixture := newStreamFixture(item)

	// A result batch of one delivers the completed item immediately, so the callback stops the
	// run while the pipeline is still open instead of at Close.
	cancelled := make(chan struct{}, 1)
	fixture.hook = func([]Result) error {
		cancel()
		select {
		case cancelled <- struct{}{}:
		default:
		}
		return nil
	}

	stream, err := NewStream(ctx, fixture.onResults, WithHashPolicy(HashReadRefresh), WithResultBuffer(1), WithResultBatch(1))
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Submit(item); err != nil {
		t.Fatal(err)
	}

	// Submit is the run's single cancellation checkpoint, so the batch after the callback
	// observes the stop and is refused.
	select {
	case <-cancelled:
	case <-time.After(10 * time.Second):
		t.Fatal("the run never delivered the completed item")
	}
	if err := stream.Submit(newFixtureItem(path)); !errors.Is(err, context.Canceled) {
		t.Fatalf("Submit() after cancellation error = %v, want context cancellation", err)
	}
	if err := stream.Wait(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait() error = %v, want context cancellation", err)
	}

	// The completed item published its entry before its result, so caller cancellation cannot
	// lose it.
	result, terminalErr := item.terminal(t)
	if terminalErr != nil {
		t.Fatalf("item failed: %v", terminalErr)
	}
	signature, valid := readStoredSignature(t, path)
	if !valid {
		t.Fatal("the completed item published no valid signature before cancellation")
	}
	if sum := sha256.Sum256(content); !bytes.Equal(signature.SHA256, sum[:]) || !bytes.Equal(result.SHA256, signature.SHA256) {
		t.Fatalf("signature = %x, result = %x", signature.SHA256, result.SHA256)
	}
}

func TestRunSignatureCacheIsBestEffortForReadOnlyFile(t *testing.T) {
	// Hash content successfully even if the filesystem rejects the cache write.
	requireSignatureXattrSupport(t)
	path := writeSourceFile(t, t.TempDir(), "readonly.bin", []byte("read-only signature fixture"))
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(content)

	// Reading and hashing must succeed independently of a writable cache attribute.
	result, summary := runSignatureHash(t, path, HashReadRefresh)
	if !bytes.Equal(result.SHA256, want[:]) {
		t.Fatalf("SHA256 = %x, want %x", result.SHA256, want)
	}

	// Accept either a valid owner-writable xattr or a non-fatal warning.
	signature, valid := readStoredSignature(t, path)
	if valid && !bytes.Equal(signature.SHA256, want[:]) {
		t.Fatalf("signature = %x, want %x", signature.SHA256, want)
	}
	if !valid && summary.Failures == 0 {
		t.Fatalf("read-only cache miss had no warning: %#v", summary)
	}
}

// TestRunIgnoresUnsupportedSignatureXattr pins the no-op policy for a file system without the
// managed attribute namespace: the cache is simply absent, so a run neither fails nor records a
// cache failure. A real xattr failure is still reported.
func TestRunIgnoresUnsupportedSignatureXattr(t *testing.T) {
	// Simulate a filesystem without the managed namespace, regardless of this host's support.
	hooks := fsHooks{
		readEntry: func(*os.File, os.FileInfo) (signature.Entry, signature.Status, error) {
			// signature.ReadFile reports an unsupported attribute as a missing entry.
			return signature.Entry{}, signature.Missing, nil
		},
		writeEntry:     func(*os.File, signature.Entry) error { return errors.ErrUnsupported },
		writeEntryPath: func(string, signature.Entry) error { return errors.ErrUnsupported },
	}

	// Keep an existing target so replacement also exercises the no-cache policy.
	root := t.TempDir()
	content := []byte("unsupported xattr fixture")
	input := writeSourceFile(t, root, "source.txt", content)
	target := writeSourceFile(t, root, "target.txt", []byte("old"))

	// A transfer refreshes both the source and its fresh target, exercising cache reads and writes.
	run := func() (*fixtureItem, SignatureCacheSummary) {
		t.Helper()

		// Collect the summary after the transfer has completed both files.
		item := newFixtureItem(input, target)
		var summary SignatureCacheSummary
		handler := func(event Event) {
			if update, ok := event.(*EventSignatureCacheSummary); ok {
				summary = update.Summary
			}
		}
		if err := runFixture(
			context.Background(),
			newStreamFixture(item),
			[]Item{item},
			WithHashPolicy(HashCachedOrReadRefresh),
			Overwrite(true),
			WithEventHandler(handler),
			withFS(hooks),
		); err != nil {
			t.Fatalf("runFixture() error = %v, want nil: an unsupported xattr is not a failure", err)
		}
		return item, summary
	}

	// Unsupported attributes leave the transfer successful without cache failures.
	item, summary := run()
	result, err := item.terminal(t)
	if err != nil {
		t.Fatalf("item failed: %v", err)
	}
	if want := sha256.Sum256(content); !bytes.Equal(result.SHA256, want[:]) {
		t.Fatalf("SHA256 = %x, want %x", result.SHA256, want)
	}
	if summary.Failures != 0 || summary.Writes != 0 {
		t.Fatalf("summary = %#v, want no recorded failure and no write", summary)
	}

	// A broken attribute is still a failure, so the no-op policy cannot swallow real ones.
	hooks.writeEntry = func(*os.File, signature.Entry) error { return errors.New("xattr is broken") }
	hooks.writeEntryPath = func(string, signature.Entry) error { return errors.New("xattr is broken") }
	if _, summary := run(); summary.Failures == 0 {
		t.Fatalf("summary = %#v, want the broken attribute recorded", summary)
	}
}
