# Testing

Run all commands from the repository root.

## Prerequisites

- Go 1.18 or newer.
- Enough free space under the system temporary directory for test files and compiled binaries.
- A host file system that supports the operations exercised by the selected tests.

## Standard checks

Run the complete test suite:

```sh
go test ./...
```

Run static analysis:

```sh
go vet ./...
```

Run the suite with the race detector:

```sh
go test -race ./...
```

## End-to-end tests

The command end-to-end test builds the `acp` command, copies a directory tree through the CLI, and verifies:

- regular and empty file contents;
- nested destination paths;
- successful job status and destinations;
- JSON report decoding;
- SHA256 generation for every copied file.

Run only the command test with:

```sh
go test -run '^TestACPCommandE2E$' -v .
```

The library end-to-end test uses the public Go API directly with two destinations and verifies:

- the complete index, prepare, copy, cleanup, and event pipeline;
- regular, empty, and nested file contents at both destinations;
- successful report status and destination sets;
- file size and exact SHA256 values.

Run only the library test with:

```sh
go test -run '^TestACPLibraryE2E$' -v .
```

Run both end-to-end tests with:

```sh
go test -run '^TestACP(Command|Library)E2E$' -v .
```

Both tests are skipped when `go test` is run with `-short`.

## Focused tests

Run cache concurrency tests:

```sh
go test -race -run '^TestCache' -count=20 .
```

Run path and mountpoint tests:

```sh
go test -run '^(TestComparePath|TestSourceRoot|TestFindMountpoint)$' .
```

Run linear stream-order tests:

```sh
go test -run '^(TestRunStreamCopiesRequestsToLinearTarget|TestForwardPreparedOrdersOnlyLinearTargets|TestRunStreamAppliesBoundedBackpressure|TestRunStreamCancellationDrainsPrefetchedJobs|TestLinearTargetStopsWhenDiskUsageEstimateIsInsufficient)$' .
```

On Linux, include `TestRunStreamMapsDeviceFullToTargetNoSpace` to exercise an actual `/dev/full` write failure.

Run `acp-rewrite` tests:

```sh
go test ./cmd/acp-rewrite
```

## Cross-platform checks

Build all packages for Linux and Windows:

```sh
GOOS=linux GOARCH=amd64 go build ./...
GOOS=windows GOARCH=amd64 go build ./...
```

Compile the Windows-specific root package tests without running them:

```sh
GOOS=windows GOARCH=amd64 go test -c -o /tmp/acp-windows.test .
```

## Change checklist

Before submitting a change:

1. Run `gofmt` on modified Go files.
2. Run `go test ./...`.
3. Run `go test -race ./...` for concurrency, cache, pipeline, or event changes.
4. Run `go vet ./...`.
5. Cross-build when changing paths, system calls, memory mapping, or file metadata.
6. Confirm that source files, comments, tests, and documentation contain only English text.
