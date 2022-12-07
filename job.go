package acp

import (
	"encoding/hex"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/abc950309/acp/mmap"
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
	copyer *Copyer
	source *source

	name    string      // base name of the file
	size    int64       // length in bytes for regular files; system-dependent for others
	mode    os.FileMode // file mode bits
	modTime time.Time   // modification time

	lock      sync.Mutex
	writeTime time.Time
	status    jobStatus

	successTargets []string
	failedTargets  map[string]error
	hash           []byte
}

func (c *Copyer) newJobFromFileInfo(source *source, info os.FileInfo) (*baseJob, error) {
	job := &baseJob{
		copyer: c,
		source: source,

		name:    info.Name(),
		size:    info.Size(),
		mode:    info.Mode(),
		modTime: info.ModTime(),
	}
	if job.mode.IsDir() || job.mode&unexpectFileMode != 0 {
		return nil, fmt.Errorf("unexpected file, path= %s", source.src())
	}

	c.submit(&EventUpdateJob{job.report()})
	return job, nil
}

func (j *baseJob) setStatus(s jobStatus) {
	j.lock.Lock()
	defer j.lock.Unlock()
	j.status = s

	if s == jobStatusCopying {
		j.writeTime = time.Now()
	}

	j.copyer.submit(&EventUpdateJob{j.report()})
}

func (j *baseJob) setHash(h []byte) {
	j.lock.Lock()
	defer j.lock.Unlock()

	j.hash = h
	j.copyer.submit(&EventUpdateJob{j.report()})
}

func (j *baseJob) succes(path string) {
	j.lock.Lock()
	defer j.lock.Unlock()

	j.successTargets = append(j.successTargets, path)
	j.copyer.submit(&EventUpdateJob{j.report()})
}

func (j *baseJob) fail(path string, err error) {
	j.lock.Lock()
	defer j.lock.Unlock()

	if j.failedTargets == nil {
		j.failedTargets = make(map[string]error, 1)
	}
	j.failedTargets[path] = err
	j.copyer.submit(&EventUpdateJob{j.report()})
}

func (j *baseJob) report() *Job {
	return &Job{
		Base: j.source.base,
		Path: j.source.path,

		Status:         statusMapping[j.status],
		SuccessTargets: j.successTargets,
		FailTargets:    j.failedTargets,

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
	ch  chan struct{}
}

func (wj *writeJob) done() {
	wj.src.Close()
	close(wj.ch)
}

type Job struct {
	Base string   `json:"base"`
	Path []string `json:"path"`

	Status         string           `json:"status"`
	SuccessTargets []string         `json:"success_target,omitempty"`
	FailTargets    map[string]error `json:"fail_target,omitempty"`

	Size      int64       `json:"size"`
	Mode      os.FileMode `json:"mode"`
	ModTime   time.Time   `json:"mod_time"`
	WriteTime time.Time   `json:"write_time"`
	SHA256    string      `json:"sha256"`
}
