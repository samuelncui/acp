# Testing

Run all commands from the repository root.

## Prerequisites

- Go 1.26.8 or newer: `go.mod` declares `go 1.26.8`, and the code uses
  `context.WithoutCancel` and `errors.Join`.
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

Run every local gate with the single command that owns them:

```sh
make check
```

It runs `go test ./...`, `go vet ./...`, the four cross-builds
(linux/amd64, windows/amd64, darwin/arm64, freebsd/amd64) and a `gofmt` check, and fails on
the first gate that fails. The individual commands below are the fallback for running one gate
on its own; apply the `GOFLAGS= GOWORK=off` prefix from above to them as well when this module
is checked out inside another Go workspace.

Run the complete test suite:

```sh
go test ./...
```

Run static analysis:

```sh
go vet ./...
```

Run the suite with the race detector (not part of `make check`):

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

The library end-to-end test uses the af05f05c shell and the push engine directly, with two
destinations, and verifies:

- the shell's wildcard enumeration and the complete prepare, copy, cleanup, and event pipeline;
- regular, empty, and nested file contents at both destinations;
- exactly one result per submitted item, with the submitted instance in `Result.Job` and one
  target outcome per destination;
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

Run every command end-to-end test, including the interrupt and exit-status cases, with:

```sh
go test -run '^TestACPCommand' -v .
```

The interrupt test sends `SIGINT` in the middle of a run and requires every selected file to
appear exactly once in the report; because the stop abandons work, the command must exit
non-zero. `TestACPCommandExitsNonZeroWhenACopyFails` refuses one target with `-n` and requires
a non-zero exit status together with a report that names the refused target.

The command end-to-end tests are skipped when `go test` is run with `-short`.

## Focused tests

Run cache concurrency tests:

```sh
go test -race -run '^TestCache' -count=20 .
```

Run path and mountpoint tests, which resolve real paths to their mount point and pin the
failure a path that cannot be made absolute reports:

```sh
go test -run '^(TestComparePath|TestSourceRoot|TestFS|TestGetMountpointReportsAbsFailure|TestFindMountpoint)$' .
```

Run option and event tests, which pin the last-wins option rule, the validation of every option,
and the single-goroutine event handler contract:

```sh
go test -run '^(TestWithEventHandlerIsLastWins|TestNewDoesNotWriteIntoTheCallersOptionSlice|TestRunCallsOneEventGoroutineAtATime|TestNewStreamRejectsInvalidOptions|TestNewStreamRejectsNilResultsCallback|TestNewStreamRejectsJobOptions|TestNewRejectsInvalidOptions|TestNewRejectsNegativeDeviceThreads)$' .
```

Run report tests, which pin the JSON contract with the standard library, the nil handling of
report errors, the indented writer, and the terminal row of a failed item:

```sh
go test -run '^(TestErrorJSONMarshal|TestJobFailTargetsJSON|TestReportStandardLibraryRoundTrip|TestReportToJSONStringIndentUsesSpaces|TestRunReportsAFailedItemAsATerminalRow)$' .
```

Run the command's report and exit-status tests, which pin the report shape, the failure
decision, the stored report file, and the exact-target branch:

```sh
go test -run '^(TestReportKeepsFailuresInsideTheFileRow|TestReportAccountsForEveryEntryAfterAGracefulStop|TestReportFailureDecision|TestStoreReportWritesTheFailureDocument|TestAccurateTargetUsesItsTargets)$' ./cmd/acp
```

Run disk usage tests, which pin the target error identities and the reservation accounting
across a refresh:

```sh
go test -run '^(TestMappingErrorIdentities|TestDiskUsageRefreshKeepsInflightReservations|TestDiskUsageAccountsLandedBytes)$' .
```

Run the fatal-panic safety net test:

```sh
go test -run '^TestWrapRecordsAFatalPanic$' .
```

Run the shell's file selection and enumeration tests, which pin the relative-path mapping, the
platform path order, and the fast failure on a repeated relative path:

```sh
go test -run '^(TestWalkWildcardMapsSourcesOntoTargets|TestWalkWildcardMapsASingleFileOntoTargets|TestShellRejectsRepeatedRelativePaths|TestNewRejectsInvalidInput|TestSourceRoot)$' .
```

Run item contract tests, which pin one result per accepted item, one outcome per requested target
in request order, a multi-chunk copy to every target, failure routing, single-goroutine callback
delivery, and the report row of a failed item:

```sh
go test -run '^(TestRunReportsEveryAcceptedItemExactlyOnce|TestRunKeepsLinearOrderingPastAnUnprocessedItem|TestRunReportsUnprocessableItemsAsFailures|TestRunReportsOneOutcomePerRequestedTargetInOrder|TestRunCopiesAChunkedSourceToEveryTarget|TestRunReportsTargetFailureAsItemOutcome|TestRunDeliversResultsFromOneGoroutine|TestRunReportsAFailedItemAsATerminalRow|TestPrepareReportsUnstartedItemsWithoutReadingThem|TestPrepareReadsNoContentWhenThePolicyNeedsNone|TestWriteKeepsTargetOutcomesWhenTheReadAlsoFails|TestWriteReportsTheFactsItReadWhenTheSourceChanged|TestRunRejectsReuseOnlyPolicyForItemsWithTargets|TestRunKeepsEveryOtherPolicyForItemsWithTargets|TestRunReturnsErrorWhenTheResultsCallbackPanics|TestRunReturnsErrorWhenAnEventHandlerPanics|TestWriteReportsTargetlessReadFailureAsItemFailure)$' .
```

