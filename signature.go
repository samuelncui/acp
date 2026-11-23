package acp

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/samuelncui/acp/internal/signaturepb"
	"github.com/sirupsen/logrus"
	"google.golang.org/protobuf/proto"
)

const (
	// The legacy layout is exactly 56 bytes that begin with the magic; every new entry is Protobuf.
	signatureMagic      = "ACPS"
	signatureVersion    = uint8(1)
	signatureSHA256     = uint8(1)
	signatureLegacySize = 56
	// signatureMaxSize bounds a stored entry; the longest valid one is far smaller. A longer value
	// is an invalid entry.
	signatureMaxSize = 512
	// signatureMaxEncoding bounds an item's encoding name so every entry fits signatureMaxSize.
	signatureMaxEncoding = 64
	signatureSampleLimit = 5
)

var (
	errSignatureXattrUnsupported = errors.New("signature xattr is unsupported")
	errSignatureOversized        = errors.New("signature xattr is larger than any valid entry")
)

// The managed-xattr operations are indirected so a test can exercise the missing, unsupported
// and failing paths on a file system that accepts the attribute.
var (
	readManagedXattr      = readSignatureXattr
	writeManagedXattr     = writeSignatureXattr
	writeManagedPathXattr = writeSignaturePathXattr
)

// signatureXattrIgnorable reports whether an xattr error leaves the cache in a well-defined
// "no entry" state instead of describing a broken cache. An absent attribute and a file system
// without the managed attribute namespace both mean the same thing: there is no stored hash.
func signatureXattrIgnorable(err error) bool {
	return errors.Is(err, errSignatureXattrUnsupported) || isSignatureXattrMissing(err) || isSignatureXattrUnsupported(err)
}

// CachedSignature is a SHA-256 content signature of the bytes on disk, bound to file metadata.
type CachedSignature struct {
	Size    int64
	MtimeNS int64
	SHA256  [32]byte
}

// DecodedSignature describes the decoded content of an encoded file.
type DecodedSignature struct {
	// Encoding is the caller-defined, non-empty encoding name.
	Encoding string
	// Size is the decoded size.
	Size int64
	// SHA256 is the SHA-256 of the decoded content; empty when unknown.
	SHA256 []byte
}

// SignatureEntry is the managed signature xattr of a file. Size and MtimeNS describe the file on
// disk and decide freshness; SHA256 is the hash of the bytes on disk and is empty when unknown.
// Decoded is present only for an encoded file, where the entry is also the marker that the file
// is encoded.
type SignatureEntry struct {
	Size    int64
	MtimeNS int64
	SHA256  []byte
	Decoded *DecodedSignature
}

// SignatureStatus tells whether a file's managed signature entry describes its current version.
type SignatureStatus uint8

const (
	// SignatureMissing means the file has no entry, or its file system has no managed attribute.
	SignatureMissing SignatureStatus = iota
	// SignatureStale means the entry's size or mtime no longer match the file.
	SignatureStale
	// SignatureFresh means the entry's size and mtime match the file.
	SignatureFresh
)

// SignatureCacheSummary describes cache activity for one Copyer run.
type SignatureCacheSummary struct {
	Hits       int64    `json:"hits"`
	Misses     int64    `json:"misses"`
	Stale      int64    `json:"stale"`
	Writes     int64    `json:"writes"`
	Failures   int64    `json:"failures"`
	FirstError string   `json:"first_error,omitempty"`
	Samples    []string `json:"samples,omitempty"`
}

func (e SignatureEntry) equal(other SignatureEntry) bool {
	if e.Size != other.Size || e.MtimeNS != other.MtimeNS || !bytes.Equal(e.SHA256, other.SHA256) {
		return false
	}
	if e.Decoded == nil || other.Decoded == nil {
		return e.Decoded == other.Decoded
	}
	return e.Decoded.Encoding == other.Decoded.Encoding && e.Decoded.Size == other.Decoded.Size &&
		bytes.Equal(e.Decoded.SHA256, other.Decoded.SHA256)
}

