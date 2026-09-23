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

## Requirements

Building the library or a command needs Go 1.26.8 or newer: `go.mod` declares `go 1.26.8`,
and the code uses `context.WithoutCancel` and `errors.Join`.

## Library API

The engine is push-based. The caller submits items, ACP reports every accepted item through one
results callback, and `Wait` returns the run's terminal error:

```go
func NewStream(ctx context.Context, onResults func([]Result) error, opts ...Option) (*StreamCopyer, error)
func (c *StreamCopyer) Submit(items ...Item) error
func (c *StreamCopyer) Close() error
func (c *StreamCopyer) Wait() error
```

```go
stream, err := acp.NewStream(ctx, func(results []acp.Result) error {
	for _, result := range results {
		job := result.Job.(*myJob) // the exact instance that was submitted
		if result.Err != nil {
			recordFailure(job, result.Err)
			continue
		}
		recordFacts(job, result)
	}
	return nil
}, acp.WithHashPolicy(acp.HashRead))
if err != nil {
	return err // creation or validation error
}

if err := stream.Submit(&acp.SimpleJob{Path: "/data/a.bin", Dsts: []string{"/tape/a.bin"}}); err != nil {
	// the run accepts no more work
}
if err := stream.Close(); err != nil {
	// the final flush failed
}
return stream.Wait() // the run's terminal error
```

An item is pure data. It has no callback, so the caller finds its own state by asserting
`Result.Job` back to the instance it submitted:

```go
type Item interface {
	Source() string
	Targets() []string // an empty slice writes nothing: it reads and hashes only when the
	                   // hash policy produces a hash, so the default HashOff records no hash
}

// Optional: override the source device mode for this item.
type ReadModeItem interface {
	ReadMode() ReadMode
}

type SimpleJob struct {
	Path string
	Dsts []string
}
```

A result reports the content facts of one finished item, one outcome per requested target in
request order, and the item's own error:

```go
type Result struct {
	Job  Item  // the exact instance that was submitted
	Err  error // nil = the item completed, even when every target failed

	Size      int64
	Mode      fs.FileMode
	ModTime   time.Time
	WriteTime time.Time

	SHA256            []byte // nil when the hash policy produces no hash
	SignatureCacheHit bool

	Targets []TargetResult
}

type TargetResult struct {
	Path      string
	Size      int64
	WriteTime time.Time
	Err       error // nil when this target was written
}
```

### Error rules

1. **A result that carries any error is delivered immediately, as its own batch.** `Result.Err`
   (a source ACP could not describe or open, a targetless read that failed, an item a stop
   abandoned) and `TargetResult.Err` (one target that was not written) both skip the batch, so
   the caller can persist the outcome before it submits the next batch. A nil `Result.Err` means
   the item completed, even when every requested target failed.
2. **Results without an error are buffered into batches.** They wait in the result buffer
   (`WithResultBuffer`) until a delivery has `WithResultBatch` results, the result flush interval
   elapses (`WithResultFlushInterval`), or `Close` flushes the rest. The result buffer is the
   depth of the queue behind the delivery, so a caller that stops reading the batches slows the
   pipeline instead of growing an unbounded queue.
3. **An error returned by `onResults` is a cancellation, not a hard stop.** Items already past
   the read stage finish and are delivered normally; items still inside the read pipeline are
   delivered as failures carrying that error; the batch in hand is still submitted, the next
   `Submit` returns an error, and `Wait` returns it.
4. **Two outlets for a run error.** `NewStream` returns creation and validation errors. `Wait`
   returns run-time errors: a pipeline failure, an error `onResults` returned, and a submission
   the run no longer accepts. `Close` returns the wrap-up and flush error.
5. **A graceful stop is the caller's context plus `Close`.** The item feed is the pipeline's
   only cancellation checkpoint: `Submit` consults the context, the results callback error and
   an exhausted linear target before it accepts a batch. Every later stage runs on a context
   that never cancels, so it only drains and forwards what it holds, and a stage that pulls an
   item it can no longer start reports it with the stopping error. Every accepted item
   therefore appears in exactly one result. A fatal pipeline failure is the exception: it ends
   the pipeline without a per-item promise.
6. **Caller code is called under panic protection.** A panic in `onResults` or in an event
   handler becomes an error `Wait` returns instead of unwinding a pipeline goroutine; a panic in
   `Item.Source`/`Item.Targets` becomes that item's `Result.Err`, so the item is still reported
   exactly once and the run continues. A panic that escapes pipeline code itself is recorded as
   the run error and ends the run.
