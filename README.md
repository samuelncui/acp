# acp

ACP is a file copy tool and Go library with concurrent workers, ordered access for linear
media, buffered or mapped reads, multiple targets from one content read, metadata preservation,
progress events and JSON reports. An optional xattr cache stores SHA-256 content hashes.

## Requirements

Build with Go 1.26.8 or newer, as declared in `go.mod`.

## Operating model and stages

For the duration of a run, source contents and path relationships stay stable. The caller does
not submit one item's output as another item's source in the same run, or edit files, directories,
symlinks or hardlinks during copying. ACP owns its output changes; the rewrite command intentionally
replaces its own source after reading it. ACP does not detect or recover from unsupported input
changes. Files may change between runs.

| Stage | Responsibility |
| --- | --- |
| `index` | Evaluate each needed `Item` method once, collect source facts (reusing available enumeration metadata), and resolve target paths and devices. Validate source/target aliases and overwrite rules before any target mutation. Pass the source facts unchanged to later stages. |
| `prepare` | Open the source once when content or a cached hash is needed. Read the stored signature as required by the hash policy through that descriptor. Reserve a bounded file lifetime before dequeue; preparation does not read content ahead itself. |
| `copy` | Read content once and, when enabled, compute one hash while fanning out to the targets. Complete ordinary-file metadata, cache work, synchronization and closure before final replacement; device nodes use direct output. |
| `results` | Form one `Result` per accepted item and deliver it through one callback goroutine. Fatal pipeline failures are the exception to per-item completion; see [delivery and lifecycle](#delivery-and-lifecycle). |

Indexed source facts are immutable; content-byte progress is tracked separately.

Content streams through shared immutable chunks. Hashing and writing consume independent
references in parallel; each source is read once and each enabled digest is computed once.
Random targets have independent bounded consumers of the same bytes. Linear source data reads
remain ordered through EOF, then source Close runs independently of the next read. A linear
target has one application Write lane: the next file may start after the previous file's last
Write, while target Close, hash completion and metadata/publication settle asynchronously.
This permits application overlap; the filesystem and device still decide physical scheduling.

The per-run pool allocates lazily, with at most 512 backing buffers of 1 MiB (512 MiB of content
backing, not a total RSS limit). After all consumers and completion workers exit, backing returns
to the shared reusable pool with no reference to the completed stream. Eight credits are reserved for the ordered foreground file.
For linear targets, reads stay within the next 512 MiB from the writer's consumed frontier;
this is an upper bound, not a guarantee that the window fills. Each tiny file still occupies a
1 MiB backing, and the 256 full file lifetimes also include pending completion and result delivery.
Hash queues and each random-target queue hold four chunk references; file write queues hold
references sized to the file prefix, at most 512. References share the global backing budget.

Configured source threads bound actual Read calls; configured target threads bound concurrent
file data writers. Linear devices force their corresponding thread count to one. Preparation
uses `min(max(source threads, 8), 256)` workers. Ordered reads reserve an opportunity for the
earliest unfinished source; that reservation advances at source EOF independently of Write and
Close, including with one source thread. Workers release Read permits before waiting on queues.
Random-target files reserve their writer permit in dispatch order before reading, so future files
cannot consume all backing while the active writers need it. File-lifetime admission is reserved before
job dequeue, so later preparation cannot take the required head's slot. Open descriptors scale
with admitted files and their target counts, rather than with Read permits alone.
`WithReadBuffer` continues to count input items, independently of these content limits.

### Target completion

Random ordinary outputs complete metadata, Sync and Close before rename. Linear outputs omit
per-file Sync and Close independently after data writing; metadata/cache work on their owned
temporary paths follows closure. Distinct paths may publish concurrently; linear outputs to the
same resolved path preserve publication order, including equivalent relative and absolute names.
Results keep the caller's original target names, and the exclusive-rename fallback remains in force.
A late linear Close or metadata error stops future admission once observed; it cannot retract
already started writes. Results retain each started item's actual outcome, and completion waits
for all source/target Close, hash, cache and publication dependencies.

Ordinary targets use an exclusively created `.tmp_*` file beside the resolved final path. The
old target remains in place while ACP writes the new file; the rename happens only after that
file's content, hash, metadata and required closure complete. Success is reported after replacement, using the final
target path. A failed copy cleans up only its owned temporary file and retains cleanup errors
alongside the primary error. ACP never deletes the original to work around a rename failure.

An existing valid destination symlink is resolved once during indexing: ACP replaces its referent
and retains the symlink. A dangling destination symlink is refused. Directory symlinks are also
resolved once per parent during the run, including when child directories do not yet exist.
For missing directories, ACP finds the existing ancestor first and resolves its symlinks once.
Replacement creates a new inode
at the final path; other hardlinks to the old inode keep their old content. It requires write
permission on the destination directory and space for the complete new file while the old file
still exists. A source alias, including a hardlink or symlink alias, fails that target while
independent targets continue.

`Overwrite(false)` and `acp -n` reject targets found during indexing. For an initially missing
target, Linux and macOS use the filesystem's exclusive rename: another item's completed output
cannot be replaced, and independent targets finish concurrently. Other platforms, and filesystems
that do not support exclusive rename, serialize only the final existence check and rename.
Copying, sync and close stay outside that fallback lock. This preserves ACP's own output without
a per-file lock registry; external writers remain outside the supported operating model.

Device nodes are written directly, without rename or metadata/cache changes, and may contain
partial output after failure. Linear writers retain request order and perform neither
preallocation nor per-file sync. Ordinary non-linear files retain the allocation and sync
policy described under [platform behavior](#platform-behavior), with no free-space estimate.
Linear targets check an advisory cached free-space estimate before output creation. Each mount
gets an independent goroutine that samples immediately and refreshes every five seconds, with
at most one query in flight. Writing never waits for a capacity query, including the first one:
unknown capacity permits writing and relies on actual filesystem errors. Each positive-size
admission debits its logical size; admissions overlapping a query are also deducted from the
new observation. Empty targets skip the check, and equality permits the write. A completed
observation failure fails subsequent positive-size targets until a successful refresh; the worker
also logs that failure. Cleanup does not refund the estimate.
This estimate reserves nothing and cannot account for ongoing writes, filesystem allocation,
media buffering or overhead, so a passing estimate does not guarantee that the write will fit.
`Close`/`Wait` stop and join the polling workers after copying drains. An in-flight filesystem
capacity call cannot be cancelled and can delay shutdown, but never holds the cache lock.
Actual no-space and read-only errors stop the affected device and retain `ErrTargetNoSpace` and
`ErrTargetDropToReadonly`; `ErrTargetIO` remains an individual target failure.

Same-directory rename does not promise atomic replacement on every platform, including Windows,
or durability through power failure. ACP's existing sync policy is not a filesystem transaction.

The internal `fileio.Output` owns the temporary descriptor and path. The [rewrite command](#rewrite-command)
supplies its scanned source facts and preallocated output through internal `RewriteItem`, so
source replacement reuses the same completion path without a second temporary allocation, open
or source stat. Before replacement, rewrite closes its source descriptor and mapping, as required
by [Windows file sharing](https://learn.microsoft.com/en-us/windows/win32/api/fileapi/nf-fileapi-createfilew).
Rewrite retains its own state, hardlink and report responsibilities.

## Library API

The caller submits items to a push engine and consumes their results:

```go
func NewStream(ctx context.Context, onResults func([]Result) error, opts ...Option) (*StreamCopyer, error)
func (c *StreamCopyer) Submit(items ...Item) error
func (c *StreamCopyer) Close() error
func (c *StreamCopyer) Wait() error
```

```go
stream, err := acp.NewStream(ctx, func(results []acp.Result) error {
	for _, result := range results {
		job := result.Job.(*acp.SimpleJob) // the exact instance that was submitted
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

### Terminology

| Term | Meaning |
| --- | --- |
| Item | One caller-owned unit submitted to the push engine through `Item`. |
| Job | The compatibility `Job` report row; `SimpleJob` is the existing convenience implementation of `Item`, and internal job types carry pipeline state. These roles retain their names. |
| Target | One path requested by `Item.Targets` or the shell's target options; “destination” describes the same path. |
| Result | One item's terminal facts, error and target outcomes. `Result.Job` retains the submitted item. |
| Result buffer | The bounded queue of results waiting for delivery. |
| Result batch | The slice passed to one invocation of the results callback. |
| Content hash / cached signature | SHA-256 identifies content; a cached signature stores that hash together with size and mtime. |
| Stream / compatibility shell | The stream accepts items; the shell enumerates job options and produces report rows over that stream. |

`Copyer`, `StreamCopyer`, `AccurateJob`, `WildcardJob`, `Result.Job` and the report's existing
field names are public API names. Their spelling and type roles are preserved for compatibility.
A rewrite entry describes persisted rewrite work; it is separate from the stream's submitted item
and the report row.

### Delivery and lifecycle

Use one submitter, then call `Close` and `Wait`. `Submit` blocks when the read buffer is full
and rejects nil items. `Close` ends the feed and drains accepted work; it returns wrap-up or
flush errors. `NewStream` returns creation and option-validation errors; `Wait` returns the
terminal run error.

`Result.Err` describes an item ACP could not process, including a source description/open failure,
a failed targetless read or an item abandoned by a graceful stop. `TargetResult.Err` describes
one failed target. A nil `Result.Err` can accompany failed targets, so callers must inspect both.
Any result carrying an error is delivered immediately as its own batch. Successful results wait
for `WithResultBatch`, `WithResultFlushInterval`, or the final `Close` flush. `WithResultBuffer` bounds the queue;
slow callbacks apply backpressure. ACP owns batching; persistence belongs to the caller.
Copy a callback's result slice if retaining it beyond that call.

Result order is unspecified; target outcomes within a result retain request order. Never call
`Submit`, `Close` or `Wait` from inside `onResults`, since delivery must return before the run
can finish.

Cancellation of the caller's context, a callback error or an exhausted linear target stops new
submissions. Accepted items that cannot start receive the stopping error; work already past the
read stage finishes and is reported. A callback error does not retract the submission already
in progress. Closing the feed alone is not a stopping error. A fatal pipeline failure ends the
run without a per-item completion guarantee.

Caller methods, the results callback and event handlers run under panic protection. A panic
in an item method fails that item; a callback or event-handler panic becomes a run error. A
panic escaping pipeline code is a fatal run error, preserving the panic value.

## Options

| Option | Scope | Meaning |
| --- | --- | --- |
| `SetFromDevice(...)` | run | Source device options |
| `SetToDevice(...)` | run | Target device options |
| `LinearDevice(bool)` | device | Enable linear device behavior |
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
description.

`WithReadMode` applies to the source device only. An item implementing `ReadModeItem` overrides
that choice for its own source; an invalid mode or panic fails that item without stopping the
run. Buffered reads are the default; platform differences are listed below.

## Events

`WithEventHandler` receives `Event` values serially on one goroutine. A command needing both a
progress bar and another consumer composes them into that handler. `EventFinished` is sent once,
last; `Close` and `Wait` wait for the handler, so a handler that never returns holds the run open.

| Event | What it reports |
| --- | --- |
| `EventUpdateCount` | Accepted totals (`Bytes`, `Files`), with `Finished` on the last update |
| `EventUpdateProgress` | Completed totals (`Bytes`, `Files`), with `Finished` on the last update |
| `EventUpdateJob` | One terminal `Job` report row, emitted by the compatibility shell |
| `EventReportError` | A pipeline error (`Error`: source, target, cause) |
| `EventSignatureCacheSummary` | Aggregate cache activity when a policy manages the cache |
| `EventFinished` | The run ended |

## Content hash policy

`HashPolicy` selects both how an item's content hash is produced and how the stored hash is
used, because refreshing the cache requires a computed hash. A targetless item records content
facts; a transfer reads its source to write it somewhere:

| Policy | Targetless item | Transfer |
| --- | --- | --- |
| `HashOff` | no hash or cache use | no hash or cache use |
| `HashCachedOnly` | stored hash reused; a miss leaves the item without one | rejected: a copy always reads its source |
| `HashCachedOrRead` | stored hash reused; a miss reads and hashes | rejected: a copy always reads its source |
| `HashCachedOrReadRefresh` | stored hash reused; a miss reads, hashes and refreshes | always reads, hashes and refreshes |
| `HashRead` | always reads and hashes; no cache use | always reads and hashes; no cache use |
| `HashReadRefresh` | always reads and hashes, and refreshes the cache | always reads, hashes and refreshes the cache |

Transfers read source content and compute a hash only when the policy enables hashing. The two
reuse-only policies fail the item with `Result.Err` when it requests targets; they do not fail
stream creation. `HashCachedOrReadRefresh` transfers as `HashReadRefresh`.

## Content signature cache

The managed xattr holds a fixed binary SHA-256, size and nanosecond mtime. A lookup compares the
stored metadata with the indexed facts to detect a cache made stale between runs. The source
entry is read at most once when needed; refresh compares that saved value with the computed
entry and skips an unchanged write. A fresh staged target receives a new entry directly only
when refresh is enabled, without reading or invalidating an old one.
The target entry is written before restoring restrictive permissions; all metadata is complete
before replacement.

Source cache lookup uses its content descriptor. Source refresh runs after hash and source Close
through the stable source path; a closed linear output uses its owned temporary path. Random
output cache work uses its still-open descriptor before Sync and Close. Platforms may reopen
these paths for metadata operations without rereading content. Rewrite waits for source cache
and closure before replacing its source. The managed key is excluded from ordinary xattr
copying. Cache diagnostics are aggregated in `EventSignatureCacheSummary`; they never become
copy or `Wait` errors. A filesystem without the managed attribute namespace stores nothing:
reads miss and writes are silent no-ops.

## Platform behavior

| Platform | Buffered source access time | Managed signature attribute | Non-linear file allocation | Metadata restore | Mapped read |
| --- | --- | --- | --- | --- | --- |
| Linux | `O_NOATIME`, falling back when refused | `user.acp.signature` | `fallocate` | xattrs, mode, owner as root, times | `syscall.Mmap`; sequential/prefetch advice for files up to 16 MiB |
| Darwin | not suppressed | `acp.signature` | `Truncate` | xattrs, mode, owner as root, times | `syscall.Mmap` |
| FreeBSD | not suppressed | `acp.signature` in the user namespace | `Truncate` | mode and times | descriptor-backed `ReadAt` |
| Windows | not suppressed | none | `Truncate` | mode and times | `CreateFileMapping` and `MapViewOfFile` |
| other | not suppressed | none | `Truncate` | mode and times | descriptor-backed `ReadAt` |

Zero-length files need no preallocation. Non-linear writers sync content before closing.
Mapped reads do not suppress access time. See [platform verification limits](TESTING.md#platform-and-environment-limits)
for checks requiring a native runtime.

## af05f05c compatibility shell

The `af05f05c` surface delegates to the push engine and retains its exported names, field types
and [report format](#report). `v0.1.0` and `v0.2.0` are withdrawn and are not compatibility
baselines.

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
  platform's path order.
- A repeated relative path inside one `WildcardJob` is an enumeration error: `WaitErr` returns it
  and nothing is copied.
- `Overwrite(bool) Option`, `SetFromDevice`, `SetToDevice`, `LinearDevice`, `DeviceThreads`,
  `WithReadMode`, `WithReadBuffer`, `WithResultBuffer`, `WithResultBatch`,
  `WithResultFlushInterval`, `WithHashPolicy`, `WithLogger`, `WithEventHandler`,
  `WithProgressBar`, `WithHash(bool)`, `Event*`, `EventHandler`, `Error`, `Job`, `Report`,
  `ReportGetter`, `NewReportGetter`, `NewProgressBar`, `Cache[K,V]`, `DeviceOption`,
  `WildcardJobOption`, `Option`, `ReadCachedSignature`, `CachedSignature`,
  `DecodeCachedSignature`, `ErrTargetNoSpace`, `ErrTargetDropToReadonly`, `ErrTargetIO`,
  `CopyAttrs`, `UnexpectFileMode`.
- `WithHash(true)` maps to `HashReadRefresh`; `WithHash(false)` maps to `HashOff`.
- Pipeline errors are returned by `WaitErr` and cause the command to fail. The shell produces
  reports from final results, including failed items.
- `Cache[K,V]` initializes a key once while allowing different keys to initialize concurrently.

## Memory reader

The `mmap` package includes code derived from `golang.org/x/exp/mmap`; its upstream
notice and license are retained in [mmap/LICENSE](mmap/LICENSE).

The `mmap` package retains its source descriptor. Explicit `Close` releases a mapping before
closing the descriptor and is idempotent on every platform. An open empty file is readable as
empty, not closed; a failed mapping setup releases its resources. Slice bounds are checked
before allocation. `Read` returns `io.EOF` at the end; `ReadAt` rejects an offset past the end.
`File` returns the owned descriptor, so keep the reader reachable while using it. The finalizer
is a fallback, not a substitute for `Close`.

## Install

```sh
go install github.com/samuelncui/acp/cmd/acp@latest
go install github.com/samuelncui/acp/cmd/acp-rewrite@latest
```

## Usage

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
Any item failure, any target that was not written, cancellation, and any pipeline failure make
it exit `1`, and the requested report is attempted before exit. Failure to write that report also
exits `1`; invalid flags or missing positional arguments exit `2`. Independent items and targets
continue after individual failures. `-notarget` selects an index operation with no destinations,
including when `-target` flags were supplied.

### Examples

Place flags before positional source and target paths; Go flag parsing stops at the first positional argument.

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
acp -target target1 -target target2 example

# do not copy, just get a dir index, write to `report.json`
acp -notarget -report report.json example

# copy from a tape-like source to a tape-like target
acp -from-linear -to-linear example target/
```

## Report

The compatibility shell owns report rows; the push engine returns `Result` values. Each terminal
item, including a failed item, produces one `Job` row. `Base` is the source base directory and
`Path` is its relative segment slice, encoded as a JSON array. Their joined path is the additive
`FullPath` (`full_path`); `SignatureCacheHit` (`signature_cache_hit`) is also additive.
`NewReportGetter` keys and sorts rows by the joined relative path. Overlapping job options that
produce the same relative key share a row.

`-report` writes these rows under `files`. Per-target failures use the final target path in
`fail_target`; item-level failures use the empty key. Pipeline errors appear only in the
top-level `errors` array. Empty `files` and `errors` arrays are omitted, so an empty report is
`{}`. Nil errors remain absent. `-report-indent` and `Report.ToJSONString(true)` use two-space
indentation. Standard `encoding/json` handles reports without a custom decoder.

JSON report paths require valid UTF-8. Invalid filename bytes cause an encoding error instead
of a lossy path; native copies and in-memory results retain host filename semantics. Individual
rows and errors use `*Job` and `*Error` because their JSON methods have pointer receivers.
`json.Marshal` and `json.Encoder` return encoding errors; `Report.ToJSONString` returns an empty
string on encoding failure. Commands preserve the previous report and exit `1` if the requested
report cannot be saved.

## Rewrite command

`acp-rewrite -state state.json -report report.json ROOT` replaces regular files under the
[target completion contract](#target-completion), preserves each scanned hardlink group, and
accumulates report rows across runs. It reports final paths and duplicate content by size and
SHA-256. `-dryrun` saves the selected work without rewriting it; repeated `-ignore PATH` excludes
a file or subtree.
Busy entries remain in the state's busy queue; missing and failed entries remain pending for an
explicit later invocation. Missing paths are also recorded in the state's missing list. As with `acp`,
exit codes are `0` for success, `1` for execution or partial failure, and `2` for usage errors.

The persisted pending queue includes the current entry, its hardlinks and the unprocessed tail.
An entry leaves pending only after its copy, replacement, relinks, report and progress writes
succeed. An interrupted current entry may be rewritten again. Individual failures retain that
entry and allow independent entries to continue; a state or report persistence failure stops new
entries. State and report files must both decode before any source or saved temporary file is
changed.

Rewrite requires valid UTF-8 paths for its root, selected regular files and hardlinks, and the
directories holding its state/report documents and their temporary files. The document basenames
themselves retain host filename semantics. Scanning rejects an unsupported selected filename
before any file is rewritten; no lossy queue is saved. `-ignore` can exclude an unsupported file or subtree.
Whitespace, control characters, backslashes and the literal Unicode replacement character
are preserved wherever the host filesystem supports them.

State and report writes use the shared temporary-file allocation and JSON writer: encode, sync,
close, then rename. Temporary hardlinks use exclusive allocation with collision retry. Cleanup
removes only allocated paths or ownership recorded in state; a cleanup failure keeps that
ownership for a later run and is reported alongside the primary failure.

## Testing

See [TESTING.md](TESTING.md) for unit, race, end-to-end, and cross-platform test instructions
and the [release SOP](TESTING.md#release-sop).
