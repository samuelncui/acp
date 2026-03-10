//go:build linux
// +build linux

package acp

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func truncate(file *os.File, size int64) error {
	if err := syscall.Fallocate(int(file.Fd()), 0, 0, size); err != nil {
		return err
	}
	return nil
}

func isNoAttrErr(err error) bool {
	return errors.Is(err, unix.ENODATA)
}

func checkXattrKey(key string) bool {
	return true
}
