package acp

import (
	"context"
	"sync"

	"github.com/sirupsen/logrus"
)

type Copyer struct {
	*option
	running sync.WaitGroup
	eventCh chan Event
}

func New(ctx context.Context, opts ...Option) (*Copyer, error) {
	opt := newOption()
	for _, o := range opts {
		if o == nil {
			continue
		}
		opt = o(opt)
	}
	if err := opt.check(); err != nil {
		return nil, err
	}

	c := &Copyer{
		option:  opt,
		eventCh: make(chan Event, 128),
	}

	c.running.Add(1)
	go wrap(ctx, func() { c.run(ctx) })

	return c, nil
}

func (c *Copyer) Wait() {
	c.running.Wait()
}

func (c *Copyer) run(ctx context.Context) error {
	defer c.running.Done()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	go wrap(ctx, func() { c.eventLoop(ctx) })

	indexed, err := c.index(ctx)
	if err != nil {
		return err
	}

	prepared := c.prepare(ctx, indexed)
	copyed := c.copy(ctx, prepared)
	c.cleanupJob(ctx, copyed)

	// empty pipes
	for range indexed {
	}
	for range prepared {
	}
	for range copyed {
	}

	return nil
}

func (c *Copyer) eventLoop(ctx context.Context) {
	chans := make([]chan Event, len(c.eventHanders))
	for idx := range chans {
		chans[idx] = make(chan Event, 128)
	}

	for idx, ch := range chans {
		handler := c.eventHanders[idx]
		events := ch

		go wrap(ctx, func() {
			for {
				e, ok := <-events
				if !ok {
					handler(&EventFinished{})
					return
				}
				handler(e)
			}
		})
	}

	defer func() {
		for _, ch := range chans {
			close(ch)
		}
	}()
	for {
		select {
		case e, ok := <-c.eventCh:
			if !ok {
				return
			}
			for _, ch := range chans {
				ch <- e
			}
		case <-ctx.Done():
			return
		}
	}
}

func (c *Copyer) logf(l logrus.Level, format string, args ...any) {
	c.logger.Logf(l, format, args...)
}

func (c *Copyer) submit(e Event) {
	c.eventCh <- e
}

func (c *Copyer) reportError(src, dst string, err error) {
	e := &Error{Src: src, Dst: dst, Err: err}
	c.logf(logrus.ErrorLevel, e.Error())
	c.submit(&EventReportError{Error: e})
}
