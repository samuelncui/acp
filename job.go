package acp

import (
	"encoding/hex"
	"io"
	"io/fs"
	"sync"
	"time"
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
	src    *source
	path   string

	size    int64       // length in bytes for regular files; system-dependent for others
	mode    fs.FileMode // file mode bits
	modTime time.Time   // modification time

	lock      sync.Mutex
	writeTime time.Time
	status    jobStatus

	targets        []string
	successTargets []string
	failedTargets  map[string]error
	hash           []byte
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
		Base: j.src.base,
		Path: j.src.path,

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
	src io.ReadCloser
	ch  chan struct{}
}

func newWriteJob(job *baseJob, src io.ReadCloser, needWait bool) *writeJob {
	j := &writeJob{
		baseJob: job,
		src:     src,
	}
	if needWait {
		j.ch = make(chan struct{})
	}
	return j
}

func (wj *writeJob) done() {
	wj.src.Close()

	if wj.ch != nil {
		close(wj.ch)
	}
}

func (wj *writeJob) wait() {
	if wj.ch == nil {
		return
	}
	<-wj.ch
}

type Job struct {
	Base string   `json:"base"`
	Path []string `json:"path"`

	Status         string           `json:"status"`
	SuccessTargets []string         `json:"success_target,omitempty"`
	FailTargets    map[string]error `json:"fail_target,omitempty"`

	Size      int64       `json:"size"`
	Mode      fs.FileMode `json:"mode"`
	ModTime   time.Time   `json:"mod_time"`
	WriteTime time.Time   `json:"write_time"`
	SHA256    string      `json:"sha256"`
}
