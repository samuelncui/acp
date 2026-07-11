# Repository Guide for Agents

## Scope

This file applies to the entire repository.

All source files, comments, tests, error messages, and documentation in this repository must be written in English.

## Project overview

`acp` is an advanced file copy tool and Go library. Its main features include concurrent copy workers, ordered traversal for linear devices, memory-mapped reads, multiple destinations, file metadata preservation, progress events, and JSON reports.

The repository also contains `cmd/acp-rewrite`, which rewrites regular files through temporary copies, persists resumable state, preserves hardlink groups, and reports duplicate content by size and SHA256.

## Main data flow

The copy pipeline is:

1. `index`: walk sources, validate regular files, collect metadata, and order jobs.
2. `prepare`: open each source through a normal file or memory mapping.
3. `copy`: stream each source to one or more destination writers and optionally calculate SHA256.
4. `cleanup`: restore metadata and publish the final job status.
5. `eventLoop`: fan out progress, job, and error events to registered handlers.

Pipeline changes must preserve channel ownership, wait for every worker, propagate failures into reports, and avoid leaving source readers or destination files open.

## Current maintenance context

The current change set addresses these areas:

- make memoized cache entries safe for concurrent use with `sync.Map` and per-key `sync.Once`;
- support empty files without passing a nil reader into the copy pipeline;
- serialize concrete report errors without unsafe interface pointer conversion;
- retry failed hardlink relinking through persisted rewrite state;
- select the longest matching mountpoint with directory-boundary checks;
- use `path/filepath` for host file-system paths;
- centralize platform-aware path ordering in `comparePath`;
- add semantic, race, cross-platform, CLI end-to-end, and library end-to-end coverage.

The working tree may already contain staged or unstaged changes. Preserve them and do not reset, rewrite, or discard unrelated work.

## Change guidelines

- Keep changes narrow and follow the style of the surrounding package.
- Use `path/filepath` for local file-system paths. Use `path` only for slash-delimited logical paths.
- Keep path ordering platform-aware through the shared `comparePath` function.
- Preserve the `Cache` guarantee that one key is initialized once while allowing different keys to initialize concurrently.
- Treat a zero-length regular file as a valid copy job.
- Keep report errors round-trippable, including messages that contain percent signs.
- Persist retry state before considering a rewrite or hardlink operation complete.
- Preserve build-tagged behavior in `syscall_*`, `mmap/*`, and `cmd/acp-rewrite/file_*` files.
- Do not add concurrency unless it has explicit ownership, completion, and error semantics.
- Add semantic regression tests for every behavior change.

## Verification

Follow [TESTING.md](TESTING.md). At minimum, run:

```sh
go test ./...
go test -race ./...
go vet ./...
```

Cross-build for Linux and Windows after changing path handling, system calls, memory mapping, or file metadata.
