package acp

import "fmt"

// ReadMode selects how a source device reads its content.
type ReadMode uint8

const (
	// ReadBuffered reads through the kernel's buffered path, which is the default.
	ReadBuffered ReadMode = iota
	// ReadMapped reads through a memory mapping.
	ReadMapped
)

func (m ReadMode) String() string {
	switch m {
	case ReadBuffered:
		return "buffered"
	case ReadMapped:
		return "mapped"
	}
	return fmt.Sprintf("unknown(%d)", uint8(m))
}

type deviceOption struct {
	linear   bool
	threads  int
	readMode ReadMode
	// skipCacheRefresh keeps a refreshing hash policy from writing signatures on this side.
	skipCacheRefresh bool
}

func (do *deviceOption) check() error {
	if do.threads < 0 {
		return fmt.Errorf("device threads cannot be negative, threads=%d", do.threads)
	}
	if do.threads == 0 {
		do.threads = 8
	}
	if do.linear {
		do.threads = 1
	}
	if do.readMode != ReadBuffered && do.readMode != ReadMapped {
		return fmt.Errorf("unknown read mode, mode= %s", do.readMode)
	}
	return nil
}

type DeviceOption func(*deviceOption) *deviceOption

func LinearDevice(b bool) DeviceOption {
	return func(d *deviceOption) *deviceOption {
		d.linear = b
		return d
	}
}

func DeviceThreads(threads int) DeviceOption {
	return func(d *deviceOption) *deviceOption {
		d.threads = threads
		return d
	}
}

// WithReadMode selects how a source device reads its content. It applies to the source
// device only and defaults to buffered reads.
func WithReadMode(mode ReadMode) DeviceOption {
	return func(d *deviceOption) *deviceOption {
		d.readMode = mode
		return d
	}
}

// RefreshSignatureCache selects whether a refreshing hash policy writes computed signatures onto
// this device's files: sources on the source device, written targets on the target device. It
// defaults to true and never changes hashing or reuse of stored signatures, nor the marker of an
// encoded target, which is not a cache write.
func RefreshSignatureCache(b bool) DeviceOption {
	return func(d *deviceOption) *deviceOption {
		d.skipCacheRefresh = !b
		return d
	}
}
