package acp

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"os"
	"strings"
	"testing"
)

// encodeCachedSignature returns the legacy fixed-size layout that earlier releases wrote, so tests
// can seed an entry written by an older ACP.
func encodeCachedSignature(signature CachedSignature) []byte {
	encoded := make([]byte, signatureLegacySize)
	copy(encoded[:4], signatureMagic)
	encoded[4] = signatureVersion
	encoded[5] = signatureSHA256
	binary.BigEndian.PutUint64(encoded[8:16], uint64(signature.Size))
	binary.BigEndian.PutUint64(encoded[16:24], uint64(signature.MtimeNS))
	copy(encoded[24:], signature.SHA256[:])
	return encoded
}

// newCachedSignature binds a hash to the size and mtime of indexed facts.
func newCachedSignature(hash []byte, indexed *stat) (CachedSignature, error) {
	signature := CachedSignature{Size: indexed.size, MtimeNS: indexed.modTime.UnixNano()}
	if len(hash) != len(signature.SHA256) {
		return CachedSignature{}, errors.New("invalid SHA-256 size")
	}
	copy(signature.SHA256[:], hash)
	return signature, nil
}

func sha(text string) []byte {
	sum := sha256.Sum256([]byte(text))
	return sum[:]
}

func TestSignatureEntryCodec(t *testing.T) {
	// Round-trip every field, including the decoded description, through the Protobuf form.
	want := SignatureEntry{
		Size: 42, MtimeNS: -123456789, SHA256: sha("stored"),
		Decoded: &DecodedSignature{Encoding: "zstd", Size: 1 << 40, SHA256: sha("plain")},
	}
	encoded, err := encodeSignatureEntry(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeSignatureEntry(encoded)
	if err != nil || !got.equal(want) {
		t.Fatalf("decoded entry = %#v / %v, want %#v", got, err, want)
	}
	if len(encoded) > signatureMaxSize/2 {
		t.Fatalf("entry size = %d, the bound %d should fit it comfortably", len(encoded), signatureMaxSize)
	}

	// Absent hashes and an absent decoded description stay absent.
	bare, err := encodeSignatureEntry(SignatureEntry{Size: 1, MtimeNS: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := DecodeSignatureEntry(bare); err != nil || got.SHA256 != nil || got.Decoded != nil {
		t.Fatalf("bare entry = %#v / %v", got, err)
	}

	// Older readers decode only the legacy layout: a new entry never starts with its magic and is
	// not a valid 56-byte v1 entry, so they miss instead of misreading it.
	for _, entry := range []SignatureEntry{want, {Size: 1, MtimeNS: 2, SHA256: sha("x")}, {Size: 0, MtimeNS: 0, SHA256: sha("y")}} {
		value, err := encodeSignatureEntry(entry)
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(string(value), signatureMagic) {
			t.Fatalf("a Protobuf entry begins with the legacy magic: %x", value)
		}
		if _, err := decodeLegacySignature(value); err == nil {
			t.Fatalf("the legacy decoder accepted a Protobuf entry: %x", value)
		}
	}
}

func TestSignatureEntryRejectsInvalid(t *testing.T) {
	marshal := func(entry SignatureEntry) []byte {
		value, err := encodeSignatureEntry(entry)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	valid := SignatureEntry{Size: 1, MtimeNS: 1, SHA256: sha("a")}
	tests := map[string][]byte{
		"empty":                 nil,
		"garbage":               []byte("corrupt"),
		"short stored hash":     marshal(SignatureEntry{Size: 1, SHA256: []byte{1, 2}}),
		"negative size":         marshal(SignatureEntry{Size: -1}),
		"no encoding":           marshal(SignatureEntry{Size: 1, Decoded: &DecodedSignature{Size: 1}}),
		"negative decoded size": marshal(SignatureEntry{Size: 1, Decoded: &DecodedSignature{Encoding: "zstd", Size: -1}}),
		"short decoded hash":    marshal(SignatureEntry{Size: 1, Decoded: &DecodedSignature{Encoding: "zstd", SHA256: []byte{1}}}),
		"oversized":             append(marshal(valid), make([]byte, signatureMaxSize)...),
		"legacy size":           append(encodeCachedSignature(CachedSignature{}), 0),
	}
	for name, value := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeSignatureEntry(value); err == nil {
				t.Fatalf("DecodeSignatureEntry(%x) error = nil", value)
			}
			if _, err := DecodeCachedSignature(value); err == nil {
				t.Fatalf("DecodeCachedSignature(%x) error = nil", value)
			}
		})
	}

	// A decoded-only entry is valid, but has no plain signature.
	decodedOnly := marshal(SignatureEntry{Size: 1, Decoded: &DecodedSignature{Encoding: "zstd", Size: 5}})
	if _, err := DecodeSignatureEntry(decodedOnly); err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeCachedSignature(decodedOnly); err == nil {
		t.Fatal("DecodeCachedSignature accepted an entry without the stored hash")
	}
}

func TestReadSignatureEntryStatuses(t *testing.T) {
	requireSignatureXattrSupport(t)
	path := writeSourceFile(t, t.TempDir(), "file.bin", []byte("content"))
	if _, status, err := ReadSignatureEntry(path); err != nil || status != SignatureMissing {
		t.Fatalf("status = %v / %v, want missing", status, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	entry := SignatureEntry{
		Size: info.Size(), MtimeNS: info.ModTime().UnixNano(),
		Decoded: &DecodedSignature{Encoding: "zstd", Size: 99, SHA256: sha("plain")},
	}
	seedEntry(t, path, entry)

	// A fresh entry that only describes the decoded content is not a plain cache hit.
	got, status, err := ReadSignatureEntry(path)
	if err != nil || status != SignatureFresh || !got.equal(entry) {
		t.Fatalf("read = %#v %v / %v", got, status, err)
	}
	if _, hit, err := ReadCachedSignature(path); err != nil || hit {
		t.Fatalf("ReadCachedSignature hit = %v / %v for a decoded-only entry", hit, err)
	}

	// A stale entry is still returned, so the marker survives an mtime change.
	entry.MtimeNS++
	seedEntry(t, path, entry)
	got, status, err = ReadSignatureEntry(path)
	if err != nil || status != SignatureStale || got.Decoded == nil || got.Decoded.Encoding != "zstd" {
		t.Fatalf("stale read = %#v %v / %v", got, status, err)
	}

	// Both hashes make the plain view hit.
	entry.MtimeNS--
	entry.SHA256 = sha("stored")
	seedEntry(t, path, entry)
	signature, hit, err := ReadCachedSignature(path)
	if err != nil || !hit || string(signature.SHA256[:]) != string(entry.SHA256) {
		t.Fatalf("ReadCachedSignature = %#v %v / %v", signature, hit, err)
	}
}

func TestOversizedSignatureXattrIsInvalid(t *testing.T) {
	requireSignatureXattrSupport(t)
	path := writeSourceFile(t, t.TempDir(), "file.bin", []byte("content"))
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := writeSignatureXattr(file, make([]byte, 4*signatureMaxSize)); err != nil {
		t.Skipf("filesystem does not store a large attribute: %v", err)
	}
	if entry, status, err := ReadSignatureEntry(path); err == nil || status != SignatureMissing || entry.Decoded != nil {
		t.Fatalf("oversized entry = %#v %v / %v, want an error", entry, status, err)
	}
}

// seedEntry stores an entry exactly as given on a file.
func seedEntry(t *testing.T, path string, entry SignatureEntry) {
	t.Helper()
	encoded, err := encodeSignatureEntry(entry)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := writeSignatureXattr(file, encoded); err != nil {
		t.Fatal(err)
	}
}
