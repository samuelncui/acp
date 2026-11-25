package acp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/samuelncui/acp/signature"
)

// TestRunReadsTheStoredHashThroughTheItemDescriptor pins where a stored hash is read: through the
// descriptor the item opened for its content, never by reopening the stable source path.
func TestRunReadsTheStoredHashThroughTheItemDescriptor(t *testing.T) {
	for _, mode := range []ReadMode{ReadBuffered, ReadMapped} {
		t.Run(mode.String(), func(t *testing.T) {
			// Seed a reusable entry before the run while keeping the source stable throughout it.
			requireSignatureXattrSupport(t)
			content := []byte("descriptor cache read fixture")
			input := writeSourceFile(t, t.TempDir(), "source.txt", content)
			seeded := seedStaleSignature(t, input, []byte("stored value"))

			// Observe descriptor identity directly without changing the file or its path.
			var opened *os.File
			var opens, reads int
			observe := withFS(fsHooks{
				open: func(path string, mode ReadMode, info os.FileInfo) (itemSource, error) {
					source, err := openNativeSource(path, mode, info)
					opens++
					opened = source.file
					return source, err
				},
				readEntry: func(file *os.File, info os.FileInfo) (signature.Entry, signature.Status, error) {
					reads++
					if file != opened {
						t.Errorf("cache read descriptor = %p, want the source descriptor %p", file, opened)
					}
					return signature.ReadFile(file, info)
				},
			})

			// A reusable entry bypasses content without opening or reading the cache again.
			var summary SignatureCacheSummary
			handler := func(event Event) {
				if update, ok := event.(*EventSignatureCacheSummary); ok {
					summary = update.Summary
				}
			}
			item := newFixtureItem(input)
			if err := runFixture(
				context.Background(),
				newStreamFixture(item),
				[]Item{item},
				WithHashPolicy(HashCachedOrRead),
				SetFromDevice(WithReadMode(mode)),
				WithEventHandler(handler),
				observe,
			); err != nil {
				t.Fatal(err)
			}

			// The result and summary reflect one descriptor-based cache lookup.
			result, err := item.terminal(t)
			if err != nil {
				t.Fatalf("item failed: %v", err)
			}
			if !result.SignatureCacheHit || !bytes.Equal(result.SHA256, seeded.SHA256[:]) {
				t.Fatalf("result = %#v, want the stored hash %x of the file the item opened", result, seeded.SHA256)
			}
			if summary.Hits != 1 || summary.Misses != 0 || summary.Failures != 0 {
				t.Fatalf("summary = %#v, want one hit read through the item's descriptor", summary)
			}
			if opens != 1 || reads != 1 {
				t.Fatalf("source opens=%d cache reads=%d, want one each", opens, reads)
			}
		})
	}
}

// TestRunWritesComputedCacheAfterSourceClose pins where a computed hash is published:
// through the stable source path after the content descriptor has closed.
func TestRunWritesComputedCacheAfterSourceClose(t *testing.T) {
	for _, mode := range []ReadMode{ReadBuffered, ReadMapped} {
		t.Run(mode.String(), func(t *testing.T) {
			// A stored entry that differs from the computed hash requires one source publication.
			requireSignatureXattrSupport(t)
			content := []byte("descriptor cache write fixture")
			input := writeSourceFile(t, t.TempDir(), "source.txt", content)
			seedStaleSignature(t, input, []byte("stale stored value"))

			// Observe one content open and path publication after descriptor closure.
			var opened *os.File
			var opens, writes int
			observe := withFS(fsHooks{
				open: func(path string, mode ReadMode, info os.FileInfo) (itemSource, error) {
					source, err := openNativeSource(path, mode, info)
					opens++
					opened = source.file
					return source, err
				},
				writeEntryPath: func(path string, entry signature.Entry) error {
					writes++
					if path != input {
						t.Errorf("cache path = %q, want %q", path, input)
					}
					if _, err := opened.Stat(); !errors.Is(err, os.ErrClosed) {
						t.Errorf("cache publication preceded source Close: %v", err)
					}
					return signature.WritePath(path, entry)
				},
			})

			// Run a content refresh and retain the aggregate warning and write counts.
			var summary SignatureCacheSummary
			handler := func(event Event) {
				if update, ok := event.(*EventSignatureCacheSummary); ok {
					summary = update.Summary
				}
			}
			item := newFixtureItem(input)
			if err := runFixture(
				context.Background(),
				newStreamFixture(item),
				[]Item{item},
				WithHashPolicy(HashReadRefresh),
				SetFromDevice(WithReadMode(mode)),
				WithEventHandler(handler),
				observe,
			); err != nil {
				t.Fatal(err)
			}

			// Only the completed content hash is published, exactly once through the owned source.
			result, err := item.terminal(t)
			if err != nil {
				t.Fatalf("item failed: %v", err)
			}
			want := sha256.Sum256(content)
			if !bytes.Equal(result.SHA256, want[:]) {
				t.Fatalf("SHA256 = %x, want %x: the item hashes the content it opened", result.SHA256, want)
			}
			if summary.Hits != 0 || summary.Misses != 0 || summary.Writes != 1 || summary.Failures != 0 {
				t.Fatalf("summary = %#v, want one write and no reuse accounting", summary)
			}
			if opens != 1 || writes != 1 {
				t.Fatalf("source opens=%d cache writes=%d, want one each", opens, writes)
			}
			stored, valid := readStoredSignature(t, input)
			if !valid || !bytes.Equal(stored.SHA256, want[:]) {
				t.Fatalf("stored signature for the item's file = %#v, valid=%t, want %x", stored, valid, want)
			}
			if _, err := opened.Stat(); !errors.Is(err, os.ErrClosed) {
				t.Fatalf("source descriptor after the run = %v, want closed", err)
			}
		})
	}
}

