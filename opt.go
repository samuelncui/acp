package acp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sirupsen/logrus"
)

type source struct {
	base string
	path string
}

func (s *source) src() string {
	return filepath.Join(s.base, s.path)
}

func (s *source) dst(dst string) string {
	return filepath.Join(dst, s.path)
}

func (s *source) append(next string) *source {
	return &source{base: s.base, path: filepath.Join(s.path, next)}
}

func comparePath(a, b string) int {
	a = strings.ReplaceAll(filepath.ToSlash(a), "/", "\x00")
	b = strings.ReplaceAll(filepath.ToSlash(b), "/", "\x00")
	return strings.Compare(a, b)
}

// defaultReadBuffer is how many items ACP may hold in its read buffer when the caller
// does not set one.
const defaultReadBuffer = 4096

type option struct {
	batch BatchSource

	fromDevice *deviceOption
	toDevice   *deviceOption

	createFlag int
	hashPolicy HashPolicy

	readBuffer int

	logger       *logrus.Logger
	eventHanders []EventHandler
}

func newOption() *option {
	return &option{
		fromDevice: new(deviceOption),
		toDevice:   new(deviceOption),
		createFlag: os.O_WRONLY | os.O_CREATE | os.O_EXCL,
		readBuffer: defaultReadBuffer,
	}
}

func (o *option) check() error {
	if err := o.fromDevice.check(); err != nil {
		return fmt.Errorf("check source device failed, %w", err)
	}
	if err := o.toDevice.check(); err != nil {
		return fmt.Errorf("check target device failed, %w", err)
	}
	if o.toDevice.readMode != ReadBuffered {
		return fmt.Errorf("read mode is a source option, mode= %s", o.toDevice.readMode)
	}
	if o.readBuffer < 1 {
		return fmt.Errorf("read buffer must be at least one item, items=%d", o.readBuffer)
	}
	if o.toDevice.overwrite {
		o.createFlag = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	}
	if !o.hashPolicy.valid() {
		return fmt.Errorf("unknown hash policy, policy= %s", o.hashPolicy)
	}
	if o.logger == nil {
		o.logger = logrus.StandardLogger()
	}

	return nil
}

type Option func(*option) *option

func SetFromDevice(opts ...DeviceOption) Option {
	return func(o *option) *option {
		for _, opt := range opts {
			if opt == nil {
				continue
			}
			o.fromDevice = opt(o.fromDevice)
		}
		return o
	}
}

func SetToDevice(opts ...DeviceOption) Option {
	return func(o *option) *option {
		for _, opt := range opts {
			if opt == nil {
				continue
			}
			o.toDevice = opt(o.toDevice)
		}
		return o
	}
}

func Overwrite(b bool) DeviceOption {
	return func(d *deviceOption) *deviceOption {
		d.overwrite = b
		return d
	}
}

// WithReadBuffer sets how many items ACP may hold in its read buffer. A Job runner
// passes its configured read buffer here.
func WithReadBuffer(items int) Option {
	return func(o *option) *option {
		o.readBuffer = items
		return o
	}
}

// withBatchSource installs the caller's item source.
func withBatchSource(source BatchSource) Option {
	return func(o *option) *option {
		o.batch = source
		return o
	}
}

func WithProgressBar() Option {
	return WithEventHandler(NewProgressBar())
}

// WithHashPolicy sets the content hash policy, which also decides how the stored hash
// cache is read and refreshed. It defaults to HashOff.
func WithHashPolicy(policy HashPolicy) Option {
	return func(o *option) *option {
		o.hashPolicy = policy
		return o
	}
}

func WithLogger(logger *logrus.Logger) Option {
	return func(o *option) *option {
		o.logger = logger
		return o
	}
}

func WithEventHandler(h EventHandler) Option {
	return func(o *option) *option {
		o.eventHanders = append(o.eventHanders, h)
		return o
	}
}
