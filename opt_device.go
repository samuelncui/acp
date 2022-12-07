package acp

type deviceOption struct {
	linear  bool
	threads int
}

func (do *deviceOption) check() {
	if do.threads == 0 {
		do.threads = 8
	}
	if do.linear {
		do.threads = 1
	}
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
