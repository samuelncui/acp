package acp

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/samuelncui/acp/internal/fileio"
	"github.com/sirupsen/logrus"
)

type counter struct {
	bytes, files int64
}

// buildJob resolves one submitted item into a job. An item ACP cannot describe becomes a job
// that reports the failure through its own result, not a pipeline failure.
func (c *StreamCopyer) buildJob(item Item, order uint64) *baseJob {
	// A failed target description still evaluates the source once for its terminal result.
	targets, err := itemTargets(item)
	if err != nil {
		c.logf(logrus.ErrorLevel, "read item targets failed, err= %v", err)
		job := c.namedFailureJob(item, order)
		job.itemError = err
		return job
	}

	// Record source facts only after both caller-owned descriptions are available.
	job := c.namedFailureJob(item, order)
	job.targets = targets
	if job.itemError == nil {
		job.itemError = c.indexItem(job)
	}
	if job.itemError != nil {
		c.logf(logrus.ErrorLevel, "read item failed, %v", job.itemError)
	}
	return job
}

// namedFailureJob asks for the source exactly once, including when the caller panics.
func (c *StreamCopyer) namedFailureJob(item Item, order uint64) *baseJob {
	job := &baseJob{copyer: c, item: item, order: order, readMode: c.fromDevice.readMode}
	var sourceName string
	job.itemError = protectCall("Item.Source", func() { sourceName = item.Source() })
	if job.itemError == nil {
		job.path = sourceName
	}
	return job
}

// indexItem owns source facts and target decisions; later stages reuse them unchanged.
func (c *StreamCopyer) indexItem(job *baseJob) error {
	// Validate the read policy before acquiring file resources.
	if len(job.targets) > 0 && !c.hashPolicy.appliesToTransfer() {
		return fmt.Errorf(
			"check hash policy failed, policy= %s, source= %q: a copy always reads its source so it cannot reuse a stored hash",
			c.hashPolicy, job.path,
		)
	}
	if selected, ok := job.item.(ReadModeItem); ok {
		if err := protectCall("Item.ReadMode", func() { job.readMode = selected.ReadMode() }); err != nil {
			return err
		}
		if job.readMode != ReadBuffered && job.readMode != ReadMapped {
			return fmt.Errorf("unknown item read mode, mode= %s", job.readMode)
		}
	}

	// Resolve optional endpoints before treating any identifier as a native path.
	if item, ok := job.item.(ContentItem); ok {
		if err := protectCall("Item.ContentSource/ContentTargets", func() {
			job.contentSource = item.ContentSource()
			job.contentTargets = append([]ContentTarget(nil), item.ContentTargets()...)
		}); err != nil {
			return err
		}
		if len(job.contentTargets) != 0 && len(job.contentTargets) != len(job.targets) {
			return fmt.Errorf("content targets must align with target identifiers")
		}
		job.endpoints = job.contentSource != nil
		contentTargets := 0
		for _, target := range job.contentTargets {
			if target != nil && !c.toDevice.linear {
				return fmt.Errorf("content targets require a linear target device")
			}
			if target != nil {
				contentTargets++
			}
			job.endpoints = job.endpoints || target != nil
		}
		if contentTargets > 1 {
			return fmt.Errorf("an item supports at most one content target")
		}
	}
	if job.contentSource != nil && !c.fromDevice.linear {
		return fmt.Errorf("content sources require a linear source device")
	}
	if job.contentSource != nil && job.readMode != ReadBuffered {
		return fmt.Errorf("content sources require buffered reads")
	}
	if job.contentSource == nil {
		job.path = filepath.Clean(job.path)
	}

	// Enumeration and rewrite already own a source snapshot; direct items obtain it here.
	var info os.FileInfo
	switch item := job.item.(type) {
	case *compatItem:
		info = item.info
	case *fileio.RewriteItem:
		info = item.Info
	}
	if info == nil && job.contentSource == nil {
		var err error
		info, err = os.Stat(job.path)
		if err != nil {
			return fmt.Errorf("get source stat failed, source= %q, %w", job.path, err)
		}
	}
	if info != nil && !info.Mode().IsRegular() {
		return fmt.Errorf("source is not a regular file, source= %q, mode= %s", job.path, info.Mode())
	}
	var err error
	if info != nil {
		job.stat, err = newStatWithContent(job.path, info, job.endpoints)
	}
	if err != nil {
		return fmt.Errorf("read source stat failed, source= %q, %w", job.path, err)
	}

	// Snapshot already-captured native metadata for stream headers without listing attributes again.
	if job.endpoints && info != nil {
		job.metadata, err = snapshotContentMetadata(nativeContentMetadata(job.stat))
		if err != nil {
			return err
		}
	}

	// Rewrite alone can replace its own source, using the temporary it already recorded.
	if item, ok := job.item.(*fileio.RewriteItem); ok {
		dev, err := c.getDevice(item.Path)
		if err != nil {
			return fmt.Errorf("get target device failed, %w", err)
		}
		job.outputs = []targetSpec{{name: item.Path, path: item.Path, device: dev, output: item.Output}}
		return nil
	}
	job.outputs = make([]targetSpec, 0, len(job.targets))
	for i, name := range job.targets {
		if len(job.contentTargets) != 0 && job.contentTargets[i] != nil {
			job.outputs = append(job.outputs, targetSpec{name: name, content: job.contentTargets[i]})
			continue
		}
		target, err := c.indexTarget(name, info)
		if err != nil {
			job.fail(name, err)
			continue
		}
		job.outputs = append(job.outputs, target)
	}
	return nil
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
