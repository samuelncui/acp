//go:build darwin
// +build darwin

package signature

import (
	"errors"

	"golang.org/x/sys/unix"
)

// attribute is the platform's name of the managed signature attribute.
const attribute = "acp.signature"

func isNoAttr(err error) bool {
	return errors.Is(err, unix.ENOATTR) || errors.Is(err, unix.ENODATA)
}
