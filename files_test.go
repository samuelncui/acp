package acp

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSelectFilesMapsSourcesOntoTargets(t *testing.T) {
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

	entries, err := SelectFiles([]string{source}, targets)
	if err != nil {
		t.Fatalf("select files: %v", err)
	}

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
		if entry.Path != relative {
			t.Fatalf("entry %d path = %q, want %q", index, entry.Path, relative)
		}
		if base, _ := filepath.Split(source); entry.Base != base {
			t.Fatalf("entry %d base = %q, want %q", index, entry.Base, base)
		}
		if entry.Source != filepath.Join(source, want[index]) {
			t.Fatalf("entry %d source = %q", index, entry.Source)
		}
		if len(entry.Targets) != len(targets) {
			t.Fatalf("entry %d targets = %v, want one per target directory", index, entry.Targets)
		}
		for targetIndex, target := range targets {
			if got, wantTarget := entry.Targets[targetIndex], filepath.Join(target, relative); got != wantTarget {
				t.Fatalf("entry %d target %d = %q, want %q", index, targetIndex, got, wantTarget)
			}
		}
	}
}

func TestSelectFilesMapsASingleFileOntoTargets(t *testing.T) {
	root := t.TempDir()
	source := writeSourceFile(t, root, "plain.txt", []byte("plain"))
	target := filepath.Join(root, "target")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}

	entries, err := SelectFiles([]string{source}, []string{target})
	if err != nil {
		t.Fatalf("select files: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %+v, want one", entries)
	}
	entry := entries[0]
	base, _ := filepath.Split(source)
	if entry.Source != source || entry.Path != "plain.txt" || entry.Base != base {
		t.Fatalf("entry = %+v", entry)
	}
	if len(entry.Targets) != 1 || entry.Targets[0] != filepath.Join(target, "plain.txt") {
		t.Fatalf("targets = %v", entry.Targets)
	}
}

func TestSelectFilesRemovesRepeatedRelativePaths(t *testing.T) {
	// Two source trees with the same name would write the same target twice, so the
	// caller receives one entry per relative path.
	root := t.TempDir()
	first := filepath.Join(root, "first", "tree")
	second := filepath.Join(root, "second", "tree")
	writeSourceFile(t, first, "data.txt", []byte("first"))
	writeSourceFile(t, second, "data.txt", []byte("second"))
	target := filepath.Join(root, "target")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}

	entries, err := SelectFiles([]string{second, first}, []string{target})
	if err != nil {
		t.Fatalf("select files: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %+v, want one entry per relative path", entries)
	}
	entry := entries[0]
	if entry.Path != filepath.Join("tree", "data.txt") {
		t.Fatalf("entry path = %q", entry.Path)
	}
	if entry.Source != filepath.Join(first, "data.txt") && entry.Source != filepath.Join(second, "data.txt") {
		t.Fatalf("entry source = %q, want one of the submitted trees", entry.Source)
	}
}

func TestSelectFilesRejectsInvalidInput(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	file := writeSourceFile(t, root, "plain.txt", []byte("plain"))

	tests := []struct {
		name    string
		sources []string
		targets []string
	}{
		{name: "no source", sources: nil, targets: []string{target}},
		{name: "missing source", sources: []string{filepath.Join(root, "missing")}, targets: []string{target}},
		{name: "missing target", sources: []string{file}, targets: []string{filepath.Join(root, "missing-target")}},
		{name: "target is a file", sources: []string{file}, targets: []string{file}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := SelectFiles(test.sources, test.targets); err == nil {
				t.Fatal("SelectFiles() error = nil")
			}
		})
	}
}
