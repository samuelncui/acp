package acp

import (
	"fmt"
	"os"
	"path"
	"sort"
	"strings"

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
	src []*source
	dst []string

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
	sort.Slice(o.src, func(i, j int) bool {
		return comparePath(o.src[i].path, o.src[j].path) < 0
	})
	for _, s := range o.src {
		src := s.src()
		if _, err := os.Stat(src); err != nil {
			return fmt.Errorf("check src path '%s', %w", src, err)
		}
	}

	o.fromDevice.check()
	o.toDevice.check()
	if o.logger == nil {
		o.logger = logrus.StandardLogger()
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
			o.src = append(o.src, &source{base: base, path: []string{name}})
		}
		return o
	}
}

func AccurateSource(base string, paths ...[]string) Option {
	return func(o *option) *option {
		base = path.Clean(base)
		for _, path := range paths {
			o.src = append(o.src, &source{base: base, path: path})
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
