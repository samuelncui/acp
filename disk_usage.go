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

// A sample covers at most 64 single-output files of at most one copy chunk each.
// Below that logical headroom, each positive-size target gets a fresh observation.
const spaceSampleFiles = 64

type spaceEstimate struct {
	device    string
	available int64
	files     int
}

func (c *StreamCopyer) checkLinearSpace(job *writeJob, target targetSpec) error {
	// Only consecutive small single-output items can reuse the last mount's estimate.
	size := job.stat.size
	reuse := size > 0 && size <= batchSize && len(job.outputs) == 1
	if !reuse {
		c.linearSpace = spaceEstimate{}
	}
	if size <= 0 {
		return nil
	}

	// Bound stale observations by both admitted file count and debited logical bytes.
	sample := c.linearSpace
	fresh := sample.files == 0 || sample.device != target.device ||
		sample.files >= spaceSampleFiles || sample.available < spaceSampleFiles*batchSize
	if fresh {
		c.linearSpace = spaceEstimate{}
		available, err := c.availableSpace(target.device)
		if err != nil {
			return err
		}
		sample = spaceEstimate{device: target.device, available: available}
	}
	if size > sample.available {
		return fmt.Errorf("%w, want=%d have=%d", ErrTargetNoSpace, size, sample.available)
	}

	// Admission consumes estimated space even if the output later fails; never credit cleanup.
	// This advisory logical estimate cannot account for allocation or metadata overhead.
	if reuse {
		sample.available -= size
		sample.files++
		c.linearSpace = sample
	}
	return nil
}

// availableSpace reads filesystem capacity; ordinary targets rely on real allocation errors.
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
