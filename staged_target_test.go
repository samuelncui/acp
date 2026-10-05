package acp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samuelncui/acp/internal/fileio"
)

type interruptedSource struct {
	*os.File
	read bool
}

func (r *interruptedSource) Read(buf []byte) (int, error) {
	if r.read {
		return 0, io.ErrClosedPipe
	}
	r.read = true
	return r.File.Read(buf)
}

func TestStagedOverwritePreservesOldTargetOnFailure(t *testing.T) {
	for _, phase := range []string{"allocation", "read", "metadata", "sync", "close", "rename"} {
		t.Run(phase, func(t *testing.T) {
			// Every failure happens after output acquisition, including a read after one full chunk.
			root := t.TempDir()
			source := writeSourceFile(t, root, "source", bytes.Repeat([]byte("n"), 2*batchSize))
			old := []byte("original target content")
			target := writeSourceFile(t, root, "target", old)
			before, err := os.Stat(target)
			if err != nil {
				t.Fatal(err)
			}
			oldAllocate, oldRestore, oldCommit, oldOpen := allocateTarget, restoreTarget, commitTarget, openSourceContent
			t.Cleanup(func() {
				allocateTarget, restoreTarget, commitTarget, openSourceContent = oldAllocate, oldRestore, oldCommit, oldOpen
			})
			sentinel := errors.New("injected output failure")
			switch phase {
			case "allocation":
				allocateTarget = func(*os.File, int64) error { return sentinel }
			case "read":
				openSourceContent = func(path string, mode ReadMode, info os.FileInfo) (itemSource, error) {
					src, err := oldOpen(path, mode, info)
					if err == nil {
						src.reader = &interruptedSource{File: src.file}
					}
					return src, err
				}
			case "metadata":
				restoreTarget = func(string, *stat) error { return sentinel }
			case "sync":
				allocateTarget = func(file *os.File, size int64) error {
					if err := oldAllocate(file, size); err != nil {
						return err
					}
					restoreTarget = func(path string, info *stat) error {
						if err := oldRestore(path, info); err != nil {
							return err
						}
						return file.Close()
					}
					return nil
				}
			case "close":
				commitTarget = func(out *fileio.Output, overwrite bool) error {
					_ = out.File.Close()
					return out.Commit(overwrite)
				}
			case "rename":
				commitTarget = func(out *fileio.Output, overwrite bool) error {
					if err := out.Close(); err != nil {
						return err
					}
					return sentinel
				}
			}

			// The public result names only the final path; cleanup must not replace or remove it.
			item := newFixtureItem(source, target)
			if err := runFixture(context.Background(), newStreamFixture(item), []Item{item}, Overwrite(true)); err != nil {
				t.Fatal(err)
			}
			result, err := item.terminal(t)
			if err != nil || len(result.Targets) != 1 || result.Targets[0].Path != target || result.Targets[0].Err == nil {
				t.Fatalf("outcome=%+v / %v", result, err)
			}
			data, err := os.ReadFile(target)
			if err != nil || !bytes.Equal(data, old) {
				t.Fatalf("old target changed: %q / %v", data, err)
			}
			after, err := os.Stat(target)
			if err != nil || !os.SameFile(before, after) {
				t.Fatalf("old target identity changed: %v", err)
			}
			leftovers, err := filepath.Glob(filepath.Join(root, ".tmp_*"))
			if err != nil || len(leftovers) != 0 {
				t.Fatalf("temporary files leaked: %v / %v", leftovers, err)
			}
		})
	}
}