7. **`Close` then `Wait`**, with a single submitter. `Submit` blocks while the read buffer is
   full, which is the feed's backpressure, and a nil item is a submission error. A caller must
   not call `Close`, `Wait` or `Submit` from inside `onResults`: the callback runs on the
   delivery goroutine, so `Close` would wait for the call it is in.

### Result order

Result order is **unspecified**. Results arrive in completion order, so a failure may arrive
before a success of the same submission batch. A linear target still *writes* in request order,
because a single writer consumes the items in that order; that is a property of the medium, not
of the callback.

### Result persistence

ACP owns delivery, not persistence. Every result travels through the one `onResults` callback,
and ACP batches successful results itself: they wait in the result buffer (`WithResultBuffer`)
until a delivery holds `WithResultBatch` results, the result flush interval elapses
(`WithResultFlushInterval`), or `Close` flushes the rest, while a result that carries an error is
delivered immediately as its own batch. A caller that wants a write-goroutine pool or a second
queue does that behind the callback: persist the batch, return, and let ACP keep feeding. A
blocking callback is what applies backpressure to the pipeline — the callback runs on the
delivery goroutine, so a slow persistence layer slows the copy instead of growing an unbounded
queue. The slice handed to the callback is valid for the duration of that call: a caller that
keeps results beyond it copies them.

### Stop modes

| Mode | Trigger | Every accepted item reported |
| --- | --- | --- |
| graceful | the caller cancels `ctx` | yes, with the stopping error for the items that had not started |
| callback cancellation | `onResults` returns an error | yes, same mechanism as the context |
| close | the caller calls `Close` | yes: `Close` drains what was already submitted |
| hard stop | a fatal pipeline failure | no per-item promise |

## Options

| Option | Scope | Meaning |
| --- | --- | --- |
| `SetFromDevice(...)` | run | Source device options |
| `SetToDevice(...)` | run | Target device options |
| `LinearDevice(bool)` | device | One worker and request order for a tape-like device |
| `DeviceThreads(int)` | device | Worker count, default 8, forced to 1 for a linear device |
| `WithReadMode(ReadBuffered\|ReadMapped)` | source device | Read sources buffered or through a memory mapping |
| `Overwrite(bool)` | run | Replace an existing target instead of refusing it; applies to every target |
| `WithReadBuffer(items)` | run | Backpressure limit of the feed, default 4096 items |
| `WithResultBuffer(items)` | run | Depth of the result queue, default 256 items |
| `WithResultBatch(items)` | run | Results one delivery carries, default 256 items |
| `WithResultFlushInterval(d)` | run | How long a successful result may wait for its batch, default 1s |
| `WithHashPolicy(HashPolicy)` | run | Content hash and stored hash policy, default `HashOff` |
| `WithLogger(*logrus.Logger)` | run | Logger for pipeline diagnostics |
| `WithEventHandler(EventHandler)` | run | Progress, count, and error events |
| `WithProgressBar()` | run | Terminal progress bar handler |

Validation happens in `NewStream`/`New`, which reject: a negative `DeviceThreads`; any
`ReadMode` other than `ReadBuffered` or `ReadMapped`; `WithReadMode` applied to the target
device; an unknown `HashPolicy`; `WithReadBuffer(0)` or less; `WithResultBuffer(0)` or less;
`WithResultBatch(0)` or less; and a `WithResultFlushInterval` below 100ms. A job option
(`AccurateJob`, `WildcardJob`) is valid only for `New`, which is the shell that enumerates it;
`NewStream` rejects it instead of running an empty stream.

Repeated value options are last-wins, `WithEventHandler` included: the handler registered last is
the only one that receives events, and `WithEventHandler(nil)` clears the registration.
`WithProgressBar()` installs its handler through the same option, so a later `WithEventHandler`
replaces the bar. The job options and `SetFromDevice`/`SetToDevice` accumulate by design: a
second `WildcardJob` adds another walk, and a second device option adjusts the same device
description. An event handler is called from one goroutine per registration and never
concurrently, so it may keep unguarded state; registering the same handler twice replaces the first
registration, so it is still called once. The results callback is called from one goroutine as
well, never concurrently.

`WithReadMode` applies to the source device only. An item implementing `ReadModeItem` overrides
that choice for its own source; ACP calls the method once during indexing, and an invalid mode
or panic fails that item without stopping the run. Items without the method keep the device mode.
Buffered reads are the default and avoid
updating the source access time where the platform allows it (`O_NOATIME`, with a fallback when
the open is refused); mapped reads use the `mmap` package, whose reader reports the end of an
empty mapping immediately. Either way one descriptor serves the whole item.

