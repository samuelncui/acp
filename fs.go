package acp

import (
	"fmt"
	"syscall"
)

type fileSystem struct {
	TypeName      string
	MountPoint    string
	TotalSize     int64
	AvailableSize int64
}

func getFileSystem(path string) (*fileSystem, error) {
	stat := new(syscall.Statfs_t)

	if err := syscall.Statfs(path, stat); err != nil {
		return nil, fmt.Errorf("read statfs fail, err= %w", err)
	}

	return &fileSystem{
		TypeName:      unpaddingInt8s(stat.Fstypename[:]),
		MountPoint:    unpaddingInt8s(stat.Mntonname[:]),
		TotalSize:     int64(stat.Blocks) * int64(stat.Bsize),
		AvailableSize: int64(stat.Bavail) * int64(stat.Bsize),
	}, nil
}

func unpaddingInt8s(buf []int8) string {
	result := make([]byte, 0, len(buf))
	for _, c := range buf {
		if c == 0x00 {
			break
		}

		result = append(result, byte(c))
	}

	return string(result)
}
