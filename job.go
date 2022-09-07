package acp

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/abc950309/acp/mmap"
)

type JobType uint8

const (
	JobTypeNormal = JobType(iota)
	JobTypeEnterDir
	JobTypeExitDir
)

type Job struct {
	Source       string
	RelativePath string
	Type         JobType
	Name         string      // base name of the file
	Size         int64       // length in bytes for regular files; system-dependent for others
	Mode         os.FileMode // file mode bits
	ModTime      time.Time   // modification time
}

func newJobFromFileInfo(base, path string, info os.FileInfo) (*Job, error) {
	if !strings.HasPrefix(path, base) {
		return nil, fmt.Errorf("path do not contains base, path= '%s', base= '%s'", path, base)
	}

	job := &Job{
		Source:       path,
		RelativePath: path[len(base):],
		Name:         info.Name(),
		Size:         info.Size(),
		Mode:         info.Mode(),
		ModTime:      info.ModTime(),
	}
	return job, nil
}

type writeJob struct {
	*Job
	src *mmap.ReaderAt
}

type metaJob struct {
	*Job

	successTarget []string
	failTarget    map[string]string
	hash          []byte
}
