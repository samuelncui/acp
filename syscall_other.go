//go:build !linux && !darwin && !windows
// +build !linux,!darwin,!windows

package acp

import (
	"fmt"
	"os"
)

func truncate(_ *os.File, _ int64) error {
	return nil
}

func copyAttrs(name string, j *baseJob) error {
	if err := os.Chmod(name, j.mode); err != nil {
		return fmt.Errorf("chmod fail, %w", err)
	}
	if err := os.Chtimes(name, j.modTime, j.modTime); err != nil {
		return fmt.Errorf("chtimes fail, %w", err)
	}
	return nil
}
