package acp

import (
	"fmt"
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
