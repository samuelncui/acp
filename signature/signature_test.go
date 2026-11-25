package signature

import (
	"crypto/sha256"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/samuelncui/acp/signature/internal/signaturepb"
)

// encodeLegacy returns the legacy fixed-size layout that earlier releases wrote, so tests
// can seed an entry written by an older ACP.
func encodeLegacy(signature Entry) []byte {
	encoded := make([]byte, legacySize)
	copy(encoded[:4], magic)
	encoded[4] = version
	encoded[5] = algorithm
	binary.BigEndian.PutUint64(encoded[8:16], uint64(signature.Size))
	binary.BigEndian.PutUint64(encoded[16:24], uint64(signature.MtimeNS))
	copy(encoded[24:], signature.SHA256)
	return encoded
}

// writeFile creates a regular file with fixed content.
func writeFile(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "file.bin")
	if err := os.WriteFile(path, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// requireSupport skips the test on a file system that cannot store the attribute.
func requireSupport(t *testing.T) {
	t.Helper()

	path := writeFile(t)
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	err = WriteFile(file, Entry{Size: 5, MtimeNS: 1})
	_ = file.Close()
	if err != nil {
		t.Skipf("temporary filesystem does not support signature xattrs: %v", err)
	}
}

func sha(text string) []byte {
	sum := sha256.Sum256([]byte(text))
	return sum[:]
}

func TestEntryCodec(t *testing.T) {
	// Round-trip every field, including the decoded description, through the Protobuf form.
	want := Entry{
		Size: 42, MtimeNS: -123456789, SHA256: sha("stored"),
		Decoded: &Decoded{Encoding: "zstd", Size: 1 << 40, SHA256: sha("plain")},
	}
	encoded, err := encode(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decode(encoded)
	if err != nil || !got.Equal(want) {
		t.Fatalf("decoded entry = %#v / %v, want %#v", got, err, want)
	}
	if len(encoded) > maxSize/2 {
		t.Fatalf("entry size = %d, the bound %d should fit it comfortably", len(encoded), maxSize)
	}

	// Absent hashes and an absent decoded description stay absent.
	bare, err := encode(Entry{Size: 1, MtimeNS: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := decode(bare); err != nil || got.SHA256 != nil || got.Decoded != nil {
		t.Fatalf("bare entry = %#v / %v", got, err)
	}

	// Older readers decode only the legacy layout: a new entry never starts with its magic and is
	// not a valid 56-byte v1 entry, so they miss instead of misreading it.
	for _, entry := range []Entry{want, {Size: 1, MtimeNS: 2, SHA256: sha("x")}, {Size: 0, MtimeNS: 0, SHA256: sha("y")}} {
		value, err := encode(entry)
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(string(value), magic) {
			t.Fatalf("a Protobuf entry begins with the legacy magic: %x", value)
		}
		if _, err := decodeLegacy(value); err == nil {
			t.Fatalf("the legacy decoder accepted a Protobuf entry: %x", value)
		}
	}
}

func TestDecodeRejectsInvalid(t *testing.T) {
	// The writers refuse these entries, so build the stored form without them.
	marshal := func(entry Entry) []byte {
		message := &signaturepb.SignatureEntry{Size: entry.Size, MtimeNs: entry.MtimeNS, Sha256: entry.SHA256}
		if entry.Decoded != nil {
			message.Decoded = &signaturepb.Decoded{Encoding: entry.Decoded.Encoding, Size: entry.Decoded.Size, Sha256: entry.Decoded.SHA256}
		}
		value, err := proto.Marshal(message)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	valid := Entry{Size: 1, MtimeNS: 1, SHA256: sha("a")}
	tests := map[string][]byte{
		"empty":                 nil,
		"garbage":               []byte("corrupt"),
		"short stored hash":     marshal(Entry{Size: 1, SHA256: []byte{1, 2}}),
		"negative size":         marshal(Entry{Size: -1}),
		"no encoding":           marshal(Entry{Size: 1, Decoded: &Decoded{Size: 1}}),
		"negative decoded size": marshal(Entry{Size: 1, Decoded: &Decoded{Encoding: "zstd", Size: -1}}),
		"short decoded hash":    marshal(Entry{Size: 1, Decoded: &Decoded{Encoding: "zstd", SHA256: []byte{1}}}),
		"oversized":             append(marshal(valid), make([]byte, maxSize)...),
		"legacy size":           append(encodeLegacy(Entry{}), 0),
	}
	for name, value := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := decode(value); err == nil {
				t.Fatalf("decode(%x) error = nil", value)
			}
		})
	}

	// A decoded-only entry is valid.
	decodedOnly := marshal(Entry{Size: 1, Decoded: &Decoded{Encoding: "zstd", Size: 5}})
	if _, err := decode(decodedOnly); err != nil {
		t.Fatal(err)
	}
}

func TestReadStatuses(t *testing.T) {
	requireSupport(t)
	path := writeFile(t)
	if _, status, err := Read(path); err != nil || status != Missing {
		t.Fatalf("status = %v / %v, want missing", status, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	entry := Entry{
		Size: info.Size(), MtimeNS: info.ModTime().UnixNano(),
		Decoded: &Decoded{Encoding: "zstd", Size: 99, SHA256: sha("plain")},
	}
	seedEntry(t, path, entry)

	// A fresh entry that only describes the decoded content is not a plain cache hit.
	got, status, err := Read(path)
	if err != nil || status != Fresh || !got.Equal(entry) {
		t.Fatalf("read = %#v %v / %v", got, status, err)
	}
	if _, hit, err := ReadCached(path); err != nil || hit {
		t.Fatalf("ReadCached hit = %v / %v for a decoded-only entry", hit, err)
	}

	// A stale entry is still returned, so the marker survives an mtime change.
	entry.MtimeNS++
	seedEntry(t, path, entry)
	got, status, err = Read(path)
	if err != nil || status != Stale || got.Decoded == nil || got.Decoded.Encoding != "zstd" {
		t.Fatalf("stale read = %#v %v / %v", got, status, err)
	}

	// Both hashes make the plain view hit.
	entry.MtimeNS--
	entry.SHA256 = sha("stored")
	seedEntry(t, path, entry)
	signature, hit, err := ReadCached(path)
	if err != nil || !hit || !signature.Equal(entry) {
		t.Fatalf("ReadCached = %#v %v / %v", signature, hit, err)
	}
}

func TestOversizedAttributeIsInvalid(t *testing.T) {
	requireSupport(t)
	path := writeFile(t)
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := writeXattr(file, make([]byte, 4*maxSize)); err != nil {
		t.Skipf("filesystem does not store a large attribute: %v", err)
	}
	if entry, status, err := Read(path); err == nil || status != Missing || entry.Decoded != nil {
		t.Fatalf("oversized entry = %#v %v / %v, want an error", entry, status, err)
	}
}

// seedEntry stores an entry exactly as given on a file.
func seedEntry(t *testing.T, path string, entry Entry) {
	t.Helper()
	encoded, err := encode(entry)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := writeXattr(file, encoded); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyCodec(t *testing.T) {
	// Round-trip every signed and fixed-width field through the stable codec.
	hash := sha256.Sum256([]byte("fixture"))
	want := Entry{Size: 42, MtimeNS: -123456789, SHA256: hash[:]}
	encoded := encodeLegacy(want)
	got, err := decode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(want) {
		t.Fatalf("decoded signature = %#v, want %#v", got, want)
	}

	// Reject each independent header invariant without accepting partial data.
	tests := []struct {
		name   string
		mutate func([]byte) []byte
	}{
		{name: "size", mutate: func(value []byte) []byte { return value[:len(value)-1] }},
		{name: "magic", mutate: func(value []byte) []byte { value[0] ^= 0xff; return value }},
		{name: "version", mutate: func(value []byte) []byte { value[4]++; return value }},
		{name: "algorithm", mutate: func(value []byte) []byte { value[5]++; return value }},
		{name: "reserved", mutate: func(value []byte) []byte { value[6] = 1; return value }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			invalid := tt.mutate(append([]byte(nil), encoded...))
			if _, err := decode(invalid); err == nil {
				t.Fatal("decode() error = nil")
			}
		})
	}
}

func TestWritersRefuseEntriesReadWouldReject(t *testing.T) {
	requireSupport(t)
	tests := map[string]Entry{
		"negative size":         {Size: -1},
		"short hash":            {Size: 1, SHA256: []byte{1, 2, 3}},
		"no encoding":           {Size: 1, Decoded: &Decoded{Size: 1}},
		"negative decoded size": {Size: 1, Decoded: &Decoded{Encoding: "zstd", Size: -1}},
		"short decoded hash":    {Size: 1, Decoded: &Decoded{Encoding: "zstd", SHA256: []byte{1}}},
		"long encoding":         {Size: 1, Decoded: &Decoded{Encoding: strings.Repeat("e", MaxEncoding+1)}},
	}
	for name, entry := range tests {
		t.Run(name, func(t *testing.T) {
			path := writeFile(t)
			if _, err := encode(entry); err == nil {
				t.Fatal("encode accepted the entry")
			}
			if err := WritePath(path, entry); err == nil {
				t.Fatal("WritePath accepted the entry")
			}
			file, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			if err := WriteFile(file, entry); err == nil {
				t.Fatal("WriteFile accepted the entry")
			}

			// A refused write leaves no entry behind.
			if _, status, err := Read(path); err != nil || status != Missing {
				t.Fatalf("Read after refused writes = %v, %v; want Missing", status, err)
			}
		})
	}

	// The longest allowed encoding name round-trips.
	path := writeFile(t)
	want := Entry{Size: 7, Decoded: &Decoded{Encoding: strings.Repeat("e", MaxEncoding), Size: 9, SHA256: sha("d")}, SHA256: sha("s")}
	if err := WritePath(path, want); err != nil {
		t.Fatal(err)
	}
	got, _, err := Read(path)
	if err != nil || !got.Equal(want) {
		t.Fatalf("Read = %#v, %v; want %#v", got, err, want)
	}
}
