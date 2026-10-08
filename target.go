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
		target.path, err = resolveOutputPath(name)
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

	// Refuse source aliases and unsupported target types before any output is created.
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

	// Retain the mount observation alongside the canonical destination identity.
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

	// Anchor relative paths without cleaning away link/.. before the symlink walk.
	// Windows drive-relative paths use that drive's cwd; root-relative paths use its volume.
	if !filepath.IsAbs(path) {
		volume := filepath.VolumeName(path)
		base, err := filepath.Abs(volume + ".")
		if err != nil {
			return "", err
		}
		path = path[len(volume):]
		if len(path) > 0 && os.IsPathSeparator(path[0]) {
			path = filepath.VolumeName(base) + path
		} else {
			path = base + string(filepath.Separator) + path
		}
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
	// Linear media cannot preallocate; reject insufficient estimated space before output creation.
	if c.toDevice.linear {
		if err := c.checkLinearSpace(job, target); err != nil {
			return nil, err
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
