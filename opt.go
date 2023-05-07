package acp

import (
	"os"
	"path"

	"github.com/sirupsen/logrus"
)

type source struct {
	base string
	path []string
}

func (s *source) src() string {
	return s.base + path.Join(s.path...)
}

func (s *source) dst(dst string) string {
	return dst + path.Join(s.path...)
}

func (s *source) append(next ...string) *source {
	copyed := make([]string, len(s.path)+len(next))
	copy(copyed, s.path)
	copy(copyed[len(s.path):], next)

	return &source{base: s.base, path: copyed}
}

type option struct {
	accurateJobs []*accurateJob
	wildcardJobs []*wildcardJob

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
		o.accurateJobs = append(o.accurateJobs, &accurateJob{src: src, dsts: dsts})
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

func comparePath(a, b []string) int {
	al, bl := len(a), len(b)

	l := al
	if bl < al {
		l = bl
	}

	for idx := 0; idx < l; idx++ {
		if a[idx] < b[idx] {
			return -1
		}
		if a[idx] > b[idx] {
			return 1
		}
	}

	if al < bl {
		return -1
	}
	if al > bl {
		return 1
	}
	return 0
}

// isChild return -1(not) 0(equal) 1(child)
func isChild(parent, child []string) int {
	pl, cl := len(parent), len(child)
	if pl > cl {
		return -1
	}

	for idx := 0; idx < pl; idx++ {
		if parent[idx] != child[idx] {
			return -1
		}
	}

	if pl == cl {
		return 0
	}
	return 1
}