## Events

`WithEventHandler` receives the run's `Event` values; `WithProgressBar()` is one handler built
from them, and because a run keeps one handler, a command that wants a bar *and* its own
collector composes them (which is what `cmd/acp` does). A run can emit this set, and not every run
emits all of it: `EventUpdateJob` only comes from the `af05f05c` shell, `EventSignatureCacheSummary`
only from a run that manages the signature cache, which the default `HashOff` policy does not, and
`EventReportError` only when the run recorded a pipeline-level error:

| Event | What it reports |
| --- | --- |
| `EventUpdateCount` | the feed's indexed totals (`Bytes`, `Files`), with `Finished` set on the last one |
| `EventUpdateProgress` | the copy stage's completed totals (`Bytes`, `Files`), with `Finished` set on the last one |
| `EventUpdateJob` | one terminal report row (`Job`), published by the `af05f05c` shell |
| `EventReportError` | one pipeline-level error (`Error`: source, target, cause) |
| `EventSignatureCacheSummary` | the run's aggregate cache activity (`SignatureCacheSummary`) |
| `EventFinished` | the run ended; the last event of one registration |

Events are delivered under the same contract as the results callback:

- **One goroutine per registration, never concurrently.** A handler may keep unguarded state
  between events, which is what lets the progress bar hold its file total without a lock. A run
  has at most one registration: `WithEventHandler` is last-wins, and `WithEventHandler(nil)`
  clears it.
- **`EventFinished` is delivered exactly once per registration**, after every other event of
  that run.
- **The run waits for its handlers.** `Close` and `Wait` return only after every registered
  handler received `EventFinished`, so a handler that never returns blocks the run instead of
  being abandoned.
- **A panic in a handler is an error `Wait` returns**, exactly like a panic in the results
  callback; ACP never unwinds the dispatching goroutine on the caller's behalf.

## Content hash policy

`HashPolicy` selects both how an item's content hash is produced and how the stored hash is
used, because refreshing the cache requires a computed hash. A targetless item records content
facts; a transfer reads its source to write it somewhere:

| Policy | Targetless item | Transfer |
| --- | --- | --- |
| `HashOff` | no hash, cache untouched | no hash, cache untouched |
| `HashCachedOnly` | stored hash reused; a miss leaves the item without one | rejected: a copy always reads its source |
| `HashCachedOrRead` | stored hash reused; a miss reads and hashes | rejected: a copy always reads its source |
| `HashCachedOrReadRefresh` | stored hash reused; a miss reads, hashes and refreshes | always reads, hashes and refreshes |
| `HashRead` | always reads and hashes, cache untouched | always reads and hashes, cache untouched |
| `HashReadRefresh` | always reads and hashes, and refreshes the cache | always reads, hashes and refreshes the cache |

A transfer always reads its source and produces a computed hash, so a reused stored hash would
describe content the run never computed. `HashCachedOnly` and `HashCachedOrRead` are therefore
rejected for an item that requests a target, as that item's `Result.Err`, instead of silently
behaving as `HashRead`. `HashCachedOrReadRefresh` keeps its cache promise on a transfer and
transfers as `HashReadRefresh`. A refresh publishes the stored value only when it differs from
the computed one, which keeps an identical rewrite from touching the attribute. A targetless
item whose policy neither reads content nor uses the cache completes without opening the source
at all.

## Content signature cache

ACP stores a fixed binary SHA-256 plus the file size and nanosecond mtime in
`user.acp.signature` on Linux and the canonical `acp.signature` user attribute on Darwin
and FreeBSD.

