//go:build !linux && !darwin && !windows
// +build !linux,!darwin,!windows

package acp

import (
	"fmt"
	"io/fs"
	"os"
)

type sysStat struct{}

func readSysStat(path string, stat fs.FileInfo) (*sysStat, error) {
	return nil, nil
}

func truncate(file *os.File, size int64) error {
	if err := file.Truncate(size); err != nil {
		return err
	}
	return nil
}

func writeSysStat(name string, j *stat) error {
	if err := os.Chmod(name, j.mode); err != nil {
		return fmt.Errorf("chmod fail, %w", err)
	}
	if err := os.Chtimes(name, j.modTime, j.modTime); err != nil {
		return fmt.Errorf("chtimes fail, %w", err)
	}
	return nil
}
