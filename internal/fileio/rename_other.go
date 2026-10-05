//go:build !linux && !darwin

package fileio

import "errors"

func renameExclusive(_, _ string) error { return errors.ErrUnsupported }
