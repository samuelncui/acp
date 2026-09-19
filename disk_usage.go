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
	// ErrTargetIO reports an I/O failure of one target. Unlike ErrTargetDropToReadonly it does
	// not describe the whole device, so it must not abort a target that is still writable.
	ErrTargetIO = fmt.Errorf("acp: target have io error")

	errorMapping = []errorPair{
		{from: syscall.ENOSPC, to: ErrTargetNoSpace},
		{from: syscall.EROFS, to: ErrTargetDropToReadonly},
		{from: syscall.EIO, to: ErrTargetIO},
	}
	abortErrors = []error{
		ErrTargetNoSpace,
		ErrTargetDropToReadonly,
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

	return m.account(usage.Available(), need)
}

// account applies one fresh measurement to the reservations already counted. A measurement
// replaces the capacity estimate, never the commitments made against it: the reservations of
// copies still in flight on the same mount point stay counted. Bytes that landed since the
// previous measurement have already consumed that capacity, so they stop holding a
// reservation of their own.
func (m *diskUsageCache) account(available, need int64) error {
	if landed := m.freeSpace - available; landed > 0 {
		m.used -= landed
	}
	m.freeSpace = available

	// The item asking for space right now keeps its own reservation even when everything
	// counted before it has already landed.
	if m.used < need {
		m.used = need
	}
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
			// Keep both identities: the classified target error and the syscall that caused
			// it, so errors.Is finds either one.
			return errors.Join(p.to, err)
		}
	}

	return err
}

func checkErrorAbort(err error) bool {
	for _, e := range abortErrors {
		if errors.Is(err, e) {
			return true
		}
	}

	return false
}
