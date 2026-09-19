# Open work

## Review `cmd/acp-rewrite` on its own terms

The tool exists for a real operational need: after a ZFS pool gains vdevs, existing files keep
their old block layout, so rewriting them spreads the data across the new topology. The v0.2.1
API reshape moved it onto the new surface as-is; it has never been reviewed on its own terms.

To settle: whether it stays a separate binary or becomes a subcommand of `acp`; its flag set
(`-dryrun`, `-state`, `-report`, `-report-indent`, `-ignore`); the resumable state and report
formats, now that a report must round-trip through `encoding/json` as a public document;
hardlink-group handling and duplicate reporting; the rewrite order it uses for a linear or
object-backed target; and whether the existing suite — `rewriteFile`, the state and report round
trips, temporary-file cleanup and the resume run are covered — needs a real-device end-to-end
case. It also needs a README section of its own: today `README.md` documents its report behaviour
and `AGENTS.md` describes what it is for.

## Platform verification still owed

Linux-specific runtime paths run on the isolated Linux acceptance host (`go test` for the
Linux-only file, including the `/dev/full` and filled-tmpfs `ENOSPC` cases, `O_NOATIME` and its
permission fallback, `fallocate`, `madvise`). Windows and FreeBSD stay compile-verified only until
a runtime is available (`mmap_other.go`, `signature_xattr_freebsd.go`, Windows path ordering).
