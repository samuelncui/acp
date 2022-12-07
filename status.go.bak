package acp

import "sync/atomic"

type Status struct {
	Stage       int64
	CopyedBytes int64
	TotalBytes  int64
	CopyedFiles int64
	TotalFiles  int64
}

func (c *Copyer) Status() Status {
	return Status{
		Stage:       atomic.LoadInt64(&c.stage),
		CopyedBytes: atomic.LoadInt64(&c.copyedBytes),
		TotalBytes:  atomic.LoadInt64(&c.totalBytes),
		CopyedFiles: atomic.LoadInt64(&c.copyedFiles),
		TotalFiles:  atomic.LoadInt64(&c.totalFiles),
	}
}