// encodeSignatureEntry returns the Protobuf form every write uses. It starts with a field tag, so
// it never begins with the legacy magic.
func encodeSignatureEntry(entry SignatureEntry) ([]byte, error) {
	message := &signaturepb.SignatureEntry{Size: entry.Size, MtimeNs: entry.MtimeNS, Sha256: entry.SHA256}
	if entry.Decoded != nil {
		message.Decoded = &signaturepb.Decoded{
			Encoding: entry.Decoded.Encoding,
			Size:     entry.Decoded.Size,
			Sha256:   entry.Decoded.SHA256,
		}
	}
	return proto.Marshal(message)
}

func validSignatureHash(hash []byte) bool {
	return len(hash) == 0 || len(hash) == sha256.Size
}

// DecodeSignatureEntry decodes the managed xattr value: the Protobuf form, or the legacy
// fixed-size layout that begins with the legacy magic.
func DecodeSignatureEntry(encoded []byte) (SignatureEntry, error) {
	if len(encoded) == 0 || len(encoded) > signatureMaxSize {
		return SignatureEntry{}, fmt.Errorf("decode signature entry failed, size=%d", len(encoded))
	}
	if bytes.HasPrefix(encoded, []byte(signatureMagic)) {
		signature, err := decodeLegacySignature(encoded)
		if err != nil {
			return SignatureEntry{}, err
		}
		return SignatureEntry{Size: signature.Size, MtimeNS: signature.MtimeNS, SHA256: signature.SHA256[:]}, nil
	}

	var message signaturepb.SignatureEntry
	if err := proto.Unmarshal(encoded, &message); err != nil {
		return SignatureEntry{}, fmt.Errorf("decode signature entry failed, %w", err)
	}
	entry := SignatureEntry{Size: message.Size, MtimeNS: message.MtimeNs, SHA256: message.Sha256}
	if entry.Size < 0 || !validSignatureHash(entry.SHA256) {
		return SignatureEntry{}, fmt.Errorf("decode signature entry failed, invalid size or hash")
	}
	if decoded := message.Decoded; decoded != nil {
		if decoded.Encoding == "" || decoded.Size < 0 || !validSignatureHash(decoded.Sha256) {
			return SignatureEntry{}, fmt.Errorf("decode signature entry failed, invalid decoded description")
		}
		entry.Decoded = &DecodedSignature{Encoding: decoded.Encoding, Size: decoded.Size, SHA256: decoded.Sha256}
	}
	return entry, nil
}

// decodeLegacySignature decodes the fixed, big-endian layout earlier releases wrote.
func decodeLegacySignature(encoded []byte) (CachedSignature, error) {
	// Reject any representation outside the one legacy codec version.
	if len(encoded) != signatureLegacySize {
		return CachedSignature{}, fmt.Errorf("decode cached signature failed, size=%d", len(encoded))
	}
	if string(encoded[:4]) != signatureMagic {
		return CachedSignature{}, fmt.Errorf("decode cached signature failed, invalid magic")
	}
	if encoded[4] != signatureVersion {
		return CachedSignature{}, fmt.Errorf("decode cached signature failed, version=%d", encoded[4])
	}
	if encoded[5] != signatureSHA256 {
		return CachedSignature{}, fmt.Errorf("decode cached signature failed, algorithm=%d", encoded[5])
	}
	if encoded[6] != 0 || encoded[7] != 0 {
		return CachedSignature{}, fmt.Errorf("decode cached signature failed, reserved bytes are not zero")
	}

	// Decode fixed-width facts only after the complete header is valid.
	var signature CachedSignature
	signature.Size = int64(binary.BigEndian.Uint64(encoded[8:16]))
	signature.MtimeNS = int64(binary.BigEndian.Uint64(encoded[16:24]))
	copy(signature.SHA256[:], encoded[24:])
	return signature, nil
}

// DecodeCachedSignature decodes the plain view of a managed xattr value, in either encoding. It
// fails for an entry without a SHA-256 of the bytes on disk.
func DecodeCachedSignature(encoded []byte) (CachedSignature, error) {
	entry, err := DecodeSignatureEntry(encoded)
	if err != nil {
		return CachedSignature{}, err
	}
	return plainSignature(entry)
}

func plainSignature(entry SignatureEntry) (CachedSignature, error) {
	if len(entry.SHA256) != sha256.Size {
		return CachedSignature{}, fmt.Errorf("cached signature has no SHA-256 of the stored bytes")
	}
	signature := CachedSignature{Size: entry.Size, MtimeNS: entry.MtimeNS}
	copy(signature.SHA256[:], entry.SHA256)
	return signature, nil
}

