package acp

import (
	"context"
	"fmt"
	"io/fs"
	"testing"
	"time"
)

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