The xattr is a disposable optimization. A policy that reuses a stored hash may skip reading
content, and a policy that refreshes rewrites the source and every successful target. One
descriptor serves the whole item: the stored hash is read through the descriptor that will read
the content, and the computed hash is published through it before it closes, so a cache
operation never reopens a path and nothing about cache writing outlives its item. A target
carries its own descriptor and keeps it open until the item's hash is known, so the target's
entry is published through the descriptor that wrote it, while that descriptor is still open. The
one path-based step runs before a target has a descriptor at all: an overwrite drops the target's
stale entry before the file is truncated, so a crash cannot leave the old entry describing the new
bytes.
Writers publish an entry only for the version whose bytes produced the hash, and they state that
version's size and mtime. The evidence is the descriptor the item still owns: a source keeps its
own metadata, so it must still show the facts the item observed, and a source that cannot be shown
to be that version publishes nothing instead of binding its hash to metadata the run never saw. A
target receives the item's metadata after the entry is published, so its length is what the
descriptor has to show. An entry whose file changes after it was written fails the reader's
metadata comparison and is treated as stale. Missing, stale, corrupt, read-only, full, or
unsupported xattrs are summarized as warnings and never become copy errors. A file system without
the managed attribute namespace stores no cache at all, which is a no-op: reading it is a miss and
writing or removing it records no failure. ACP's managed key is not copied as an ordinary source
xattr. The run reports its aggregate summary as `EventSignatureCacheSummary`.

## Platform behaviour

One descriptor per item and the same result contract hold on every platform; the platform
changes how a read avoids the access time, where the signature cache lives, how a target is
preallocated, what metadata can be restored, and how a mapped read is implemented.

| Platform | Source access time | Managed signature attribute | Target preallocation | Metadata restore | Mapped read |
| --- | --- | --- | --- | --- | --- |
| Linux | suppressed: a buffered source opens with `O_NOATIME`, falling back to an ordinary open when the flag is refused | `user.acp.signature`, in the `user.` namespace | `fallocate` reserves the whole target size | xattrs, mode, owner as root, times | `syscall.Mmap`, plus `MADV_SEQUENTIAL` and `MADV_WILLNEED` (one advice per call) for files up to 16 MiB |
| Darwin | not suppressed | `acp.signature` | `Truncate` | xattrs, mode, owner as root, times | `syscall.Mmap` |
| FreeBSD | not suppressed | `acp.signature`, in the user extended-attribute namespace | `Truncate` | mode and times only | no mapping: the descriptor is read with `ReadAt` |
| Windows | not suppressed | none: the cache is a silent no-op | `Truncate` | mode and times only | `CreateFileMapping` plus `MapViewOfFile` |
| other | not suppressed | none: the cache is a silent no-op | `Truncate` | mode and times only | no mapping: the descriptor is read with `ReadAt` |

- **Access time.** Suppression exists on Linux only, where `O_NOATIME` needs ownership or
  privilege, so `openSource` falls back to an ordinary open when the flag is refused. A mapped
  read never suppresses the access time on any platform: `mmap.Open` opens the file without
  `O_NOATIME`.
- **Signature attribute.** `user.acp.signature` is Linux's `user.` namespace spelling of one
  fixed binary codec; Darwin and FreeBSD address the canonical `acp.signature` directly, FreeBSD
  through the user extended-attribute namespace. A platform without a managed attribute
  namespace is a silent no-op rather than an error: reading it is a cache miss, and writing or
  removing it records no failure.
- **Preallocation.** Linux reserves the target's whole size with `fallocate`; Darwin, FreeBSD,
  Windows and other platforms use `Truncate`. Both run only for a non-linear target with a
  non-zero size, so a zero-length file and a tape-like target are left alone.
- **Metadata restore.** Only the unix builds restore xattrs, and only they restore ownership,
  and only while the process runs as root; every platform restores mode and times. ACP's managed
  signature key is never copied as an ordinary source xattr.
- **Mapped reads.** `ReadMapped` is one package with a per-platform implementation: `Mmap` on
  Linux and Darwin, `CreateFileMapping` plus `MapViewOfFile` on Windows, and a descriptor-backed
  `ReadAt` reader on every other platform, so the option works everywhere even where no mapping
  exists. `Close` releases the mapping and then the descriptor the reader owns, and an empty file
  has no mapping but still has that descriptor.

## af05f05c compatibility shell

The shell restores the public surface of `af05f05c`, the commit before the streaming API, so
existing callers and both commands keep working, and it carries today's additive options and errors
beside that surface. It is a shell over the push engine, not a second implementation:

```go
c, err := acp.New(ctx, acp.WildcardJob(acp.Source("example"), acp.Target("target")), acp.WithHash(true))
if err != nil {
	return err
}
c.Wait()
```

- `New(ctx, opts...) (*Copyer, error)`, `(*Copyer).Wait()`, `(*Copyer).WaitErr() error`.
  `Wait`/`WaitErr` close the stream and report how the run ended.