func TestStagedCopyFollowsTargetLinkAndReplacesOneInode(t *testing.T) {
	// Replacing the referent retains the symlink and leaves its other hardlink's old content intact.
	root := t.TempDir()
	source := writeSourceFile(t, root, "source", []byte("new"))
	target := writeSourceFile(t, root, "target", []byte("old"))
	alias, link := filepath.Join(root, "alias"), filepath.Join(root, "link")
	if err := os.Link(target, alias); err != nil {
		t.Skipf("hardlinks unsupported: %v", err)
	}
	if err := os.Symlink("target", link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	fixed := time.Unix(1700000000, 0)
	if err := os.Chtimes(source, fixed, fixed); err != nil {
		t.Fatal(err)
	}
	previous := commitTarget
	commits := 0
	commitTarget = func(out *fileio.Output, overwrite bool) error {
		commits++
		data, err := os.ReadFile(target)
		if err != nil || string(data) != "old" {
			t.Errorf("target changed before commit: %q / %v", data, err)
		}
		info, err := out.File.Stat()
		if err != nil || info.Size() != 3 || !info.ModTime().Equal(fixed) {
			t.Errorf("temporary metadata incomplete: %v / %v", info, err)
		}
		if !strings.HasPrefix(filepath.Base(out.Temporary), ".tmp_") {
			t.Errorf("unexpected temporary name: %q", out.Temporary)
		}
		return previous(out, overwrite)
	}
	t.Cleanup(func() { commitTarget = previous })
	item := newFixtureItem(source, link)
	if err := runFixture(context.Background(), newStreamFixture(item), []Item{item}, Overwrite(true)); err != nil {
		t.Fatal(err)
	}
	result, err := item.terminal(t)
	if err != nil || result.Targets[0].Err != nil || result.Targets[0].Path != link || commits != 1 {
		t.Fatalf("outcome=%+v commits=%d / %v", result, commits, err)
	}
	for path, want := range map[string]string{target: "new", link: "new", alias: "old"} {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != want {
			t.Fatalf("%q=%q, want %q / %v", path, data, want, err)
		}
	}
	if value, err := os.Readlink(link); err != nil || value != "target" {
		t.Fatalf("target link replaced: %q / %v", value, err)
	}
}

func TestTargetDeviceResolvedOncePerItem(t *testing.T) {
	// Two target files share one parent; resolution must not repeat its filesystem walk.
	root := t.TempDir()
	source := writeSourceFile(t, root, "source", []byte("content"))
	item := newFixtureItem(source, filepath.Join(root, "one"), filepath.Join(root, "two"))
	stream, err := NewStream(context.Background(), newStreamFixture(item).onResults)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	calls := 0
	stream.getDevice = func(string) (string, error) { calls++; return root, nil }
	previous := resolveDirectory
	directories := 0
	resolveDirectory = func(path string) (string, error) { directories++; return previous(path) }
	t.Cleanup(func() { resolveDirectory = previous })

	// Each target resolves its device once, while both reuse the same parent observation.
	if err := stream.Submit(item); err != nil {
		t.Fatal(err)
	}
	if err := stream.Wait(); err != nil {
		t.Fatal(err)
	}
	result, err := item.terminal(t)
	if err != nil || calls != 2 || directories != 1 {
		t.Fatalf("device lookups=%d parent resolutions=%d / %v", calls, directories, err)
	}
	for _, target := range result.Targets {
		if target.Err != nil {
			t.Fatal(target.Err)
		}
	}
}

func TestStagedNoOverwriteHandlesTwoItemsForOneDestination(t *testing.T) {
	for _, linear := range []bool{false, true} {
		t.Run(map[bool]string{false: "parallel", true: "linear"}[linear], func(t *testing.T) {
			// Both items are indexed before either may commit. A directory alias names the same output.
			root := t.TempDir()
			dir, alias := filepath.Join(root, "data"), filepath.Join(root, "alias")
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(dir, alias); err != nil {
				t.Skipf("symlinks unsupported: %v", err)
			}
			first := newFixtureItem(writeSourceFile(t, root, "first", []byte("first")), filepath.Join(dir, "target"))
			second := newFixtureItem(writeSourceFile(t, root, "second", []byte("second")), filepath.Join(alias, "target"))
			fixture := newStreamFixture(first, second)
			stream, err := NewStream(context.Background(), fixture.onResults, SetToDevice(LinearDevice(linear)))
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
			previous := commitTarget
			allowCommit := make(chan struct{})
			commitTarget = func(out *fileio.Output, overwrite bool) error { <-allowCommit; return previous(out, overwrite) }
			t.Cleanup(func() { commitTarget = previous })
			err = stream.Submit(first, second)
			close(allowCommit)
			if err != nil {
				t.Fatal(err)
			}
			if err := stream.Wait(); err != nil {
				t.Fatal(err)
			}

			// Exactly one item wins; the other cannot replace its content or report a false success.
			succeeded, refused := 0, 0
			var winner string
			for _, item := range []*fixtureItem{first, second} {
				result, err := item.terminal(t)
				if err != nil || len(result.Targets) != 1 {
					t.Fatalf("outcome=%+v / %v", result, err)
				}
				if result.Targets[0].Err == nil {
					succeeded++
					winner = filepath.Base(item.source)
				} else if errors.Is(result.Targets[0].Err, os.ErrExist) {
					refused++
				} else {
					t.Fatal(result.Targets[0].Err)
				}
			}
			data, err := os.ReadFile(filepath.Join(dir, "target"))
			if err != nil || succeeded != 1 || refused != 1 || string(data) != winner {
				t.Fatalf("success=%d refused=%d content=%q winner=%q / %v", succeeded, refused, data, winner, err)
			}
			if linear && winner != "first" {
				t.Fatalf("linear target lost request order: winner=%q", winner)
			}
			if paths, err := filepath.Glob(filepath.Join(dir, ".tmp_*")); err != nil || len(paths) != 0 {
				t.Fatalf("temporary files leaked: %v / %v", paths, err)
			}
		})
	}
}

func TestStagedNoOverwriteCommitsIndependentTargetsConcurrently(t *testing.T) {
	// Both independent outputs must reach replacement before either is released.
	root := t.TempDir()
	item := newFixtureItem(writeSourceFile(t, root, "source", []byte("content")),
		filepath.Join(root, "first"), filepath.Join(root, "second"))
	fixture := newStreamFixture(item)
	stream, err := NewStream(context.Background(), fixture.onResults)
	if err != nil {
		t.Fatal(err)
	}
	previous := commitTarget
	started, release := make(chan struct{}, 2), make(chan struct{})
	commitTarget = func(out *fileio.Output, overwrite bool) error {
		started <- struct{}{}
		<-release
		return previous(out, overwrite)
	}
	defer func() {
		close(release)
		_ = stream.Close()
		commitTarget = previous
	}()
	if err := stream.Submit(item); err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for range 2 {
		select {
		case <-started:
		case <-deadline.C:
			t.Fatal("one target's replacement blocked an independent target")
		}
	}
}

func TestStagedTargetKeepsSymlinkParentTraversal(t *testing.T) {
	for _, tail := range []string{"existing", "new", "nested/new"} {
		t.Run(tail, func(t *testing.T) {
			// The kernel resolves link/.. to tree, while lexical cleaning would select root.
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, "tree", "child"), 0o755); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(root, "link")
			if err := os.Symlink(filepath.Join("tree", "child"), link); err != nil {
				t.Skipf("symlinks unsupported: %v", err)
			}
			source := writeSourceFile(t, root, "source", []byte("new content"))
			decoy := writeSourceFile(t, root, "existing", []byte("unrelated"))
			if tail == "existing" {
				writeSourceFile(t, filepath.Join(root, "tree"), tail, []byte("old content"))
			}
			path := link + string(filepath.Separator) + ".." + string(filepath.Separator) + filepath.FromSlash(tail)

			// Index inspection, replacement and the reported name must refer to the same file.
			item := newFixtureItem(source, path)
			if err := runFixture(context.Background(), newStreamFixture(item), []Item{item}, Overwrite(true)); err != nil {
				t.Fatal(err)
			}
			result, err := item.terminal(t)
			if err != nil || len(result.Targets) != 1 || result.Targets[0].Err != nil || result.Targets[0].Path != path {
				t.Fatalf("outcome=%+v / %v", result, err)
			}
			if data, err := os.ReadFile(filepath.Join(root, "tree", filepath.FromSlash(tail))); err != nil || string(data) != "new content" {
				t.Fatalf("native destination=%q / %v", data, err)
			}
			if data, err := os.ReadFile(decoy); err != nil || string(data) != "unrelated" {
				t.Fatalf("unrelated destination changed: %q / %v", data, err)
			}
		})
	}
}

