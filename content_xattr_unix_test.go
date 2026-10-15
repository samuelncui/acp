//go:build darwin || linux

package acp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestContentNativeXattrsRoundTrip(t *testing.T) {
	// Native capture passes ordinary empty/binary attributes once and excludes cache namespaces.
	root := t.TempDir()
	source := writeSourceFile(t, root, "source", []byte("abc"))
	attrs := map[string][]byte{"user.empty": {}, "user.binary": {0, 10, 255}, "user.raw=key": {1}}
	for name, value := range attrs {
		if err := unix.Setxattr(source, name, value, 0); err != nil {
			if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EPERM) {
				t.Skipf("ordinary xattrs unavailable: %v", err)
			}
			t.Fatal(err)
		}
	}
	if err := unix.Setxattr(source, signatureXattrName, []byte("stale cache"), 0); err != nil {
		t.Fatal(err)
	}
	var captured ContentMetadata
	archive := &testContentItem{
		SimpleJob: &SimpleJob{Path: source, Dsts: []string{"member"}},
		targets: []ContentTarget{testContentTarget(func(
			_ context.Context, metadata ContentMetadata,
		) (ContentWriter, error) {
			captured = metadata
			return &testContentWriter{Writer: io.Discard, finish: func(error) error { return nil }}, nil
		})},
	}
	callback, _ := collectResults()
	if err := runStream(
		context.Background(),
		callback,
		[]Item{archive},
		SetToDevice(LinearDevice(true)),
		WithHashPolicy(HashRead),
	); err != nil {
		t.Fatal(err)
	}
	if _, ok := captured.Xattrs[signatureXattrName]; ok {
		t.Fatal("managed cache leaked into content metadata")
	}
	for name, value := range attrs {
		if !bytes.Equal(captured.Xattrs[name], value) {
			t.Fatalf("captured %q=%v", name, captured.Xattrs[name])
		}
	}

	// Restored native outputs apply attributes and regenerate their cache from actual bytes.
	captured.Xattrs["acp.signature"] = []byte("foreign cache")
	captured.Xattrs["user.acp.signature"] = []byte("foreign cache")
	destination := filepath.Join(root, "restored")
	restore := &testContentItem{
		SimpleJob: &SimpleJob{Path: "member", Dsts: []string{destination}},
		source: testContentSource(func(context.Context) (fs.File, error) {
			return &testContentFile{ReadCloser: io.NopCloser(strings.NewReader("abc")), metadata: captured}, nil
		}),
	}
	callback, batches := collectResults()
	if err := runStream(
		context.Background(),
		callback,
		[]Item{restore},
		SetFromDevice(LinearDevice(true)),
		WithHashPolicy(HashReadRefresh),
	); err != nil {
		t.Fatal(err)
	}
	if result := batches()[0][0]; result.Err != nil || result.Targets[0].Err != nil {
		t.Fatalf("result=%#v", result)
	}
	for name, value := range attrs {
		data := make([]byte, 10)
		n, err := unix.Getxattr(destination, name, data)
		if err != nil || !bytes.Equal(data[:n], value) {
			t.Fatalf("restored %q=%v, err=%v", name, data[:n], err)
		}
	}
	file, err := os.Open(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	data, err := readSignatureXattr(file)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeCachedSignature(data); err != nil {
		t.Fatalf("destination cache not regenerated: %v", err)
	}
}

func TestContentNativeXattrLimitsPrecedeBufferAllocation(t *testing.T) {
	for _, stage := range []string{"name list", "name", "value", "count", "aggregate"} {
		t.Run(stage, func(t *testing.T) {
			// Intercept the syscall's size probes to prove rejection before the offending allocation.
			previousList, previousRead := listOrdinaryXattrs, readOrdinaryXattr
			t.Cleanup(func() { listOrdinaryXattrs, readOrdinaryXattr = previousList, previousRead })
			var names []byte
			count, size := 1, 0
			if stage == "count" {
				count = ContentMaxXattrCount + 1
			}
			if stage == "aggregate" {
				count, size = 4, ContentMaxXattrValueBytes
			}
			if stage == "value" {
				size = ContentMaxXattrValueBytes + 1
			}
			for i := 0; i < count; i++ {
				names = append(names, []byte("user."+strings.Repeat("a", i+1))...)
				names = append(names, 0)
			}
			if stage == "name" {
				names = append([]byte(strings.Repeat("n", ContentMaxXattrNameBytes+1)), 0)
			}
			listCalls, valuesAllocated := 0, 0
			listOrdinaryXattrs = func(_ string, data []byte) (int, error) {
				listCalls++
				if stage == "name list" {
					if data != nil {
						t.Error("oversized list buffer allocated")
					}
					return ContentMaxXattrCount*(ContentMaxXattrNameBytes+1) + 1, nil
				}
				if data != nil {
					copy(data, names)
				}
				return len(names), nil
			}
			readOrdinaryXattr = func(_, _ string, data []byte) (int, error) {
				if data != nil {
					valuesAllocated++
					clear(data)
				}
				return size, nil
			}

			// Each boundary fails explicitly; earlier valid values remain bounded.
			if _, err := readXattrsWithContent("unused", true); err == nil {
				t.Fatal("oversized metadata accepted")
			}
			if stage == "name list" && listCalls != 1 {
				t.Fatalf("list calls=%d", listCalls)
			}
			wantAllocations := 0
			if stage == "aggregate" {
				wantAllocations = 3
			}
			if valuesAllocated != wantAllocations {
				t.Fatalf("value allocations=%d, want=%d", valuesAllocated, wantAllocations)
			}
		})
	}
}
