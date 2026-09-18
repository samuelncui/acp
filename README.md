# acp
An Advanced Copy Tools, with following extra features:
- Process bar
- Sorted copy order, to improve tape device read performance
- Concurrent source preparation with request-ordered writes for linear targets
- Multi target path, read once write many
- Read file with mmap, with small file prefetch hint
- JSON format job report
- Optional SHA-256 xattr cache for hash-only streams
- Can use as a golang library

## Content signature cache

Library callers can opt in with `WithSignatureCache(true)` and bypass reads with `ForceRehash(true)`. ACP stores a fixed binary SHA-256, file size, and nanosecond mtime value in `user.acp.signature` on Linux and the canonical `acp.signature` user attribute on Darwin and FreeBSD.

The xattr is a disposable optimization. Hash-only jobs may reuse a metadata-valid value. Jobs with targets always read and hash content, then refresh the source and every successful target before `WaitErr` or `RunStream` returns. Missing, stale, corrupt, read-only, full, or unsupported xattrs are summarized as warnings and never become copy errors. ACP's managed key is not copied as an ordinary source xattr.

# Install
```
# Install acp
go install github.com/samuelncui/acp/cmd/acp
```

# Usage

```
Usage of acp:
  -n    do not overwrite exist file
  -notarget
        do not have target, use as dir index tool
  -report string
        json report storage path
  -target value
        use target flag to give multi target path
```

## Example

```
# copy `example` dir to `target` dir
acp example target/

# copy `example` dir to `target` dir, and output a report to `report.json`
acp -report report.json example target/

# copy `example` dir to `target1` and `target2` dir
acp example -target target1 -target target2

# do not copy, just get a dir index, write to `report.json`
acp example -notarget -report report.json
```

## Testing

See [TESTING.md](TESTING.md) for unit, race, end-to-end, and cross-platform test instructions.
