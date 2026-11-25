//go:build darwin || linux
// +build darwin linux

package signature

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// errOversized reports an attribute longer than any valid entry.
var errOversized = errors.New("signature xattr is larger than any valid entry")

func readXattr(file *os.File) ([]byte, error) {
	// One spare byte makes an oversized entry fail decoding on platforms that return a truncated
	// attribute; the others report ERANGE.
	value := make([]byte, maxSize+1)
	n, err := unix.Fgetxattr(int(file.Fd()), attribute, value)
	if errors.Is(err, unix.ERANGE) {
		return nil, errOversized
	}
	if err != nil {
		return nil, err
	}
	return value[:n], nil
}

func writeXattr(file *os.File, value []byte) error {
	return unix.Fsetxattr(int(file.Fd()), attribute, value, 0)
}

func writePathXattr(path string, value []byte) error {
	return unix.Setxattr(path, attribute, value, 0)
}

func isMissing(err error) bool {
	return isNoAttr(err)
}

func isUnsupported(err error) bool {
	return errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP)
}
