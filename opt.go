package acp

import (
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

type option struct {
	accurateJobs []*accurateJob
	wildcardJobs []*wildcardJob
	streamSource StreamSource
	streamSink   StreamSink

	fromDevice *deviceOption
	toDevice   *deviceOption

	createFlag int
	withHash   bool

	logger       *logrus.Logger
	eventHanders []EventHandler
}

func newOption() *option {
	return &option{
		fromDevice: new(deviceOption),
		toDevice:   new(deviceOption),
		createFlag: os.O_WRONLY | os.O_CREATE | os.O_EXCL,
	}
}

func (o *option) check() error {
	for _, job := range o.wildcardJobs {
		if err := job.check(); err != nil {
			return err
		}
	}

	o.fromDevice.check()
	o.toDevice.check()
	if o.fromDevice.linear || o.toDevice.linear {
		o.fromDevice.threads = 1
		o.toDevice.threads = 1
	}
	if o.logger == nil {
		o.logger = logrus.StandardLogger()
	}

	return nil
}

type Option func(*option) *option

type accurateJob struct {
	src  string
	dsts []string
}

func AccurateJob(src string, dsts []string) Option {
	return func(o *option) *option {
		o.accurateJobs = append(o.accurateJobs, &accurateJob{src: filepath.Clean(src), dsts: dsts})
		return o
	}
}

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

func Overwrite(b bool) Option {
	return func(o *option) *option {
		if b {
			o.createFlag = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
			return o
		}

		o.createFlag = os.O_WRONLY | os.O_CREATE | os.O_EXCL
		return o
	}
}

func WithProgressBar() Option {
	return WithEventHandler(NewProgressBar())
}

func WithHash(b bool) Option {
	return func(o *option) *option {
		o.withHash = b
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