func TestTargetDirectoryResolutionEvaluatesSymlinksOnce(t *testing.T) {
	// Missing descendants must not repeatedly walk the same existing ancestors.
	root := t.TempDir()
	path := filepath.Join(root, "missing", "child", "grandchild")
	previous := resolveDirectory
	calls := 0
	resolveDirectory = func(path string) (string, error) {
		calls++
		return previous(path)
	}
	t.Cleanup(func() { resolveDirectory = previous })
	resolved, err := resolveOutputPath(path)
	if err != nil {
		t.Fatal(err)
	}
	existing, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(existing, "missing", "child", "grandchild"); resolved != want || calls != 1 {
		t.Fatalf("resolved=%q want=%q symlink walks=%d", resolved, want, calls)
	}
}

func TestStagedTargetRejectsMissingDirectoryTraversal(t *testing.T) {
	for _, tail := range []string{"missing/../target", "directory/", "missing/.", "missing/.."} {
		t.Run(tail, func(t *testing.T) {
			// Native path traversal requires an existing parent before .. and a file basename.
			root := t.TempDir()
			source := writeSourceFile(t, root, "source", []byte("content"))
			path := root + string(filepath.Separator) + filepath.FromSlash(tail)
			item := newFixtureItem(source, path)
			if err := runFixture(context.Background(), newStreamFixture(item), []Item{item}); err != nil {
				t.Fatal(err)
			}

			// Refusal must not create a different destination by normalizing away the bad component.
			result, err := item.terminal(t)
			if err != nil || len(result.Targets) != 1 || result.Targets[0].Err == nil {
				t.Fatalf("outcome=%+v / %v", result, err)
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 1 || entries[0].Name() != "source" {
				t.Fatalf("unexpected output=%+v / %v", entries, err)
			}
		})
	}
}

func TestRewriteClosesSourceBeforeReplacement(t *testing.T) {
	for _, mode := range []ReadMode{ReadBuffered, ReadMapped} {
		t.Run(mode.String(), func(t *testing.T) {
			// Rewrite intentionally replaces the source; its descriptor and mapping must be released.
			path := writeSourceFile(t, t.TempDir(), "source", []byte("content"))
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			out, err := fileio.NewOutput(path)
			if err != nil {
				t.Fatal(err)
			}
			defer out.Discard()
			oldOpen, oldCommit := openSourceContent, commitTarget
			t.Cleanup(func() { openSourceContent, commitTarget = oldOpen, oldCommit })
			var source *os.File
			openSourceContent = func(path string, mode ReadMode, info os.FileInfo) (itemSource, error) {
				opened, err := oldOpen(path, mode, info)
				source = opened.file
				return opened, err
			}
			commitTarget = func(out *fileio.Output, overwrite bool) error {
				if _, err := source.Stat(); !errors.Is(err, os.ErrClosed) {
					return errors.New("source is still open at replacement")
				}
				return oldCommit(out, overwrite)
			}

			// Enforce the sharing restriction on every host instead of waiting for a Windows failure.
			var results []Result
			err = runStream(context.Background(), func(batch []Result) error { results = append(results, batch...); return nil },
				[]Item{&fileio.RewriteItem{Path: path, Info: info, Output: out}},
				Overwrite(true), WithHashPolicy(HashRead), SetFromDevice(WithReadMode(mode)))
			if err != nil || len(results) != 1 || results[0].Err != nil || len(results[0].Targets) != 1 || results[0].Targets[0].Err != nil {
				t.Fatalf("rewrite outcome=%+v / %v", results, err)
			}
		})
	}
}
