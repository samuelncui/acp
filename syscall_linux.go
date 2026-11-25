//go:build linux
// +build linux

package acp

import (
	"os"
	"syscall"

	"github.com/samuelncui/acp/signature"
)

func truncate(file *os.File, size int64) error {
	if err := syscall.Fallocate(int(file.Fd()), 0, 0, size); err != nil {
		return err
	}
	return nil
}

func checkXattrKey(key string) bool {
	return !signature.IsAttribute(key)
}
