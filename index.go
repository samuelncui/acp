package acp

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/sirupsen/logrus"
)

type counter struct {
	bytes, files int64
}

// buildJob resolves one submitted item into a job. An item ACP cannot describe becomes a job
// that reports the failure through its own result, not a pipeline failure.
func (c *StreamCopyer) buildJob(item Item, order uint64) *baseJob {
	targets, err := itemTargets(item)
	if err != nil {
		c.logf(logrus.ErrorLevel, "read item targets failed, err= %v", err)
		job := c.namedFailureJob(item, order)
		job.itemError = err
		return job
	}

	job, err := c.newJob(item, targets, order)
	if err == nil {
		return job
	}
	c.logf(logrus.ErrorLevel, "read item failed, %v", err)
	if job == nil {
		job = c.namedFailureJob(item, order)
	}
	job.itemError = err
	return job
}

// namedFailureJob builds the job of an item ACP could not describe, so the failure still names
// the item. A source the caller cannot report leaves the job unnamed.
func (c *StreamCopyer) namedFailureJob(item Item, order uint64) *baseJob {
	job := &baseJob{copyer: c, item: item, order: order}

	var sourceName string
	if err := protectCall("Item.Source", func() { sourceName = item.Source() }); err != nil {
		return job
	}

	job.path = filepath.Clean(sourceName)
	return job
}

// newJob resolves one item's source facts and validates the run options that apply to it. The
// returned job carries the identity it resolved even when it reports an error, so the failure
// still names the item.
func (c *StreamCopyer) newJob(item Item, targets []string, order uint64) (*baseJob, error) {
	var sourceName string
	if err := protectCall("Item.Source", func() { sourceName = item.Source() }); err != nil {
		return nil, err
	}

	path := filepath.Clean(sourceName)
	job := &baseJob{
		copyer:  c,
		item:    item,
		path:    path,
		targets: targets,
		order:   order,
	}

	// A transfer always reads its source, so a policy that trades a computed hash for a
	// stored one cannot describe it. Reject it here instead of silently reading the source.
	if len(targets) > 0 && !c.hashPolicy.appliesToTransfer() {
		return job, fmt.Errorf(
			"check hash policy failed, policy= %s, source= '%s': a copy always reads its source so it cannot reuse a stored hash",
			c.hashPolicy, path,
		)
	}

	info, err := os.Stat(path)
	if err != nil {
		return job, fmt.Errorf("get source stat failed, source= '%s', %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return job, fmt.Errorf("source is not a regular file, source= '%s', mode= %s", path, info.Mode())
	}

	stat, err := newStat(path, info)
	if err != nil {
		return job, fmt.Errorf("read source stat failed, source= '%s', %w", path, err)
	}
	job.stat = stat

	return job, nil
}

// itemTargets copies the targets the caller requested, turning a panic in the caller's code
// into an item failure.
func itemTargets(item Item) ([]string, error) {
	var targets []string
	if err := protectCall("Item.Targets", func() { targets = item.Targets() }); err != nil {
		return nil, err
	}
	return append([]string(nil), targets...), nil
}
