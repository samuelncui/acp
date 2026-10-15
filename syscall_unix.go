//go:build darwin || linux
// +build darwin linux

package acp

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

var listOrdinaryXattrs = unix.Listxattr
var readOrdinaryXattr = unix.Getxattr

type xattr struct {
	key   string
	value []byte
}

type sysStat struct {
	*syscall.Stat_t
	xattrs []xattr
}

func readSysStat(path string, stat fs.FileInfo) (*sysStat, error) {
	return readSysStatWithContent(path, stat, false)
}

func readSysStatWithContent(path string, stat fs.FileInfo, bounded bool) (*sysStat, error) {
	// Native observations retain ownership facts and platform-selected ordinary attributes.
	sysstat, ok := stat.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, fmt.Errorf("stat sys failed, %T", stat.Sys())
	}

	// Capture ordinary attributes once alongside the native ownership observation.
	xattrs, err := readXattrsWithContent(path, bounded)
	if err != nil {
		return nil, fmt.Errorf("read xattrs failed, %w", err)
	}
	return &sysStat{Stat_t: sysstat, xattrs: xattrs}, nil
}

func writeSysStat(name string, j *stat) error {
	// Ordinary attributes precede restrictive permissions.
	if err := writeXattrs(name, j.sys.xattrs); err != nil {
		return fmt.Errorf("write xattr fail, %w", err)
	}

	// Preserve native ownership when observed; endpoint ownership stays with the creating user.
	if err := os.Chmod(name, j.mode); err != nil {
		return fmt.Errorf("chmod fail, %w", err)
	}
	if j.sys.Stat_t != nil && os.Geteuid() == 0 {
		if err := os.Chown(name, int(j.sys.Uid), int(j.sys.Gid)); err != nil {
			return fmt.Errorf("chown fail, %w", err)
		}
	}

	// Restore the observed timestamp after permission and ownership changes.
	if err := os.Chtimes(name, j.modTime, j.modTime); err != nil {
		return fmt.Errorf("chtimes fail, %w", err)
	}
	return nil
}

func readXattrs(path string) ([]xattr, error) {
	return readXattrsWithContent(path, false)
}

func readXattrsWithContent(path string, bounded bool) ([]xattr, error) {
	// Refuse unbounded name lists before allocating their buffers.
	size, err := listOrdinaryXattrs(path, nil)
	if err != nil {
		if errors.Is(err, unix.ENOTSUP) || isNoAttrErr(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("count xattrs failed, %w", err)
	}
	if size == 0 {
		return nil, nil
	}

	if bounded && size > ContentMaxXattrCount*(ContentMaxXattrNameBytes+1) {
		return nil, fmt.Errorf("content xattr name list exceeds metadata limits")
	}
	keyBuf := make([]byte, size)
	n, err := listOrdinaryXattrs(path, keyBuf)
	if err != nil {
		if errors.Is(err, unix.ENOTSUP) || isNoAttrErr(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("list xattrs failed, %w", err)
	}
	keyBuf = keyBuf[:n]

	// Capture each selected attribute once, validating value sizes before allocation.
	start, total := 0, 0
	xattrs := make([]xattr, 0)
	for i, b := range keyBuf {
		if b != 0 {
			continue
		}
		if i <= start {
			start = i + 1
			continue
		}

		if bounded && i-start > ContentMaxXattrNameBytes {
			return nil, fmt.Errorf("content xattr name exceeds metadata limits")
		}
		name := string(keyBuf[start:i])
		start = i + 1
		if name == "" {
			continue
		}
		if !checkXattrKey(name) {
			continue
		}

		valSize, err := readOrdinaryXattr(path, name, nil)
		if err != nil {
			if isNoAttrErr(err) {
				continue
			}
			return nil, err
		}
		if bounded {
			total, err = checkContentXattr(name, valSize, len(xattrs)+1, total)
			if err != nil {
				return nil, err
			}
		}
		if valSize == 0 {
			xattrs = append(xattrs, xattr{key: name, value: []byte{}})
			continue
		}

		val := make([]byte, valSize)
		n, err := readOrdinaryXattr(path, name, val)
		if err != nil {
			if isNoAttrErr(err) {
				continue
			}
			return nil, err
		}

		xattrs = append(xattrs, xattr{key: name, value: val[:n]})
	}

	return xattrs, nil
}

func writeXattrs(path string, xattrs []xattr) error {
	for _, xattr := range xattrs {
		if err := unix.Setxattr(path, xattr.key, xattr.value, 0); err != nil {
			if errors.Is(err, unix.ENOTSUP) ||
				errors.Is(err, unix.EPERM) ||
				errors.Is(err, unix.EROFS) {
				continue
			}
			return err
		}
	}
	return nil
}

func nativeContentMetadata(j *stat) ContentMetadata {
	metadata := ContentMetadata{Info: j.info}
	for _, attr := range j.sys.xattrs {
		if metadata.Xattrs == nil {
			metadata.Xattrs = make(map[string][]byte)
		}
		metadata.Xattrs[attr.key] = attr.value
	}
	return metadata
}

func contentStat(metadata ContentMetadata) *stat {
	// Keep endpoint ownership unknown while preserving portable metadata and ordinary attributes.
	attrs := make([]xattr, 0, len(metadata.Xattrs))
	for name, value := range metadata.Xattrs {
		if !checkXattrKey(name) {
			continue
		}
		attrs = append(attrs, xattr{key: name, value: value})
	}
	return &stat{
		info: metadata.Info, size: metadata.Info.Size(), mode: metadata.Info.Mode(),
		modTime: metadata.Info.ModTime(), sys: &sysStat{xattrs: attrs},
	}
}
