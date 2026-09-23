package acp

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// Report row status vocabulary. A row is published once, when the item's data and metadata
// lifecycle has finished, so a completed and a failed item both read JobStatusFinished.
const (
	JobStatusPending   = "pending"
	JobStatusPreparing = "preparing"
	JobStatusCopying   = "copying"
	JobStatusFinishing = "finishing"
	JobStatusFinished  = "finished"
)

type baseJob struct {
	copyer   *StreamCopyer
	item     Item
	path     string
	stat     *stat
	order    uint64
	readMode ReadMode

	// itemError reports an item ACP could not process at all, instead of a completion.
	itemError error

	lock      sync.Mutex
	writeTime time.Time

	targets        []string
	successTargets []string
	failedTargets  map[string]error
	hash           []byte
	cacheHit       bool
	hashValid      bool
}

// startWrite records when the copy stage took the item, which is the write time of every
// target outcome.
func (j *baseJob) startWrite() {
	j.lock.Lock()
	defer j.lock.Unlock()

	j.writeTime = time.Now()
}

func (j *baseJob) setHash(h []byte) {
	j.lock.Lock()
	defer j.lock.Unlock()

	j.hash = h
}

func (j *baseJob) setCachedHash(h []byte) {
	j.lock.Lock()
	defer j.lock.Unlock()

	j.hash = h
	j.cacheHit = true
	j.hashValid = true
}

func (j *baseJob) validateHash() {
	j.lock.Lock()
	j.hashValid = true
	j.lock.Unlock()
}

// computedHash returns the hash this run computed from the whole source, which is the only hash a
// refresh may publish. A reused stored hash and a read that stopped early are not such a hash.
func (j *baseJob) computedHash() ([]byte, bool) {
	j.lock.Lock()
	defer j.lock.Unlock()

	if !j.hashValid || j.cacheHit {
		return nil, false
	}
	return j.hash, true
}

// setSize replaces the indexed size with the number of bytes the item actually read. The item
// reports the facts it observed: a source that changed while a run is in progress is out of
// scope, so no change check fails the item and no stale size is published.
func (j *baseJob) setSize(size int64) {
	j.lock.Lock()
	defer j.lock.Unlock()

	j.stat.size = size
}

func (j *baseJob) success(path string) {
	j.lock.Lock()
	defer j.lock.Unlock()

	j.successTargets = append(j.successTargets, path)
}

func (j *baseJob) fail(path string, err error) {
	// One failed target is an item outcome, not a pipeline failure: the caller classifies
	// target errors and decides whether the operation continues.
	j.copyer.logf(logrus.ErrorLevel, "copy failed, source= %q target= %q, %v", j.path, path, err)
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
}

// failAll marks every requested target failed with one error, which reports a job that could not
// be written anywhere as a completed item with failed target outcomes.
func (j *baseJob) failAll(err error) {
	j.copyer.logf(logrus.ErrorLevel, "copy failed, source= %q targets= %v, %v", j.path, j.targets, err)

	j.lock.Lock()
	defer j.lock.Unlock()

	for _, target := range j.targets {
		if j.failedTargets == nil {
			j.failedTargets = make(map[string]error, len(j.targets))
		}
		j.failedTargets[target] = err
	}
}

// result reports the content facts of a finished job, one outcome per requested target in
// request order, and the item's own error. A job ACP could not describe has neither a resolved
// stat nor target outcomes, so both are reported only when they exist.
func (j *baseJob) result() Result {
	j.lock.Lock()
	defer j.lock.Unlock()

	result := Result{
		Job:               j.item,
		Err:               j.itemError,
		WriteTime:         j.writeTime,
		SHA256:            append([]byte(nil), j.hash...),
		SignatureCacheHit: j.cacheHit,
		Targets:           make([]TargetResult, 0, len(j.targets)),
	}
	if j.stat != nil {
		result.Size, result.Mode, result.ModTime = j.stat.size, j.stat.mode, j.stat.modTime
	}
	for _, target := range j.targets {
		outcome := TargetResult{Path: target, Size: result.Size, WriteTime: j.writeTime}
		switch err, failed := j.failedTargets[target]; {
		case failed:
			outcome.Err = err
		case j.itemError != nil:
			// The item never reached this target, so its outcome is the item error instead of
			// a write that never happened.
			outcome.Err = j.itemError
		}
		result.Targets = append(result.Targets, outcome)
	}

	return result
}

type writeJob struct {
	*baseJob
	reader io.ReadCloser
	size   int64

	// source is the descriptor this item owns: the reader that closes it, and the file the item
	// reads its stored hash through and publishes its computed one through. It is nil for a job
	// that opened no source.
	source *os.File

	consumed chan struct{}

	// hardStop ends the wait for a consumer when the pipeline fails fatally.
	hardStop <-chan struct{}

	// skipContent marks an item whose policy never reads the source content.
	skipContent bool
}

