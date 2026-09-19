package acp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// walkTestEntries runs the wildcard enumeration the shell uses, with a stream in place because
// the walk reports the sources it cannot read through it.
func walkTestEntries(t *testing.T, sources, targets []string) []*compatItem {
	t.Helper()

	option := WildcardJob(Source(sources...), Target(targets...))(newOption())
	if err := option.check(); err != nil {
		t.Fatalf("check options: %v", err)
	}

	copyer := &Copyer{option: option, stream: newTestStream(t)}
	entries, err := copyer.walkWildcard(option.wildcardJobs[0])
	if err != nil {
		t.Fatalf("walk wildcard: %v", err)
	}

	return entries
}

func TestWalkWildcardMapsSourcesOntoTargets(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	writeSourceFile(t, source, "plain.txt", []byte("plain"))
	writeSourceFile(t, source, "empty.txt", nil)
	writeSourceFile(t, source, filepath.Join("nested", "data"), []byte("nested"))
	if err := os.MkdirAll(filepath.Join(source, "emptydir"), 0o755); err != nil {
		t.Fatal(err)
	}

	targets := []string{filepath.Join(root, "target-a"), filepath.Join(root, "target-b")}
	for _, target := range targets {
		if err := os.MkdirAll(target, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	entries := walkTestEntries(t, []string{source}, targets)

	// Directories are not entries, and the platform's path order covers the result.
	want := []string{
		"empty.txt",
		filepath.Join("nested", "data"),
		"plain.txt",
	}
	if len(entries) != len(want) {
		t.Fatalf("entries = %d, want %d: %+v", len(entries), len(want), entries)
	}
	for index, entry := range entries {
		relative := filepath.Join("source", want[index])
		if got := strings.Join(entry.path, "/"); got != filepath.ToSlash(relative) {
			t.Fatalf("entry %d path = %q, want %q", index, got, relative)
		}
		if base, _ := filepath.Split(source); entry.base != base {
			t.Fatalf("entry %d base = %q, want %q", index, entry.base, base)
		}
		if entry.source != filepath.Join(source, want[index]) {
			t.Fatalf("entry %d source = %q", index, entry.source)
		}
		if len(entry.targets) != len(targets) {
			t.Fatalf("entry %d targets = %v, want one per target directory", index, entry.targets)
		}
		for targetIndex, target := range targets {
			if got, wantTarget := entry.targets[targetIndex], filepath.Join(target, relative); got != wantTarget {
				t.Fatalf("entry %d target %d = %q, want %q", index, targetIndex, got, wantTarget)
			}
		}
	}

	// The same enumeration drives a real run, so every mapped target also lands on disk.
	copyer, err := New(context.Background(), WildcardJob(Source(source), Target(targets...)))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := copyer.WaitErr(); err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, entry := range entries {
		for _, target := range entry.targets {
			if _, err := os.Stat(target); err != nil {
				t.Fatalf("copied file %q: %v", target, err)
			}
		}
	}
}

func TestWalkWildcardMapsASingleFileOntoTargets(t *testing.T) {
	root := t.TempDir()
	source := writeSourceFile(t, root, "plain.txt", []byte("plain"))
	target := filepath.Join(root, "target")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}

	entries := walkTestEntries(t, []string{source}, []string{target})
	if len(entries) != 1 {
		t.Fatalf("entries = %+v, want one", entries)
	}
	entry := entries[0]
	base, _ := filepath.Split(source)
	if entry.source != source || len(entry.path) != 1 || entry.path[0] != "plain.txt" || entry.base != base {
		t.Fatalf("entry = %+v", entry)
	}
	if len(entry.targets) != 1 || entry.targets[0] != filepath.Join(target, "plain.txt") {
		t.Fatalf("targets = %v", entry.targets)
	}
}

func TestShellRejectsRepeatedRelativePaths(t *testing.T) {
	// Two source trees with the same name would write the same target twice, so the run now
	// fails before it copies anything instead of keeping one entry per relative path.
	root := t.TempDir()
	first := filepath.Join(root, "first", "tree")
	second := filepath.Join(root, "second", "tree")
	writeSourceFile(t, first, "data.txt", []byte("first"))
	writeSourceFile(t, second, "data.txt", []byte("second"))
	target := filepath.Join(root, "target")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}

	copyer, err := New(context.Background(), WildcardJob(Source(second, first), Target(target)))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	err = copyer.WaitErr()
	if err == nil || !strings.Contains(err.Error(), "same relative path") {
		t.Fatalf("WaitErr() error = %v, want a repeated relative path", err)
	}

	// The ambiguous target file must not be written.
	ambiguous := filepath.Join(target, "tree", "data.txt")
	if _, err := os.Stat(ambiguous); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ambiguous target stat error = %v, want %v", err, os.ErrNotExist)
	}
}

func TestNewRejectsInvalidInput(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	file := writeSourceFile(t, root, "plain.txt", []byte("plain"))

	// An empty WildcardJob or Source() carries no source at all and is a no-op option, so the
	// old "no source" case has no equivalent here.
	tests := []struct {
		name    string
		sources []string
		targets []string
	}{
		{name: "missing source", sources: []string{filepath.Join(root, "missing")}, targets: []string{target}},
		{name: "missing target", sources: []string{file}, targets: []string{filepath.Join(root, "missing-target")}},
		{name: "target is a file", sources: []string{file}, targets: []string{file}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			option := WildcardJob(Source(test.sources...), Target(test.targets...))
			if _, err := New(context.Background(), option); err == nil {
				t.Fatal("New() error = nil")
			}
		})
	}
}
