# Repository Guide for Agents

## Scope

This file applies to the entire repository.

All source files, comments, tests, error messages, and documentation in this repository must be written in English.

## Project overview

`acp` is an advanced file copy tool and Go library. Its main features include concurrent copy workers, ordered traversal for linear devices, memory-mapped reads, multiple destinations, file metadata preservation, progress events, and JSON reports.

The repository also contains `cmd/acp-rewrite`, which rewrites regular files through temporary copies, persists resumable state, preserves hardlink groups, and reports duplicate content by size and SHA256.

## Main data flow

The copy pipeline is:

1. `index`: validate each submitted item, collect its source metadata, and order it.
2. `prepare`: open the one descriptor the item owns, through a plain file or a memory mapping,
   and read the item's stored hash through it.
3. `copy`: stream each source to one or more destination writers, optionally calculate SHA256, and
   publish the item's computed cache entries through the descriptors that read and wrote the files.
4. `cleanup`: restore metadata and finalize one `Result` per item.
5. `results`: finalize one `Result` per item into the result buffer, deliver a result that carries
   an error immediately, and let the delivery stage hand up to `WithResultBatch` buffered results
   to the callback when a batch fills, the interval elapses, or the run ends.
6. `eventLoop`: fan out progress, count, and error events to registered handlers.

Pipeline changes must preserve channel ownership, wait for every worker, propagate failures into
results, and avoid leaving source readers or destination files open.

The pipeline has exactly one cancellation checkpoint: the item feed in `Submit`, which consults
the caller's context, the results callback error and an exhausted linear target before it accepts
a batch. Every later stage runs on a context that never cancels, so it only ever drains and
forwards what it holds; a stage that pulls an item it can no longer start marks the item with the
stopping reason, and the delivery stage is the only goroutine that invokes the results callback.
A graceful stop therefore gives every accepted item exactly one result. `hardStop` stays reserved
for fatal failures, which make no per-item promise; a panic that escapes pipeline code triggers
it, so every handoff — the read buffer, a chunk send, an event send and the result buffer — has a
`hardStop` escape instead of leaving a blocked send behind.

## Current maintenance context

The public API is the v0.2.1 push engine plus the `af05f05c` compatibility shell. The library
entry point is `NewStream` with `Submit`/`Close`/`Wait`; `Run` and `BatchSource` are gone, and
`New`/`Copyer`/`AccurateJob`/`WildcardJob` are the shell over that engine.

- Results travel through one callback, `onResults`, called from one goroutine and never
  concurrently: `Result.Job` is the exact submitted item, so an item is pure data and carries no
  callback. The engine owns both the queue and the batch: a result that carries any error is
  delivered immediately as its own batch, while results without an error wait in the result
  buffer (`WithResultBuffer`) until a delivery holds `WithResultBatch` results, the result flush
  interval elapses (`WithResultFlushInterval`), or `Close` flushes them. Result order is
  unspecified, while a linear target still writes in request order. `Close`, `Wait` and `Submit`
  must not be called from inside `onResults`: the callback runs on the delivery goroutine that
  `Close` waits for.
- An error `onResults` returns is a cancellation, not a hard stop: accepted items past the read
  stage finish, items still inside the read pipeline report that error, the batch in hand is
  still submitted, the next `Submit` fails, and `Wait` returns it. `NewStream` returns creation
  and validation errors, `Wait` returns run-time errors, and `Close` returns the flush error.
- `Close` is not a stopping reason for an item the run already accepted: it ends the feed and
  drains the pipeline, so every submitted item is still reported.
- The report is encoded and decoded by `encoding/json` alone: `Job` and `Error` own their JSON,
  a nil error stays absent, and `Report.ToJSONString(true)` indents with two spaces.
- A report row keeps the `af05f05c` shape: `Base` is the directory the source-relative `Path`
  segments are resolved against, so the JSON carries `path` as an array. `FullPath` and
  `SignatureCacheHit` are additive fields, `NewReportGetter` keys a row by the joined relative
  path, and the push engine owns no report row at all — the shell fills the row from its own
  compatibility item, which carries the base and the segments its walk knows.
- The shell publishes one terminal report row per item, so a failed item still has a row, with
  the item-level failure under the empty target key.
- `cmd/acp` uses the shell and exits non-zero when any item, target or pipeline failure
  occurred, and writes the report either way; `cmd/acp-rewrite` reports the final path of a
  rewrite and refuses to start on a state or report file it cannot decode.
- A fatal pipeline panic is recorded as the run error and ends the pipeline instead of only
  being logged.
- An I/O error has its own identity (`ErrTargetIO`) instead of aborting the target as a
  read-only device, and a disk-usage refresh keeps the reservations of copies still in flight.
- A file system without the managed signature attribute stores no cache at all, which is a
  no-op rather than a recorded failure.
- `mmap` returns `io.EOF` at the end of the content, releases a mapping whose `madvise` failed,
  closes idempotently on every platform, validates a slice range before allocating it, and retains
  the descriptor it opened: `Close` removes the mapping and then closes that descriptor.
- One source descriptor serves a whole item. The stored hash is read through the descriptor that
  reads the content, and the computed hash is published through it, synchronously, after the hash
  is complete and before the descriptor closes. A target publishes its entry through the
  descriptor the writer wrote it with, while that descriptor is still open; no cache operation
  reopens a path, and no cache write outlives its item.
- Repeated options are last-wins (`WithEventHandler` included), and the shell rejects a repeated
  relative path instead of copying one target twice.

The working tree may already contain staged or unstaged changes. Preserve them and do not reset, rewrite, or discard unrelated work.

## Change guidelines

