package acp

import (
	"fmt"
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

		mp := mount.Mountpoint
		if !strings.HasSuffix(mp, "/") {
			mp = mp + "/"
		}

		mountPoints.Add(mp)
	}

	mps := mountPoints.ToSlice()
	return Cache(func(path string) string {
		for _, mp := range mps {
			if strings.HasPrefix(path, mp) {
				return mp
			}
		}
		return ""
	}), nil
}
