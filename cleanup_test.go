package acp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

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
