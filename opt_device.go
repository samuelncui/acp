package acp

import "fmt"

type deviceOption struct {
	linear  bool
	threads int
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