- `AccurateJob(src, dsts)` copies one exact source to exact target paths; `WildcardJob(Source(...),
  Target(...))`, `AccurateSource(base, paths...)`, `WildcardJobOption` walk sources, keep regular
  files, map each source-relative path onto every target directory, and sort them in the
  platform's path order. The walking, the mapping and the ordering are unexported helpers now:
  the intermediate revision's `SelectFiles` and `FileEntry` are gone.
- A repeated relative path inside one `WildcardJob` is an enumeration error that **ends the
  run**: `WaitErr` returns it and nothing is copied, instead of logging it and keeping the
  first file.
- `Overwrite(bool) Option`, `SetFromDevice`, `SetToDevice`, `LinearDevice`, `DeviceThreads`,
  `WithReadMode`, `WithReadBuffer`, `WithResultBuffer`, `WithResultBatch`,
  `WithResultFlushInterval`, `WithHashPolicy`, `WithLogger`, `WithEventHandler`,
  `WithProgressBar`, `WithHash(bool)`, `Event*`, `EventHandler`, `Error`, `Job`, `Report`,
  `ReportGetter`, `NewReportGetter`, `NewProgressBar`, `Cache[K,V]`, `DeviceOption`,
  `WildcardJobOption`, `Option`, `ReadCachedSignature`, `CachedSignature`,
  `DecodeCachedSignature`, `ErrTargetNoSpace`, `ErrTargetDropToReadonly`, `ErrTargetIO`,
  `CopyAttrs`, `UnexpectFileMode`.
- `WithHash(true)` maps to `HashReadRefresh` and `WithHash(false)` to `HashOff`; a repeated hash
  option is last-wins. Every symbol added since (`WithReadMode`, the result options,
  `WithHashPolicy`, `ErrTargetIO` and so on) is additive: every
  `af05f05c` symbol keeps working, and `compat_af05f05c_test.go` fails the build if one is renamed
  or retyped.
- Each item is translated into one terminal `EventUpdateJob` row, so `Report`, `NewReportGetter`
  and the JSON report keep their shape, including a row for an item ACP could not process: its
  failure travels under the empty `fail_target` key. The row is keyed by the joined relative path,
  exactly as `af05f05c` keyed it, so two items that resolve to one relative path share a row; a
  repeated relative path inside one job option already ends the run, and a library caller that
  submits overlapping job options should treat the shared row as a known limit of the report
  surface.
- A row carries `base`, the directory the source-relative `path` segments are resolved against, and
  `path` as an array. `full_path` names the source as one whole path and `signature_cache_hit`
  reports where the hash came from; both are additive fields that a row written before them does not
  have. `NewReportGetter` keys a row by the joined relative path.
- Two behaviours differ from `af05f05c` and are deliberate. First, an error the pipeline reports
  for a path — a walk that could not read a directory, a target that could not be removed after
  its metadata failed — is a run error, so `WaitErr` returns it and `cmd/acp` exits non-zero;
  `af05f05c` only logged such an error and published its event, and finished with a success
  status. Second, `WithHash(true)` manages the content-signature cache on the source and on every
  target, which `af05f05c` had no code for: a computed hash is published as a disposable
  size-and-mtime entry, and an overwritten target drops its stale entry before it is truncated.

## Withdrawn versions and how to migrate

`v0.1.0` and `v0.2.0` are **withdrawn**: neither is the compatibility baseline, both were
published from the abandoned intermediate streaming API, and their tags are revoked. `v0.2.1`
is source-compatible with `af05f05c` — the `New`/`Copyer` shell above — so a caller of the
baseline upgrades without changes. A caller that built against the withdrawn tags, or against
the intermediate names of this development branch, migrates like this:

| Removed | `v0.2.1` |
| --- | --- |
| `RunStream(ctx, source, sink, opts...) error` | `NewStream(ctx, onResults, opts...)`, then `Submit`, `Close`, `Wait` |
| `StreamSource` (`Next(ctx) (*StreamRequest, error)`, `io.EOF` ends) | the caller's own loop calling `Submit(items...)` |
| `StreamSink` (`Write(ctx, *StreamResult) error`, `Flush(ctx) error`) | the `onResults func([]Result) error` callback; ACP owns the flush and the batch |
| `StreamRequest{ID, Source, Targets}` | the caller's own `Item` (`Source()`, `Targets()`); the ID becomes a field of that item |
| `StreamResult{ID, Job *Job}` | `Result`: `Result.Job` is the submitted `Item`, so the caller recovers its own state by assertion |
| `WithSignatureCache(bool)` (v0.2.0) | `WithHashPolicy(HashCachedOrReadRefresh)` — it transfers as a refresh policy but still reuses a valid stored hash — or `WithHashPolicy(HashOff)` to disable the cache |
| `ForceRehash(bool)` (v0.2.0) | `WithHashPolicy(HashReadRefresh)`: always hash the content and rewrite the stored entry |
| `SourceWithPath(base, paths ...string)` (v0.1.0 and v0.2.0) | `AccurateSource(base, segments ...[]string)`: the shell keeps the `af05f05c` form, so each whole path becomes its segment slice — `SourceWithPath(b, "a/x")` is `AccurateSource(b, []string{"a", "x"})` |
| `Job.Path string` (v0.1.0 and v0.2.0 reports) | `Job.Path []string`: the row now carries the source-relative segments beside `Base`, and `FullPath` repeats the whole path |
| `Run`, `BatchSource`, `RunStream` request/result variants of this branch's earlier revisions | the same mapping: one feed loop, one results callback |

