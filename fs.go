package acp

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	mapset "github.com/deckarep/golang-set/v2"
	"github.com/moby/sys/mountinfo"
)

// absPath resolves a path against the current working directory. It is a variable so a test
// can force the failure path that a real working directory almost never produces.
var absPath = filepath.Abs

// getMountpointResolver keeps only the mount list for a run. Each indexed target carries
// its resolved device, so the resolver retains no per-file cache.
func getMountpointResolver() (func(string) (string, error), error) {
	mounts, err := mountinfo.GetMounts(nil)
	if err != nil {
		return nil, fmt.Errorf("get mounts fail, %w", err)
	}
	mountPoints := mapset.NewThreadUnsafeSet[string]()
	for _, mount := range mounts {
		if mount != nil && mount.Mountpoint != "" {
			mountPoints.Add(filepath.Clean(mount.Mountpoint))
		}
	}
	mps := mountPoints.ToSlice()
	return func(path string) (string, error) {
		abs, err := absPath(path)
		if err != nil {
			return "", fmt.Errorf("get abs from file path failed, path= %q, %w", path, err)
		}
		return findMountpoint(abs, mps), nil
	}, nil
}

// openSource opens a source for buffered reading without updating its access time.
// O_NOATIME needs ownership or privilege, so a refused open falls back to an ordinary one.
func openSource(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|openNoAtime, 0)
	if err == nil || !errors.Is(err, fs.ErrPermission) {
		return file, err
	}

	return os.Open(path)
}

func findMountpoint(path string, mountPoints []string) string {
	// Match directory boundaries directly; resolving each target must not allocate a prefix per mount.
	matched := ""
	for _, mountPoint := range mountPoints {
		if !strings.HasPrefix(path, mountPoint) {
			continue
		}
		if len(path) > len(mountPoint) && !strings.HasSuffix(mountPoint, string(filepath.Separator)) &&
			path[len(mountPoint)] != filepath.Separator {
			continue
		}
		if len(mountPoint) > len(matched) {
			matched = mountPoint
		}
	}
	return matched
}
