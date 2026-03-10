//go:build windows
// +build windows

package acp

import (
	"fmt"
	"os"
)

func truncate(file *os.File, size int64) error {
	if err := file.Truncate(size); err != nil {
		return err
	}
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
