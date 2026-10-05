package fileio

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestExclusiveLinkRetriesCollisions(t *testing.T) {
	// Occupied files and dangling symlinks must survive a collision during link allocation.
	dir := t.TempDir()
	src := filepath.Join(dir, "source")
	occupied := filepath.Join(dir, ".tmp_occupied")
	dangling := filepath.Join(dir, ".tmp_dangling")
	free := filepath.Join(dir, ".tmp_free")
	for _, name := range []string{src, occupied} {
		if err := os.WriteFile(name, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(dir, "absent"), dangling); err != nil {
		t.Skipf("symlink: %v", err)
	}
	old := tempName
	index := 0
	tempName = func(string) (string, error) {
		names := []string{occupied, dangling, free}
		name := names[index]
		index++
		return name, nil
	}
	t.Cleanup(func() { tempName = old })

	// Only the successful allocation is returned as an owned path.
	if got, err := Link(src, dir); err != nil || got != free {
		t.Fatalf("link = %q / %v", got, err)
	}
	if data, err := os.ReadFile(occupied); err != nil || string(data) != "keep" {
		t.Fatalf("collision changed: %q / %v", data, err)
	}
	if _, err := os.Lstat(dangling); err != nil {
		t.Fatalf("dangling collision removed: %v", err)
	}
}

func TestAtomicJSONFailureKeepsPreviousDocument(t *testing.T) {
	// A failed encoder must leave the old document and unrelated legacy temporary names alone.
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	for _, name := range []string{path, path + ".tmp"} {
		if err := os.WriteFile(name, []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := WriteJSON(path, make(chan int), true); err == nil {
		t.Fatal("unsupported JSON accepted")
	}
	for _, name := range []string{path, path + ".tmp"} {
		if data, err := os.ReadFile(name); err != nil || string(data) != "old" {
			t.Fatalf("old document changed: %q / %v", data, err)
		}
	}

	// Rename failure also removes only the exclusive scratch file and preserves the destination.
	blocked := filepath.Join(dir, "directory")
	if err := os.Mkdir(blocked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blocked, "keep"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteJSON(blocked, struct{}{}, false); err == nil {
		t.Fatal("rename into nonempty directory succeeded")
	}
	leftovers, err := filepath.Glob(filepath.Join(dir, ".tmp_*"))
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("leftovers: %v / %v", leftovers, err)
	}
	if err := Remove(filepath.Join(dir, "absent")); err != nil {
		t.Fatal(err)
	}
	if err := Remove(blocked); err == nil {
		t.Fatal("nonempty directory removed")
	} else {
		var cleanup *CleanupError
		if !errors.As(err, &cleanup) || cleanup.Path != blocked {
			t.Fatalf("cleanup ownership lost: %v", err)
		}
	}
}
