//go:build !darwin && !freebsd && !linux
// +build !darwin,!freebsd,!linux

package acp

import (
	"errors"
	"os"
)

const signatureXattrName = "acp.signature"

func readSignatureXattr(*os.File) ([]byte, error) {
	return nil, errSignatureXattrUnsupported
}

func writeSignatureXattr(*os.File, []byte) error {
	return errSignatureXattrUnsupported
}

func removeSignatureXattr(*os.File) error {
	return errSignatureXattrUnsupported
}

func isSignatureXattrMissing(err error) bool {
	return false
}

func isSignatureXattrUnsupported(err error) bool {
	return errors.Is(err, errSignatureXattrUnsupported)
}
