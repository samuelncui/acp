# Testing

Run commands from the repository root with Go 1.26.8 or newer and enough temporary disk space.
When checked out inside another Go workspace, isolate this module from parent settings:

```sh
export GOFLAGS=
export GOWORK=off
```

## Test maintenance

Use the existing entrypoints below for tests, regressions and performance work: `go test`
(including benchmarks) and the maintained release-tool scripts. Prefer reproducible commands over
repeated manual agent steps. Recurring regressions belong in checked-in tests or harnesses with
fixtures under the existing owner; extend those before adding another runner or script.

Maintain tests, harnesses and fixtures with the implementation. Update or remove obsolete
assertions and unsupported scenarios in the same change. Test meaningful behavior and failure
boundaries; avoid duplicate checks at the same boundary or assertions that merely mirror code.
Distinct unit and integration boundaries remain useful. Keep verification proportional to the
change; a small edit does not by itself require new automation.

Record the source, exact commands, passes, failures, skips and unrun checks with their reasons.
Use the [performance policy](#performance-comparisons) for timings. Existing native-device
acceptance and human content/consumer review remain required where applicable; record manual
steps, their results and why automation cannot establish that outcome.

## Standard checks

```sh
make check
go test -race ./...
```

`make check` owns `go test ./...`, `go vet ./...`, cross-builds for linux/amd64,
windows/amd64, darwin/arm64 and freebsd/amd64, the Python 3 release-tool regressions, and the
`gofmt` check. It stops on the first failure. The race suite is separate and is required for
concurrency, cache, pipeline and event changes. Individual gates remain available as `make test`,
`make vet`, `make cross` and `make race`.

## Behavior and test boundaries

[README.md](README.md#operating-model-and-stages) owns the behavior being verified. Unit and
stage tests isolate decisions, failure boundaries and resource ownership; library and command
integration tests verify their public wiring. Similar assertions at these boundaries can serve
different purposes. Keep operation-count checks for the documented single evaluations, without
requiring removed bookkeeping or unsupported mid-copy mutation handling.

Use this inventory to select or update coverage; it is not a record of tests run:

| Boundary | Unit or stage coverage | Integration coverage |
| --- | --- | --- |
| Selection, path mapping, option validation and the `af05f05c` surface | `files_test.go`, `opt_test.go`, `opt_windows_test.go`, `compat_af05f05c_test.go` | `library_e2e_test.go`, `e2e_test.go` |
| Input-method evaluation, reused source facts, target resolution and alias/overwrite validation | `item_test.go`, `stream_test.go`, `fs_test.go`, `target_test.go` | Public stream and shell copies in `library_e2e_test.go` |
| Source ownership, one content pass/hash, read modes, empty files and multiple targets | `copy_test.go`, `item_test.go`, `fs_test.go` | Library and command E2E |
| Staged replacement, failure preservation, final-path outcomes and owned temporary cleanup | `staged_target_test.go`, `target_test.go`, `internal/fileio/output_test.go`, `internal/fileio/temp_test.go` | Library/command E2E and rewrite failure/resume tests |
| Metadata, hash policies, between-run cache staleness, cache warnings and descriptor use | `signature_test.go`, `signature_xattr_copy_test.go`, `cleanup_test.go` | Real cache copies in those tests; metadata and report hashes in library/command E2E |
| Results, batching, cancellation, backpressure, linear ordering and event delivery | `item_test.go`, `stream_test.go`, `copy_test.go`, `opt_test.go` | Command interrupt, report and progress tests in `e2e_test.go` |
| Panic handling, channel completion and resource release | `recover_test.go`, `copy_test.go`, `stream_test.go` | Caller callback/handler panic tests driving a real stream |
| Independent cache initialization and filesystem error identities | `cache_test.go`, `fs_test.go`, `disk_usage_test.go`, `full_linux_test.go` | Real copies and Linux device/filled-volume cases |
| JSON round trips, report keys, native path bytes and persisted input validation | `report_test.go`, `report_paths_test.go`, `cmd/acp/report_test.go`, `cmd/acp-rewrite/load_test.go`, rewrite path tests | Command report, exit-status and invalid-input tests |
| Rewrite selection, hardlinks, pending work, saved history, persistence failure and resume | `cmd/acp-rewrite/main_test.go`, `cmd/acp-rewrite/repair_test.go` | `cmd/acp-rewrite/e2e_test.go` |
| Mapped reader lifetime, EOF, bounds and platform advice | `mmap/mmap_test.go`, `mmap/mmap_linux_test.go` | Buffered/mapped copies and native platform runs |

Exercise preallocation, write, metadata, sync, close and rename failures at their respective
boundaries. Include both ordinary files and direct device output, plus the linear-writer path.
Verify source replacement through rewrite as well as source-alias refusal through ordinary copy.
Cover competing and independent targets, including exclusive-rename fallback. Check final bytes
and reported outcomes, not just intermediate object state.

Root command E2E tests build `cmd/acp`; `library_e2e_test.go` invokes the library directly.
Rewrite process tests live beside that command. These E2E tests skip under `-short`.
`go test -race` instruments the test process and library calls; E2E commands built with ordinary
`go build` are not race-instrumented. Reader cleanup tests call `Close` explicitly and never
assert finalizer timing.

## Focused checks

```sh
# Public library and command boundaries
go test -run '^TestACP(Command|Library)' -v .
go test -run '^TestRewriteCommand' -v ./cmd/acp-rewrite

# Cache concurrency, hash policies and reader/resource boundaries
go test -race -run '^(TestCache|Test.*Signature|Test.*HashPolicy)' .
go test -race ./internal/fileio ./mmap

# Command unit and persistence boundaries
go test -short ./cmd/acp-rewrite
go test ./cmd/acp
```

Select other tests by their current names with `go test -run '^TestName$'` in the owning package.
Run the full required gates after focused checks; a command matching no tests is not evidence.

## Performance comparisons

Use an explicitly accepted stable baseline and a reviewed common harness with unchanged fixtures
and identical runtime settings. Record source identities, Go version, OS/architecture, CPU,
filesystem, source sizes/counts, target count, read/hash mode and raw output outside maintained
documentation. Measure baseline and candidate serially, with other builds, tests and agents
stopped on the measurement host; report cache limits.

```sh
GOMAXPROCS=4 make bench
```

Run one ordinary Go benchmark invocation per source with `-benchtime=1s -count=1 -benchmem`
across the eight existing workloads. Go calibrates iterations; do not force equal iteration
counts or repeated whole-suite samples.
Compare time, bytes/op and allocations/op, recording all raw values and percentage deltas.

For rewrite persistence changes, also run `GOMAXPROCS=4 make bench-rewrite` once per source.
Its 100-entry and 1,000-entry fixtures measure one state/report checkpoint pair using the same
harness on both sources. Full snapshots still serialize the whole queue and accumulated report;
the benchmark does not claim constant-cost checkpoints. Run these timings separately from builds
and the main copy benchmarks.

An increase above 10% retains `flagged: true` and requires review; valid comparisons and release
checks still complete. A flag alone is not a regression verdict. Exactly 10% is unflagged;
a positive value against a zero baseline is flagged with no percentage, while two zero values
have a zero delta. Investigate anomalies and manually retest only affected cases when needed.
Fix confirmed regressions before acceptance. Never replace the baseline or repeat measurements
automatically, require confidence/waiver machinery, or claim an unrun improvement.

For each proposed optimization, identify the bottleneck and target metric before changing code.
Isolate changes in the comparison when practical, preserve the supported behavior, and keep the
original results. Revert an optimization and its supporting machinery when it has no demonstrated
benefit or makes the relevant workload worse; do not accumulate ineffective alternatives.

Release collection and evidence validation use `make performance-collect` and
`make performance-check` below. Ordinary `make check` and `make bench` require no release inputs.

## Platform and environment limits

Tests requiring xattrs, symlinks or permission refusal must probe host support and report skips.
Once an xattr probe succeeds, missing expected publication is a failure; retain separate coverage
for unsupported attributes. See [platform behavior](README.md#platform-behavior) for the contract.

Run Linux device and filled-volume tests on Linux. The filled-volume test uses disposable tmpfs
and skips if mounting is refused. Use actual device/allocation errors to verify error mapping and
owned-output cleanup. Check the linear space estimate at the write-entry boundary.
Linux invalid-UTF-8 filename tests cover the native filesystem boundary; portable state/report
tests cover JSON validation. Record which of these host-dependent tests ran.

Cross-builds compile production packages, not foreign-platform test files. When platform-specific
tests change and their runtime is unavailable, compile them through the maintained entrypoint:

```sh
make cross-tests
```

It compiles all test packages for the four production build targets and removes its temporary
binaries automatically. It does not execute those foreign-platform binaries.

Compilation does not verify Linux access-time flags, allocation/advice calls, Windows mapping
and replacement behavior, FreeBSD attributes, or LTFS/linear-media runtime behavior. Report
unrun native checks explicitly; cross-builds do not establish rename atomicity or power-loss safety.

`make check` vets the host platform only. Windows vet reports the `unsafe.Slice` conversion of
`syscall.MapViewOfFile`'s returned address in `mmap/mmap_windows.go`; that known diagnostic is
accepted instead of replacing it with deprecated `reflect.SliceHeader` manipulation.

## Release SOP

Run this procedure for each new release. Published commits, tags and versions stay intact;
later local improvements belong to a subsequent release.

1. **Review behavior and consumers.** Compare the final source with the public baseline. Review
   the [library API and terminology](README.md#library-api), CLI/rewrite examples and fixtures,
   documentation, and the [test boundaries](#behavior-and-test-boundaries). Keep useful examples
   representative of current behavior. Inspect packages, helpers, tests and temporary files for
   actual consumers before removing dead code; an absent runtime import alone does not prove
   that test or platform code is unused. Resolve unintended capability removals.
2. **Audit public content and history.** Review final tracked source, dependency metadata,
   documentation, licenses and fixtures for credentials, private services, personal paths and
   real user data. Use [Gitleaks 8.30.1](https://github.com/gitleaks/gitleaks/releases/tag/v8.30.1)
   with [rules from the same tag](https://github.com/gitleaks/gitleaks/blob/v8.30.1/config/gitleaks.toml).
   Verify the tool and rule-file checksums against fixed expected values before use; pin and
   checksum-verify any project overrides too. Do not use an unpinned latest version or rules.
   Use a complete Git history checkout for scanning plus manual review, including all history
   reachable from the proposed release and unpublished intermediate commits and messages before
   squashing. Record the scanner/rules and inspected ranges; report coverage gaps. Keep raw scanner
   reports private and share only redacted summaries. A finding in already published history needs
   a separate remediation decision; this procedure does not authorize rewriting that history.

   [.gitleaksignore](.gitleaksignore) records explicitly approved **HISTORY ONLY** exceptions as
   exact `commit:file:rule:line` fingerprints. Apply these exceptions only to Git history scans.
   Disable loading this file for current-source and artifact scans; these exceptions never apply
   to either. An entry exempts only its complete fingerprint, not a whole commit, path or rule.
   Wildcard commit, path or rule exemptions are forbidden. New findings require review and explicit
   approval before adding any exception.
3. **Prepare clean exact inputs.** Recheck `origin/main` against the current public tip. Before
   squashing, preserve unpublished commits and working changes in a recoverable local backup.
   Squash only the reviewed unpublished range into a release commit whose parent is that rechecked
   tip; do not squash when there is no unpublished range, and never publish backup refs. If the tip
   moves, reconcile and review again. Freeze the version/tag and exact commit with a clean working
   tree; confirm `go.mod` retains the intended module identity. Use an isolated checkout of that
   commit with the workspace isolation above so ignored local files cannot become release inputs.
4. **Run gates and finish review.** Run `make release-check` with the explicit inputs below. It runs
   `make check` and `make race` on an isolated exact source snapshot, including the
   command/library integration tests without `-short`. Follow the
   [platform and environment limits](#platform-and-environment-limits); cross-builds are not foreign
   runtime acceptance, and host-dependent skips remain explicit. Review and fix until no unresolved,
   unaccepted finding remains. A source fix requires a new clean commit and verification of the
   final source; record exact passes, failures and unrun gates rather than inheriting old results.
5. **Approve publication.** Review the final diff, commit message, tag target and Release text.
   Inspect and scan any distributed binaries or archives for contents, licenses and private-path leakage,
   and record their checksums. Include shipped generated code and dependency sources in content
   checks; do not blanket-exclude them. Obtain explicit approval for the exact source/version and any
   artifacts before pushing, tagging or creating a Release. Publish only that approved new release;
   do not rebuild or relabel an already shipped version. Verify the public tag and module source
   resolve to the accepted commit, and any uploaded assets match the accepted checksums.
6. **Retain evidence and clean owned resources.** Report actual results and accepted limitations.
   Keep verification output outside maintained documentation and sensitive reports private.
   Remove only temporary resources owned by the release run; retain the local backup until delivery
   is confirmed.

### Automated release gates

[dev/release.py](dev/release.py) owns the local release entrypoints. It uses Python 3.9 or newer,
Git, Make and the exact Go patch version declared by the reviewed harness. It has no sibling
repository or third-party Python dependency. Invalid evidence and failed checks fail closed;
performance flags require review. No command changes branches,
squashes, pushes, tags or publishes. A release candidate must already be one commit with the
rechecked public `origin/main` tip as its sole parent. Preparing that commit and the recoverable
local backup remains an explicitly authorized operator action. No unpublished work means there
is no new release candidate to prepare.

Supply the official Gitleaks **8.30.1 archive**, rather than an arbitrary executable, and its
default rule file through `GITLEAKS_ARCHIVE` and `GITLEAKS_RULES`. Download them from the
[tagged release](https://github.com/gitleaks/gitleaks/releases/tag/v8.30.1) and
[tagged configuration](https://raw.githubusercontent.com/gitleaks/gitleaks/v8.30.1/config/gitleaks.toml).
The tool checks hardcoded SHA256 values for Linux x64, Darwin arm64 or Darwin x64, extracts its
own scanner privately, and checks its version. It also checks the pinned default and
[ACP rule](dev/content-rules.toml) checksums before use. Updating these pins requires review.
Default global exclusions for vendor and generated content are removed. Source and artifact
scans use an empty ignore file and disable inline `gitleaks:allow`. Only Git history scans use
the exact published-ancestor fingerprints in `.gitleaksignore`; commit messages have no exceptions.
Printable binary strings, including UTF-16 paths, are checked separately.

Before squashing, audit the complete retained history and final source:

```sh
make release-history RELEASE_ARGS="--tip $HISTORY_TIP --public-base $PUBLIC_BASE --out $HISTORY_OUTPUT"
```

All source arguments are full commit SHAs. The history command scans every commit reachable from
the supplied tip, including intermediate changes and commit messages. Preserve a local backup
branch at that tip before the approved squash. Final acceptance rescans both the retained history
and the release commit; their trees must match exactly. Shallow checkouts, links, submodules and
Git archive export omissions are rejected. Ignored workstation files never become source inputs.

For performance, explicitly choose a distinct accepted baseline commit and reviewed common harness
commit. Neither is inferred from a branch or automatically accepted. After all other builds,
tests and agents have stopped on the measurement host, run:

```sh
make performance-collect PERF_ARGS="--baseline $PERF_BASELINE --candidate $RELEASE_COMMIT --harness $PERF_HARNESS --out $PERF_OUTPUT --environment $PERF_ENVIRONMENT --idle"
make performance-check PERF_ARGS="--baseline $PERF_BASELINE --candidate $RELEASE_COMMIT --harness $PERF_HARNESS --evidence $PERF_OUTPUT"
```

Use a short environment label without spaces in this Make example; invoke the Python command
directly with normal quoted arguments for longer labels. Record the storage and cache conditions
in that private label. The collector copies the existing `bench_test.go`, `workload_bench_test.go`
and the `runStream` helper from the pinned harness to both exact source snapshots. Other root
tests are removed from those temporary benchmark builds. The collector measures the baseline,
then the candidate, on the same temporary filesystem under the [performance comparison policy](#performance-comparisons).
Fixture setup and cleanup add to elapsed time beyond the benchmark duration.

The defaults are four CPUs, CGO disabled and Go's normal GC. `--cpu` accepts a positive CPU count;
Go's unsuffixed benchmark names for `--cpu 1` are accepted. The read fixtures contain exactly
64 MiB and the refresh fixtures exactly 4 KiB; fixture creation verifies those sizes before
measurement. Benchmark loggers discard routine messages so console I/O does not affect timing
or split Go's result rows; operation errors still fail the benchmark. The OS cache is uncontrolled,
so results describe warm-cache behavior. Filesystem
identity, the exact command, toolchain/runtime settings, source trees, harness bytes and raw-output
checksums are recorded. High initial host load refuses collection before source preparation or
compilation. Each invocation records host load, which includes the collector's own work and cannot
identify competing work by itself. `--idle` confirms that other builds, tests and agents have
stopped. Keep that window reserved until collection ends.

Changes limited to `AGENTS.md`, `README.md`, `TESTING.md`, `LICENSE`, `mmap/LICENSE`,
`dev/release.py` and `dev/test_release.py` do not
require measuring unchanged Go inputs again. The checker compares the committed trees, keeps
the original measured candidate in its report, and verifies the collector against the current
tool or the measured commit. Commands, harness bytes, environment and output checks still apply.
Any other changed file requires matching measurements; this exception never changes the baseline.

Each source log must contain one completed row for every workload, with `ns/op`, `B/op` and
`allocs/op`, followed by Go's successful `PASS` and package `ok` summary. Missing or duplicate rows,
missing metrics, failed or unfinished commands, changed identities and modified output fail
validation. The comparison saves `comparison.json` and prints all 24 raw metric pairs, percentage
deltas and review flags. Apply the [same review policy](#performance-comparisons) locally and in
CI, retaining the original report and any investigation output outside maintained documentation.

For final acceptance, set `RELEASE_INPUTS` to a private JSON file with these fields:

```json
{
  "version": "v0.2.3",
  "candidate": "<full release commit SHA>",
  "public_base": "<full reviewed origin/main SHA>",
  "history_tip": "<full retained pre-squash tip SHA>",
  "backup_ref": "refs/heads/<local unpublished backup>",
  "baseline": "<full explicitly accepted performance baseline SHA>",
  "harness": "<full reviewed common harness SHA>",
  "performance": "/tmp/acp-private-pair",
  "artifacts": []
}
```

`backup_ref` is required when `history_tip` differs from `candidate`. `artifacts: []` explicitly
declares a Go-module-only release; it does not accept any binary assets. An asset entry must give
`path`, `sha256`, `kind` (`source` or `commands`), `root` (archive prefix, or empty), and `members`
(the **complete** mapping of ordinary member paths to SHA256 values). Only tar.gz, tgz and zip
archives are accepted. Links, unsafe/duplicate paths, undeclared members and nested archives fail.
A source archive must equal the whole candidate tree. Every archive must contain the candidate's
exact README and LICENSE. A command archive additionally declares `goos` and `goarch`, contains
both commands built with `-trimpath -buildvcs=true` from the clean candidate checkout, and has
`licenses` mapping each shipped `module@version` to a nonempty list of its reviewed license member
paths. Go build metadata must identify the exact clean candidate and declared supported platform.
Member hashes bind the human package/license review to the bytes actually scanned. Cross-platform
metadata and source tests do not replace the foreign-runtime acceptance described above.

```sh
RELEASE_INPUTS=/tmp/acp-private-inputs.json RELEASE_OUTPUT=output/release-acceptance \
  make release-check
```

This command never runs benchmarks. It requires completed matching paired evidence, checks the
clean candidate and public tip, scans source/history/messages, runs `make check` and `make race`,
then verifies and scans the exact declared archives and binary strings. It rechecks source and
the remote public tip before recording acceptance. A source fix requires a new candidate;
changed benchmark inputs require new matching evidence. Missing scanner inputs, baseline, harness, evidence or artifact inventories
cannot produce acceptance. `acceptance.json` includes the full performance comparison, including
every raw value, delta and flag. When flags are present, the final console message requires review
and fixes for confirmed regressions before acceptance. Completing these checks does not resolve
that review or grant publication approval.

Output directories must be new and either outside the repository or under ignored `output/`.
They are private (0700, with 0600 evidence); scanner reports are redacted, remain temporary and
are removed with owned scratch directories. Console errors withhold scanner matches and private
command diagnostics; saved command logs redact workstation home and private temporary paths.
Keep evidence and logs private, never upload them as public CI artifacts.
`make test-release-tools` runs synthetic comparison and isolated-Git regressions without timings;
real scanner tests additionally run when its two pinned input variables are supplied.

Human behavior/consumer and license review, documented platform limitations, backup/squash
authorization, and explicit approval of the exact source/version/assets still apply. A successful
gate records `publication_approved: false`; it is never permission to publish.

## Change checklist

1. Run `make check` and the required race suite outside the execution sandbox.
2. Inspect the final diff and confirm that removals retain the behavior at the appropriate boundary.
3. Review documentation for one contract owner, current behavior and valid links.
4. Report exact failed/unrun checks and environment-dependent skips.
