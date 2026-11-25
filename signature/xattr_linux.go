//go:build linux
// +build linux

package signature

import (
	"errors"

	"golang.org/x/sys/unix"
)

// attribute is the platform's name of the managed signature attribute.
const attribute = "user.acp.signature"

func isNoAttr(err error) bool {
	return errors.Is(err, unix.ENODATA)
}
