package acp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
)

// failNativeSource makes reads of one native source fail with err.
func failNativeSource(t *testing.T, path string, err error) {
	t.Helper()
	previous := openSourceContent
	openSourceContent = func(name string, mode ReadMode, info os.FileInfo) (itemSource, error) {
		source, openErr := previous(name, mode, info)
		if openErr == nil && name == path {
			source.reader = failingReader{ReadCloser: source.reader, err: err}
		}
		return source, openErr
	}
	t.Cleanup(func() { openSourceContent = previous })
}

func TestSourceFailuresAreTaggedApartFromTargetFailures(t *testing.T) {
	// A native target reports a source read failure as its own error; the tag tells them apart.
	root, out := t.TempDir(), t.TempDir()
	bad := writeSourceFile(t, root, "bad", []byte("bad content"))
	good := writeSourceFile(t, root, "good", []byte("good content"))
	failNativeSource(t, bad, syscall.EIO)
	blocked := filepath.Join(out, "blocked")
	if err := os.WriteFile(blocked, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	badCopy := &SimpleJob{Path: bad, Dsts: []string{filepath.Join(out, "bad")}}
	badVerify := &SimpleJob{Path: bad}
	targetFailure := &SimpleJob{Path: good, Dsts: []string{filepath.Join(blocked, "under-a-file")}}
	onResults, snapshot := collectResults()
	if err := runStream(context.Background(), onResults, []Item{badCopy, badVerify, targetFailure}, WithHashPolicy(HashRead)); err != nil {
		t.Fatal(err)
	}
	results := resultsByItem(t, snapshot())
	if err := results[badCopy].Targets[0].Err; !errors.Is(err, ErrSourceRead) || !errors.Is(err, syscall.EIO) {
		t.Fatalf("source failure on a target = %v", err)
	}
	if err := results[badVerify].Err; !errors.Is(err, ErrSourceRead) || !errors.Is(err, syscall.EIO) {
		t.Fatalf("source failure of a targetless item = %v", err)
	}
	if err := results[targetFailure].Targets[0].Err; err == nil || errors.Is(err, ErrSourceRead) {
		t.Fatalf("target failure = %v, want an error without the source tag", err)
	}
}

// memberWriter collects one content target's bytes and settles a failed member when asked.
type memberWriter struct {
	lock    sync.Mutex
	members map[string][]byte
	settle  bool
}

func (w *memberWriter) target(name string) ContentTarget {
	return testContentTarget(func(context.Context, ContentMetadata) (ContentWriter, error) {
		var buffer bytes.Buffer
		return &testContentWriter{Writer: &buffer, finish: func(cause error) error {
			if cause != nil {
				if w.settle {
					return fmt.Errorf("%w, member %s padded", ErrMemberSettled, name)
				}
				return cause
			}
			w.lock.Lock()
			defer w.lock.Unlock()
			w.members[name] = buffer.Bytes()
			return nil
		}}, nil
	})
}

func TestContentTargetSettledFailureEndsOnlyItsMember(t *testing.T) {
	// A native source that fails while a content target writes it stops the stream unless the
	// target's Finish settles the member.
	for _, settle := range []bool{true, false} {
		t.Run(fmt.Sprintf("settle=%t", settle), func(t *testing.T) {
			root := t.TempDir()
			paths := []string{
				writeSourceFile(t, root, "a", []byte("first")),
				writeSourceFile(t, root, "b", []byte("second")),
				writeSourceFile(t, root, "c", []byte("third")),
			}
			failNativeSource(t, paths[1], syscall.EIO)
			writer := &memberWriter{members: map[string][]byte{}, settle: settle}
			items := make([]Item, len(paths))
			for i, path := range paths {
				name := filepath.Base(path)
				items[i] = &testContentItem{SimpleJob: &SimpleJob{Path: path, Dsts: []string{"member-" + name}},
					targets: []ContentTarget{writer.target(name)}}
			}

			onResults, snapshot := collectResults()
			runErr := runStream(context.Background(), onResults, items, SetToDevice(LinearDevice(true)), WithHashPolicy(HashRead))
			results := resultsByItem(t, snapshot())
			failed := results[items[1]].Targets[0].Err
			if !errors.Is(failed, ErrSourceRead) || !errors.Is(failed, syscall.EIO) {
				t.Fatalf("failed member error = %v", failed)
			}
			if settle {
				if runErr != nil || !errors.Is(failed, ErrMemberSettled) {
					t.Fatalf("run error = %v, failed member error = %v", runErr, failed)
				}
				if string(writer.members["a"]) != "first" || string(writer.members["c"]) != "third" {
					t.Fatalf("members = %q", writer.members)
				}
				return
			}
			if !errors.Is(runErr, syscall.EIO) || results[items[2]].Targets[0].Err == nil || writer.members["c"] != nil {
				t.Fatalf("run error = %v, later member = %#v", runErr, results[items[2]])
			}
		})
	}
}

func TestContentSourceSettledFailureEndsOnlyItsMember(t *testing.T) {
	// A content source that settles its failure in Read and Close keeps the stream admitting
	// members; an unsettled failure still stops it.
	for _, settle := range []bool{true, false} {
		t.Run(fmt.Sprintf("settle=%t", settle), func(t *testing.T) {
			out := t.TempDir()
			failure := error(syscall.EIO)
			if settle {
				failure = errors.Join(ErrMemberSettled, syscall.EIO)
			}
			member := func(data string, broken bool) ContentSource {
				return testContentSource(func(context.Context) (fs.File, error) {
					reader := io.Reader(bytes.NewReader([]byte(data)))
					closeErr := error(nil)
					if broken {
						reader, closeErr = io.MultiReader(bytes.NewReader([]byte(data[:2])), failingReader{err: failure}), failure
					}
					return &testContentFile{
						ReadCloser: &testContentReader{Reader: reader, close: func() error { return closeErr }},
						metadata:   ContentMetadata{Info: contentInfo{name: "member", size: int64(len(data)), mode: 0o600}},
					}, nil
				})
			}
			items := make([]Item, 3)
			for i, data := range []string{"first", "second", "third"} {
				items[i] = &testContentItem{SimpleJob: &SimpleJob{Path: fmt.Sprintf("member-%d", i), Dsts: []string{filepath.Join(out, data)}},
					source: member(data, i == 1)}
			}

			onResults, snapshot := collectResults()
			runErr := runStream(context.Background(), onResults, items, SetFromDevice(LinearDevice(true)), WithHashPolicy(HashRead))
			results := resultsByItem(t, snapshot())
			if err := results[items[1]].Targets[0].Err; !errors.Is(err, ErrSourceRead) || !errors.Is(err, syscall.EIO) {
				t.Fatalf("failed member error = %v", err)
			}
			if _, err := os.Stat(filepath.Join(out, "second")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed member output exists: %v", err)
			}
			_, thirdErr := os.Stat(filepath.Join(out, "third"))
			if settle {
				if runErr != nil || thirdErr != nil || results[items[2]].Targets[0].Err != nil {
					t.Fatalf("run error = %v, third output = %v, result = %#v", runErr, thirdErr, results[items[2]])
				}
				return
			}
			if !errors.Is(runErr, syscall.EIO) || thirdErr == nil {
				t.Fatalf("run error = %v, third output = %v", runErr, thirdErr)
			}
		})
	}
}

func TestContentTargetSettledOpenRefusalEndsOnlyItsMember(t *testing.T) {
	// A content target that refuses one member before emitting anything keeps admitting members.
	root := t.TempDir()
	writer := &memberWriter{members: map[string][]byte{}}
	refused := fmt.Errorf("%w, unsupported member name", ErrMemberSettled)
	items := make([]Item, 3)
	for i, data := range []string{"first", "second", "third"} {
		name := data
		target := writer.target(name)
		if i == 1 {
			target = testContentTarget(func(context.Context, ContentMetadata) (ContentWriter, error) { return nil, refused })
		}
		items[i] = &testContentItem{SimpleJob: &SimpleJob{Path: writeSourceFile(t, root, name, []byte(data)), Dsts: []string{"member-" + name}},
			targets: []ContentTarget{target}}
	}
	onResults, snapshot := collectResults()
	if err := runStream(context.Background(), onResults, items, SetToDevice(LinearDevice(true)), WithHashPolicy(HashRead)); err != nil {
		t.Fatal(err)
	}
	results := resultsByItem(t, snapshot())
	if err := results[items[1]].Targets[0].Err; !errors.Is(err, ErrMemberSettled) || errors.Is(err, ErrSourceRead) {
		t.Fatalf("refused member error = %v", err)
	}
	if string(writer.members["first"]) != "first" || string(writer.members["third"]) != "third" {
		t.Fatalf("members = %q", writer.members)
	}
}
