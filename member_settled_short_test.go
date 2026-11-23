package acp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

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
