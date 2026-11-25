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
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// failNativeSource makes reads of one native source fail with err.
func failNativeSource(path string, err error) Option {
	return withFS(fsHooks{open: func(name string, mode ReadMode, info os.FileInfo) (itemSource, error) {
		source, openErr := openNativeSource(name, mode, info)
		if openErr == nil && name == path {
			source.reader = failingReader{ReadCloser: source.reader, err: err}
		}
		return source, openErr
	}})
}

func TestSourceFailuresAreTaggedApartFromTargetFailures(t *testing.T) {
	// A native target reports a source read failure as its own error; the tag tells them apart.
	root, out := t.TempDir(), t.TempDir()
	bad := writeSourceFile(t, root, "bad", []byte("bad content"))
	good := writeSourceFile(t, root, "good", []byte("good content"))
	failBad := failNativeSource(bad, syscall.EIO)
	blocked := filepath.Join(out, "blocked")
	if err := os.WriteFile(blocked, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	badCopy := &SimpleJob{Path: bad, Dsts: []string{filepath.Join(out, "bad")}}
	badVerify := &SimpleJob{Path: bad}
	targetFailure := &SimpleJob{Path: good, Dsts: []string{filepath.Join(blocked, "under-a-file")}}
	onResults, snapshot := collectResults()
	if err := runStream(context.Background(), onResults, []Item{badCopy, badVerify, targetFailure}, WithHashPolicy(HashRead), failBad); err != nil {
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
			writer := &memberWriter{members: map[string][]byte{}, settle: settle}
			items := make([]Item, len(paths))
			for i, path := range paths {
				name := filepath.Base(path)
				items[i] = &testContentItem{SimpleJob: &SimpleJob{Path: path, Dsts: []string{"member-" + name}},
					targets: []ContentTarget{writer.target(name)}}
			}

			onResults, snapshot := collectResults()
			runErr := runStream(context.Background(), onResults, items, SetToDevice(LinearDevice(true)), WithHashPolicy(HashRead), failNativeSource(paths[1], syscall.EIO))
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

func TestFinishThatReturnsItsCauseDoesNotInheritTheSourceSettlement(t *testing.T) {
	// A writer that returns or wraps its cause has not settled the target stream, even though the
	// cause carries the source's own ErrMemberSettled.
	for _, finish := range map[string]func(error) error{
		"returns cause": func(cause error) error { return cause },
		"wraps cause":   func(cause error) error { return fmt.Errorf("finish member: %w", cause) },
		"joins cause":   func(cause error) error { return errors.Join(errors.New("framing incomplete"), cause) },
	} {
		writer := &memberWriter{members: map[string][]byte{}}
		items := make([]Item, 3)
		for i, data := range []string{"first", "second", "third"} {
			target := writer.target(data)
			failure := error(nil)
			if i == 1 {
				failure = errors.Join(ErrMemberSettled, syscall.EIO)
				target = testContentTarget(func(context.Context, ContentMetadata) (ContentWriter, error) {
					return &testContentWriter{Writer: io.Discard, finish: finish}, nil
				})
			}
			items[i] = &testContentItem{SimpleJob: &SimpleJob{Path: fmt.Sprintf("member-%d", i), Dsts: []string{"out-" + data}},
				source: contentMember(data, failure), targets: []ContentTarget{target}}
		}
		onResults, _ := collectResults()
		err := runStream(context.Background(), onResults, items, SetFromDevice(LinearDevice(true)), SetToDevice(LinearDevice(true)),
			WithHashPolicy(HashRead))
		if err == nil || writer.members["third"] != nil {
			t.Fatalf("run error = %v, third member = %q; a Finish that only passes its cause on must stop", err, writer.members["third"])
		}
	}
}

func TestFinishSettlesWithItsOwnMarkerBesideTheCause(t *testing.T) {
	// A writer that settles wraps ErrMemberSettled itself; keeping the cause beside it is fine.
	writer := &memberWriter{members: map[string][]byte{}}
	items := make([]Item, 3)
	for i, data := range []string{"first", "second", "third"} {
		target := writer.target(data)
		failure := error(nil)
		if i == 1 {
			failure = errors.Join(ErrMemberSettled, syscall.EIO)
			target = testContentTarget(func(context.Context, ContentMetadata) (ContentWriter, error) {
				return &testContentWriter{Writer: io.Discard, finish: func(cause error) error {
					return fmt.Errorf("%w, padded: %w", ErrMemberSettled, cause)
				}}, nil
			})
		}
		items[i] = &testContentItem{SimpleJob: &SimpleJob{Path: fmt.Sprintf("member-%d", i), Dsts: []string{"out-" + data}},
			source: contentMember(data, failure), targets: []ContentTarget{target}}
	}
	onResults, _ := collectResults()
	if err := runStream(context.Background(), onResults, items, SetFromDevice(LinearDevice(true)), SetToDevice(LinearDevice(true)),
		WithHashPolicy(HashRead)); err != nil || string(writer.members["third"]) != "third" {
		t.Fatalf("run error = %v, third member = %q", err, writer.members["third"])
	}
}

// uncomparableError is an error value whose dynamic type cannot be compared with ==.
type uncomparableError struct{ parts []string }

func (e uncomparableError) Error() string { return fmt.Sprint(e.parts) }

// settledMatcher settles through an Is method, as errors.Is allows.
type settledMatcher struct{}

func (settledMatcher) Error() string { return "settled by method" }

func (settledMatcher) Is(target error) bool { return target == ErrMemberSettled }

func TestSettledByWriterCountsOnlyMarkersOutsideTheCause(t *testing.T) {
	cause := error(&finishCause{err: errors.Join(ErrMemberSettled, syscall.EIO)})
	for name, test := range map[string]struct {
		err  error
		want bool
	}{
		"nil":                        {nil, false},
		"cause":                      {cause, false},
		"wrapped cause":              {fmt.Errorf("finish: %w", cause), false},
		"joined cause":               {errors.Join(errors.New("framing"), cause), false},
		"own marker":                 {fmt.Errorf("%w, padded", ErrMemberSettled), true},
		"own marker beside cause":    {errors.Join(fmt.Errorf("%w, padded", ErrMemberSettled), cause), true},
		"own marker through Is":      {settledMatcher{}, true},
		"uncomparable error":         {uncomparableError{parts: []string{"a"}}, false},
		"uncomparable beside marker": {errors.Join(uncomparableError{}, ErrMemberSettled), true},
	} {
		if got := settledByWriter(test.err, cause); got != test.want {
			t.Errorf("%s: settledByWriter = %t, want %t", name, got, test.want)
		}
	}
}

func TestDrainedMemberReportsItsSourceFailure(t *testing.T) {
	// A source failure while a refused member is drained is reported on that item, settled or not.
	for _, failure := range []error{syscall.EIO, errors.Join(ErrMemberSettled, syscall.EIO)} {
		t.Run(failure.Error(), func(t *testing.T) {
			item := refusedMemberItem("member", contentMember("member", failure))
			onResults, snapshot := collectResults()
			_ = runStream(context.Background(), onResults, []Item{item}, SetFromDevice(LinearDevice(true)),
				SetToDevice(LinearDevice(true)), WithHashPolicy(HashOff))
			if err := resultsByItem(t, snapshot())[item].Err; !errors.Is(err, ErrSourceRead) || !errors.Is(err, syscall.EIO) {
				t.Fatalf("drained member error = %v", err)
			}
		})
	}
}

// refusedMemberItem is a content-source member whose only content target refuses it.
func refusedMemberItem(name string, source ContentSource) *testContentItem {
	refused := fmt.Errorf("%w, unsupported member name", ErrMemberSettled)
	return &testContentItem{SimpleJob: &SimpleJob{Path: name, Dsts: []string{"out-" + name}}, source: source,
		targets: []ContentTarget{testContentTarget(func(context.Context, ContentMetadata) (ContentWriter, error) { return nil, refused })}}
}

func TestDrainingARefusedMemberCrossesTheReadAheadWindow(t *testing.T) {
	// A refused member larger than the read-ahead window is still read to its end without a hash
	// consumer, so draining must advance the window like any consumer does.
	reader := &generatedReader{remaining: readAheadBytes + batchSize}
	large := refusedMemberItem("large", testContentSource(func(context.Context) (fs.File, error) {
		return &testContentFile{ReadCloser: reader,
			metadata: ContentMetadata{Info: contentInfo{name: "large", size: readAheadBytes + batchSize, mode: 0o600}}}, nil
	}))
	writer := &memberWriter{members: map[string][]byte{}}
	next := &testContentItem{SimpleJob: &SimpleJob{Path: "next", Dsts: []string{"out-next"}}, source: contentMember("next", nil),
		targets: []ContentTarget{writer.target("next")}}

	done := make(chan error, 1)
	go func() {
		onResults, _ := collectResults()
		done <- runStream(context.Background(), onResults, []Item{large, next}, SetFromDevice(LinearDevice(true)),
			SetToDevice(LinearDevice(true)), WithHashPolicy(HashOff))
	}()
	select {
	case err := <-done:
		if err != nil || reader.remaining != 0 || string(writer.members["next"]) != "next" {
			t.Fatalf("run error = %v, unread = %d, next = %q", err, reader.remaining, writer.members["next"])
		}
	case <-time.After(60 * time.Second):
		t.Fatal("draining a refused member beyond the read-ahead window deadlocked")
	}
}

// contentMember is a content source of data that fails with failure after its first two bytes.
func contentMember(data string, failure error) ContentSource {
	return testContentSource(func(context.Context) (fs.File, error) {
		reader := io.Reader(bytes.NewReader([]byte(data)))
		if failure != nil {
			reader = io.MultiReader(bytes.NewReader([]byte(data[:2])), failingReader{err: failure})
		}
		return &testContentFile{
			ReadCloser: &testContentReader{Reader: reader, close: func() error { return nil }},
			metadata:   ContentMetadata{Info: contentInfo{name: "member", size: int64(len(data)), mode: 0o600}},
		}, nil
	})
}

func TestContentTargetUnsettledFinishStopsDespiteSettledSource(t *testing.T) {
	// The source settling its own failure does not settle the target stream: Finish decides.
	finishErr := errors.New("member framing is incomplete")
	writer := &memberWriter{members: map[string][]byte{}}
	items := make([]Item, 3)
	for i, data := range []string{"first", "second", "third"} {
		target := writer.target(data)
		failure := error(nil)
		if i == 1 {
			failure = errors.Join(ErrMemberSettled, syscall.EIO)
			target = testContentTarget(func(context.Context, ContentMetadata) (ContentWriter, error) {
				return &testContentWriter{Writer: io.Discard, finish: func(error) error { return finishErr }}, nil
			})
		}
		items[i] = &testContentItem{SimpleJob: &SimpleJob{Path: fmt.Sprintf("member-%d", i), Dsts: []string{"out-" + data}},
			source: contentMember(data, failure), targets: []ContentTarget{target}}
	}
	onResults, _ := collectResults()
	err := runStream(context.Background(), onResults, items, SetFromDevice(LinearDevice(true)), SetToDevice(LinearDevice(true)),
		WithHashPolicy(HashRead))
	if !errors.Is(err, finishErr) || writer.members["third"] != nil {
		t.Fatalf("run error = %v, third member = %q; an unsettled Finish must stop the stream", err, writer.members["third"])
	}
}

func TestContentTargetPanickingFinishStopsEvenWhenSettled(t *testing.T) {
	// A panicking Finish never settles its member, whatever it was about to return.
	writer := &memberWriter{members: map[string][]byte{}}
	items := make([]Item, 3)
	for i, data := range []string{"first", "second", "third"} {
		target := writer.target(data)
		failure := error(nil)
		if i == 1 {
			failure = errors.Join(ErrMemberSettled, syscall.EIO)
			target = testContentTarget(func(context.Context, ContentMetadata) (ContentWriter, error) {
				return &testContentWriter{Writer: io.Discard, finish: func(error) error { panic("finish exploded") }}, nil
			})
		}
		items[i] = &testContentItem{SimpleJob: &SimpleJob{Path: fmt.Sprintf("member-%d", i), Dsts: []string{"out-" + data}},
			source: contentMember(data, failure), targets: []ContentTarget{target}}
	}
	onResults, _ := collectResults()
	err := runStream(context.Background(), onResults, items, SetFromDevice(LinearDevice(true)), SetToDevice(LinearDevice(true)),
		WithHashPolicy(HashRead))
	if err == nil || writer.members["third"] != nil {
		t.Fatalf("run error = %v, third member = %q; a panicking Finish must stop the stream", err, writer.members["third"])
	}
}

func TestContentTargetSettledRefusalKeepsSiblingTargets(t *testing.T) {
	// A refused content target fails alone: the item's native target is still written and the
	// item itself does not fail.
	root, out := t.TempDir(), t.TempDir()
	refused := fmt.Errorf("%w, unsupported member name", ErrMemberSettled)
	path := writeSourceFile(t, root, "file", bytes.Repeat([]byte("x"), 3*batchSize))
	item := &testContentItem{SimpleJob: &SimpleJob{Path: path, Dsts: []string{"member", filepath.Join(out, "native")}},
		targets: []ContentTarget{testContentTarget(func(context.Context, ContentMetadata) (ContentWriter, error) { return nil, refused }), nil}}
	onResults, snapshot := collectResults()
	if err := runStream(context.Background(), onResults, []Item{item}, SetToDevice(LinearDevice(true)), WithHashPolicy(HashRead)); err != nil {
		t.Fatal(err)
	}
	result := resultsByItem(t, snapshot())[item]
	if result.Err != nil || !errors.Is(result.Targets[0].Err, ErrMemberSettled) || result.Targets[1].Err != nil {
		t.Fatalf("result = %#v", result)
	}
	if info, err := os.Stat(filepath.Join(out, "native")); err != nil || info.Size() != 3*batchSize {
		t.Fatalf("native target = %v, %v", info, err)
	}
}

func TestContentSourceSettledRefusalWithoutHashKeepsTheStream(t *testing.T) {
	// Without a hash consumer the refused member is still read to its end, so the source stream
	// keeps its boundary and later members are admitted.
	writer := &memberWriter{members: map[string][]byte{}}
	refused := fmt.Errorf("%w, unsupported member name", ErrMemberSettled)
	items := make([]Item, 3)
	for i, name := range []string{"first", "second", "third"} {
		target := writer.target(name)
		if i == 1 {
			target = testContentTarget(func(context.Context, ContentMetadata) (ContentWriter, error) { return nil, refused })
		}
		data := strings.Repeat(name[:1], 3*batchSize)
		items[i] = &testContentItem{SimpleJob: &SimpleJob{Path: fmt.Sprintf("member-%d", i), Dsts: []string{"out-" + name}},
			source: contentMember(data, nil), targets: []ContentTarget{target}}
	}
	onResults, _ := collectResults()
	if err := runStream(context.Background(), onResults, items, SetFromDevice(LinearDevice(true)), SetToDevice(LinearDevice(true)),
		WithHashPolicy(HashOff)); err != nil {
		t.Fatal(err)
	}
	if len(writer.members["third"]) != 3*batchSize {
		t.Fatalf("third member has %d bytes", len(writer.members["third"]))
	}
}

func TestContentSourceSettledShortReadKeepsTheStream(t *testing.T) {
	// A settled short read keeps its marker instead of becoming an ordinary size mismatch.
	out := t.TempDir()
	items := make([]Item, 3)
	for i, data := range []string{"first", "second", "third"} {
		failure := error(nil)
		if i == 1 {
			failure = errors.Join(ErrMemberSettled, io.ErrUnexpectedEOF)
		}
		items[i] = &testContentItem{SimpleJob: &SimpleJob{Path: fmt.Sprintf("member-%d", i), Dsts: []string{filepath.Join(out, data)}},
			source: contentMember(data, failure)}
	}
	onResults, snapshot := collectResults()
	if err := runStream(context.Background(), onResults, items, SetFromDevice(LinearDevice(true)), WithHashPolicy(HashRead)); err != nil {
		t.Fatal(err)
	}
	if err := resultsByItem(t, snapshot())[items[1]].Targets[0].Err; !errors.Is(err, ErrMemberSettled) || !errors.Is(err, ErrSourceRead) {
		t.Fatalf("short member error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "third")); err != nil {
		t.Fatal(err)
	}
}
