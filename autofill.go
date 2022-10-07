package acp

import (
	"fmt"
	"math"
	"strings"
)

func (c *Copyer) applyAutoFillLimit() error {
	if !c.autoFill {
		return nil
	}

	counts := make(map[string]int, len(c.dst))
	infos := make(map[string]*fileSystem, len(c.dst))
	for _, d := range c.dst {
		fsInfo, err := getFileSystem(d)
		if err != nil {
			return fmt.Errorf("get file system fail, %s, %w", d, err)
		}

		infos[d] = fsInfo
		counts[d] = counts[d] + 1
	}

	min := int64(math.MaxInt64)
	for mp, info := range infos {
		size := info.AvailableSize / int64(counts[mp])
		if size < min {
			min = size
		}
	}

	c.jobsLock.Lock()
	defer c.jobsLock.Unlock()

	idx := c.getAutoFillCutoffIdx(min)
	if idx < 0 {
		return nil
	}
	if idx == 0 {
		return fmt.Errorf("cannot found available auto fill slice, filesystem_size= %d", min)
	}

	cutoff := c.jobs[idx:]
	c.jobs = c.jobs[:idx]
	last := ""
	for _, job := range cutoff {
		if job.parent != nil {
			job.parent.done(job)
		}
		if strings.HasPrefix(job.source.relativePath, last) {
			continue
		}

		c.noSpaceSource = append(c.noSpaceSource, job.source)
		last = job.source.relativePath + "/"
	}
	return nil
}

func (c *Copyer) getAutoFillCutoffIdx(limit int64) int {
	left := limit
	targetIdx := -1
	for idx, job := range c.jobs {
		left -= job.size
		if left < 0 {
			targetIdx = idx
		}
	}
	if targetIdx < 0 {
		return -1
	}

	if c.autoFillSplitDepth <= 0 {
		return targetIdx
	}

	for idx := targetIdx; idx >= 0; idx-- {
		if strings.Count(c.jobs[idx].source.relativePath, "/") < c.autoFillSplitDepth {
			return idx
		}
	}

	return 0
}
