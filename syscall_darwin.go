//go:build darwin
// +build darwin

package acp

import (
	"errors"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

func truncate(file *os.File, size int64) error {
	if err := file.Truncate(size); err != nil {
		return err
	}
	return nil
}

func isNoAttrErr(err error) bool {
	return errors.Is(err, unix.ENOATTR) || errors.Is(err, unix.ENODATA)
}

func checkXattrKey(key string) bool {
	return !strings.HasPrefix(key, "system.")
}
