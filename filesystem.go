package acp

import (
	"io"
	"os"

	"github.com/samuelncui/acp/internal/fileio"
)

var closeTarget = (*fileio.Output).Close

// transferFilesystem substitutes file operations without exposing transfer ownership to callers.
type transferFilesystem interface {
	Open(path string, mode ReadMode, info os.FileInfo) (itemSource, error)
	Create(job *writeJob, target targetSpec) (transferOutput, error)
}

// transferOutput retains the owned path after Close until Commit or Discard settles it.
type transferOutput interface {
	io.WriteCloser
	Sync() error
	Cache(job *baseJob, linear bool)
	Restore(*stat) error
	Commit(overwrite bool) error
	Discard() error
}

type nativeTransferFilesystem struct {
	copyer *StreamCopyer
}

func (f nativeTransferFilesystem) Open(path string, mode ReadMode, info os.FileInfo) (itemSource, error) {
	return openSourceContent(path, mode, info)
}

func (f nativeTransferFilesystem) Create(job *writeJob, target targetSpec) (transferOutput, error) {
	// The existing allocator owns cleanup until it hands an output to this adapter.
	output, err := f.copyer.prepareTarget(job, target)
	if err != nil {
		return nil, err
	}
	return &nativeTransferOutput{copyer: f.copyer, target: target, output: output}, nil
}

type nativeTransferOutput struct {
	copyer *StreamCopyer
	target targetSpec
	output *fileio.Output
}

func (o *nativeTransferOutput) Write(data []byte) (int, error) {
	return o.output.File.Write(data)
}

func (o *nativeTransferOutput) Close() error {
	return closeTarget(o.output)
}

func (o *nativeTransferOutput) Sync() error {
	return o.output.File.Sync()
}

func (o *nativeTransferOutput) Cache(job *baseJob, linear bool) {
	// Linear completion has closed its descriptor but still owns the temporary path.
	if linear {
		o.copyer.refreshCachePath(o.output.Temporary, job, true)
		return
	}

	// Random completion retains its descriptor until metadata and Sync finish.
	o.copyer.refreshCacheEntry(o.output.File, o.target.name, job, true)
}

func (o *nativeTransferOutput) Restore(info *stat) error {
	return restoreTarget(o.output.Temporary, info)
}

func (o *nativeTransferOutput) Commit(overwrite bool) error {
	return commitTarget(o.output, overwrite)
}

func (o *nativeTransferOutput) Discard() error {
	return o.output.Discard()
}

func (c *StreamCopyer) fs() transferFilesystem {
	if c.filesystem != nil {
		return c.filesystem
	}
	return nativeTransferFilesystem{copyer: c}
}
