package acp

import (
	"context"
	"errors"
	"io"
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
	// Source returns the exact native path to read, or the content source display identifier.
	Source() string

	// Targets returns native paths or content target display identifiers. An empty slice writes nothing: the source is
	// read and hashed only when the hash policy produces a hash, so the default HashOff reports
	// neither a copy nor a hash.
	Targets() []string
}

// ContentMetadata is an immutable snapshot observed when a source opens. Endpoint implementations
// retain ownership of their input; ACP copies the snapshot before forwarding it to targets.
type ContentMetadata struct {
	Info   fs.FileInfo
	Xattrs map[string][]byte
}

// ContentSource opens one member in an ordered content stream. ACP observes Stat and optional
// Xattrs once before opening targets. ACP owns any returned file, even alongside an error, and
// closes it once before another member opens or its native outputs can be committed.
type ContentSource interface {
	Open(context.Context) (fs.File, error)
}

// XattrReader optionally supplies ordinary attributes from an opened content source. Values are
// opaque bytes. Providers normalize missing or unsupported attribute facilities to an empty map.
type XattrReader interface {
	Xattrs() (map[string][]byte, error)
}

// ContentTarget opens one member using observed source metadata. Its matching target string is
// a display identifier, never a filesystem path.
type ContentTarget interface {
	Open(context.Context, ContentMetadata) (ContentWriter, error)
}

// ContentWriter receives member bytes. ACP calls Finish exactly once after writes stop, passing
// the transfer failure, if any; Finish must not publish a truncated member as successful.
type ContentWriter interface {
	io.Writer
	Finish(cause error) error
}

// ErrSourceRead identifies a failure of an item's source while its transfer runs: opening or
// observing a content source, reading or decoding a source, a content size mismatch, or closing
// the source. ACP wraps it wherever it reports that failure, in Result.Err or inside a
// TargetResult.Err, so a caller can tell a source failure from a target's own failure.
var ErrSourceRead = errors.New("read source failed")

// ErrMemberSettled marks an endpoint failure that ends only its own member. A content source that
// wraps it in an Open, Read or Close error, a content target that wraps it in an Open error and
// returns no writer, or a content writer that wraps it in its own Finish error has left its ordered
// stream at a usable boundary: that member fails, but later members are still admitted instead of
// the endpoint failure stopping the run. A writer settles only with a marker it adds itself: one it
// merely passes on from the cause Finish received, by returning, wrapping or joining that cause,
// does not count. A refused content target fails alone; the item's other
// targets continue. A content writer's own Write error always stops the run.
var ErrMemberSettled = errors.New("acp: endpoint member failure is settled")

// ContentItem optionally supplies stream endpoints. A nil source selects the native Source path.
// ContentTargets is either nil or aligned with Targets; nil entries select native target paths.
// Each endpoint side requires LinearDevice(true); content sources require buffered reads.
// At most one target endpoint is supported per item. Native sides retain their device options.
type ContentItem interface {
	Item
	ContentSource() ContentSource
	ContentTargets() []ContentTarget
}

// ReadModeItem optionally selects how one item's source content is read. Items that do not
// implement it use the source device mode set by WithReadMode, which defaults to ReadBuffered.
// ACP calls ReadMode once while indexing a describable item. A panic or invalid mode fails only
// that item; it does not stop the run.
type ReadModeItem interface {
	ReadMode() ReadMode
}

// TargetEncoderItem optionally encodes every native target of one item. ACP calls
// TargetEncoder once while indexing; a nil encode writes plaintext, and a non-nil one requires a
// non-empty encoding name of at most 64 bytes. For each native output ACP
// calls the encoder with that output's file writer, writes the item's plaintext into the returned
// writer and closes it once after the last write. Close flushes the encoded tail into dst and
// returns only after every write into dst has finished. An encoded target is never preallocated
// and always receives a managed signature entry that names the encoding and describes the
// plaintext; the entry marks the file as encoded, so a failure to write it fails that target.
// Content targets are not encoded.
type TargetEncoderItem interface {
	TargetEncoder() (encoding string, encode func(dst io.Writer) (io.WriteCloser, error))
}

// ErrEncodedContent identifies a native source whose item decoder failed: an error from the
// decoder function or its reader, or plaintext that differs from the declared size. The native
// source's own read errors keep their identity instead.
var ErrEncodedContent = errors.New("acp: encoded content cannot be decoded")

// SourceDecoderItem optionally decodes one item's native source. ACP calls SourceDecoder once
// while indexing; a nil decode reads plaintext, and a non-nil one requires a non-empty encoding
// name of at most 64 bytes. size is the plaintext size: it replaces the
// stored file's size in results, progress, read-ahead and target allocation, and the decoded
// bytes must match it. When the source starts reading, ACP calls decode with the opened native
// reader and reads plaintext from the returned reader until io.EOF; it closes that reader once
// before the native descriptor. A decoded source reuses and refreshes only the decoded part of
// the managed signature entry, for its encoding and size, and requires native targets.
type SourceDecoderItem interface {
	SourceDecoder() (encoding string, decode func(src io.Reader) (io.ReadCloser, error), size int64)
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

// Targets returns the destinations; an empty slice reads without writing, and it produces a
// hash only when the run's hash policy does.
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
	// when its copy stage started. A decoded source reports its declared plaintext Size.
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
	Size int64
	// StoredSize counts the bytes ACP wrote to a written native target: Size, or the encoded
	// size when the item encodes its targets. It is zero for a failed or content target.
	StoredSize int64
	WriteTime  time.Time

	// Err is nil when the target was written. Target failures keep their error identity, so
	// errors.Is(err, ErrTargetNoSpace) identifies an exhausted medium.
	Err error
}
