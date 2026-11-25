//go:build freebsd
// +build freebsd

package signature

import (
	"errors"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// attribute is the platform's name of the managed signature attribute.
const attribute = "acp.signature"

func readXattr(file *os.File) ([]byte, error) {
	// One bounded read captures any valid entry and detects an oversized attribute.
	value := make([]byte, maxSize+1)
	n, err := unix.ExtattrGetFd(
		int(file.Fd()),
		unix.EXTATTR_NAMESPACE_USER,
		attribute,
		uintptr(unsafe.Pointer(&value[0])),
		len(value),
	)
	if err != nil {
		return nil, err
	}
	return value[:n], nil
}

func writeXattr(file *os.File, value []byte) error {
	var data uintptr
	if len(value) > 0 {
		data = uintptr(unsafe.Pointer(&value[0]))
	}
	_, err := unix.ExtattrSetFd(
		int(file.Fd()),
		unix.EXTATTR_NAMESPACE_USER,
		attribute,
		data,
		len(value),
	)
	return err
}

func writePathXattr(path string, value []byte) error {
	// Keep the same user namespace and bounded encoded value as descriptor publication.
	var data uintptr
	if len(value) > 0 {
		data = uintptr(unsafe.Pointer(&value[0]))
	}
	_, err := unix.ExtattrSetFile(path, unix.EXTATTR_NAMESPACE_USER, attribute, data, len(value))
	return err
}

func isMissing(err error) bool {
	return errors.Is(err, unix.ENOATTR)
}

func isUnsupported(err error) bool {
	return errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP)
}
