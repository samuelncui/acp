//go:build windows
// +build windows

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

func readSysStatWithContent(path string, info fs.FileInfo, bounded bool) (*sysStat, error) {
	return readSysStat(path, info)
}

func nativeContentMetadata(j *stat) ContentMetadata { return ContentMetadata{Info: j.info} }

func contentStat(metadata ContentMetadata) *stat {
	return &stat{info: metadata.Info, size: metadata.Info.Size(), mode: metadata.Info.Mode(), modTime: metadata.Info.ModTime()}
}
