//go:build freebsd
// +build freebsd

package acp

import (
	"errors"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

const signatureXattrName = "acp.signature"

func readSignatureXattr(file *os.File) ([]byte, error) {
	// Query the exact value size before allocating the bounded codec payload.
	size, err := unix.ExtattrGetFd(int(file.Fd()), unix.EXTATTR_NAMESPACE_USER, signatureXattrName, 0, 0)
	if err != nil {
		return nil, err
	}
	value := make([]byte, size)
	if size == 0 {
		return value, nil
	}

	// Read through the same descriptor so path replacement cannot redirect the lookup.
	n, err := unix.ExtattrGetFd(
		int(file.Fd()),
		unix.EXTATTR_NAMESPACE_USER,
		signatureXattrName,
		uintptr(unsafe.Pointer(&value[0])),
		len(value),
	)
	if err != nil {
		return nil, err
	}
	return value[:n], nil
}

func writeSignatureXattr(file *os.File, value []byte) error {
	var data uintptr
	if len(value) > 0 {
		data = uintptr(unsafe.Pointer(&value[0]))
	}
	_, err := unix.ExtattrSetFd(
		int(file.Fd()),
		unix.EXTATTR_NAMESPACE_USER,
		signatureXattrName,
		data,
		len(value),
	)
	return err
}

func removeSignatureXattr(file *os.File) error {
	return unix.ExtattrDeleteFd(int(file.Fd()), unix.EXTATTR_NAMESPACE_USER, signatureXattrName)
}

func isSignatureXattrMissing(err error) bool {
	return errors.Is(err, unix.ENOATTR)
}

func isSignatureXattrUnsupported(err error) bool {
	return errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP)
}
