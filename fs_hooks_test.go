package acp

import (
	"os"

	"github.com/samuelncui/acp/internal/fileio"
	"github.com/samuelncui/acp/signature"
)

// fsHooks replaces individual file operations of the native transfer filesystem; a nil hook keeps
// the native operation, which a hook reaches through the native functions it wraps.
type fsHooks struct {
	open           func(path string, mode ReadMode, info os.FileInfo) (itemSource, error)
	allocate       func(file *os.File, size int64) error
	readEntry      func(file *os.File, info os.FileInfo) (signature.Entry, signature.Status, error)
	writeEntry     func(file *os.File, entry signature.Entry) error
	writeEntryPath func(path string, entry signature.Entry) error
	restore        func(path string, info *stat) error
	commit         func(out *fileio.Output, overwrite bool) error
}

// withFS installs the hooks on one run, over any filesystem an earlier option installed.
func withFS(hooks fsHooks) Option {
	return func(o *option) *option {
		previous := o.wrapFilesystem
		o.wrapFilesystem = func(base transferFilesystem) transferFilesystem {
			if previous != nil {
				base = previous(base)
			}
			return hookFilesystem{transferFilesystem: base, hooks: hooks}
		}
		return o
	}
}

type hookFilesystem struct {
	transferFilesystem
	hooks fsHooks
}

func (f hookFilesystem) Open(path string, mode ReadMode, info os.FileInfo) (itemSource, error) {
	if f.hooks.open != nil {
		return f.hooks.open(path, mode, info)
	}
	return f.transferFilesystem.Open(path, mode, info)
}

func (f hookFilesystem) Create(job *writeJob, target targetSpec) (transferOutput, error) {
	output, err := f.transferFilesystem.Create(job, target)
	if err != nil {
		return output, err
	}
	return hookOutput{transferOutput: output, hooks: f.hooks}, nil
}

func (f hookFilesystem) Allocate(file *os.File, size int64) error {
	if f.hooks.allocate != nil {
		return f.hooks.allocate(file, size)
	}
	return f.transferFilesystem.Allocate(file, size)
}

func (f hookFilesystem) ReadEntry(file *os.File, info os.FileInfo) (signature.Entry, signature.Status, error) {
	if f.hooks.readEntry != nil {
		return f.hooks.readEntry(file, info)
	}
	return f.transferFilesystem.ReadEntry(file, info)
}

func (f hookFilesystem) WriteEntry(file *os.File, entry signature.Entry) error {
	if f.hooks.writeEntry != nil {
		return f.hooks.writeEntry(file, entry)
	}
	return f.transferFilesystem.WriteEntry(file, entry)
}

func (f hookFilesystem) WriteEntryPath(path string, entry signature.Entry) error {
	if f.hooks.writeEntryPath != nil {
		return f.hooks.writeEntryPath(path, entry)
	}
	return f.transferFilesystem.WriteEntryPath(path, entry)
}

// hookOutput intercepts the metadata restore and the commit of a native output.
type hookOutput struct {
	transferOutput
	hooks fsHooks
}

func (o hookOutput) Restore(info *stat) error {
	if native, ok := o.transferOutput.(*nativeTransferOutput); ok && o.hooks.restore != nil {
		return o.hooks.restore(native.output.Temporary, info)
	}
	return o.transferOutput.Restore(info)
}

func (o hookOutput) Commit(overwrite bool) error {
	if native, ok := o.transferOutput.(*nativeTransferOutput); ok && o.hooks.commit != nil {
		return o.hooks.commit(native.output, overwrite)
	}
	return o.transferOutput.Commit(overwrite)
}
