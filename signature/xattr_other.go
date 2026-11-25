//go:build !darwin && !freebsd && !linux
// +build !darwin,!freebsd,!linux

package signature

import (
	"errors"
	"os"
)

func readXattr(*os.File) ([]byte, error) {
	return nil, errors.ErrUnsupported
}

func writeXattr(*os.File, []byte) error {
	return errors.ErrUnsupported
}

func writePathXattr(string, []byte) error {
	return errors.ErrUnsupported
}

func isMissing(err error) bool {
	return false
}

func isUnsupported(error) bool {
	return false
}
