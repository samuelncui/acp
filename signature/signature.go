// Package signature owns the managed signature entry of a file: the extended attribute in which a
// copy records the SHA-256 of the file's bytes, and of their decoded form for an encoded file,
// bound to the file's size and modification time.
package signature

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"os"

	"github.com/samuelncui/acp/signature/internal/signaturepb"
	"google.golang.org/protobuf/proto"
)

const (
	// The legacy layout is exactly 56 bytes that begin with the magic; every new entry is Protobuf.
	magic      = "ACPS"
	version    = uint8(1)
	algorithm  = uint8(1)
	legacySize = 56
	// maxSize bounds a stored entry; the longest valid one is far smaller. A longer value is an
	// invalid entry.
	maxSize = 512
	// MaxEncoding bounds an encoding name so every entry fits the size bound of a stored entry.
	MaxEncoding = 64
)

// Ignorable reports whether an attribute error leaves a file in a well-defined "no entry" state
// instead of describing a broken entry. An absent attribute and a file system without the managed
// attribute namespace both mean the same thing: there is no stored entry.
func Ignorable(err error) bool {
	return errors.Is(err, errors.ErrUnsupported) || isMissing(err) || isUnsupported(err)
}

// IsAttribute reports whether an extended attribute name is the managed signature attribute, in
// either platform spelling.
func IsAttribute(name string) bool {
	return name == "acp.signature" || name == "user.acp.signature"
}

// Decoded describes the decoded content of an encoded file.
type Decoded struct {
	// Encoding is the caller-defined, non-empty encoding name.
	Encoding string
	// Size is the decoded size.
	Size int64
	// SHA256 is the SHA-256 of the decoded content; empty when unknown.
	SHA256 []byte
}

// Entry is the managed signature xattr of a file. Size and MtimeNS describe the file on disk and
// decide freshness; SHA256 is the hash of the bytes on disk and is empty when unknown. Decoded is
// present only for an encoded file, where the entry is also the marker that the file is encoded.
type Entry struct {
	Size    int64
	MtimeNS int64
	SHA256  []byte
	Decoded *Decoded
}

// Status tells whether a file's managed signature entry describes its current version.
type Status uint8

const (
	// Missing means the file has no entry, or its file system has no managed attribute.
	Missing Status = iota
	// Stale means the entry's size or mtime no longer match the file.
	Stale
	// Fresh means the entry's size and mtime match the file.
	Fresh
)

// Equal reports whether two entries hold the same facts.
func (e Entry) Equal(other Entry) bool {
	if e.Size != other.Size || e.MtimeNS != other.MtimeNS || !bytes.Equal(e.SHA256, other.SHA256) {
		return false
	}
	if e.Decoded == nil || other.Decoded == nil {
		return e.Decoded == other.Decoded
	}
	return e.Decoded.Encoding == other.Decoded.Encoding && e.Decoded.Size == other.Decoded.Size &&
		bytes.Equal(e.Decoded.SHA256, other.Decoded.SHA256)
}

// encode returns the Protobuf form every write uses. It starts with a field tag, so it never
// begins with the legacy magic. It refuses an entry that Read would reject.
func encode(entry Entry) ([]byte, error) {
	if err := validate(entry); err != nil {
		return nil, fmt.Errorf("encode signature entry failed, %w", err)
	}
	if entry.Decoded != nil && len(entry.Decoded.Encoding) > MaxEncoding {
		return nil, fmt.Errorf("encode signature entry failed, encoding is longer than %d bytes", MaxEncoding)
	}

	message := &signaturepb.SignatureEntry{Size: entry.Size, MtimeNs: entry.MtimeNS, Sha256: entry.SHA256}
	if entry.Decoded != nil {
		message.Decoded = &signaturepb.Decoded{
			Encoding: entry.Decoded.Encoding,
			Size:     entry.Decoded.Size,
			Sha256:   entry.Decoded.SHA256,
		}
	}
	encoded, err := proto.Marshal(message)
	if err != nil {
		return nil, err
	}
	if len(encoded) > maxSize {
		return nil, fmt.Errorf("encode signature entry failed, size=%d", len(encoded))
	}
	return encoded, nil
}

func validHash(hash []byte) bool {
	return len(hash) == 0 || len(hash) == sha256.Size
}

// decode decodes the managed xattr value: the Protobuf form, or the legacy fixed-size layout that
// begins with the legacy magic.
func decode(encoded []byte) (Entry, error) {
	if len(encoded) == 0 || len(encoded) > maxSize {
		return Entry{}, fmt.Errorf("decode signature entry failed, size=%d", len(encoded))
	}
	if bytes.HasPrefix(encoded, []byte(magic)) {
		return decodeLegacy(encoded)
	}

	var message signaturepb.SignatureEntry
	if err := proto.Unmarshal(encoded, &message); err != nil {
		return Entry{}, fmt.Errorf("decode signature entry failed, %w", err)
	}
	entry := Entry{Size: message.Size, MtimeNS: message.MtimeNs, SHA256: message.Sha256}
	if decoded := message.Decoded; decoded != nil {
		entry.Decoded = &Decoded{Encoding: decoded.Encoding, Size: decoded.Size, SHA256: decoded.Sha256}
	}
	if err := validate(entry); err != nil {
		return Entry{}, fmt.Errorf("decode signature entry failed, %w", err)
	}
	return entry, nil
}

