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

func getMountpointCache() (func(string) string, error) {
	mounts, err := mountinfo.GetMounts(nil)
	if err != nil {
		return nil, fmt.Errorf("get mounts fail, %w", err)
	}

	mountPoints := mapset.NewThreadUnsafeSet[string]()
	for _, mount := range mounts {
		if mount == nil {
			continue
		}
		if mount.Mountpoint == "" {
			continue
		}

		mountPoints.Add(filepath.Clean(mount.Mountpoint))
	}

	mps := mountPoints.ToSlice()
	return Cache(func(path string) string {
		path, err := filepath.Abs(path)
		if err != nil {
			panic(fmt.Errorf("get abs from file path failed, path= '%s', %w", path, err))
		}

		return findMountpoint(path, mps)
	}), nil
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
	matched := ""
	for _, mountPoint := range mountPoints {
		if path != mountPoint {
			prefix := mountPoint
			if !strings.HasSuffix(prefix, string(filepath.Separator)) {
				prefix += string(filepath.Separator)
			}
			if !strings.HasPrefix(path, prefix) {
				continue
			}
		}
		if len(mountPoint) > len(matched) {
			matched = mountPoint
		}
	}
	return matched
}
