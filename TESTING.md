# Testing

Run all commands from the repository root.

## Prerequisites

- Go 1.18 or newer.
- Enough free space under the system temporary directory for test files and compiled binaries.
- A host file system that supports the operations exercised by the selected tests.
- A host file system that supports the managed signature xattr for the cache tests. A test
  that cannot store it skips itself with a diagnostic.

When this module is checked out inside another Go workspace, run every command with an
explicit empty `GOFLAGS` and `GOWORK=off` so the parent build flags and workspace do not
leak into module resolution:

```sh
GOFLAGS= GOWORK=off go test ./...
```

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

- file selection, item submission, and the complete prepare, copy, cleanup, and event pipeline;
- regular, empty, and nested file contents at both destinations;
- one terminal callback per submitted item with one target outcome per destination;
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

Run file selection tests:

```sh
go test -run '^TestSelectFiles' .
```

Run item contract tests, which pin one terminal callback per accepted item, one outcome per
requested target in request order, failure routing, and single-goroutine callback delivery:

```sh
go test -run '^(TestRunReportsEveryAcceptedItemExactlyOnce|TestRunKeepsLinearOrderingPastAnUnprocessedItem|TestRunReportsUnprocessableItemsAsFailures|TestRunReportsOneOutcomePerRequestedTargetInOrder|TestRunReportsTargetFailureAsItemOutcome|TestRunDeliversTerminalCallbacksFromOneGoroutine|TestPrepareReportsUnstartedItemsWithoutReadingThem|TestPrepareCompletesWithoutOpeningTheSourceWhenPolicyNeedsNoContent)$' .
```

Run graceful-stop tests, which drive a source that blocks or spans several batches and
cancel the context mid-run:

```sh
go test -run '^(TestRunStopsFeedingItemsAfterGracefulStop|TestRunReturnsStoppingErrorWhenBatchSourceEndsFirst|TestRunCancellationDrainsPrefetchedItems|TestRunReportsEveryAcceptedItemExactlyOnce)$' .
```

Run linear stream-order tests:

```sh
go test -run '^(TestRunCopiesItemsToLinearTarget|TestForwardPreparedOrdersOnlyLinearTargets|TestRunAppliesBoundedBackpressure|TestLinearTargetStopsWhenDiskUsageEstimateIsInsufficient|TestStoppedLinearTargetDoesNotReadBatchSource)$' .
```

Run the target-failure drain tests, which prove every read buffer is released and no
goroutine deadlocks:

```sh
go test -race -run '^(TestWriteFailureDrainsBuffersAndTargets|TestDeviceFullWriteFailureDrainsQueuedBuffers)$' .
```

On Linux, include `TestRunMapsDeviceFullToTargetNoSpace` and
`TestDeviceFullWriteFailureDrainsQueuedBuffers` to exercise actual `/dev/full` write failures:

```sh
go test -race -run '^(TestRunMapsDeviceFullToTargetNoSpace|TestDeviceFullWriteFailureDrainsQueuedBuffers)$' .
```

Run hash policy tests, which pin the reuse, read, and refresh matrix:

```sh
go test -run '^(TestRunHashPolicyMatrix|TestRunRefreshWritesOnlyWhenStoredHashDiffers|TestRunTransferAlwaysReadsAndRefreshesTargets|TestOverwriteInvalidatesSignatureWithoutCache|TestRunCorruptSignatureIsWarning|TestRunSignatureCacheZeroLength)$' .
```

Run every signature cache test, including the drain tests:

```sh
go test -run '^TestRunSignature|^TestSignatureCache' .
```

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
