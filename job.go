package acp

import (
	"encoding/hex"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/abc950309/acp/mmap"
)

type jobType uint8

const (
	jobTypeNormal = jobType(iota)
	jobTypeDir
)

type jobStatus uint8

const (
	jobStatusPending = jobStatus(iota)
	jobStatusPreparing
	jobStatusCopying
	jobStatusFinishing
	jobStatusFinished
)

var (
	statusMapping = map[jobStatus]string{
		jobStatusPending:   "pending",
		jobStatusPreparing: "preparing",
		jobStatusCopying:   "copying",
		jobStatusFinishing: "finishing",
		jobStatusFinished:  "finished",
	}
)

type baseJob struct {
	parent *baseJob
	source *source
	typ    jobType

	name    string      // base name of the file
	size    int64       // length in bytes for regular files; system-dependent for others
	mode    os.FileMode // file mode bits
	modTime time.Time   // modification time

	lock      sync.Mutex
	writeTime time.Time
	status    jobStatus
	children  map[*baseJob]struct{}

	successTargets []string
	failedTargets  map[string]error
	hash           []byte

	// utils
	comparableRelativePath string
}

func newJobFromFileInfo(parent *baseJob, source *source, info os.FileInfo) (*baseJob, error) {
	job := &baseJob{
		parent: parent,
		source: source,

		name:    info.Name(),
		size:    info.Size(),
		mode:    info.Mode(),
		modTime: info.ModTime(),

		comparableRelativePath: strings.ReplaceAll(source.relativePath, "/", "\x00"),
	}
	if job.mode.IsDir() {
		job.typ = jobTypeDir
	}

	return job, nil
}

func (j *baseJob) setStatus(s jobStatus) {
	j.lock.Lock()
	defer j.lock.Unlock()
	j.status = s

	if s == jobStatusCopying {
		j.writeTime = time.Now()
	}
}

func (j *baseJob) setHash(h []byte) {
	j.lock.Lock()
	defer j.lock.Unlock()
	j.hash = h
}

func (j *baseJob) done(child *baseJob) int {
	if j.typ == jobTypeNormal {
		return 0
	}

	j.lock.Lock()
	defer j.lock.Unlock()

	delete(j.children, child)
	return len(j.children)
}

func (j *baseJob) succes(path string) {
	j.lock.Lock()
	defer j.lock.Unlock()
	j.successTargets = append(j.successTargets, path)
}

func (j *baseJob) fail(path string, err error) {
	j.lock.Lock()
	defer j.lock.Unlock()

	if j.failedTargets == nil {
		j.failedTargets = make(map[string]error, 1)
	}
	j.failedTargets[path] = err
}

func (j *baseJob) report() *File {
	j.lock.Lock()
	defer j.lock.Unlock()

	fails := make(map[string]string, len(j.failedTargets))
	for n, e := range j.failedTargets {
		fails[n] = e.Error()
	}

	return &File{
		Source:       j.source.path(),
		RelativePath: j.source.relativePath,

		Status:         statusMapping[j.status],
		SuccessTargets: j.successTargets,
		FailTargets:    fails,

		Size:      j.size,
		Mode:      j.mode,
		ModTime:   j.modTime,
		WriteTime: j.writeTime,
		SHA256:    hex.EncodeToString(j.hash),
	}
}

type writeJob struct {
	*baseJob
	src *mmap.ReaderAt
}
