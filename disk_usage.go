package acp

import (
	"errors"
	"fmt"
	"sync"
	"syscall"

	"github.com/samuelncui/godf"
)

const (
	defaultDiskUsageFreshInterval = 1024 * 1024 * 1024 * 2
)

var (
	ErrTargetNoSpace        = fmt.Errorf("acp: target have no space")
	ErrTargetDropToReadonly = fmt.Errorf("acp: target droped into readonly")

	errorMapping = []errorPair{
		{from: syscall.ENOSPC, to: ErrTargetNoSpace},
		{from: syscall.EROFS, to: ErrTargetDropToReadonly},
		{from: syscall.EIO, to: ErrTargetDropToReadonly},
	}
)

type errorPair struct {
	from error
	to   error
}

type diskUsageCache struct {
	mountPoint    string
	freshInterval int64

	lock      sync.Mutex
	freeSpace int64
	used      int64
}

func newDiskUsageCache(mountPoint string, freshInterval int64) *diskUsageCache {
	return &diskUsageCache{
		mountPoint:    mountPoint,
		freshInterval: freshInterval,
	}
}

func (m *diskUsageCache) check(need int64) error {
	m.lock.Lock()
	defer m.lock.Unlock()

	m.used += need
	if m.used <= m.freeSpace && m.used < m.freshInterval {
		return nil
	}

	usage, err := godf.NewDiskUsage(m.mountPoint)
	if err != nil {
		return fmt.Errorf("get disk usage fail, mount_point= %s, %w", m.mountPoint, err)
	}

	m.freeSpace = int64(usage.Available())
	m.used = need
	if m.used > m.freeSpace {
		return fmt.Errorf("%w, want= %d have= %d", ErrTargetNoSpace, m.used, m.freeSpace)
	}

	return nil
}

func mappingError(err error) error {
	if err == nil {
		return nil
	}

	for _, p := range errorMapping {
		if errors.Is(err, p.from) {
			return fmt.Errorf("%w: %w", p.to, err)
		}
	}

	return err
}
