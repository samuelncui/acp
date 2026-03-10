package acp

import (
	"fmt"
	"io/fs"
	"time"
)

type stat struct {
	size    int64       // length in bytes for regular files; system-dependent for others
	mode    fs.FileMode // file mode bits
	modTime time.Time   // modification time
	sys     *sysStat
}

func newStat(path string, fi fs.FileInfo) (*stat, error) {
	sysStat, err := readSysStat(path, fi)
	if err != nil {
		return nil, fmt.Errorf("read sys stat failed, %w", err)
	}

	return &stat{
		size:    fi.Size(),
		mode:    fi.Mode(),
		modTime: fi.ModTime(),
		sys:     sysStat,
	}, nil
}
