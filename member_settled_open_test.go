package acp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
