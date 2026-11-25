//go:build !darwin && !freebsd && !linux
// +build !darwin,!freebsd,!linux

package acp

import (
	"errors"
	"os"
)

// Raw attribute access is exercised on Darwin, FreeBSD and Linux; elsewhere the tests that need it skip
// through requireSignatureXattrSupport.
func writeRawSignature(*os.File, []byte) error { return errors.ErrUnsupported }

func readRawSignature(*os.File) ([]byte, error) { return nil, errors.ErrUnsupported }