// ReadCachedSignature returns the plain signature of the bytes on disk only when the entry's size
// and mtime still match the regular file and the entry has that hash. Missing, stale, decoded-only
// and unsupported xattrs are cache misses.
func ReadCachedSignature(path string) (CachedSignature, bool, error) {
	entry, status, err := ReadSignatureEntry(path)
	if err != nil {
		return CachedSignature{}, false, err
	}
	signature, plainErr := plainSignature(entry)
	return signature, status == SignatureFresh && plainErr == nil, nil
}

// ReadSignatureEntry returns a regular file's managed entry and whether it is fresh. A stale
// entry is still returned, so a caller can see its Decoded marker when only the mtime changed.
// Missing and unsupported xattrs report SignatureMissing.
func ReadSignatureEntry(path string) (SignatureEntry, SignatureStatus, error) {
	// Bind metadata and xattr reads to one regular-file descriptor this lookup owns.
	file, err := os.Open(path)
	if err != nil {
		return SignatureEntry{}, SignatureMissing, fmt.Errorf("open signature source failed, %w", err)
	}
	defer file.Close()

	// Capture the regular-file facts that bind the cached signature.
	info, err := file.Stat()
	if err != nil {
		return SignatureEntry{}, SignatureMissing, fmt.Errorf("stat signature source failed, %w", err)
	}
	return readSignatureEntryInfo(file, info)
}

// readSignatureEntryInfo reads the entry through a descriptor whose metadata the caller already
// holds. Pipeline lookups call it directly; ReadSignatureEntry obtains the metadata itself.
func readSignatureEntryInfo(file *os.File, info os.FileInfo) (SignatureEntry, SignatureStatus, error) {
	// Only regular-file metadata can validate a stored content signature.
	if !info.Mode().IsRegular() {
		return SignatureEntry{}, SignatureMissing, fmt.Errorf("signature source is not a regular file")
	}

	// Decode the managed xattr while preserving miss and failure semantics. A file system
	// without the attribute namespace is a miss, not a failure: there is simply no entry.
	encoded, err := readManagedXattr(file)
	if err != nil {
		if signatureXattrIgnorable(err) {
			return SignatureEntry{}, SignatureMissing, nil
		}
		return SignatureEntry{}, SignatureMissing, fmt.Errorf("read signature xattr failed, %w", err)
	}
	entry, err := DecodeSignatureEntry(encoded)
	if err != nil {
		return SignatureEntry{}, SignatureMissing, err
	}

	// Compare with the stored file's own facts, never a decoded size, so a file changed between
	// runs rejects its old entry.
	if entry.Size != info.Size() || entry.MtimeNS != info.ModTime().UnixNano() {
		return entry, SignatureStale, nil
	}
	return entry, SignatureFresh, nil
}

// signatureCache is the aggregate accounting of one run's cache activity. It owns no writer and
// no queue: an item reads its stored hash and publishes its computed one through the descriptor it
// already owns, so a cache operation never reopens a path and never outlives its item.
type signatureCache struct {
	lock    sync.Mutex
	summary SignatureCacheSummary
}

func newSignatureCache() *signatureCache {
	return &signatureCache{}
}

// storedHash returns the stored hash an item may reuse from a fresh entry. A plain read trusts
// only the hash of the bytes on disk; a decoding read trusts only the decoded hash, and only for
// the encoding and declared size the item names now.
func (j *baseJob) storedHash(entry *SignatureEntry) []byte {
	if j.decode == nil {
		if len(entry.SHA256) == sha256.Size {
			return entry.SHA256
		}
		return nil
	}
	decoded := entry.Decoded
	if decoded == nil || decoded.Encoding != j.decodeEncoding || decoded.Size != j.stat.size || len(decoded.SHA256) != sha256.Size {
		return nil
	}
	return decoded.SHA256
}

