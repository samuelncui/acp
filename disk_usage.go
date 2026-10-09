package acp

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"syscall"
	"time"

	"github.com/samuelncui/godf"
	"github.com/sirupsen/logrus"
)

var (
	ErrTargetNoSpace        = fmt.Errorf("acp: target have no space")
	ErrTargetDropToReadonly = fmt.Errorf("acp: target droped into readonly")
	// ErrTargetIO reports an I/O failure of one target. Unlike ErrTargetDropToReadonly it does
	// not describe the whole device, so it must not abort a target that is still writable.
	ErrTargetIO = fmt.Errorf("acp: target have io error")

	errorMapping = []errorPair{
		{from: syscall.ENOSPC, to: ErrTargetNoSpace},
		{from: syscall.EROFS, to: ErrTargetDropToReadonly},
		{from: syscall.EIO, to: ErrTargetIO},
	}
	abortErrors = []error{
		ErrTargetNoSpace,
		ErrTargetDropToReadonly,
	}
)

type errorPair struct {
	from error
	to   error
}

const spaceRefreshInterval = 5 * time.Second

// The ordered writer owns the monitor map; each monitor shares only its cached observation.
// No filesystem call runs while lock is held.
type spaceEstimate struct {
	lock      sync.Mutex
	available int64
	debited   int64
	known     bool
	err       error
	stop      chan struct{}
	done      chan struct{}
}

func (s *spaceEstimate) admit(size int64) error {
	// Unknown capacity permits writing; a completed observation retains its failure identity.
	s.lock.Lock()
	defer s.lock.Unlock()
	if s.err != nil {
		return s.err
	}
	if s.known && size > s.available {
		return fmt.Errorf("%w, want=%d have=%d", ErrTargetNoSpace, size, s.available)
	}

	// Never refund failed output cleanup or lose admissions made during an in-flight query.
	s.debited += size
	if s.known {
		s.available -= size
	}
	return nil
}

func (s *spaceEstimate) refresh(query func() (int64, error)) error {
	// Start charging overlapping admissions before the blocking observation, outside the writer's lock.
	s.lock.Lock()
	s.debited = 0
	s.lock.Unlock()
	available, err := query()

	// Publish one coherent result, conservatively charging admissions overlapping the query.
	s.lock.Lock()
	defer s.lock.Unlock()
	s.available = max(0, available-s.debited)
	s.known, s.err = err == nil, err
	return err
}

func (c *StreamCopyer) checkLinearSpace(job *writeJob, target targetSpec) error {
	// Empty files neither need an observation nor consume another file's estimate.
	size := job.stat.size
	if size <= 0 {
		return nil
	}

	// Each encountered mount has one serial polling worker for this stream's lifetime.
	if c.linearSpace == nil {
		c.linearSpace = make(map[string]*spaceEstimate)
	}
	sample := c.linearSpace[target.device]
	if sample == nil {
		sample = &spaceEstimate{stop: make(chan struct{}), done: make(chan struct{})}
		c.linearSpace[target.device] = sample
		go c.pollSpace(target.device, sample)
	}
	return sample.admit(size)
}

func (c *StreamCopyer) pollSpace(device string, sample *spaceEstimate) {
	// Completion follows panic recovery, so Wait also observes a failed polling worker.
	defer close(sample.done)
	c.wrap(context.Background(), func() {
		// One query at a time bounds work even when statfs takes longer than the interval.
		ticker := time.NewTicker(spaceRefreshInterval)
		defer ticker.Stop()
		query := func() (int64, error) { return c.availableSpace(device) }
		for {
			if err := sample.refresh(query); err != nil {
				c.logf(logrus.WarnLevel, "refresh target capacity failed, mount=%q, err=%v", device, err)
			}
			select {
			case <-sample.stop:
				return
			case <-c.hardStop:
				return
			case <-ticker.C:
			}
			// A pending tick must not start another query after shutdown was requested.
			select {
			case <-sample.stop:
				return
			case <-c.hardStop:
				return
			default:
			}
		}
	})
}

func (c *StreamCopyer) stopSpaceMonitors() {
	// Writers have drained before shutdown; stop every ticker before joining any blocked query.
	for _, sample := range c.linearSpace {
		if sample.stop != nil {
			close(sample.stop)
		}
	}
	for _, sample := range c.linearSpace {
		if sample.done != nil {
			<-sample.done
		}
	}
	c.linearSpace = nil
}

// availableSpace reads filesystem capacity; ordinary targets rely on real allocation errors.
func availableSpace(mountPoint string) (int64, error) {
	usage, err := godf.NewDiskUsage(mountPoint)
	if err != nil {
		return 0, fmt.Errorf("get disk usage failed, mount=%q, %w", mountPoint, err)
	}
	return usage.Available(), nil
}

func mappingError(err error) error {
	if err == nil {
		return nil
	}

	// A cleanup error may have a different identity from the primary I/O failure.
	for _, p := range errorMapping {
		if errors.Is(err, p.from) && !errors.Is(err, p.to) {
			err = errors.Join(p.to, err)
		}
	}

	return err
}

func checkErrorAbort(err error) bool {
	for _, e := range abortErrors {
		if errors.Is(err, e) {
			return true
		}
	}

	return false
}
