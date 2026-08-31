//go:build darwin || linux
// +build darwin linux

package acp

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func readSignatureXattr(file *os.File) ([]byte, error) {
	// Query the exact value size before allocating the bounded codec payload.
	size, err := unix.Fgetxattr(int(file.Fd()), signatureXattrName, nil)
	if err != nil {
		return nil, err
	}
	value := make([]byte, size)
	if size == 0 {
		return value, nil
	}

	// Read through the same descriptor so path replacement cannot redirect the lookup.
	n, err := unix.Fgetxattr(int(file.Fd()), signatureXattrName, value)
	if err != nil {
		return nil, err
	}
	return value[:n], nil
}

func writeSignatureXattr(file *os.File, value []byte) error {
	return unix.Fsetxattr(int(file.Fd()), signatureXattrName, value, 0)
}

func removeSignatureXattr(file *os.File) error {
	return unix.Fremovexattr(int(file.Fd()), signatureXattrName)
}

func isSignatureXattrMissing(err error) bool {
	return isNoAttrErr(err)
}

func isSignatureXattrUnsupported(err error) bool {
	return errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP)
}
