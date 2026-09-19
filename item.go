package acp

import (
	"io/fs"
	"time"
)

// Item is one unit of work owned by the caller.
//
// The caller implements Item over whatever state it owns, so the item is the handle that ties a
// result back to that state: Result.Job is the exact instance that was submitted, and no id
// lookup or payload plumbing is needed. ACP calls Source and Targets under panic protection: the
// first call describes the item, and an item whose description failed is asked for its Source once
// more when it is reported, so a failure path may read Source twice. ACP never keeps the item
// afterwards.
type Item interface {
	// Source returns the exact path to read.
	Source() string

	// Targets returns the destinations of a copy. An empty slice hashes the source without
	// writing it anywhere.
	Targets() []string
}

// SimpleJob is an Item that carries no state of its own: Path is the source and Dsts are the
// destinations. A caller that submits a SimpleJob finds its result by matching Result.Job
// against the pointer it submitted, because SimpleJob has no callback of its own.
type SimpleJob struct {
	Path string
	Dsts []string
}

// Source returns the path to read.
func (j *SimpleJob) Source() string { return j.Path }

// Targets returns the destinations; an empty slice hashes without writing.
func (j *SimpleJob) Targets() []string { return j.Dsts }

// Result reports the content facts of one finished item, one outcome per requested target, and
// the item's own error.
//
// A result that carries an error is delivered as soon as it is produced; results without an
// error are buffered. Result order is unspecified: results arrive in completion order, and a
// failure may therefore arrive before a success of the same submission batch. A linear target
// still writes in request order, because one writer consumes items in that order.
type Result struct {
	// Job is the exact Item instance that was submitted.
	Job Item

	// Err is the item-level error. A nil Err means the item completed, even when every
	// requested target failed: an item that could not be processed at all is what fails,
	// and its target outcomes are still reported.
	Err error

	// Size, Mode and ModTime describe the source facts the item observed, and WriteTime is
	// when its copy stage started.
	Size      int64
	Mode      fs.FileMode
	ModTime   time.Time
	WriteTime time.Time

	// SHA256 is the hash of the bytes actually read; it is nil when the hash policy produces
	// no hash. SignatureCacheHit reports that a stored hash was reused instead of computed.
	SHA256            []byte
	SignatureCacheHit bool

	// Targets holds one outcome per requested target, in request order. A target that was
	// not written still has an outcome.
	Targets []TargetResult
}

// TargetResult is the outcome of one requested target.
type TargetResult struct {
	Path string
	// Size is the item's size as read, so a failed or partially written target reports that
	// size rather than a count of the bytes that landed there.
	Size      int64
	WriteTime time.Time

	// Err is nil when the target was written. Target failures keep their error identity, so
	// errors.Is(err, ErrTargetNoSpace) identifies an exhausted medium.
	Err error
}
