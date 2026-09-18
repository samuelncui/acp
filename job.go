package acp

import (
	"context"
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

	JobStatusPending   = "pending"
	JobStatusPreparing = "preparing"
	JobStatusCopying   = "copying"
	JobStatusFinishing = "finishing"
	JobStatusFinished  = "finished"
)

var (
	statusMapping = map[jobStatus]string{
		jobStatusPending:   JobStatusPending,
		jobStatusPreparing: JobStatusPreparing,
		jobStatusCopying:   JobStatusCopying,
		jobStatusFinishing: JobStatusFinishing,
		jobStatusFinished:  JobStatusFinished,
	}
)

type baseJob struct {
	copyer *Copyer
	item   Item
	src    *source
	path   string
	stat   *stat
	order  uint64

	// itemError reports an item ACP could not process at all through its failure
	// callback instead of a completion.
	itemError error

	lock      sync.Mutex
	writeTime time.Time
	status    jobStatus

	targets        []string
	successTargets []string
	failedTargets  map[string]error
	hash           []byte
	cacheHit       bool
	hashValid      bool
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

func (j *baseJob) setCachedHash(h []byte) {
	j.lock.Lock()
	defer j.lock.Unlock()

	j.hash = h
	j.cacheHit = true
	j.hashValid = true
	j.copyer.submit(&EventUpdateJob{j.report()})
}

func (j *baseJob) validateHash() {
	j.lock.Lock()
	j.hashValid = true
	j.lock.Unlock()
}

func (j *baseJob) success(path string) {
	j.lock.Lock()
	defer j.lock.Unlock()

	j.successTargets = append(j.successTargets, path)
	j.copyer.submit(&EventUpdateJob{j.report()})
}

func (j *baseJob) fail(path string, err error) {
	// One failed target is an item outcome, not a pipeline failure: the caller classifies
	// target errors and decides whether the operation continues.
	j.copyer.reportItemError(j.path, path, err)
	j.lock.Lock()
	defer j.lock.Unlock()

	for index, target := range j.successTargets {
		if target != path {
			continue
		}
		j.successTargets = append(j.successTargets[:index], j.successTargets[index+1:]...)
		break
	}
	if j.failedTargets == nil {
		j.failedTargets = make(map[string]error, 1)
	}

	j.failedTargets[path] = err
	j.copyer.submit(&EventUpdateJob{j.report()})
}

func (j *baseJob) report() *Job {
	return &Job{
		FullPath: j.path,
		Base:     j.src.base,
		Path:     j.src.path,

		Status:         statusMapping[j.status],
		SuccessTargets: j.successTargets,
		FailTargets:    j.failedTargets,

		Size:      j.stat.size,
		Mode:      j.stat.mode,
		ModTime:   j.stat.modTime,
		WriteTime: j.writeTime,
		SHA256:    hex.EncodeToString(j.hash),

		SignatureCacheHit: j.cacheHit,
	}
}

// failAll marks every requested target failed with one error, which reports a job that
// could not be written anywhere as a completed item with failed target outcomes.
func (j *baseJob) failAll(err error) {
	j.copyer.reportItemError(j.path, "", err)

	j.lock.Lock()
	defer j.lock.Unlock()

	if len(j.targets) == 0 {
		j.failedTargets = map[string]error{"": err}
		return
	}
	for _, target := range j.targets {
		if j.failedTargets == nil {
			j.failedTargets = make(map[string]error, len(j.targets))
		}
		j.failedTargets[target] = err
	}
	j.copyer.submit(&EventUpdateJob{j.report()})
}

// result reports the content facts of a finished job and one outcome per requested
// target, in request order.
func (j *baseJob) result() *Result {
	j.lock.Lock()
	defer j.lock.Unlock()

	result := &Result{
		Source:            j.path,
		Size:              j.stat.size,
		Mode:              j.stat.mode,
		ModTime:           j.stat.modTime,
		WriteTime:         j.writeTime,
		SHA256:            append([]byte(nil), j.hash...),
		SignatureCacheHit: j.cacheHit,
		Targets:           make([]TargetResult, 0, len(j.targets)),
	}
	for _, target := range j.targets {
		outcome := TargetResult{Path: target, Size: j.stat.size, WriteTime: j.writeTime}
		if err, failed := j.failedTargets[target]; failed {
			outcome.Err = err
		}
		result.Targets = append(result.Targets, outcome)
	}

	return result
}

type writeJob struct {
	*baseJob
	reader   io.ReadCloser
	size     int64
	consumed chan struct{}

	// skipContent marks an item whose policy never reads the source content.
	skipContent bool
}

func newWriteJob(job *baseJob, src io.ReadCloser, size int64, waitConsumed bool) *writeJob {
	j := &writeJob{
		baseJob: job,
		reader:  src,
		size:    size,
	}
	if waitConsumed {
		j.consumed = make(chan struct{})
	}
	return j
}

func (wj *writeJob) finishSource() {
	if wj.reader != nil {
		_ = wj.reader.Close()
	}

	if wj.consumed != nil {
		close(wj.consumed)
	}
}

func (wj *writeJob) waitConsumed(ctx context.Context) bool {
	if wj.consumed == nil {
		return true
	}

	select {
	case <-wj.consumed:
		return true
	case <-ctx.Done():
		return false
	}
}

type Job struct {
	FullPath string `json:"full_path"`
	Base     string `json:"base"`
	Path     string `json:"path"`

	Status         string           `json:"status"`
	SuccessTargets []string         `json:"success_target,omitempty"`
	FailTargets    map[string]error `json:"fail_target,omitempty"`

	Size      int64       `json:"size"`
	Mode      fs.FileMode `json:"mode"`
	ModTime   time.Time   `json:"mod_time"`
	WriteTime time.Time   `json:"write_time"`
	SHA256    string      `json:"sha256"`

	SignatureCacheHit bool `json:"signature_cache_hit,omitempty"`
}
