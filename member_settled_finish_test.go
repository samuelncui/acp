package acp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"syscall"
	"testing"
)

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