The `af05f05c` report surface stays: `Report`, `Job`, `Error`, `NewReportGetter` and the JSON
document keep their shape, and the shell fills one terminal row per item.

## Divergence from upstream `mmap`

This module started from the Go project's `mmap` package and is maintained here now: reads run
through the descriptor ACP owns, the end of the content is `io.EOF` and an open mapping of an empty
file is empty rather than closed, `Close` is idempotent and removes the mapping before it closes
that descriptor, a slice range is validated before anything is allocated, and a platform without
a mapping reads through the descriptor instead. Upstream changes are not merged automatically.

Two details of that contract are worth stating: a `ReadAt` that starts past the end of the content
reports an invalid offset rather than `io.EOF`, because only reaching the end of the content is the
end of the file; and `File` hands out the descriptor the reader owns, so a caller that keeps it must
keep the reader reachable, since the finalizer closes the descriptor once the reader is gone.

# Install
```
# Install acp
go install github.com/samuelncui/acp/cmd/acp
```

# Usage

```
Usage of acp:
  -cpu.arm
    	allow ARM features to be detected; can potentially crash
  -cpu.disable string
    	disable cpu features; comma separated list
  -cpu.features
    	lists cpu features and exits
  -from-linear
    	copy from linear device, such like tape drive
  -n	not overwrite exist file
  -notarget
    	do not have target, use as dir index tool
  -p	display progress bar (default true)
  -report string
    	json report storage path
  -report-indent
    	json report with indent
  -target value
    	use target flag to give multi target path
  -to-linear
    	copy to linear device, such like tape drive
```

The command exits `0` only when every selected item was copied to every requested target.
Any item failure, any target that was not written, and any pipeline failure make it exit
non-zero, and the report file is written either way.

## Example

```
# copy `example` dir to `target` dir
acp example target/

# copy `example` dir to `target` dir, and output a report to `report.json`
acp -report report.json example target/

# the same report, indented with two spaces
acp -report report.json -report-indent example target/

# copy one file to one exact path
acp example/a.bin target-b.bin

# copy `example` dir to `target1` and `target2` dir
acp example -target target1 -target target2

# do not copy, just get a dir index, write to `report.json`
acp example -notarget -report report.json

# copy from a tape-like source to a tape-like target
acp -from-linear -to-linear example target/
```

## Report

`-report` writes one `files` row per item plus, when the run recorded a pipeline problem, a
top-level `errors` array; an empty `files` or `errors` array is omitted, so a run with no items and
no problems writes `{}`. Rows are ordered by the joined relative path that keys them, so the same
items always write the same document. Per-item and per-target failures stay inside the row's
`fail_target` map, keyed by the target path, or by the empty key when ACP could not process the item
itself: an unreadable source, an item abandoned by a stop, or a targetless item whose content read
failed. The empty key is the one slot no requested target can occupy, so every item-level failure
has the same shape, and a failed item still gets a terminal row. The top-level `errors` array
carries pipeline problems only, so a single failure is recorded once. A row names its source the
`af05f05c` way: `base` plus the source-relative `path` array, with `full_path` as the additive whole
path.

`-report-indent` writes the same document with a two-space indent (`cmd/acp-rewrite` indents its
accumulated report with a tab). The report is plain JSON:
`encoding/json` reads and writes it, including the `fail_target` map, so no ACP-specific
decoder is needed. `cmd/acp-rewrite` accumulates its `-report` across runs and treats a report
it cannot decode as a fatal error instead of starting over with an empty history.

## Testing

See [TESTING.md](TESTING.md) for unit, race, end-to-end, and cross-platform test instructions.
