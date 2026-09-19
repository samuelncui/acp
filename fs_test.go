package acp

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFS(t *testing.T) {
	// Resolve a real file to the mount point that contains it.
	root := t.TempDir()
	resolve, err := getMountpointCache()
	if err != nil {
		t.Fatalf("get mount point cache: %v", err)
	}

	path := filepath.Join(root, "nested", "file.txt")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}

	mount, err := resolve(path)
	if err != nil {
		t.Fatalf("resolve %q: %v", path, err)
	}
	if mount == "" {
		t.Fatalf("resolve %q = %q, want a mount point", path, mount)
	}
	if !filepath.IsAbs(mount) {
		t.Fatalf("mount point %q is not absolute", mount)
	}

	// The resolved mount point contains the resolved path, on a directory boundary.
	absolute, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(mount, absolute)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		t.Fatalf("mount point %q does not contain %q", mount, absolute)
	}

	// The cache resolves one path once and keeps reporting the same mount point.
	again, err := resolve(path)
	if err != nil {
		t.Fatalf("resolve %q again: %v", path, err)
	}
	if again != mount {
		t.Fatalf("resolve %q = %q after %q", path, again, mount)
	}
}

func TestGetMountpointReportsAbsFailure(t *testing.T) {
	// A path that cannot be made absolute must produce an error, not a panic that unwinds a
	// pipeline goroutine before it can report anything.
	previous := absPath
	absPath = func(string) (string, error) { return "", errors.New("abs failed") }
	t.Cleanup(func() { absPath = previous })

	resolve, err := getMountpointCache()
	if err != nil {
		t.Fatalf("get mount point cache: %v", err)
	}
	mount, err := resolve("relative/path")
	if err == nil {
		t.Fatalf("resolve() = %q, want an error", mount)
	}
	if !strings.Contains(err.Error(), "abs failed") || !strings.Contains(err.Error(), "relative/path") {
		t.Fatalf("resolve() error = %v, want the failed path and its cause", err)
	}
}

func TestFindMountpoint(t *testing.T) {
	root := string(filepath.Separator)
	mount := filepath.Join(root, "mnt")
	nested := filepath.Join(mount, "archive")
	mountPoints := []string{root, nested, mount}

	tests := []struct {
		name string
		path string
		want string
	}{
		{name: "nested child", path: filepath.Join(nested, "file"), want: nested},
		{name: "exact mountpoint", path: nested, want: nested},
		{name: "parent child", path: filepath.Join(mount, "file"), want: mount},
		{name: "directory boundary", path: filepath.Join(root, "mnt2", "file"), want: root},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := findMountpoint(tt.path, mountPoints); got != tt.want {
				t.Fatalf("findMountpoint(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

// TestOpenSourceReadsContentAndReportsRefusals pins the buffered source open: the reader gets the
// file content, a missing file keeps its error, and a refusal the no-atime flag cannot pass is
// reported instead of being retried into a success. The fallback itself is what keeps a source
// owned by another user readable on a platform with O_NOATIME.
func TestOpenSourceReadsContentAndReportsRefusals(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "source.txt")
	if err := os.WriteFile(path, []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}

	file, err := openSource(path)
	if err != nil {
		t.Fatalf("openSource() error = %v", err)
	}
	content, err := io.ReadAll(file)
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close source: %v", err)
	}
	if string(content) != "fixture" {
		t.Fatalf("content = %q, want %q", content, "fixture")
	}

	if _, err := openSource(filepath.Join(root, "missing.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("openSource(missing) error = %v, want %v", err, os.ErrNotExist)
	}

	// A file this process may not read stays unreadable, so the fallback reports the refusal
	// instead of turning it into a silent success.
	if os.Geteuid() == 0 {
		t.Skip("running as root, which can read a file with no permissions")
	}
	refused := filepath.Join(root, "refused.txt")
	if err := os.WriteFile(refused, []byte("fixture"), 0o000); err != nil {
		t.Fatal(err)
	}
	if _, err := openSource(refused); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("openSource(refused) error = %v, want %v", err, os.ErrPermission)
	}
}
