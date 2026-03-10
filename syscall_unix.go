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

type xattr struct {
	key   string
	value []byte
}

type sysStat struct {
	*syscall.Stat_t
	xattrs []xattr
}

func readSysStat(path string, stat fs.FileInfo) (*sysStat, error) {
	sysstat, ok := stat.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, fmt.Errorf("stat sys failed, %T", stat.Sys())
	}

	xattrs, err := readXattrs(path)
	if err != nil {
		return nil, fmt.Errorf("read xattrs failed, %w", err)
	}

	return &sysStat{Stat_t: sysstat, xattrs: xattrs}, nil
}

func writeSysStat(name string, j *stat) error {
	if err := writeXattrs(name, j.sys.xattrs); err != nil {
		return fmt.Errorf("write xattr fail, %w", err)
	}
	if err := os.Chmod(name, j.mode); err != nil {
		return fmt.Errorf("chmod fail, %w", err)
	}
	if os.Geteuid() == 0 {
		if err := os.Chown(name, int(j.sys.Uid), int(j.sys.Gid)); err != nil {
			return fmt.Errorf("chown fail, %w", err)
		}
	}
	if err := os.Chtimes(name, j.modTime, j.modTime); err != nil {
		return fmt.Errorf("chtimes fail, %w", err)
	}
	return nil
}

func readXattrs(path string) ([]xattr, error) {
	size, err := unix.Listxattr(path, nil)
	if err != nil {
		if errors.Is(err, unix.ENOTSUP) || isNoAttrErr(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("count xattrs failed, %w", err)
	}
	if size == 0 {
		return nil, nil
	}

	keyBuf := make([]byte, size)
	n, err := unix.Listxattr(path, keyBuf)
	if err != nil {
		if errors.Is(err, unix.ENOTSUP) || isNoAttrErr(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("list xattrs failed, %w", err)
	}
	keyBuf = keyBuf[:n]

	start := 0
	xattrs := make([]xattr, 0)
	for i, b := range keyBuf {
		if b != 0 {
			continue
		}
		if i <= start {
			start = i + 1
			continue
		}

		name := string(keyBuf[start:i])
		start = i + 1
		if name == "" {
			continue
		}
		if !checkXattrKey(name) {
			continue
		}

		valSize, err := unix.Getxattr(path, name, nil)
		if err != nil {
			if isNoAttrErr(err) {
				continue
			}
			return nil, err
		}
		if valSize == 0 {
			xattrs = append(xattrs, xattr{key: name, value: []byte{}})
			continue
		}

		val := make([]byte, valSize)
		n, err := unix.Getxattr(path, name, val)
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