// validate holds the facts every stored entry must satisfy, for both reading and writing.
func validate(entry Entry) error {
	if entry.Size < 0 || !validHash(entry.SHA256) {
		return errors.New("invalid size or hash")
	}
	if decoded := entry.Decoded; decoded != nil {
		if decoded.Encoding == "" || decoded.Size < 0 || !validHash(decoded.SHA256) {
			return errors.New("invalid decoded description")
		}
	}
	return nil
}

// decodeLegacy decodes the fixed, big-endian layout earlier releases wrote.
func decodeLegacy(encoded []byte) (Entry, error) {
	// Reject any representation outside the one legacy codec version.
	if len(encoded) != legacySize {
		return Entry{}, fmt.Errorf("decode cached signature failed, size=%d", len(encoded))
	}
	if string(encoded[:4]) != magic {
		return Entry{}, fmt.Errorf("decode cached signature failed, invalid magic")
	}
	if encoded[4] != version {
		return Entry{}, fmt.Errorf("decode cached signature failed, version=%d", encoded[4])
	}
	if encoded[5] != algorithm {
		return Entry{}, fmt.Errorf("decode cached signature failed, algorithm=%d", encoded[5])
	}
	if encoded[6] != 0 || encoded[7] != 0 {
		return Entry{}, fmt.Errorf("decode cached signature failed, reserved bytes are not zero")
	}

	// Decode fixed-width facts only after the complete header is valid.
	return Entry{
		Size:    int64(binary.BigEndian.Uint64(encoded[8:16])),
		MtimeNS: int64(binary.BigEndian.Uint64(encoded[16:24])),
		SHA256:  append([]byte(nil), encoded[24:]...),
	}, nil
}

// ReadCached returns the file's entry and whether it is a fresh plain hash: the entry's size and
// mtime still match the regular file and the entry has the SHA-256 of the bytes on disk. Missing,
// stale, decoded-only and unsupported xattrs are cache misses; the returned entry is then not
// usable as a hash and the error is nil.
func ReadCached(path string) (Entry, bool, error) {
	entry, status, err := Read(path)
	if err != nil {
		return Entry{}, false, err
	}
	return entry, status == Fresh && len(entry.SHA256) == sha256.Size, nil
}

// Read returns a regular file's managed entry and whether it is fresh. A stale entry is still
// returned, so a caller can see its Decoded marker when only the mtime changed. Missing and
// unsupported xattrs report Missing.
func Read(path string) (Entry, Status, error) {
	// Bind metadata and xattr reads to one regular-file descriptor this lookup owns.
	file, err := os.Open(path)
	if err != nil {
		return Entry{}, Missing, fmt.Errorf("open signature source failed, %w", err)
	}
	defer file.Close()

	// Capture the regular-file facts that bind the cached signature.
	info, err := file.Stat()
	if err != nil {
		return Entry{}, Missing, fmt.Errorf("stat signature source failed, %w", err)
	}
	return ReadFile(file, info)
}

// ReadFile reads the entry through a descriptor whose metadata the caller already holds. Pipeline
// lookups call it directly; Read obtains the metadata itself.
func ReadFile(file *os.File, info os.FileInfo) (Entry, Status, error) {
	// Only regular-file metadata can validate a stored content signature.
	if !info.Mode().IsRegular() {
		return Entry{}, Missing, fmt.Errorf("signature source is not a regular file")
	}

	// Decode the managed xattr while preserving miss and failure semantics. A file system
	// without the attribute namespace is a miss, not a failure: there is simply no entry.
	encoded, err := readXattr(file)
	if err != nil {
		if Ignorable(err) {
			return Entry{}, Missing, nil
		}
		return Entry{}, Missing, fmt.Errorf("read signature xattr failed, %w", err)
	}
	entry, err := decode(encoded)
	if err != nil {
		return Entry{}, Missing, err
	}

	// Compare with the stored file's own facts, never a decoded size, so a file changed between
	// runs rejects its old entry.
	if entry.Size != info.Size() || entry.MtimeNS != info.ModTime().UnixNano() {
		return entry, Stale, nil
	}
	return entry, Fresh, nil
}

// WriteFile encodes the entry and stores it through a descriptor that owns the file. It refuses an
// entry that Read would reject.
func WriteFile(file *os.File, entry Entry) error {
	encoded, err := encode(entry)
	if err != nil {
		return err
	}
	return writeXattr(file, encoded)
}

// WritePath encodes the entry and stores it through a path the caller keeps stable. It refuses an
// entry that Read would reject.
func WritePath(path string, entry Entry) error {
	encoded, err := encode(entry)
	if err != nil {
		return err
	}
	return writePathXattr(path, encoded)
}
