package acp

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"testing"
)

func TestStopsAndPanicsAreNotTaggedAsSourceFailures(t *testing.T) {
	// A cancellation and a panic are not failures of the source, even when they happen in Open.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelled := &testContentItem{SimpleJob: &SimpleJob{Path: "cancelled", Dsts: []string{filepath.Join(t.TempDir(), "out")}},
		source: testContentSource(func(context.Context) (fs.File, error) {
			cancel()
			return &testContentFile{ReadCloser: &testContentReader{Reader: bytes.NewReader(nil), close: func() error { return nil }},
				metadata: ContentMetadata{Info: contentInfo{name: "member", mode: 0o600}}}, nil
		})}
	onResults, snapshot := collectResults()
	err := runStream(ctx, onResults, []Item{cancelled}, SetFromDevice(LinearDevice(true)), WithHashPolicy(HashRead))
	if errors.Is(err, ErrSourceRead) {
		t.Fatalf("run error = %v, a cancellation is tagged as a source failure", err)
	}
	if result := resultsByItem(t, snapshot())[cancelled]; !errors.Is(result.Err, context.Canceled) || errors.Is(result.Err, ErrSourceRead) {
		t.Fatalf("cancelled item error = %v", result.Err)
	}

	panicking := &testContentItem{SimpleJob: &SimpleJob{Path: "panicking", Dsts: []string{filepath.Join(t.TempDir(), "out")}},
		source: testContentSource(func(context.Context) (fs.File, error) { panic("open exploded") })}
	onResults, snapshot = collectResults()
	_ = runStream(context.Background(), onResults, []Item{panicking}, SetFromDevice(LinearDevice(true)), WithHashPolicy(HashRead))
	if result := resultsByItem(t, snapshot())[panicking]; result.Err == nil || errors.Is(result.Err, ErrSourceRead) {
		t.Fatalf("panicking item error = %v", result.Err)
	}

	// An Open failure of the source itself is still tagged.
	failing := &testContentItem{SimpleJob: &SimpleJob{Path: "failing", Dsts: []string{filepath.Join(t.TempDir(), "out")}},
		source: testContentSource(func(context.Context) (fs.File, error) { return nil, fs.ErrPermission })}
	onResults, snapshot = collectResults()
	_ = runStream(context.Background(), onResults, []Item{failing}, SetFromDevice(LinearDevice(true)), WithHashPolicy(HashRead))
	if result := resultsByItem(t, snapshot())[failing]; !errors.Is(result.Err, ErrSourceRead) || !errors.Is(result.Err, fs.ErrPermission) {
		t.Fatalf("failing item error = %v", result.Err)
	}
}
