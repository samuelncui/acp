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
	"time"
)

func TestCopyRejectsSourceAliasesBeforeModification(t *testing.T) {
	for _, mode := range []ReadMode{ReadBuffered, ReadMapped} {
		for _, alias := range []string{"same path", "hardlink", "symlink"} {
			t.Run(fmt.Sprintf("%v/%s", mode, alias), func(t *testing.T) {
				// One alias is unsafe, but another target must still receive the complete source.
				root := t.TempDir()
				content := bytes.Repeat([]byte("unaltered source"), 1000)
				source := writeSourceFile(t, root, "source", content)
				fixed := time.Unix(1700000000, 0)
				if err := os.Chtimes(source, fixed, fixed); err != nil {
					t.Fatal(err)
				}
				target := source
				if alias != "same path" {
					target = filepath.Join(root, "alias")
					link := os.Link
					if alias == "symlink" {
						link = os.Symlink
					}
					if err := link(source, target); err != nil {
						t.Skipf("alias unsupported: %v", err)
					}
				}
				good := filepath.Join(root, "good")
				item := newFixtureItem(source, target, good)
				if err := runFixture(context.Background(), newStreamFixture(item), []Item{item}, Overwrite(true),
					WithHashPolicy(HashRead), SetFromDevice(WithReadMode(mode))); err != nil {
					t.Fatal(err)
				}

				// Content and metadata at the source survive; exactly the independent target succeeds.
				result, err := item.terminal(t)
				if err != nil || len(result.Targets) != 2 || result.Targets[0].Err == nil || result.Targets[1].Err != nil {
					t.Fatalf("result=%+v / %v", result, err)
				}
				for _, path := range []string{source, good} {
					data, err := os.ReadFile(path)
					if err != nil || !bytes.Equal(data, content) {
						t.Fatalf("content changed at %q: %v", path, err)
					}
				}
				info, err := os.Stat(source)
				if err != nil || !info.ModTime().Equal(fixed) {
					t.Fatalf("source metadata changed: %v / %v", info, err)
				}
			})
		}
	}
}

func TestMultipleTargetsDoNotQuerySpaceOrWait(t *testing.T) {
	// Ordinary targets use actual allocation errors, with no preflight estimate or reservation.
	root := t.TempDir()
	source := writeSourceFile(t, root, "source", bytes.Repeat([]byte("x"), 4096))
	first, second := filepath.Join(root, "first"), filepath.Join(root, "second")
	item := newFixtureItem(source, first, second)
	fixture := newStreamFixture(item)
	copyer, err := NewStream(context.Background(), fixture.onResults, WithHashPolicy(HashRead))
	if err != nil {
		t.Fatal(err)
	}
	copyer.availableSpace = func(string) (int64, error) { panic("unexpected space estimate for random targets") }
	done := make(chan error, 1)
	go func() {
		if err := copyer.Submit(item); err != nil {
			done <- err
			return
		}
		_ = copyer.Close()
		done <- copyer.Wait()
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("target preparation waited for the same item's writers")
	}

	// Both independent real allocations succeeded.
	result, err := item.terminal(t)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range result.Targets {
		if target.Err != nil {
			t.Fatal(target.Err)
		}
	}
}

func TestPreallocationFailureCleansOnlyOwnedOutput(t *testing.T) {
	// Inject a real-classified allocation error while preserving independent targets on the same mount.
	root := t.TempDir()
	source := writeSourceFile(t, root, "source", []byte("new content"))
	existing := writeSourceFile(t, root, "existing", []byte("old content"))
	owned, good := filepath.Join(root, "owned"), filepath.Join(root, "good")
	previous := allocateTarget
	calls := 0
	allocateTarget = func(file *os.File, size int64) error {
		calls++
		if calls == 3 {
			return previous(file, size)
		}
		return syscall.EIO
	}
	t.Cleanup(func() { allocateTarget = previous })
	item := newFixtureItem(source, existing, owned, good)
	if err := runFixture(context.Background(), newStreamFixture(item), []Item{item}, Overwrite(true)); err != nil {
		t.Fatal(err)
	}
	result, err := item.terminal(t)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range result.Targets[:2] {
		if !errors.Is(target.Err, syscall.EIO) || !errors.Is(target.Err, ErrTargetIO) {
			t.Fatalf("wrong identity: %v", target.Err)
		}
	}
	if result.Targets[2].Err != nil {
		t.Fatal(result.Targets[2].Err)
	}
	if _, err := os.Stat(existing); err != nil {
		t.Fatalf("preexisting file removed: %v", err)
	}
	if _, err := os.Stat(owned); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("exclusive output survived: %v", err)
	}
}

func TestSourceDescriptorClosesWhenCacheOperationPanics(t *testing.T) {
	for _, stage := range []string{"lookup", "publication"} {
		t.Run(stage, func(t *testing.T) {
			// Inject a panic after the item has acquired its real source descriptor.
			root := t.TempDir()
			source := writeSourceFile(t, root, "source", []byte("cache panic fixture"))
			oldRead, oldWrite := readManagedXattr, writeManagedXattr
			sentinel := errors.New("cache operation panic")
			var opened *os.File
			policy := HashCachedOnly
			if stage == "lookup" {
				readManagedXattr = func(file *os.File) ([]byte, error) { opened = file; panic(sentinel) }
			} else {
				policy = HashReadRefresh
				readManagedXattr = func(file *os.File) ([]byte, error) { return nil, errSignatureXattrUnsupported }
				writeManagedXattr = func(file *os.File, value []byte) error { opened = file; panic(sentinel) }
			}
			t.Cleanup(func() { readManagedXattr, writeManagedXattr = oldRead, oldWrite })
			item := newFixtureItem(source)
			err := runFixture(context.Background(), newStreamFixture(item), []Item{item}, WithHashPolicy(policy))
			if !errors.Is(err, sentinel) {
				t.Fatalf("panic identity=%v", err)
			}

			// A fatal cache failure can abandon a result, but must not abandon the descriptor it owns.
			if opened == nil {
				t.Fatal("cache operation was not reached")
			}
			if _, err := opened.Stat(); !errors.Is(err, os.ErrClosed) {
				t.Fatalf("source descriptor leaked: %v", err)
			}
		})
	}
}

type panicSourceReader struct{ *os.File }

func (r *panicSourceReader) Read([]byte) (int, error) { panic("source reader panic") }

func TestSourceReadPanicReleasesBuffersAndTargets(t *testing.T) {
	// Instrument pooled references and the real source descriptor before injecting a read panic.
	trackChunkPool(t)
	root := t.TempDir()
	source := writeSourceFile(t, root, "source", bytes.Repeat([]byte("x"), 4096))
	target := filepath.Join(root, "target")
	old := openSourceContent
	var opened *os.File
	openSourceContent = func(path string, mode ReadMode, info os.FileInfo) (itemSource, error) {
		src, err := old(path, mode, info)
		if err == nil {
			opened = src.file
			src.reader = &panicSourceReader{src.file}
		}
		return src, err
	}
	t.Cleanup(func() { openSourceContent = old })
	item := newFixtureItem(source, target)
	err := runFixture(context.Background(), newStreamFixture(item), []Item{item}, WithHashPolicy(HashRead))
	if err == nil {
		t.Fatal("read panic succeeded")
	}
	if opened == nil {
		t.Fatal("source did not open")
	}
	if _, err := opened.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("source leaked: %v", err)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed output survived: %v", err)
	}
}
