package acp

import (
	"path/filepath"
	"testing"
)

func TestFS(t *testing.T) {
	mpCache, err := getMountpointCache()
	if err != nil {
		panic(err)
	}

	t.Log("mp cahce", mpCache("/Users/cuijingning/go/src/github.com/samuelncui/acp"))
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
