package acp

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	mapset "github.com/deckarep/golang-set/v2"
	"github.com/samuelncui/acp/internal/fileio"
)

// Allocation and metadata restoration retain their platform-specific implementations.
var (
	allocateTarget   = truncate
	restoreTarget    = writeSysStat
	commitTarget     = (*fileio.Output).Commit
	resolveDirectory = filepath.EvalSymlinks
)

// targetSpec holds decisions made once during indexing. name is the caller's logical path;
// path is its resolved destination, which may be a symlink referent.
type targetSpec struct {
	name, path, device string
	direct             bool
	output             *fileio.Output
}

func (c *StreamCopyer) indexTarget(name string, source os.FileInfo) (targetSpec, error) {
	// Resolve existence and source aliases before any target mutation.
	target := targetSpec{name: name}
	info, err := os.Lstat(name)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return target, fmt.Errorf("stat target failed, %w", err)
	}
	if info != nil && info.Mode()&os.ModeSymlink != 0 {
		// An existing link keeps its identity: only its live referent can be replaced.
		target.path, err = filepath.EvalSymlinks(name)
		if err != nil {
			return target, fmt.Errorf("resolve target link failed, %w", err)
		}
		info, err = os.Stat(target.path)
		if err != nil {
			return target, fmt.Errorf("stat target referent failed, %w", err)
		}
	} else {
		// Files in one parent share its stable resolution, avoiding a symlink walk per file.
		// Split preserves link/.. for filesystem resolution; Dir would clean it too early.
		parent, base := filepath.Split(name)
		if base == "" || base == "." || base == ".." {
			return target, fmt.Errorf("target path has no filename")
		}
		if parent == "" {
			parent = "."
		}
		dir, found := c.targetDirs[parent]
		if !found {
			dir, err = resolveOutputPath(parent)
			if err != nil {
				return target, fmt.Errorf("resolve target directory failed, %w", err)
			}
			if c.targetDirs == nil {
				c.targetDirs = make(map[string]string)
			}
			c.targetDirs[parent] = dir
		}
		target.path = filepath.Join(dir, base)
	}
	if info != nil {
		if source != nil && os.SameFile(source, info) {
			return target, fmt.Errorf("source and target are the same file")
		}
		if c.createFlag&os.O_TRUNC == 0 {
			return target, fmt.Errorf("target already exists, %w", os.ErrExist)
		}
		target.direct = info.Mode()&os.ModeDevice != 0
		if !info.Mode().IsRegular() && !target.direct {
			return target, fmt.Errorf("target is not a regular file or device")
		}
	}
	target.device, err = c.getDevice(target.path)
	if err != nil {
		return target, fmt.Errorf("get target device failed, %w", err)
	}
	return target, nil
}

func resolveOutputPath(path string) (string, error) {
	// Find the existing ancestor before walking symlinks, keeping native link/.. semantics.
	var missing []string
	for {
		_, err := os.Lstat(path)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		for len(path) > 0 && os.IsPathSeparator(path[len(path)-1]) {
			path = path[:len(path)-1]
		}
		parent, base := filepath.Split(path)
		if base == "" || base == ".." {
			return "", err
		}
		missing = append(missing, base)
		if parent == "" {
			parent = "."
		}
		path = parent
	}

	// Resolve once, then retain missing descendants beneath that physical directory.
	resolved, err := resolveDirectory(path)
	if err != nil {
		return "", err
	}
	for i := len(missing) - 1; i >= 0; i-- {
		resolved = filepath.Join(resolved, missing[i])
	}
	return resolved, nil
}

