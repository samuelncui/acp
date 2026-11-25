//go:build freebsd
// +build freebsd

package acp

import (
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

const signatureAttribute = "acp.signature"

// writeRawSignature stores arbitrary bytes in the managed attribute, so a test can seed a legacy
// or corrupt value that signature.WriteFile never produces.
func writeRawSignature(file *os.File, value []byte) error {
	var data uintptr
	if len(value) > 0 {
		data = uintptr(unsafe.Pointer(&value[0]))
	}
	_, err := unix.ExtattrSetFd(int(file.Fd()), unix.EXTATTR_NAMESPACE_USER, signatureAttribute, data, len(value))
	return err
}

// readRawSignature returns the managed attribute's stored bytes.
func readRawSignature(file *os.File) ([]byte, error) {
	value := make([]byte, 4096)
	n, err := unix.ExtattrGetFd(
		int(file.Fd()),
		unix.EXTATTR_NAMESPACE_USER,
		signatureAttribute,
		uintptr(unsafe.Pointer(&value[0])),
		len(value),
	)
	if err != nil {
		return nil, err
	}
	return value[:n], nil
}
