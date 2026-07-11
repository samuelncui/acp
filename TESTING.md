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

## End-to-end test

The end-to-end test builds the `acp` command, copies a directory tree through the CLI, and verifies:

- regular and empty file contents;
- nested destination paths;
- successful job status and destinations;
- JSON report decoding;
- SHA256 generation for every copied file.

Run only this test with:

```sh
go test -run '^TestACPCopyE2E$' -v .
```

The test is skipped when `go test` is run with `-short`.

## Focused tests

Run cache concurrency tests:

```sh
go test -race -run '^TestCache' -count=20 .
```

Run path and mountpoint tests:

```sh
go test -run '^(TestComparePath|TestSourceRoot|TestFindMountpoint)$' .
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
