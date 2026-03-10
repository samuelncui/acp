//go:build linux
// +build linux

package acp

import (
	"fmt"
	"os"
	"syscall"
)

func truncate(file *os.File, size int64) error {
	if err := syscall.Fallocate(int(file.Fd()), 0, 0, size); err != nil {
		return err
	}
	return nil
}

func copyAttrs(name string, j *baseJob) error {
	if err := os.Chmod(name, j.mode); err != nil {
		return fmt.Errorf("chmod fail, %w", err)
	}
	if os.Geteuid() == 0 {
		if stat, ok := j.sys.(*syscall.Stat_t); ok {
			if err := os.Chown(name, int(stat.Uid), int(stat.Gid)); err != nil {
				return fmt.Errorf("chown fail, %w", err)
			}
		}
	}
	if err := os.Chtimes(name, j.modTime, j.modTime); err != nil {
		return fmt.Errorf("chtimes fail, %w", err)
	}
	return nil
}
