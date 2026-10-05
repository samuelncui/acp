package fileio

import (
	"errors"

	"golang.org/x/sys/unix"
)

func renameExclusive(from, to string) error {
	err := unix.Renameat2(unix.AT_FDCWD, from, unix.AT_FDCWD, to, unix.RENAME_NOREPLACE)
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EOPNOTSUPP) {
		return errors.ErrUnsupported
	}
	return err
}
