package acp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	mapset "github.com/deckarep/golang-set/v2"
	"github.com/samuelncui/acp/internal/fileio"
)

func TestTargetFailurePreservesWriteCloseAndRemoveErrors(t *testing.T) {
	// Match the fileio discard fixture: a closed descriptor and owned nonempty directory fail independently.
	root := t.TempDir()
	target := writeSourceFile(t, root, "target", []byte("original"))
	owned := filepath.Join(root, ".tmp_owned")
	child := writeSourceFile(t, owned, "child", nil)
	file, err := os.Open(child)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	out := &fileio.Output{File: file, Path: target, Temporary: owned}
	copyer := newTestStream(t, SetToDevice(LinearDevice(true)))
	job := &writeJob{baseJob: &baseJob{
		copyer: copyer, item: newFixtureItem("source", target), path: "source",
		stat: &stat{size: 1}, targets: []string{target},
	}}

	// The writer must retain its primary failure when both cleanup operations fail, and drain queued buffers.
	chunks := make(chan *chunkBuffer, 2)
	var buffers []*chunkBuffer
	for range 2 {
		chunk := acquireChunk()
		chunk.data = chunk.data[:1]
		chunk.data[0] = 'x'
		buffers = append(buffers, chunk)
		chunks <- chunk
	}
	close(chunks)
	var readErr error
	transfer := newTransfer(copyer, job, newReadAhead(copyer), 0)
	targetState := &targetTransfer{spec: targetSpec{name: target, path: target, device: root}, output: &nativeTransferOutput{copyer: copyer, output: out}}
	for chunk := range chunks {
		if targetState.err != nil {
			chunk.release()
			continue
		}
		targetState.err = writeChunk(out.File, chunk)
	}
	targetState.err = errors.Join(targetState.err, readErr)
	transfer.finishTarget(targetState, mapset.NewSet[string]())
	result := job.result()
	if result.Err != nil || len(result.Targets) != 1 || result.Targets[0].Path != target {
		t.Fatalf("unexpected result: %+v", result)
	}
	failure := result.Targets[0].Err
	var cleanup *fileio.CleanupError
	if !errors.Is(failure, os.ErrClosed) || !errors.As(failure, &cleanup) || cleanup.Path != owned {
		t.Fatalf("target lost primary or cleanup error: %v", failure)
	}

	// Inspect every joined branch: errors.As alone would only find the first of the two closed-file errors.
	operations := make(map[string]*os.PathError)
	var inspect func(error)
	inspect = func(err error) {
		switch err := err.(type) {
		case *os.PathError:
			operations[err.Op] = err
		case interface{ Unwrap() []error }:
			for _, child := range err.Unwrap() {
				inspect(child)
			}
		case interface{ Unwrap() error }:
			inspect(err.Unwrap())
		}
	}
	inspect(failure)
	for _, op := range []string{"write", "close"} {
		if err := operations[op]; err == nil || err.Path != child || !errors.Is(err, os.ErrClosed) {
			t.Errorf("missing original %s error: %v", op, err)
		}
	}
	if err := operations["remove"]; err == nil || err.Path != owned || !errors.Is(failure, err.Err) {
		t.Errorf("missing original remove error: %v", err)
	}

	// Failed removal retains ownership and never touches the original target.
	if out.File != nil || out.Temporary != owned || len(chunks) != 0 {
		t.Fatalf("cleanup ownership = %+v; queued chunks = %d", out, len(chunks))
	}
	for _, chunk := range buffers {
		if chunk.refs != 0 {
			t.Errorf("queued buffer retains %d references", chunk.refs)
		}
	}
	if content, err := os.ReadFile(target); err != nil || string(content) != "original" {
		t.Fatalf("original target changed: %q / %v", content, err)
	}
	if _, err := os.Stat(child); err != nil {
		t.Fatalf("failed removal lost its owned path: %v", err)
	}
}

func TestCleanupRemovesTargetAfterMetadataFailure(t *testing.T) {
	// Refuse metadata after a real copy; the exclusive output must disappear before the result.
	root := t.TempDir()
	source := writeSourceFile(t, root, "source", []byte("fixture"))
	target := filepath.Join(root, "target")
	sentinel := errors.New("metadata refused")
	previous := restoreTarget
	restoreTarget = func(string, *stat) error { return sentinel }
	t.Cleanup(func() { restoreTarget = previous })
	item := newFixtureItem(source, target)
	fixture := newStreamFixture(item)
	if err := runFixture(context.Background(), fixture, []Item{item}); err != nil {
		t.Fatal(err)
	}
	result, terminalErr := item.terminal(t)
	if terminalErr != nil || len(result.Targets) != 1 || !errors.Is(result.Targets[0].Err, sentinel) {
		t.Fatalf("result=%+v err=%v", result, terminalErr)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed output still exists: %v", err)
	}
}
