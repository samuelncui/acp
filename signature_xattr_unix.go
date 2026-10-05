//go:build darwin || linux
// +build darwin linux

package acp

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func readSignatureXattr(file *os.File) ([]byte, error) {
	// The codec has a fixed size. One spare byte makes oversized entries fail decoding,
	// including platforms that return a truncated attribute instead of ERANGE.
	value := make([]byte, signatureEncodedSize+1)
	n, err := unix.Fgetxattr(int(file.Fd()), signatureXattrName, value)
	if err != nil {
		return nil, err
	}
	return value[:n], nil
}

func writeSignatureXattr(file *os.File, value []byte) error {
	return unix.Fsetxattr(int(file.Fd()), signatureXattrName, value, 0)
}

func isSignatureXattrMissing(err error) bool {
	return isNoAttrErr(err)
}

func isSignatureXattrUnsupported(err error) bool {
	return errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP)
}
