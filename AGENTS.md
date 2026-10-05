# Repository Guide for Agents

## Scope and documentation

This guide applies to the entire repository. Write source, comments, tests, errors and
documentation in English. Keep this public repository self-contained; do not introduce private
services, credentials, personal paths or dependencies on sibling repositories.

- [README.md](README.md) owns the current operating model, pipeline stages, public API and command
  behavior. Read the relevant contract before changing code.
- [TESTING.md](TESTING.md) owns verification boundaries, performance comparisons and release gates.
- This file owns durable maintenance principles. Link to an existing owner instead of repeating
  its contract. Documentation follows the same DRY and simplicity rules as code: describe current
  behavior, replace obsolete text, and keep plans and verification logs outside maintained docs.

The working tree may contain other work. Preserve unrelated changes; do not reset, rewrite or
discard them.

## Change principles

- Work within the [supported operating model](README.md#operating-model-and-stages). Do not add
  checks, retries or recovery for unsupported external changes during a copy.
- Use the least mechanism that satisfies a current requirement. Existing code or tests do not
  justify unnecessary machinery. Remove abandoned paths and their obsolete assertions together.
- Keep changes local to their owner and understandable from the direct control flow. Maintain
  one implementation and one documentation owner for each behavior.
- Give each behavior one owner and each decision one evaluation. Pass observed facts to their
  consumers instead of calling input methods, resolving paths or collecting metadata again.
- Add an abstraction only when it owns an independently testable invariant or materially removes
  current duplication. Keep the flow direct, branches shallow and ownership visible; naming a
  stage alone does not justify another wrapper or state object.
- Register resource release when acquiring files, mappings, hash objects or pooled buffers.
  Transfer ownership explicitly, release on failure and panic, and preserve cleanup errors
  alongside the primary failure. Cleanup may remove only paths the operation owns.
- Add concurrency only with explicit channel ownership, completion and error semantics. Wait for
  workers and preserve the documented result, cancellation and panic boundaries.
- Use `path/filepath` for native paths and `path` for slash-delimited logical paths. Preserve native
  filename bytes and platform ordering through `comparePath`; apply JSON path validation at the
  persistence boundary.
- Preserve platform-specific behavior in `syscall_*`, `mmap/*`, signature xattr implementations
  and rewrite file helpers. Avoid generalizing a host's behavior to other platforms.

## Compatibility and verification

Preserve the `af05f05c` exported surface, field types and report contract, except for the explicitly
approved [ordinary-target replacement semantics](README.md#target-completion). Further observable
compatibility changes require explicit developer approval. The compatibility shell delegates to
the stream; it must not acquire a second copy implementation or infer outcomes from report rows.

Use existing verification entrypoints first and maintain recurring checks with the implementation.
Follow the [test maintenance policy](TESTING.md#test-maintenance) for coverage, automation and evidence.

## Release and publication

Follow the [Release SOP](TESTING.md#release-sop) for every release request, including source/history,
consumer and content review and exact-source acceptance. Obtain explicit approval for the exact
source, version and artifacts before pushing, tagging or creating a Release. Published history
and versions remain intact; the SOP's backup and squash procedure applies only to unpublished work.
