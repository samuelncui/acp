package acp

import (
	"bytes"
	"strings"
	"testing"
)

func TestContentMetadataLimitsAndManagedCacheFiltering(t *testing.T) {
	// Portable snapshots retain empty and binary values without native ownership assumptions.
	metadata := ContentMetadata{Info: contentInfo{name: "a", size: 1, mode: 0600}, Xattrs: map[string][]byte{
		"user.empty": {}, "user.binary": {0, 10, 255}, "acp.signature": {1}, "user.acp.signature": {2},
	}}
	got, err := snapshotContentMetadata(metadata)
	if err != nil || len(got.Xattrs) != 2 || !bytes.Equal(got.Xattrs["user.binary"], []byte{0, 10, 255}) {
		t.Fatalf("snapshot=%#v, err=%v", got, err)
	}
	metadata.Xattrs["user.binary"][0] = 2
	if got.Xattrs["user.binary"][0] != 0 {
		t.Fatal("snapshot aliases caller-owned attributes")
	}

	// Generic snapshots preserve names that particular codecs may be unable to represent.
	for _, name := range []string{"", "raw=key", "raw\nkey", "raw\x00key", "raw\xffkey"} {
		metadata.Xattrs = map[string][]byte{name: {0, 255}}
		got, err := snapshotContentMetadata(metadata)
		if err != nil || !bytes.Equal(got.Xattrs[name], []byte{0, 255}) {
			t.Errorf("name=%q, snapshot=%v, err=%v", name, got, err)
		}
	}

	// Refuse each size boundary without truncating metadata.
	for _, name := range []string{strings.Repeat("n", ContentMaxXattrNameBytes+1)} {
		if _, err := checkContentXattr(name, 0, 1, 0); err == nil {
			t.Errorf("accepted name %q", name)
		}
	}
	for _, tc := range []struct{ size, count, total int }{
		{ContentMaxXattrValueBytes + 1, 1, 0}, {0, ContentMaxXattrCount + 1, 0}, {0, 1, ContentMaxMetadataBytes},
	} {
		if _, err := checkContentXattr("user.attr", tc.size, tc.count, tc.total); err == nil {
			t.Errorf("accepted limits=%v", tc)
		}
	}
}

func TestContentMetadataExcludesManagedValuesBeforeLimits(t *testing.T) {
	// An untrusted cache value is neither copied nor counted as ordinary metadata.
	oversized := make([]byte, ContentMaxXattrValueBytes+1)
	metadata := ContentMetadata{Info: contentInfo{name: "a", mode: 0600}, Xattrs: map[string][]byte{
		"acp.signature": oversized, "user.acp.signature": oversized,
	}}
	for i := range ContentMaxXattrCount {
		metadata.Xattrs[strings.Repeat("a", i+1)] = []byte{}
	}
	got, err := snapshotContentMetadata(metadata)
	if err != nil || len(got.Xattrs) != ContentMaxXattrCount {
		t.Fatalf("snapshot count=%d, err=%v", len(got.Xattrs), err)
	}
	for _, name := range []string{"acp.signature", "user.acp.signature"} {
		if _, ok := got.Xattrs[name]; ok {
			t.Fatalf("managed attribute %q copied", name)
		}
	}
}
