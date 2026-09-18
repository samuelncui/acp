package acp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const (
	UnexpectFileMode = os.ModeType &^ os.ModeDir
)

type counter struct {
	bytes, files int64
}

// index reads caller-owned items in batches and turns them into jobs. It stops asking the
// batch source for work as soon as the pipeline stops, and reports the items of the batch
// it already holds rather than dropping them.
func (c *Copyer) index(ctx context.Context) (<-chan *baseJob, error) {
	if c.batch == nil {
		return nil, fmt.Errorf("batch source is nil")
	}

	// The channel capacity is the read buffer, so no separate prefetch stage is needed.
	ch := make(chan *baseJob, c.readBuffer)
	go wrap(ctx, func() {
		defer close(ch)

		var bytes, files int64
		var order uint64
		defer func() {
			c.submit(&EventUpdateCount{Bytes: bytes, Files: files, Finished: true})
		}()

		for {
			// A stop ends the feed without asking the caller for another batch.
			if c.stopped(ctx) {
				return
			}

			batch, err := c.batch.Next(ctx)
			if err != nil {
				if !errors.Is(err, io.EOF) {
					err = fmt.Errorf("read batch source failed, %w", err)
					c.reportError("", "", err)
					c.setError(err)
				}
				return
			}

			for index, item := range batch {
				if !c.accept(ch, item, order, &bytes, &files) {
					c.abandonItems(ctx, batch[index:])
					return
				}
				order++
			}
			c.submit(&EventUpdateCount{Bytes: bytes, Files: files})
		}
	})
	return ch, nil
}

// accept turns one item into a job and hands it to the read buffer. It reports false when
// the pipeline stopped before the job was handed over.
func (c *Copyer) accept(ch chan<- *baseJob, item Item, order uint64, bytes, files *int64) bool {
	if item == nil {
		c.reportError("", "", fmt.Errorf("read batch source failed, item is nil"))
		return false
	}

	job, err := c.newJob(item, order)
	if err != nil {
		c.reportItemError(item.Source(), "", err)
		job = &baseJob{copyer: c, item: item, order: order, targets: itemTargets(item)}
		job.itemError = err
	} else {
		*files++
		*bytes += job.stat.size
	}

	select {
	case ch <- job:
		return true
	case <-c.hardStop:
		return false
	}
}

// abandonItems reports accepted items that the pipeline stopped before processing.
func (c *Copyer) abandonItems(ctx context.Context, items []Item) {
	reason := c.abandonment(ctx)
	for _, item := range items {
		if item == nil {
			continue
		}
		c.abandon(&baseJob{copyer: c, item: item, targets: itemTargets(item)}, reason)
	}
}

// newJob resolves one item's source facts. A source that cannot be described is reported
// as a failed item, not as a pipeline failure.
func (c *Copyer) newJob(item Item, order uint64) (*baseJob, error) {
	path := filepath.Clean(item.Source())
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("get source stat failed, source= '%s', %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("source is not a regular file, source= '%s', mode= %s", path, info.Mode())
	}

	stat, err := newStat(path, info)
	if err != nil {
		return nil, fmt.Errorf("read source stat failed, source= '%s', %w", path, err)
	}

	job := &baseJob{
		copyer:  c,
		item:    item,
		src:     &source{base: filepath.Dir(path), path: filepath.Base(path)},
		path:    path,
		stat:    stat,
		targets: itemTargets(item),
		order:   order,
	}
	c.submit(&EventUpdateJob{job.report()})

	return job, nil
}

func itemTargets(item Item) []string {
	return append([]string(nil), item.Targets()...)
}