func (c *StreamCopyer) prepareTarget(job *writeJob, target targetSpec) (out *fileio.Output, err error) {
	// Linear media cannot preallocate. Check their current free space immediately before writing.
	if c.toDevice.linear && job.stat.size > 0 {
		available, err := c.availableSpace(target.device)
		if err != nil {
			return nil, err
		}
		if job.stat.size > available {
			return nil, fmt.Errorf("%w, want=%d have=%d", ErrTargetNoSpace, job.stat.size, available)
		}
	}

	// Rewrite has already allocated and recorded its output; ordinary files allocate here.
	out = target.output
	defer func() {
		if value := recover(); value != nil {
			err = panicError("prepare target", value)
			c.setError(err)
			c.stopHard()
		}
		if err != nil && out != nil {
			err = errors.Join(err, out.Discard())
			out = nil
		}
	}()
	if out == nil {
		if err := os.MkdirAll(filepath.Dir(target.path), os.ModePerm); err != nil {
			return nil, fmt.Errorf("mkdir target failed, %w", err)
		}
		if target.direct {
			file, err := os.OpenFile(target.path, os.O_WRONLY, 0)
			if err != nil {
				return nil, fmt.Errorf("open device failed, %w", err)
			}
			out = &fileio.Output{File: file, Path: target.path}
		} else {
			out, err = fileio.NewOutput(target.path)
			if err != nil {
				return nil, fmt.Errorf("create temporary target failed, %w", err)
			}
		}
	}
	if !c.toDevice.linear && job.stat.size > 0 {
		if err := allocateTarget(out.File, job.stat.size); err != nil {
			return out, fmt.Errorf("preallocate target failed, %w", err)
		}
	}
	return out, nil
}

func (c *StreamCopyer) targetFailed(job *baseJob, path, dev string, err error, exhausted mapset.Set[string]) {
	// Classify one target's primary and cleanup failures together without losing syscall identities.
	err = mappingError(err)
	// Only actual device errors permanently exhaust a mount; an estimate can be retried.
	if errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EROFS) {
		exhausted.Add(dev)
	}
	c.endLinearTarget(err)
	job.fail(path, err)
}

func (c *StreamCopyer) consumeTarget(job *writeJob, target targetSpec, out *fileio.Output, chunks <-chan *chunkBuffer,
	cacheGate <-chan struct{}, readErr *error, exhausted mapset.Set[string]) {
	// The output owns its descriptor and temporary until commit or cleanup. Every failure is
	// reported against the requested path, and every queued buffer is released.
	var err error
	defer func() {
		if value := recover(); value != nil {
			err = panicError("target writer", value)
			c.setError(err)
			c.stopHard()
		}
		err = errors.Join(err, out.Discard())
		if err != nil {
			c.targetFailed(job.baseJob, target.name, target.device, err, exhausted)
		} else {
			job.success(target.name)
		}
		for chunk := range chunks {
			chunk.release()
		}
	}()

	// Write each source chunk once before completing metadata and the content signature.
	for chunk := range chunks {
		if err = writeChunk(out.File, chunk); err != nil {
			return
		}
	}
	if *readErr != nil {
		err = *readErr
		return
	}
	if cacheGate != nil {
		<-cacheGate
	}
	select {
	case <-c.hardStop:
		err = fmt.Errorf("target stopped by pipeline failure")
		return
	default:
	}
	if !target.direct {
		// Publish the managed attribute while the owned temporary is still writable.
		c.refreshCacheEntry(out.File, target.name, job.baseJob, true)
		if err = restoreTarget(out.Temporary, job.stat); err != nil {
			err = fmt.Errorf("restore target metadata failed, %w", err)
			return
		}
	}
	if !c.toDevice.linear {
		if err = out.File.Sync(); err != nil {
			err = fmt.Errorf("sync target failed, %w", err)
			return
		}
	}

	// Rewrite replaces its own source. Release its descriptor and mapping before rename.
	if target.output != nil {
		if err = job.finishSource(); err != nil {
			err = fmt.Errorf("close rewrite source failed, %w", err)
			return
		}
	}

	// Only a complete, closed output replaces the final path; failure leaves the old file intact.
	if err = commitTarget(out, c.createFlag&os.O_TRUNC != 0); err != nil {
		err = fmt.Errorf("commit target failed, %w", err)
	}
}

func writeChunk(writer io.Writer, chunk *chunkBuffer) error {
	// The writer releases its reference even on panic or a short write.
	defer chunk.release()
	n, err := writer.Write(chunk.data)
	if err != nil {
		return fmt.Errorf("write target failed, %w", err)
	}
	if n != len(chunk.data) {
		return fmt.Errorf("write target failed, %w", io.ErrShortWrite)
	}
	return nil
}