Run the reshape's new contract tests, which pin the result queue and the result batch as separate
options, the immediate delivery of a result that carries an error, the freshness of a delivered
batch, the cancellation semantics of a results callback error, the feed's backpressure and
refusal, the one `EventFinished` per registration, the hard-stop escape of every handoff, the
exhausted-target path, the shell's exact-target run, and the fatal-panic boundary:

```sh
go test -run '^(TestFailedResultBypassesTheResultBuffer|TestSuccessResultsAreBatchedAndFlushedOnClose|TestDeliveredBatchesAreFreshSlices|TestEventHandlerSeesExactlyOneFinishedEvent|TestResultsCallbackErrorActsAsACancellation|TestSubmitBlocksWhileTheReadBufferIsFull|TestSubmitRejectsANilItem|TestStreamReportsSubmissionFailureAsRunError|TestStreamCopyReleasesAChunkHandoffAfterHardStop|TestSubmitDropsAnEventAfterHardStop|TestFailAllReportsEveryTargetAsFailed|TestShellCopiesAnAccurateJob|TestWrapStopsAPanickingPipeline)$' .
```

Run graceful-stop tests, which drive a feed that spans several batches and cancel the context
mid-run:

```sh
go test -run '^(TestRunStopsFeedingItemsAfterGracefulStop|TestRunDoesNotReportAStopAfterACompleteRun|TestRunCancellationDrainsPrefetchedItems|TestRunReportsEveryAcceptedItemExactlyOnce)$' .
```

Run linear stream-order tests:

```sh
go test -run '^(TestRunCopiesItemsToLinearTarget|TestForwardPreparedOrdersOnlyLinearTargets|TestRunAppliesBoundedBackpressure|TestLinearTargetStopsWhenDiskUsageEstimateIsInsufficient|TestStoppedLinearTargetRefusesSubmission)$' .
```

Run the target-failure drain test, which proves every read buffer is released and no goroutine
deadlocks. It writes to `/dev/full`, so it runs on Linux only:

```sh
go test -race -run '^TestWriteFailureDrainsBuffersAndTargets$' .
```

On Linux, add the `/dev/full` end-to-end paths, which exercise an actual `ENOSPC` write failure
and its mapping onto `ErrTargetNoSpace`:

```sh
go test -race -run '^(TestRunMapsDeviceFullToTargetNoSpace|TestDeviceFullWriteFailureDrainsQueuedBuffers)$' .
```

These two select no test on another platform: the file that defines them is Linux-only.

Run hash policy tests, which pin the reuse, read, and refresh matrix, and the no-op policy of
a file system without the managed signature attribute:

```sh
go test -run '^(TestRunHashPolicyMatrix|TestRunRefreshWritesOnlyWhenStoredHashDiffers|TestRunTransferAlwaysReadsAndRefreshesTargets|TestOverwriteInvalidatesSignatureWithoutCache|TestRunCorruptSignatureIsWarning|TestRunSignatureCacheZeroLength|TestRunIgnoresUnsupportedSignatureXattr)$' .
```

Run every signature cache test, including the codec, the descriptor identity tests that prove a
cache entry never travels through a reopened path, and the publication-order test that proves an
item publishes its entry before its result:

```sh
go test -run '^(TestCachedSignatureCodec|TestSignatureCache|TestRunSignature|TestRunHashPolicy|TestRunRefresh|TestRunTransfer|TestOverwriteInvalidates|TestRunCorruptSignature|TestRunReadsTheStoredHash|TestRunWrites|TestRunPublishes)' .
```

Run `acp-rewrite` tests, which pin one rewrite through its scratch file, the state and report
round trips, the interrupted-run cleanup, and the dry-run and resume command runs:

```sh
go test ./cmd/acp-rewrite
go test -run '^(TestRewriteFileCommitsTheFinalPath|TestRewriteFileRejectsAMissingSource|TestStateRoundTrip|TestReportRoundTripKeepsHistory|TestCleanupTmpFilesRemovesOnlyScratchFiles|TestRewriteCommandResumesAfterDryRun)$' -v ./cmd/acp-rewrite
```

Run the memory-mapping contract tests, which pin the reader, slice, close and descriptor
ownership contracts:

```sh
go test -v ./mmap
```

## Cross-platform checks

Build all packages for Linux and Windows:

```sh
GOOS=linux GOARCH=amd64 go build ./...
GOOS=windows GOARCH=amd64 go build ./...
```

The Darwin and FreeBSD builds cover the platform-specific memory mapping (including the
`mmap_other.go` fallback) and the managed signature attribute:

```sh
GOOS=darwin GOARCH=arm64 go build ./...
GOOS=freebsd GOARCH=amd64 go build ./...
```

Compile the Windows-specific root package tests without running them:

```sh
GOOS=windows GOARCH=amd64 go test -c -o /tmp/acp-windows.test .
```

## Change checklist

Before submitting a change:

1. Run `make check`, the single command for the local gates: `gofmt`, `go test ./...`,
   `go vet ./...` and the four cross-builds.
2. Run `go test -race ./...` for concurrency, cache, pipeline, or event changes.
3. Confirm that source files, comments, tests, and documentation contain only English text.
