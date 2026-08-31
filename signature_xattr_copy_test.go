//go:build darwin || linux
// +build darwin linux

package acp

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
)

func TestManagedSignatureIsNotOrdinaryCopiedXattr(t *testing.T) {
	// Seed ACP's managed xattr on a regular source file.
	path := filepath.Join(t.TempDir(), "source.txt")
	if err := os.WriteFile(path, []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	err = writeSignatureXattr(file, encodeCachedSignature(CachedSignature{
		Size: info.Size(), MtimeNS: info.ModTime().UnixNano(), SHA256: sha256.Sum256([]byte("fixture")),
	}))
	_ = file.Close()
	if err != nil {
		t.Skipf("temporary filesystem does not support signature xattrs: %v", err)
	}

	// The ordinary metadata reader must exclude every platform spelling of the key.
	xattrs, err := readXattrs(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, xattr := range xattrs {
		if signatureCacheKey(xattr.key) {
			t.Fatalf("managed signature key %q was returned as an ordinary xattr", xattr.key)
		}
	}
}
