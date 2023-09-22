# acp
An Advanced Copy Tools, with following extra features:
- Process bar
- Sorted copy order, to improve tape device read performance
- Multi target path, read once write many
- Read file with mmap, with small file prefetch hint
- JSON format job report
- Can use as a golang library

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
