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

// getMountpointCache returns a memoized resolver from a path to the longest mount point that
// contains it.
func getMountpointCache() (func(string) (string, error), error) {
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
	resolve := Cache(func(path string) mountPoint {
		// A path that cannot be made absolute has no mount point to report: the caller
		// receives the failure instead of a panic it cannot recover from.
		abs, err := absPath(path)
		if err != nil {
			return mountPoint{err: fmt.Errorf("get abs from file path failed, path= '%s', %w", path, err)}
		}

		return mountPoint{point: findMountpoint(abs, mps)}
	})
	return func(path string) (string, error) {
		matched := resolve(path)
		return matched.point, matched.err
	}, nil
}

// mountPoint is the memoized outcome of resolving one path to its mount point. Both the match
// and the failure are cached, because a path that cannot be resolved cannot become resolvable.
type mountPoint struct {
	point string
	err   error
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
