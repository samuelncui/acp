package acp

import (
	"context"
	"errors"
	"io/fs"
	"testing"
)

func TestSourceOwnTimeoutIsTaggedWhileTheRunContinues(t *testing.T) {
	// A source's own deadline is its failure; only the run's cancellation is a stop.
	item := &testContentItem{SimpleJob: &SimpleJob{Path: "timeout", Dsts: []string{"out"}},
		source: testContentSource(func(context.Context) (fs.File, error) { return nil, context.DeadlineExceeded })}
	writer := &memberWriter{members: map[string][]byte{}}
	item.targets = []ContentTarget{writer.target("timeout")}
	onResults, snapshot := collectResults()
	_ = runStream(context.Background(), onResults, []Item{item}, SetFromDevice(LinearDevice(true)), SetToDevice(LinearDevice(true)),
		WithHashPolicy(HashRead))
	if err := resultsByItem(t, snapshot())[item].Err; !errors.Is(err, ErrSourceRead) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("source timeout error = %v", err)
	}
}