// TestRunPublishesTargetCacheThroughTheOpenTargetDescriptor pins where a target's cache entry is
// published: through the descriptor the writer wrote that target with, after the item's content
// hash is complete and while the descriptor is still open. A descriptor reopened by path would sit
// at offset zero and would know nothing about the item's hash.
func TestRunPublishesTargetCacheThroughTheOpenTargetDescriptor(t *testing.T) {
	// Prepare stable source content and two fresh final target paths.
	requireSignatureXattrSupport(t)
	content := []byte("target descriptor fixture")
	root := t.TempDir()
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	input := writeSourceFile(t, root, "source.txt", content)
	targets := []string{filepath.Join(root, "target-1.txt"), filepath.Join(root, "target-2.txt")}

	// Publication must retain the source metadata observed before the run.
	info, err := os.Stat(input)
	if err != nil {
		t.Fatal(err)
	}
	wantHash := sha256.Sum256(content)
	want := signature.Entry{Size: int64(len(content)), MtimeNS: info.ModTime().UnixNano(), SHA256: wantHash[:]}

	// Record the descriptor the item reads its source through, so every published entry can be
	// attributed to the file that owns it.
	var sourceFile *os.File
	var hooks fsHooks
	hooks.open = func(path string, mode ReadMode, info os.FileInfo) (itemSource, error) {
		source, err := openNativeSource(path, mode, info)
		if err == nil && path == input {
			sourceFile = source.file
		}
		return source, err
	}

	// Record each publication's descriptor, position and content facts.
	type publication struct {
		source    bool
		name      string
		offset    int64
		open      bool
		signature signature.Entry
	}
	var (
		lock         sync.Mutex
		publications []publication
	)
	hooks.writeEntry = func(file *os.File, entry signature.Entry) error {
		// Reject any publication that cannot describe a complete signature.
		if len(entry.SHA256) != sha256.Size {
			t.Errorf("published a signature without the SHA-256 of the stored bytes: %#v", entry)
			return signature.WriteFile(file, entry)
		}

		// The descriptor that read or wrote the whole content stands at its end; a descriptor
		// reopened by path would start at offset zero.
		offset, offsetErr := file.Seek(0, io.SeekCurrent)
		if offsetErr != nil {
			t.Errorf("read the publishing descriptor offset: %v", offsetErr)
		}
		_, statErr := file.Stat()

		// Record the descriptor before the real attribute write and eventual close.
		lock.Lock()
		publications = append(publications, publication{
			source:    file == sourceFile,
			name:      file.Name(),
			offset:    offset,
			open:      statErr == nil,
			signature: entry,
		})
		lock.Unlock()

		// Preserve the real xattr write so the final files can be checked independently.
		return signature.WriteFile(file, entry)
	}

	// Wait for the stream to publish and close every target descriptor.
	item := newFixtureItem(input, targets...)
	if err := runFixture(
		context.Background(),
		newStreamFixture(item),
		[]Item{item},
		WithHashPolicy(HashReadRefresh),
		withFS(hooks),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := item.terminal(t); err != nil {
		t.Fatalf("item failed: %v", err)
	}

	// Every file publishes its own entry, through its own open descriptor, carrying the hash this
	// item computed from the whole source.
	lock.Lock()
	defer lock.Unlock()
	if len(publications) != len(targets) {
		t.Fatalf("published %d cache entries, want one per file: %#v", len(publications), publications)
	}
	var sourcePublished, targetsPublished int
	for _, published := range publications {
		if !published.open {
			t.Fatalf("published %#v through a closed descriptor", published)
		}
		if !published.signature.Equal(want) {
			t.Fatalf("published signature = %#v, want the computed %#v", published.signature, want)
		}
		if published.offset != int64(len(content)) {
			t.Fatalf(
				"published through a descriptor at offset %d, want %d: the entry must travel through the descriptor that handled the whole content",
				published.offset, len(content),
			)
		}
		if published.source {
			sourcePublished++
			continue
		}
		for _, target := range targets {
			if published.name == filepath.Join(resolvedRoot, filepath.Base(target)) {
				t.Fatalf("cache published through final path %q, want the temporary writer", target)
			}
		}
		if filepath.Dir(published.name) != resolvedRoot {
			t.Fatalf("target descriptor path = %q, want a temporary file beside the target", published.name)
		}
		targetsPublished++
	}
	if sourcePublished != 0 || targetsPublished != len(targets) {
		t.Fatalf("publications = %#v, want one descriptor publication per target", publications)
	}

	// Every entry describes its own file afterwards, which is the whole point of publishing it.
	for _, path := range append([]string{input}, targets...) {
		stored, valid := readStoredSignature(t, path)
		if !valid || !stored.Equal(want) {
			t.Fatalf("stored signature for %q = %#v, valid=%t, want %#v", path, stored, valid, want)
		}
	}
}

// blockingHash keeps the first content hash of an item incomplete until the test releases it, so a
// test can observe what an item does while its hash is still unknown.
type blockingHash struct {
	hash.Hash

	started chan struct{}
	release chan struct{}

	startOnce *sync.Once
}

func (h *blockingHash) Write(p []byte) (int, error) {
	h.startOnce.Do(func() { close(h.started) })
	<-h.release

	return h.Hash.Write(p)
}

// holdContentHash makes the run's hash consumer wait on the returned release function before it
// hashes its first chunk. It reports when the consumer has started, so a test knows the item's hash
// is being held back, and it restores the pool when the test ends. Callers must register later
// cleanup that releases the gates and joins their run before this helper restores the global pool.
func holdContentHash(t *testing.T) (started <-chan struct{}, released <-chan struct{}, release func()) {
	t.Helper()

	// Every concurrent file owns its own digest; only the observation and release gates are shared.
	previous := sha256Pool
	first, gate := make(chan struct{}), make(chan struct{})
	var startOnce sync.Once
	sha256Pool = &sync.Pool{New: func() interface{} {
		return &blockingHash{Hash: sha256.New(), started: first, release: gate, startOnce: &startOnce}
	}}

	// Restore the original pool only after tests have joined their transfer workers.
	unblock := onceRelease(gate)
	t.Cleanup(func() { unblock(); sha256Pool = previous })
	return first, gate, unblock
}

// TestRunHoldsTargetCacheUntilTheHashIsComplete pins the ordering a target's cache entry depends
// on: publication waits until the item's hash is complete, so the entry always describes the
// whole content. The run is held back at the hash, and an item that completed anyway
// would have published an entry it could not describe.
func TestRunHoldsTargetCacheUntilTheHashIsComplete(t *testing.T) {
	// Hold a real file's digest while its linear target advances independently.
	requireSignatureXattrSupport(t)
	content := bytes.Repeat([]byte{'c'}, 4096)
	root := t.TempDir()
	input := writeSourceFile(t, root, "source.txt", content)
	target := filepath.Join(root, "target.txt")
	started, released, release := holdContentHash(t)

	// A linear target skips the durability sync, so its writer has nothing left to do but publish
	// by the time the hash is held back.
	item := newFixtureItem(input, target)
	done := make(chan struct{})
	var runErr error
	t.Cleanup(func() { release(); <-done })
	go func() {
		defer close(done)
		runErr = runFixture(
			context.Background(),
			newStreamFixture(item),
			[]Item{item},
			WithHashPolicy(HashReadRefresh),
			SetToDevice(LinearDevice(true)),
		)
	}()

	// Confirm the hash gate is occupied before checking target completion.
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the hash consumer never took the held hash")
	}

	// The item cannot finish while its hash is unknown, and the writer must be waiting instead of
	// publishing the entry it cannot describe yet.
	select {
	case <-done:
		t.Fatalf("the run finished while the item's hash was held back: %v", runErr)
	case <-time.After(250 * time.Millisecond):
	}
	select {
	case <-released:
		t.Fatal("the held hash was released early")
	default:
	}

	// Releasing the hash lets the item finish and publish the entry of every file it handled.
	release()
	select {
	case <-done:
		if runErr != nil {
			t.Fatalf("run error = %v, want nil", runErr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the run never finished after the hash was released")
	}

	// Both stored entries must describe the completed content before the result is accepted.
	if _, err := item.terminal(t); err != nil {
		t.Fatalf("item failed: %v", err)
	}
	for _, path := range []string{input, target} {
		signature, valid := readStoredSignature(t, path)
		if sum := sha256.Sum256(content); !valid || !bytes.Equal(signature.SHA256, sum[:]) {
			t.Fatalf("stored signature for %q = %#v, valid=%t, want %x", path, signature, valid, sha256.Sum256(content))
		}
	}
}

// TestRunRefreshKeepsLinearTargetsWorking pins the cache refresh against a linear target: the
// writer waits for the item's hash before it closes, so a linear run still completes every item and
// publishes the entry of every file it wrote.
func TestRunRefreshKeepsLinearTargetsWorking(t *testing.T) {
	requireSignatureXattrSupport(t)

	root := t.TempDir()
	const total = 4
	items := make([]Item, 0, total)
	fixtures := make([]*fixtureItem, 0, total)
	want := make(map[string][32]byte, total)
	for idx := 0; idx < total; idx++ {
		content := []byte(fmt.Sprintf("linear cache fixture %d", idx))
		source := writeSourceFile(t, root, fmt.Sprintf("source-%02d.bin", idx), content)
		target := filepath.Join(root, "target", fmt.Sprintf("%02d.bin", idx))
		item := newFixtureItem(source, target)
		items = append(items, item)
		fixtures = append(fixtures, item)
		want[target] = sha256.Sum256(content)
	}

	if err := runFixture(
		context.Background(),
		newStreamFixture(fixtures...),
		items,
		WithHashPolicy(HashReadRefresh),
		SetToDevice(LinearDevice(true)),
	); err != nil {
		t.Fatalf("run error = %v, want nil", err)
	}

	for _, item := range fixtures {
		result, err := item.terminal(t)
		if err != nil {
			t.Fatalf("item %q failed: %v", item.source, err)
		}
		if len(result.Targets) != 1 || result.Targets[0].Err != nil {
			t.Fatalf("targets of %q = %#v, want one written target", item.source, result.Targets)
		}
		target := result.Targets[0].Path
		stored, valid := readStoredSignature(t, target)
		if sum := want[target]; !valid || !bytes.Equal(stored.SHA256, sum[:]) {
			t.Fatalf("target %q signature = %x, valid=%t, want %x", target, stored.SHA256, valid, sum)
		}
	}
}

// TestRunPublishesTheCacheEntryBeforeTheResult pins that nothing about cache writing survives the
// item: by the time an item's result reaches the caller, the entry it computed is already stored.
func TestRunPublishesTheCacheEntryBeforeTheResult(t *testing.T) {
	requireSignatureXattrSupport(t)

	content := []byte("publication order fixture")
	input := writeSourceFile(t, t.TempDir(), "source.txt", content)
	want := sha256.Sum256(content)

	var (
		lock      sync.Mutex
		delivered []Result
		observed  []signature.Entry
		valid     []bool
	)
	onResults := func(results []Result) error {
		for _, result := range results {
			// The callback is the caller's first view of the item, and the entry is already
			// there: the item published it before it completed.
			signature, ok, err := signature.ReadCached(input)
			lock.Lock()
			delivered = append(delivered, result)
			observed = append(observed, signature)
			valid = append(valid, ok && err == nil)
			lock.Unlock()
		}
		return nil
	}

	// A result batch of one delivers the item as soon as it is finished, instead of when the run
	// closes, so the observation happens while the pipeline is still open.
	item := newFixtureItem(input)
	stream, err := NewStream(context.Background(), onResults, WithHashPolicy(HashReadRefresh), WithResultBuffer(1), WithResultBatch(1))
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Submit(item); err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := stream.Wait(); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}

	lock.Lock()
	defer lock.Unlock()
	if len(delivered) != 1 || delivered[0].Err != nil {
		t.Fatalf("delivered %#v, want one completed result", delivered)
	}
	if len(valid) != 1 || !valid[0] || !bytes.Equal(observed[0].SHA256, want[:]) {
		t.Fatalf("the entry observed with the result = %#v, valid=%v, want %x", observed, valid, want)
	}
}