func newWriteJob(job *baseJob, src io.ReadCloser, size int64, waitConsumed bool) *writeJob {
	j := &writeJob{
		baseJob: job,
		reader:  src,
		size:    size,
	}
	if job != nil && job.copyer != nil {
		j.hardStop = job.copyer.hardStop
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

// waitConsumed waits until the copy stage owns the reader. A graceful stop never releases the
// wait, because the consumer is what reports the item; only a fatal pipeline failure ends it.
func (wj *writeJob) waitConsumed() bool {
	if wj.consumed == nil {
		return true
	}

	select {
	case <-wj.consumed:
		return true
	case <-wj.hardStop:
		return false
	}
}

// Job is one report row in the af05f05c shape: Base is the directory the source-relative Path
// is resolved against, and Path holds the source-relative path segments that also map onto a
// target directory, so the row's JSON carries path as an array. It is part of the public report
// contract, so it is a plain data structure that a report reader can decode with encoding/json
// alone.
//
// FullPath and SignatureCacheHit are additive fields. FullPath names the source file as one
// whole path, which a consumer that does not join Base and Path needs, and SignatureCacheHit
// reports where the content hash came from; both stay empty on a row that does not record them.
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

	FullPath          string `json:"full_path,omitempty"`
	SignatureCacheHit bool   `json:"signature_cache_hit,omitempty"`
}

// jobJSON is the wire form of a report row. Target failures travel as one message per target,
// because the standard library can neither encode nor decode an error value, and a nil error
// is stored as null so it stays absent on the way back. The additive fields stay last, so a row
// written without them is the document af05f05c wrote.
type jobJSON struct {
	Base string   `json:"base"`
	Path []string `json:"path"`

	Status         string             `json:"status"`
	SuccessTargets []string           `json:"success_target,omitempty"`
	FailTargets    map[string]*string `json:"fail_target,omitempty"`

	Size      int64       `json:"size"`
	Mode      fs.FileMode `json:"mode"`
	ModTime   time.Time   `json:"mod_time"`
	WriteTime time.Time   `json:"write_time"`
	SHA256    string      `json:"sha256"`

	FullPath          string `json:"full_path,omitempty"`
	SignatureCacheHit bool   `json:"signature_cache_hit,omitempty"`
}

// MarshalJSON renders one report row with encoding/json. A row is part of the public report
// contract, so it must be readable and writable without ACP's own JSON coders.
func (j *Job) MarshalJSON() ([]byte, error) {
	if j == nil {
		return []byte("null"), nil
	}

	encoded := &jobJSON{
		Base: j.Base,
		Path: j.Path,

		Status:         j.Status,
		SuccessTargets: j.SuccessTargets,

		Size:      j.Size,
		Mode:      j.Mode,
		ModTime:   j.ModTime,
		WriteTime: j.WriteTime,
		SHA256:    j.SHA256,

		FullPath:          j.FullPath,
		SignatureCacheHit: j.SignatureCacheHit,
	}
	if len(j.FailTargets) > 0 {
		encoded.FailTargets = make(map[string]*string, len(j.FailTargets))
		for target, err := range j.FailTargets {
			if isNilError(err) {
				encoded.FailTargets[target] = nil
				continue
			}
			message := err.Error()
			encoded.FailTargets[target] = &message
		}
	}

	return json.Marshal(encoded)
}

// UnmarshalJSON reads one report row back. A null or empty failure message leaves the target
// out of the map instead of turning it into an error that was never recorded.
func (j *Job) UnmarshalJSON(buf []byte) error {
	if j == nil {
		return errors.New("decode acp job failed, receiver is nil")
	}

	decoded := new(jobJSON)
	if err := json.Unmarshal(buf, decoded); err != nil {
		return err
	}

	*j = Job{
		Base: decoded.Base,
		Path: decoded.Path,

		Status:         decoded.Status,
		SuccessTargets: decoded.SuccessTargets,

		Size:      decoded.Size,
		Mode:      decoded.Mode,
		ModTime:   decoded.ModTime,
		WriteTime: decoded.WriteTime,
		SHA256:    decoded.SHA256,

		FullPath:          decoded.FullPath,
		SignatureCacheHit: decoded.SignatureCacheHit,
	}
	for target, message := range decoded.FailTargets {
		if message == nil || *message == "" {
			continue
		}
		if j.FailTargets == nil {
			j.FailTargets = make(map[string]error, len(decoded.FailTargets))
		}
		j.FailTargets[target] = errors.New(*message)
	}

	return nil
}

// sha256Hex renders a content hash for a report row.
func sha256Hex(hash []byte) string {
	return hex.EncodeToString(hash)
}
