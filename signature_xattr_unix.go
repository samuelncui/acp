//go:build darwin || linux
// +build darwin linux

package acp

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func readSignatureXattr(file *os.File) ([]byte, error) {
	// One spare byte makes an oversized entry fail decoding on platforms that return a truncated
	// attribute; the others report ERANGE.
	value := make([]byte, signatureMaxSize+1)
	n, err := unix.Fgetxattr(int(file.Fd()), signatureXattrName, value)
	if errors.Is(err, unix.ERANGE) {
		return nil, errSignatureOversized
	}
	if err != nil {
		return nil, err
	}
	return value[:n], nil
}

func writeSignatureXattr(file *os.File, value []byte) error {
	return unix.Fsetxattr(int(file.Fd()), signatureXattrName, value, 0)
}

func writeSignaturePathXattr(path string, value []byte) error {
	return unix.Setxattr(path, signatureXattrName, value, 0)
}

func isSignatureXattrMissing(err error) bool {
	return isNoAttrErr(err)
}

func isSignatureXattrUnsupported(err error) bool {
	return errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP)
}
