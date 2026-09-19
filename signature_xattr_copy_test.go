//go:build darwin || linux
// +build darwin linux

package acp

import (
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"golang.org/x/sys/unix"
)

// controlXattrName is an ordinary attribute, spelled in the platform's own namespace, that the
// metadata restore must carry onto a target.
var controlXattrName = func() string {
	if runtime.GOOS == "darwin" {
		return "acp.copy-control"
	}
	return "user.acp.copy-control"
}()

// TestManagedSignatureIsNotOrdinaryCopiedXattr pins the metadata rule on the copy path itself: a
// transfer restores the source's ordinary xattrs onto the target, but the managed signature cache
// key is not one of them. The source carries a decoy entry no run writes for this content, so a
// target that ends up with it would prove the key travelled as an ordinary attribute, and the
// control attribute proves the restore ran at all instead of never happening.
func TestManagedSignatureIsNotOrdinaryCopiedXattr(t *testing.T) {
	root := t.TempDir()
	content := []byte("fixture")
	source := filepath.Join(root, "source.txt")
	if err := os.WriteFile(source, content, 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "target.txt")

	// A value no run would publish for this file: any managed entry on the target is a copy.
	decoy := CachedSignature{Size: 1, MtimeNS: 2, SHA256: sha256.Sum256([]byte("decoy"))}
	file, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	err = writeSignatureXattr(file, encodeCachedSignature(decoy))
	_ = file.Close()
	if err != nil {
		t.Skipf("temporary filesystem does not support signature xattrs: %v", err)
	}
	if err := unix.Setxattr(source, controlXattrName, []byte("carried"), 0); err != nil {
		t.Skipf("temporary filesystem does not support ordinary xattrs: %v", err)
	}

	// The default policy is the sharpest case: it manages no cache at all, so the only way the
	// target could receive a managed attribute is the ordinary metadata copy.
	item := newFixtureItem(source, target)
	if err := runFixture(context.Background(), newStreamFixture(item), []Item{item}); err != nil {
		t.Fatalf("run error = %v", err)
	}
	if _, err := item.terminal(t); err != nil {
		t.Fatalf("item failed: %v", err)
	}

	// The ordinary attribute reached the target, so the metadata restore ran.
	restored, err := readXattrs(target)
	if err != nil {
		t.Fatal(err)
	}
	carried := false
	for _, xattr := range restored {
		if xattr.key == controlXattrName {
			carried = true
		}
	}
	if !carried {
		t.Fatalf("the copy did not restore the ordinary attribute %q: %v", controlXattrName, restored)
	}

	// The managed key did not travel: the target has no managed attribute at all.
	targetFile, err := os.Open(target)
	if err != nil {
		t.Fatal(err)
	}
	encoded, readErr := readSignatureXattr(targetFile)
	_ = targetFile.Close()
	if readErr == nil {
		t.Fatalf("the target carries a managed signature attribute: %x", encoded)
	}
	if !signatureXattrIgnorable(readErr) {
		t.Fatalf("read the target's managed attribute: %v", readErr)
	}

	// The source keeps its own entry: the transfer neither moved nor removed it.
	sourceFile, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	kept, readErr := readSignatureXattr(sourceFile)
	_ = sourceFile.Close()
	if readErr != nil {
		t.Fatalf("read the source's managed attribute: %v", readErr)
	}
	stored, err := DecodeCachedSignature(kept)
	if err != nil || stored != decoy {
		t.Fatalf("source managed attribute = %#v / %v, want the seeded decoy %#v", stored, err, decoy)
	}
}
