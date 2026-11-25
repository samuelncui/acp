package acp

import (
	"io"
	"os"

	"github.com/samuelncui/acp/internal/fileio"
	"github.com/samuelncui/acp/signature"
)

// transferFilesystem substitutes file operations without exposing transfer ownership to callers.
type transferFilesystem interface {
	Open(path string, mode ReadMode, info os.FileInfo) (itemSource, error)
	Create(job *writeJob, target targetSpec) (transferOutput, error)
	// Allocate preallocates a random output.
	Allocate(file *os.File, size int64) error
	// ReadEntry, WriteEntry and WriteEntryPath operate on the managed signature entry of a file.
	ReadEntry(file *os.File, info os.FileInfo) (signature.Entry, signature.Status, error)
	WriteEntry(file *os.File, entry signature.Entry) error
	WriteEntryPath(path string, entry signature.Entry) error
}

// transferOutput retains the owned path after Close until Commit or Discard settles it.
type transferOutput interface {
	io.WriteCloser
	Sync() error
	// Cache publishes the output's managed signature entry. An encoded output always has one and
	// reports its write failure; a plain output's entry is optional and never fails.
	Cache(job *baseJob, stored int64, storedHash []byte) error
	Restore(*stat) error
	Commit(overwrite bool) error
	Discard() error
}

type nativeTransferFilesystem struct {
	copyer *StreamCopyer
}

func (f nativeTransferFilesystem) Open(path string, mode ReadMode, info os.FileInfo) (itemSource, error) {
	return openNativeSource(path, mode, info)
}

func (f nativeTransferFilesystem) Create(job *writeJob, target targetSpec) (transferOutput, error) {
	// The existing allocator owns cleanup until it hands an output to this adapter.
	output, err := f.copyer.prepareTarget(job, target)
	if err != nil {
		return nil, err
	}
	return &nativeTransferOutput{copyer: f.copyer, target: target, output: output}, nil
}

func (nativeTransferFilesystem) Allocate(file *os.File, size int64) error {
	return truncate(file, size)
}

func (nativeTransferFilesystem) ReadEntry(file *os.File, info os.FileInfo) (signature.Entry, signature.Status, error) {
	return signature.ReadFile(file, info)
}

func (nativeTransferFilesystem) WriteEntry(file *os.File, entry signature.Entry) error {
	return signature.WriteFile(file, entry)
}

func (nativeTransferFilesystem) WriteEntryPath(path string, entry signature.Entry) error {
	return signature.WritePath(path, entry)
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
	return o.output.Close()
}

func (o *nativeTransferOutput) Sync() error {
	return o.output.File.Sync()
}

func (o *nativeTransferOutput) Cache(job *baseJob, stored int64, storedHash []byte) error {
	// The entry of an encoded file is its marker, so its failure belongs to the target.
	if job.encode != nil {
		return o.copyer.writeEncodedMarker(o.output.File, o.output.Temporary, job, stored, storedHash)
	}

	// Linear completion has closed its descriptor but still owns the temporary path.
	if o.output.File == nil {
		o.copyer.refreshCachePath(o.output.Temporary, job, true)
		return nil
	}

	// Random completion retains its descriptor until metadata and Sync finish.
	o.copyer.refreshCacheEntry(o.output.File, o.target.name, job, true)
	return nil
}

func (o *nativeTransferOutput) Restore(info *stat) error {
	return writeSysStat(o.output.Temporary, info)
}

func (o *nativeTransferOutput) Commit(overwrite bool) error {
	return o.output.Commit(overwrite)
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
