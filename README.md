# acp
An Advanced Copy Tools, with following extra features:
- Process bar
- Sorted copy order, to improve tape device read performance
- Concurrent source preparation with request-ordered writes for linear targets
- Multi target path, read once write many
- Buffered or memory-mapped source reads
- JSON format job report
- Optional SHA-256 xattr cache for content hashes
- Can use as a golang library

## Library API

The caller owns the work. A `BatchSource` supplies items, and every item reports its own
terminal outcome, so no id lookup or result plumbing is needed:

```go
type Item interface {
	Source() string
	Targets() []string

	Completed(*Result)
	Failed(error)
}

type BatchSource interface {
	Next(context.Context) ([]Item, error) // returns io.EOF when the input ends
}

func Run(ctx context.Context, source BatchSource, opts ...Option) error
```

`Completed` reports the content facts of one processed item plus one outcome per requested
target, in request order:

```go
type Result struct {
	Source string

	Size      int64
	Mode      fs.FileMode
	ModTime   time.Time
	WriteTime time.Time

	SHA256            []byte
	SignatureCacheHit bool

	Targets []TargetResult
}

type TargetResult struct {
	Path      string
	Size      int64
	WriteTime time.Time
	Err       error // nil when this target was written and verified
}
```

### Item contract

- Every accepted item receives exactly one terminal callback, whether it completes or
  fails, and callbacks are delivered one at a time from a single pipeline goroutine.
  ACP performs no I/O on the caller's behalf inside the callback, so a callback may do its
  own.
- `Completed` is an item ACP processed. It carries one `TargetResult` per requested target
  in request order, and an item whose every target failed is still a completion. Target
  errors keep their identity, so `errors.Is(err, acp.ErrTargetNoSpace)` identifies an
  exhausted medium.
- `Failed` is reserved for items ACP could not process at all, such as a source it cannot
  stat or open, and for items abandoned by a stop. A failed item never reports a result.
- A graceful stop (the caller cancels the context) stops feeding items, finishes the items
  already being copied, abandons the rest with `Failed` carrying the stopping error,
  starts no read for an item it has not begun, and makes `Run` return the stopping error.

`Run` returns an error only for a pipeline failure, such as a `BatchSource` error, or for
the caller's stop. Per-item failures arrive through the item callbacks.

## Options

| Option | Scope | Meaning |
| --- | --- | --- |
| `SetFromDevice(...)` | run | Source device options |
| `SetToDevice(...)` | run | Target device options |
| `LinearDevice(bool)` | device | One worker and request order for a tape-like device |
| `DeviceThreads(int)` | device | Worker count, default 8, forced to 1 for a linear device |
| `Overwrite(bool)` | target device | Replace an existing target instead of refusing it |
| `WithReadMode(ReadBuffered\|ReadMapped)` | source device | Read sources buffered or through a memory mapping |
| `WithReadBuffer(int)` | run | How many items ACP may hold in its read buffer, default 4096 |
| `WithHashPolicy(HashPolicy)` | run | Content hash and stored hash policy, default `HashOff` |
| `WithLogger(*logrus.Logger)` | run | Logger for pipeline diagnostics |
| `WithEventHandler(EventHandler)` | run | Progress, job, and error events |
| `WithProgressBar()` | run | Terminal progress bar handler |

`WithReadMode` applies to the source device only; a target read mode is rejected. Buffered
reads are the default and avoid updating the source access time where the platform allows
it. Mapped reads use the `mmap` package with a fallback for empty files.

## Content hash policy

`HashPolicy` selects both how an item's content hash is produced and how the stored hash is
used, because refreshing the cache requires a computed hash:

| Policy | Hash | Stored hash |
| --- | --- | --- |
| `HashOff` | none | untouched |
| `HashCachedOnly` | reused if stored, otherwise none | untouched |
| `HashCachedOrRead` | reused if stored, otherwise read | untouched |
| `HashCachedOrReadRefresh` | reused if stored, otherwise read | written when it differs |
| `HashRead` | always read | untouched |
| `HashReadRefresh` | always read | written when it differs |

A request with targets always reads its source, so a stored hash cannot be reused there. A
policy that neither reads content nor finds a stored hash completes the item without
opening the source at all. A refresh publishes the stored value only when it differs from
the computed one, which keeps an identical rewrite from touching the attribute.

## Content signature cache

ACP stores a fixed binary SHA-256 plus the file size and nanosecond mtime in
`user.acp.signature` on Linux and the canonical `acp.signature` user attribute on Darwin
and FreeBSD.

The xattr is a disposable optimization. A policy that reuses a stored hash may skip reading
content, and a policy that refreshes rewrites the source and every successful target after
the item finishes. Writers publish the hashed version's size and mtime unchanged, so an
entry whose file changed after hashing simply fails the reader's metadata comparison and is
treated as stale. Missing, stale, corrupt, read-only, full, or unsupported xattrs are
summarized as warnings and never become copy errors. ACP's managed key is not copied as an
ordinary source xattr.

## File selection

`SelectFiles` walks sources, keeps regular files, maps each source-relative path onto every
target directory, removes repeated relative paths, and returns entries in the platform's
path order:

```go
entries, err := acp.SelectFiles([]string{"source"}, []string{"target-a", "target-b"})
// acp.FileEntry{Base, Path, Source, Targets}
```

# Install
```
# Install acp
go install github.com/samuelncui/acp/cmd/acp
```

# Usage

```
Usage of acp:
  -n    do not overwrite exist file
  -notarget
        do not have target, use as dir index tool
  -report string
        json report storage path
  -target value
        use target flag to give multi target path
```

## Example

```
# copy `example` dir to `target` dir
acp example target/

# copy `example` dir to `target` dir, and output a report to `report.json`
acp -report report.json example target/

# copy `example` dir to `target1` and `target2` dir
acp example -target target1 -target target2

# do not copy, just get a dir index, write to `report.json`
acp example -notarget -report report.json
```

## Testing

See [TESTING.md](TESTING.md) for unit, race, end-to-end, and cross-platform test instructions.
