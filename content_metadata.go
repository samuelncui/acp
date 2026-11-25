package acp

import (
	"fmt"
	"io/fs"
	"time"

	"github.com/samuelncui/acp/signature"
)

// Content endpoint metadata limits also bound native capture before allocating xattr buffers.
// The aggregate includes a conservative 64-byte framing allowance per attribute.
const (
	contentMaxXattrCount      = 256
	contentMaxXattrNameBytes  = 1 << 10
	contentMaxXattrValueBytes = 1 << 20
	contentMaxMetadataBytes   = 4 << 20
)

type contentInfo struct {
	name    string
	size    int64
	mode    fs.FileMode
	modTime time.Time
}

func (i contentInfo) Name() string       { return i.name }
func (i contentInfo) Size() int64        { return i.size }
func (i contentInfo) Mode() fs.FileMode  { return i.mode }
func (i contentInfo) ModTime() time.Time { return i.modTime }
func (i contentInfo) IsDir() bool        { return i.mode.IsDir() }
func (i contentInfo) Sys() any           { return nil }

func checkContentXattr(name string, size, count, total int) (int, error) {
	// Attribute representation belongs to the source or target codec; ACP bounds snapshot size.
	if len(name) > contentMaxXattrNameBytes {
		return total, fmt.Errorf("content xattr name exceeds metadata limit, name=%q", name)
	}
	if size < 0 || size > contentMaxXattrValueBytes || count > contentMaxXattrCount {
		return total, fmt.Errorf("content xattr exceeds metadata limits, name=%q, size=%d, count=%d", name, size, count)
	}
	total += len(name) + size + 64
	if total > contentMaxMetadataBytes {
		return total, fmt.Errorf("content xattrs exceed aggregate metadata limit")
	}
	return total, nil
}

func snapshotContentMetadata(metadata ContentMetadata) (ContentMetadata, error) {
	// Record portable facts once; endpoint Sys never supplies native ownership metadata.
	if metadata.Info == nil {
		return ContentMetadata{}, fmt.Errorf("content source metadata has no file info")
	}
	info := contentInfo{
		name: metadata.Info.Name(), size: metadata.Info.Size(),
		mode: metadata.Info.Mode(), modTime: metadata.Info.ModTime(),
	}
	if !info.mode.IsRegular() || info.size < 0 {
		return ContentMetadata{}, fmt.Errorf("content source is not a regular file with a nonnegative size")
	}

	// Validate each allocation before copying and exclude both managed cache namespaces.
	result := ContentMetadata{Info: info}
	count, total := 0, 0
	for name, value := range metadata.Xattrs {
		if signature.IsAttribute(name) {
			continue
		}
		count++
		var err error
		total, err = checkContentXattr(name, len(value), count, total)
		if err != nil {
			return ContentMetadata{}, err
		}
		if result.Xattrs == nil {
			result.Xattrs = make(map[string][]byte)
		}
		result.Xattrs[name] = append([]byte{}, value...)
	}
	return result, nil
}
