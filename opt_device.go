package acp

type deviceOption struct {
	linear bool
}

type DeviceOption func(*deviceOption) *deviceOption

func LinearDevice(b bool) DeviceOption {
	return func(d *deviceOption) *deviceOption {
		d.linear = b
		return d
	}
}
