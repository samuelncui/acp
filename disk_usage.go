package acp

import (
	"errors"
	"fmt"
	"syscall"

	"github.com/samuelncui/godf"
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

// availableSpace is queried just before a linear target starts writing. Ordinary targets
// rely on real allocation and write errors instead of a second capacity estimate.
func availableSpace(mountPoint string) (int64, error) {
	usage, err := godf.NewDiskUsage(mountPoint)
	if err != nil {
		return 0, fmt.Errorf("get disk usage failed, mount=%q, %w", mountPoint, err)
	}
	return usage.Available(), nil
}

func mappingError(err error) error {
	if err == nil {
		return nil
	}

	// A cleanup error may have a different identity from the primary I/O failure.
	for _, p := range errorMapping {
		if errors.Is(err, p.from) && !errors.Is(err, p.to) {
			err = errors.Join(p.to, err)
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