// lookup reads a source's entry through its content descriptor. It returns the fresh entry, which
// a later refresh merges into, and the stored hash only when this item can reuse it.
func (c *signatureCache) lookup(file *os.File, path string, job *baseJob) (*SignatureEntry, []byte) {
	// Treat every unusable cache read as a non-fatal miss with diagnostics.
	entry, status, err := readSignatureEntryInfo(file, job.stat.info)
	if err != nil {
		c.recordFailure(path, err)
		c.incrementMiss()
		return nil, nil
	}

	// Distinguish metadata staleness from an absent entry for aggregate reporting.
	switch status {
	case SignatureFresh:
		if hash := job.storedHash(&entry); hash != nil {
			c.incrementHit()
			return &entry, append([]byte(nil), hash...)
		}
		c.incrementMiss()
		return &entry, nil
	case SignatureStale:
		c.incrementStale()
	default:
		c.incrementMiss()
	}
	return nil, nil
}

// refreshCacheEntry publishes the computed content signature of a finished item through the
// descriptor that owns one of that item's files. It publishes nothing when the item has no hash it
// computed itself or the policy does not refresh the cache, and a nil descriptor is an item that
// never opened the file it names rather than a failure.
//
// Sources and paths stay stable during a run. Preparation retained the source's stored entry;
// targets use fresh temporary files, so neither publication needs another metadata or xattr read.
func (c *StreamCopyer) refreshCacheEntry(file *os.File, path string, job *baseJob, target bool) {
	// A missing descriptor means this item never opened the named file.
	if file == nil {
		return
	}

	// Share policy and snapshot decisions with publication after descriptor closure.
	if entry, ok := c.cacheEntry(path, job, target); ok {
		c.signatures.write(file, path, entry)
	}
}

// refreshCachePath publishes through a stable, caller-owned path after its descriptor closes.
// The caller keeps the path unchanged until publication and any target replacement finish.
func (c *StreamCopyer) refreshCachePath(path string, job *baseJob, target bool) {
	if entry, ok := c.cacheEntry(path, job, target); ok {
		c.signatures.recordWrite(path, writeEntryPath(path, entry))
	}
}

func writeEntryPath(path string, entry SignatureEntry) error {
	encoded, err := encodeSignatureEntry(entry)
	if err != nil {
		return err
	}
	return writeManagedPathXattr(path, encoded)
}

func writeEntryFile(file *os.File, entry SignatureEntry) error {
	encoded, err := encodeSignatureEntry(entry)
	if err != nil {
		return err
	}
	return writeManagedXattr(file, encoded)
}

// refreshHash returns the hash this run computed when the policy refreshes the cache and the
// side has not opted out of writing it.
func (c *StreamCopyer) refreshHash(job *baseJob, target bool) ([]byte, bool) {
	// Only a completed content hash under a refresh policy can publish a hash.
	if c.signatures == nil || !c.hashPolicy.refreshesCache() {
		return nil, false
	}

	// Each side may opt out of the write alone; hashing and the other side are unaffected.
	side := c.fromDevice
	if target {
		side = c.toDevice
	}
	if side.skipCacheRefresh {
		return nil, false
	}
	return job.computedHash()
}

func (c *StreamCopyer) cacheEntry(path string, job *baseJob, target bool) (SignatureEntry, bool) {
	hash, ok := c.refreshHash(job, target)
	if !ok {
		return SignatureEntry{}, false
	}
	if job.stat == nil {
		c.signatures.recordFailure(path, fmt.Errorf("signature metadata is missing"))
		return SignatureEntry{}, false
	}

	// A fresh target has no entry to merge with or skip.
	if target {
		return SignatureEntry{Size: job.stat.size, MtimeNS: job.stat.modTime.UnixNano(), SHA256: hash}, true
	}

	// A source keeps the half of its fresh entry this read did not compute, and skips an unchanged write.
	entry := SignatureEntry{Size: job.stat.info.Size(), MtimeNS: job.stat.info.ModTime().UnixNano()}
	existing := job.cachedEntry
	switch {
	case job.decode == nil:
		entry.SHA256 = hash
		if existing != nil {
			entry.Decoded = existing.Decoded
		}
	default:
		if existing != nil {
			entry.SHA256 = existing.SHA256
		}
		entry.Decoded = &DecodedSignature{Encoding: job.decodeEncoding, Size: job.stat.size, SHA256: hash}
	}
	if existing != nil && existing.equal(entry) {
		return SignatureEntry{}, false
	}
	return entry, true
}

