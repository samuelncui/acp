package acp

import (
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/sirupsen/logrus"
)

type source struct {
	base         string
	relativePath string
}

func (s *source) path() string {
	return s.base + s.relativePath
}

func (s *source) target(dst string) string {
	return dst + s.relativePath
}

type option struct {
	src []*source
	dst []string

	fromDevice *deviceOption
	toDevice   *deviceOption

	overwrite       bool
	withProgressBar bool
	threads         int
	withHash        bool

	autoFill           bool
	autoFillSplitDepth int

	logger *logrus.Logger
}

func newOption() *option {
	return &option{
		fromDevice: new(deviceOption),
		toDevice:   new(deviceOption),
	}
}

func (o *option) check() error {
	filteredDst := make([]string, 0, len(o.dst))
	for _, p := range o.dst {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if p[len(p)-1] != '/' {
			p = p + "/"
		}

		dstStat, err := os.Stat(p)
		if err != nil {
			return fmt.Errorf("check dst path '%s', %w", p, err)
		}
		if !dstStat.IsDir() {
			return fmt.Errorf("dst path is not a dir")
		}

		filteredDst = append(filteredDst, p)
	}
	o.dst = filteredDst

	if len(o.src) == 0 {
		return fmt.Errorf("source path not found")
	}
	for _, s := range o.src {
		if _, err := os.Stat(s.path()); err != nil {
			return fmt.Errorf("check src path '%s', %w", s.path(), err)
		}
	}

	if o.threads < 1 {
		o.threads = 4
	}
	if o.fromDevice.linear || o.toDevice.linear {
		o.threads = 1
	}
	return nil
}

type Option func(*option) *option

func Source(paths ...string) Option {
	return func(o *option) *option {
		for _, p := range paths {
			p = path.Clean(p)
			if p[len(p)-1] == '/' {
				p = p[:len(p)-1]
			}

			base, name := path.Split(p)
			o.src = append(o.src, &source{base: base, relativePath: name})
		}
		return o
	}
}

func AccurateSource(base string, relativePaths ...string) Option {
	return func(o *option) *option {
		base = path.Clean(base)

		for _, p := range relativePaths {
			p = path.Clean(p)
			if p[len(p)-1] == '/' {
				p = p[:len(p)-1]
			}

			o.src = append(o.src, &source{base: base, relativePath: p})
		}
		return o
	}
}

func Target(paths ...string) Option {
	return func(o *option) *option {
		o.dst = append(o.dst, paths...)
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
		o.overwrite = b
		return o
	}
}

func WithProgressBar(b bool) Option {
	return func(o *option) *option {
		o.withProgressBar = b
		return o
	}
}

func WithThreads(threads int) Option {
	return func(o *option) *option {
		o.threads = threads
		return o
	}
}

func WithHash(b bool) Option {
	return func(o *option) *option {
		o.withHash = b
		return o
	}
}

func WithAutoFill(on bool, depth int) Option {
	return func(o *option) *option {
		o.autoFill = on
		o.autoFillSplitDepth = depth
		return o
	}
}

func WithLogger(logger *logrus.Logger) Option {
	return func(o *option) *option {
		o.logger = logger
		return o
	}
}
