package acp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

// source is one enumerated source file: base is the directory the relative path is resolved
// against, and path is the source-relative path that also maps onto a target directory.
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

const (
	// defaultReadBuffer is how many items ACP may hold in its read buffer when the caller does
	// not set one. The read buffer is the backpressure limit of the feed.
	defaultReadBuffer = 4096
	// defaultResultBuffer is how many finished results ACP may hold for delivery before the
	// pipeline that produces them blocks.
	defaultResultBuffer = 256
	// defaultResultBatch is how many results one delivery carries.
	defaultResultBatch = 256
	// defaultResultFlushInterval is how long a successful result may wait for its batch.
	defaultResultFlushInterval = time.Second
	// minResultFlushInterval is the shortest result flush interval ACP accepts.
	minResultFlushInterval = 100 * time.Millisecond
)

type option struct {
	// Job options describe what the af05f05c compatibility shell enumerates.
	accurateJobs []*accurateJob
	wildcardJobs []*wildcardJob

	fromDevice *deviceOption
	toDevice   *deviceOption

	createFlag          int
	hashPolicy          HashPolicy
	readBuffer          int
	resultBuffer        int
	resultBatch         int
	resultFlushInterval time.Duration

	logger        *logrus.Logger
	eventHandlers []EventHandler
}

func newOption() *option {
	return &option{
		fromDevice:          new(deviceOption),
		toDevice:            new(deviceOption),
		createFlag:          os.O_WRONLY | os.O_CREATE | os.O_EXCL,
		readBuffer:          defaultReadBuffer,
		resultBuffer:        defaultResultBuffer,
		resultBatch:         defaultResultBatch,
		resultFlushInterval: defaultResultFlushInterval,
	}
}

// check validates the options and settles their defaults. Every value a caller can get wrong is
// rejected here, which is what makes New and NewStream report creation errors instead of
// failing midway through a run.
func (o *option) check() error {
	for _, job := range o.wildcardJobs {
		if err := job.check(); err != nil {
			return err
		}
	}
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
	if o.resultBuffer < 1 {
		return fmt.Errorf("result buffer must be at least one item, items=%d", o.resultBuffer)
	}
	if o.resultBatch < 1 {
		return fmt.Errorf("result batch must be at least one item, items=%d", o.resultBatch)
	}
	if o.resultFlushInterval < minResultFlushInterval {
		return fmt.Errorf("result flush interval must be at least %s, interval= %s", minResultFlushInterval, o.resultFlushInterval)
	}
	if !o.hashPolicy.valid() {
		return fmt.Errorf("unknown hash policy, policy= %s", o.hashPolicy)
	}
	if o.logger == nil {
		o.logger = logrus.StandardLogger()
	}

	return nil
}

// Option configures one run. An option applied twice is last-wins, so a later value replaces an
// earlier one instead of combining with it.
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

// Overwrite selects whether an existing target file is replaced or refused. It is a run-level
// option and applies to every requested target.
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

// WithReadBuffer sets how many items ACP may hold in its read buffer, which is also the feed's
// backpressure limit: Submit blocks while the buffer is full. It defaults to 4096 items.
func WithReadBuffer(items int) Option {
	return func(o *option) *option {
		o.readBuffer = items
		return o
	}
}

// WithResultBuffer sets how many finished results ACP may hold for delivery, which is the
// backpressure limit of the result path: a pipeline stage that produces a result blocks while the
// result buffer is full. A result that carries an error is delivered as soon as it is read, so it
// does not wait for room in a batch. It defaults to 256 items.
func WithResultBuffer(items int) Option {
	return func(o *option) *option {
		o.resultBuffer = items
		return o
	}
}

// WithResultBatch sets how many results one delivery carries: ACP hands the caller's results
// callback at most this many results at a time. It defaults to 256 items.
func WithResultBatch(items int) Option {
	return func(o *option) *option {
		o.resultBatch = items
		return o
	}
}

// WithResultFlushInterval sets how long a successful result may wait for the rest of its batch. It
// defaults to one second and accepts no value below 100ms.
func WithResultFlushInterval(d time.Duration) Option {
	return func(o *option) *option {
		o.resultFlushInterval = d
		return o
	}
}

func WithProgressBar() Option {
	return WithEventHandler(NewProgressBar())
}

// WithHashPolicy sets the content hash policy, which also decides how the stored hash cache is
// read and refreshed. It defaults to HashOff.
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

// WithEventHandler registers the run's event handler. Repeated options are last-wins, so this
// replaces every handler registered before it, and a nil handler clears the registration
// instead of leaving a handler that fails the run when it is called.
func WithEventHandler(h EventHandler) Option {
	return func(o *option) *option {
		o.eventHandlers = nil
		if h == nil {
			return o
		}

		o.eventHandlers = []EventHandler{h}
		return o
	}
}