// encodedEntry describes an encoded target: the stored file's own size and the source mtime that
// the metadata restore applies, with the plaintext facts as the encoded-file marker. The hash is
// the only optional part, and follows the refresh policy.
func (c *StreamCopyer) encodedEntry(job *baseJob, stored int64) SignatureEntry {
	entry := SignatureEntry{
		Size:    stored,
		MtimeNS: job.stat.modTime.UnixNano(),
		Decoded: &DecodedSignature{Encoding: job.encodeEncoding, Size: job.stat.size},
	}
	if hash, ok := c.refreshHash(job, true); ok {
		entry.Decoded.SHA256 = hash
	}
	return entry
}

// writeEncodedMarker publishes the entry that marks a target as encoded, through the descriptor
// of a random output or the owned temporary path of a closed linear one. Unlike a plain cache
// write, its failure is the caller's error, including a file system without the attribute.
func (c *StreamCopyer) writeEncodedMarker(file *os.File, path string, job *baseJob, stored int64, linear bool) error {
	entry := c.encodedEntry(job, stored)
	var err error
	if linear {
		err = writeEntryPath(path, entry)
	} else {
		err = writeEntryFile(file, entry)
	}
	if err != nil {
		return err
	}
	if c.signatures != nil {
		c.signatures.incrementWrite()
	}
	return nil
}

// write publishes one computed entry through the descriptor that owns the file. The snapshot
// is stored unchanged: an entry describes the version that was hashed, the reader treats an
// entry whose size or mtime no longer match as stale, and re-deriving it from live metadata would
// bind the hash to a version it never described. Every failure is an aggregate warning, because a
// cache that cannot store an entry never fails the item that computed it.
func (c *signatureCache) write(file *os.File, path string, entry SignatureEntry) {
	c.recordWrite(path, writeEntryFile(file, entry))
}

func (c *signatureCache) recordWrite(path string, err error) {
	// Descriptor and path publication share unsupported handling and aggregate warnings.
	if err != nil {
		if signatureXattrIgnorable(err) {
			return
		}
		c.recordFailure(path, fmt.Errorf("write signature xattr failed, %w", err))
		return
	}

	// Count only entries accepted by the file system.
	c.incrementWrite()
}

func (c *signatureCache) incrementWrite() {
	c.lock.Lock()
	c.summary.Writes++
	c.lock.Unlock()
}

func (c *signatureCache) incrementHit() {
	c.lock.Lock()
	c.summary.Hits++
	c.lock.Unlock()
}

func (c *signatureCache) incrementMiss() {
	c.lock.Lock()
	c.summary.Misses++
	c.lock.Unlock()
}

func (c *signatureCache) incrementStale() {
	c.lock.Lock()
	c.summary.Stale++
	c.lock.Unlock()
}

func (c *signatureCache) recordFailure(path string, err error) {
	c.lock.Lock()
	defer c.lock.Unlock()
	c.summary.Failures++
	if c.summary.FirstError == "" {
		c.summary.FirstError = err.Error()
	}
	if len(c.summary.Samples) < signatureSampleLimit {
		c.summary.Samples = append(c.summary.Samples, path)
	}
}

// snapshot returns an isolated diagnostic snapshot of the run's cache activity. Every write
// happens inside the item that caused it, so there is nothing left to drain when it is read.
func (c *signatureCache) snapshot() SignatureCacheSummary {
	c.lock.Lock()
	defer c.lock.Unlock()
	c.summary.Samples = append([]string(nil), c.summary.Samples...)
	return c.summary
}

func (c *StreamCopyer) finishSignatureCache() {
	if c.signatures == nil {
		return
	}

	// Every item has published its own entry by the time the pipeline ends, so the aggregate
	// summary is complete here.
	summary := c.signatures.snapshot()
	c.submit(&EventSignatureCacheSummary{Summary: summary})
	level := logrus.InfoLevel
	if summary.Failures > 0 {
		level = logrus.WarnLevel
	}
	c.logf(
		level,
		"signature cache summary: hit=%d miss=%d stale=%d write=%d failure=%d first_error=%q samples=%q",
		summary.Hits,
		summary.Misses,
		summary.Stale,
		summary.Writes,
		summary.Failures,
		summary.FirstError,
		summary.Samples,
	)
}

func signatureCacheKey(key string) bool {
	return key == "acp.signature" || key == "user.acp.signature"
}