- Keep changes narrow and follow the style of the surrounding package.
- Use `path/filepath` for local file-system paths. Use `path` only for slash-delimited logical paths.
- Keep path ordering platform-aware through the shared `comparePath` function.
- Preserve the `Cache` guarantee that one key is initialized once while allowing different keys to initialize concurrently.
- Treat a zero-length regular file as a valid copy job.
- Keep report errors round-trippable, including messages that contain percent signs.
- Keep the report a standard-library document: `encoding/json` must be able to encode and decode it, `Job` and `Error` own their own JSON, a missing error stays absent instead of becoming an empty one, and no custom JSON coder or unsafe pointer conversion is used for it. Keep `Job` in the `af05f05c` shape — `Base` and the source-relative `Path` segments, which JSON carries as an array — and add what today's callers need as additive fields such as `FullPath`. The push engine owns no report row: only the shell fills one, from the compatibility item its walk built.
- Keep the `af05f05c` surface source-compatible: `compat_af05f05c_test.go` uses the old calls and the old field types from outside the package, so a renamed or retyped exported symbol fails that build.
- Give every accepted item exactly one result, and the shell exactly one terminal report row per result, including an item ACP could not process. An item-level failure has no requested target to belong to, so it travels under the empty `fail_target` key on every path.
- Keep the results callback the single delivery path: one goroutine, one batch at a time, the submitted item instance in `Result.Job`, and no per-item callback on `Item`.
- Keep the engine the owner of the result queue and the batch: `WithResultBuffer` is the depth of
  the result queue, `WithResultBatch` is how many results one delivery carries, and
  `WithResultFlushInterval` is how long a successful result may wait, so a callback never
  assembles a batch of its own. `Close` must flush what the buffer still holds and report the
  flush error, and neither `Close` nor `Wait` may be called from inside the callback.
- Make the command line honest: any item, target or pipeline failure exits non-zero, and the report file is still written before it exits.
- Keep the fatal-panic safety net authoritative: a panic that escapes pipeline code is recorded as the run error with the panic value preserved and ends the pipeline, so `Wait` and `WaitErr` neither report success nor wait forever after a worker died.
- Keep target error identities distinct: `ErrTargetNoSpace` and `ErrTargetDropToReadonly` abort the target, while `ErrTargetIO` describes one failed target and must not be treated as a read-only device.
- Never discard reservations a disk-usage cache already counted for copies still in flight on the same mount point; a refresh replaces the capacity estimate, not the commitments.
- Treat a file system without the managed signature attribute namespace as a cache that stores nothing: reading is a miss, and writing or removing it is a no-op rather than a recorded failure.
- Keep `mmap` a conforming `io.Reader`/`io.ReaderAt`: the end of the content is `io.EOF`, a mapping whose `madvise` failed is released before the error returns, `Close` is idempotent on every platform, a slice range is validated before anything is allocated, and the reader owns the descriptor it opened: `Close` removes the mapping before it closes that descriptor, and `File` reports it until then.
- Repeated options are last-wins; `WithEventHandler` replaces the previous handler, `WithEventHandler(nil)` clears it, and an event handler is only ever called from one goroutine per registration.
- Validate every option in `NewStream`/`New` instead of midway through a run, and keep `Overwrite` a run-level option so it cannot be passed to `SetToDevice`.
- Persist retry state before considering a rewrite or hardlink operation complete.
- Preserve build-tagged behavior in `syscall_*`, `mmap/*`, and `cmd/acp-rewrite/file_*` files.
- Do not add concurrency unless it has explicit ownership, completion, and error semantics.
- Keep the ACP content-signature xattr a disposable size-and-mtime cache. Transfers always hash content, and managed cache keys are not copied as ordinary xattrs.
- Keep one descriptor per item. The stored hash is read through the descriptor that reads the content, the computed hash is published through that same descriptor before it closes, and a target publishes its entry through the descriptor that wrote it while that descriptor is still open. Never reopen a path to read or write a cache entry: an entry must describe the exact file version the item handled. Dropping a target's stale entry before the target is truncated is the one path-based cache step, and it happens before that target has a descriptor at all.
- Report signature-cache read and write failures as aggregate warnings without adding them to `WaitErr`.
- Treat data as stable for the duration of one run: the operator guarantees that a source does not change while work is in progress, and the implementation does not detect or recover from a mid-flow change. An item completes with the facts it observed, so its size and hash come from the actual read; a change check must never produce a failure or a completion built from pre-read facts. Revalidation a caller performs around ACP stays the caller's own policy.
- Reject a reuse-only hash policy (`HashCachedOnly`, `HashCachedOrRead`) for an item that requests targets, as an option error. A transfer always reads its source and produces a computed hash, so the policy name would promise a stored hash the run cannot use.
- Wrap every call into caller-implemented code — the results callback, event handlers, and
  `Item.Source`/`Item.Targets` — so a panic becomes an error instead of unwinding a pipeline
  goroutine that owns channel closing. A panicking callback or event handler is a run error that
  `Wait` returns, while a panic in `Item.Source`/`Item.Targets` is that item's `Result.Err`, which
  keeps the item accounted for exactly once and the run going.
- Keep the shell's enumeration faithful to `af05f05c`: regular files only, one target per requested target directory, platform path order, and one run-level failure for a repeated relative path instead of a logged skip.
- Add semantic regression tests for every behavior change.

## Verification

Follow [TESTING.md](TESTING.md). At minimum, run:

```sh
go test ./...
go test -race ./...
go vet ./...
```

Cross-build for Linux, Windows, Darwin and FreeBSD after changing path handling, system calls, memory mapping, or file metadata. The FreeBSD build covers the `mmap_other.go` and `signature_xattr_freebsd.go` paths, which no test on this host can run.
