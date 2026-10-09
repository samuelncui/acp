package acp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type sumHookHash struct {
	hash.Hash
	onSum func([]byte)
}

func (h *sumHookHash) Sum(prefix []byte) []byte {
	// Observe completed hashes without changing the implementation's bytes.
	result := h.Hash.Sum(prefix)
	h.onSum(result)
	return result
}

type countedSource struct {
	io.ReadCloser
	bytes  *atomic.Int64
	closes *atomic.Int32
}

func (r *countedSource) Read(data []byte) (int, error) {
	// Count actual source bytes, excluding later in-memory delivery.
	n, err := r.ReadCloser.Read(data)
	r.bytes.Add(int64(n))
	return n, err
}

func (r *countedSource) Close() error {
	// Record every close attempt while preserving the real descriptor error.
	r.closes.Add(1)
	return r.ReadCloser.Close()
}

func TestLinearCopyPreparesHashWhilePriorTargetFinishes(t *testing.T) {
	// Observe the second hash while the first target is still completing metadata.
	trackChunkPool(t)
	root := t.TempDir()
	first := bytes.Repeat([]byte{'a'}, 256<<10)
	second := bytes.Repeat([]byte{'b'}, 256<<10)
	wantSecond := sha256.Sum256(second)
	one := newFixtureItem(writeSourceFile(t, root, "one", first), filepath.Join(root, "target-one"))
	two := newFixtureItem(writeSourceFile(t, root, "two", second), filepath.Join(root, "target-two"))
	blocked, release, secondHashed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var signal sync.Once
	var hashCount, closeCount atomic.Int32
	var sourceBytes atomic.Int64
	oldHash, oldRestore, oldOpen := sha256Pool, restoreTarget, openSourceContent
	t.Cleanup(func() { sha256Pool, restoreTarget, openSourceContent = oldHash, oldRestore, oldOpen })
	sha256Pool = &sync.Pool{New: func() interface{} {
		return &sumHookHash{Hash: sha256.New(), onSum: func(value []byte) {
			hashCount.Add(1)
			if bytes.Equal(value, wantSecond[:]) {
				signal.Do(func() { close(secondHashed) })
			}
		}}
	}}
	openSourceContent = func(path string, mode ReadMode, info os.FileInfo) (itemSource, error) {
		// Keep the actual descriptor so cache and close ownership remain exercised.
		source, err := oldOpen(path, mode, info)
		if err == nil {
			source.reader = &countedSource{ReadCloser: source.reader, bytes: &sourceBytes, closes: &closeCount}
		}
		return source, err
	}
	restoreTarget = func(path string, info *stat) error {
		// A target already receiving data must not stop later source preparation.
		if info.info.Name() == "one" {
			close(blocked)
			<-release
		}
		return oldRestore(path, info)
	}

	// Run the existing ordered stream and always release and join it before restoring hooks.
	done := make(chan error, 1)
	go func() {
		done <- runFixture(context.Background(), newStreamFixture(one, two), []Item{one, two},
			WithHashPolicy(HashReadRefresh), SetToDevice(LinearDevice(true)))
	}()
	select {
	case <-blocked:
	case <-time.After(5 * time.Second):
		t.Error("first target did not reach metadata completion")
	}
	select {
	case <-secondHashed:
	case <-time.After(time.Second):
		t.Error("next file hash remained blocked behind prior target completion")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	// Every source byte is read once and hashed once; both targets retain correct outcomes.
	if sourceBytes.Load() != int64(len(first)+len(second)) || hashCount.Load() != 2 || closeCount.Load() != 2 {
		t.Fatalf("source bytes=%d hashes=%d closes=%d", sourceBytes.Load(), hashCount.Load(), closeCount.Load())
	}
	for index, item := range []*fixtureItem{one, two} {
		content := [][]byte{first, second}[index]
		want := sha256.Sum256(content)
		result, err := item.terminal(t)
		if err != nil || !bytes.Equal(result.SHA256, want[:]) || len(result.Targets) != 1 || result.Targets[0].Err != nil {
			t.Fatalf("unexpected result: %+v / %v", result, err)
		}
		actual, err := os.ReadFile(item.targets[0])
		if err != nil || !bytes.Equal(actual, content) {
			t.Fatalf("target content differs: %v", err)
		}
	}
}

func TestLinearCopyPreservesHashWhenTargetsRejectPreparation(t *testing.T) {
	// A rejected target must not discard the hash or source-cache work completed for the item.
	requireSignatureXattrSupport(t)
	trackChunkPool(t)
	root := t.TempDir()
	content := bytes.Repeat([]byte{'r'}, 256<<10)
	source := writeSourceFile(t, root, "source", content)
	item := newFixtureItem(source, filepath.Join(root, "target"))
	want := sha256.Sum256(content)
	copyer, err := NewStream(context.Background(), newStreamFixture(item).onResults,
		WithHashPolicy(HashReadRefresh), SetToDevice(LinearDevice(true)))
	if err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("capacity observation failed")
	copyer.getDevice = func(string) (string, error) { return root, nil }
	copyer.linearSpace = map[string]*spaceEstimate{root: {err: sentinel}}

	// Fail before output creation and still finish the ordinary source lifecycle.
	if err := copyer.Submit(item); err != nil {
		t.Fatal(err)
	}
	if err := copyer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := copyer.Wait(); err != nil {
		t.Fatal(err)
	}
	result, err := item.terminal(t)
	if err != nil || !bytes.Equal(result.SHA256, want[:]) || !errors.Is(result.Targets[0].Err, sentinel) {
		t.Fatalf("hash/target failure changed: %+v / %v", result, err)
	}

	// Source refresh stays valid even though no destination accepted this file.
	file, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	encoded, err := readSignatureXattr(file)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := DecodeCachedSignature(encoded)
	if err != nil || stored.SHA256 != want || stored.Size != int64(len(content)) {
		t.Fatalf("source signature = %+v / %v", stored, err)
	}
}

type preparationFaultReader struct {
	io.ReadCloser
	read func([]byte) (int, error)
}

func (r *preparationFaultReader) Read(data []byte) (int, error) {
	return r.read(data)
}

func TestLinearPreparedContentReleasesFailures(t *testing.T) {
	for _, failure := range []string{"read error", "read panic", "hash panic", "close error"} {
		t.Run(failure, func(t *testing.T) {
			// Inject failures into the retained original source and observe its one close.
			captureLogs(t)
			trackChunkPool(t)
			root := t.TempDir()
			content := bytes.Repeat([]byte{'f'}, 4096)
			item := newFixtureItem(writeSourceFile(t, root, "source", content), filepath.Join(root, "target"))
			sentinel := errors.New("prepared source failed")
			var sourceBytes atomic.Int64
			var closes atomic.Int32
			oldOpen, oldHash := openSourceContent, sha256Pool
			t.Cleanup(func() { openSourceContent, sha256Pool = oldOpen, oldHash })
			openSourceContent = func(path string, mode ReadMode, info os.FileInfo) (itemSource, error) {
				// Wrap the same descriptor; errors and panics must not leak it or its content buffer.
				source, err := oldOpen(path, mode, info)
				if err != nil {
					return source, err
				}
				tracked := &countedSource{ReadCloser: source.reader, bytes: &sourceBytes, closes: &closes}
				source.reader = tracked
				switch failure {
				case "read error":
					source.reader = &preparationFaultReader{ReadCloser: tracked, read: func(data []byte) (int, error) {
						return copy(data, content[:17]), sentinel
					}}
				case "read panic":
					source.reader = &preparationFaultReader{ReadCloser: tracked, read: func([]byte) (int, error) {
						panic(sentinel)
					}}
				case "close error":
					source.reader = &errorOnClose{ReadCloser: tracked, err: sentinel}
				}
				return source, nil
			}
			if failure == "hash panic" {
				sha256Pool = &sync.Pool{New: func() interface{} { return panickingHash{Hash: sha256.New()} }}
				sentinel = errPanickingHash
			}

			// Complete the real stream so error replay, cache policy and cleanup all run.
			err := runFixture(context.Background(), newStreamFixture(item), []Item{item},
				WithHashPolicy(HashReadRefresh), SetToDevice(LinearDevice(true)))
			if closes.Load() != 1 {
				t.Fatalf("source close count = %d, want 1", closes.Load())
			}
			if failure == "read panic" || failure == "hash panic" {
				if !errors.Is(err, sentinel) {
					t.Fatalf("run error = %v, want panic %v", err, sentinel)
				}
				return
			}

			// Ordinary read failures stay target errors; source-close failures stay item errors.
			if err != nil {
				t.Fatal(err)
			}
			result, itemErr := item.terminal(t)
			if failure == "close error" {
				if !errors.Is(itemErr, sentinel) {
					t.Fatalf("item error = %v, want source-close failure", itemErr)
				}
				return
			}
			if itemErr != nil || len(result.SHA256) != 0 || !errors.Is(result.Targets[0].Err, sentinel) {
				t.Fatalf("partial read outcome = %+v / %v", result, itemErr)
			}
			if _, err := os.Stat(item.targets[0]); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed target remains: %v", err)
			}
		})
	}
}

type errorOnClose struct {
	io.ReadCloser
	err error
}

func (r *errorOnClose) Close() error {
	// Release the real resource before reporting the injected close failure.
	return errors.Join(r.ReadCloser.Close(), r.err)
}
