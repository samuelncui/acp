//go:build darwin || linux
// +build darwin linux

package acp

import (
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

// signatureAttribute is the managed attribute spelled in the platform's own namespace.
var signatureAttribute = func() string {
	if runtime.GOOS == "darwin" {
		return "acp.signature"
	}
	return "user.acp.signature"
}()

// writeRawSignature stores arbitrary bytes in the managed attribute, so a test can seed a legacy
// or corrupt value that signature.WriteFile never produces.
func writeRawSignature(file *os.File, value []byte) error {
	return unix.Fsetxattr(int(file.Fd()), signatureAttribute, value, 0)
}

// readRawSignature returns the managed attribute's stored bytes.
func readRawSignature(file *os.File) ([]byte, error) {
	value := make([]byte, 4096)
	n, err := unix.Fgetxattr(int(file.Fd()), signatureAttribute, value)
	if err != nil {
		return nil, err
	}
	return value[:n], nil
}
