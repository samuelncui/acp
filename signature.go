package acp

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/sirupsen/logrus"
)

const (
	signatureMagic       = "ACPS"
	signatureVersion     = uint8(1)
	signatureSHA256      = uint8(1)
	signatureEncodedSize = 56
	signatureSampleLimit = 5
)

var errSignatureXattrUnsupported = errors.New("signature xattr is unsupported")

// The managed-xattr operations are indirected so a test can exercise the missing, unsupported
// and failing paths on a file system that accepts the attribute.
var (
	readManagedXattr   = readSignatureXattr
	writeManagedXattr  = writeSignatureXattr
	removeManagedXattr = removeSignatureXattr
)

// signatureXattrIgnorable reports whether an xattr error leaves the cache in a well-defined
// "no entry" state instead of describing a broken cache. An absent attribute and a file system
// without the managed attribute namespace both mean the same thing: there is no stored hash.
func signatureXattrIgnorable(err error) bool {
	return errors.Is(err, errSignatureXattrUnsupported) || isSignatureXattrMissing(err) || isSignatureXattrUnsupported(err)
}

// CachedSignature is a SHA-256 content signature bound to file metadata.
type CachedSignature struct {
	Size    int64
	MtimeNS int64
	SHA256  [32]byte
}

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

func encodeCachedSignature(signature CachedSignature) []byte {
	encoded := make([]byte, signatureEncodedSize)
	copy(encoded[:4], signatureMagic)
	encoded[4] = signatureVersion
	encoded[5] = signatureSHA256
	binary.BigEndian.PutUint64(encoded[8:16], uint64(signature.Size))
	binary.BigEndian.PutUint64(encoded[16:24], uint64(signature.MtimeNS))
	copy(encoded[24:], signature.SHA256[:])
	return encoded
}

// DecodeCachedSignature decodes ACP's stable, big-endian xattr representation.
func DecodeCachedSignature(encoded []byte) (CachedSignature, error) {
	// Reject any representation outside the one stable codec version.
	if len(encoded) != signatureEncodedSize {
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

type signatureReadStatus uint8

const (
	signatureReadMiss signatureReadStatus = iota
	signatureReadHit
	signatureReadStale
)

// ReadCachedSignature returns a signature only when its size and mtime still
// match the regular file. Missing, stale, and unsupported xattrs are cache misses.
func ReadCachedSignature(path string) (CachedSignature, bool, error) {
	signature, status, err := readCachedSignature(path)
	return signature, status == signatureReadHit, err
}

func readCachedSignature(path string) (CachedSignature, signatureReadStatus, error) {
	// Bind metadata and xattr reads to one regular-file descriptor this lookup owns.
	file, err := os.Open(path)
	if err != nil {
		return CachedSignature{}, signatureReadMiss, fmt.Errorf("open signature source failed, %w", err)
	}
	defer file.Close()

	return readCachedSignatureFile(file)
}

// readCachedSignatureFile reads the stored signature through a descriptor the caller already
// owns, so the entry it reports belongs to the file version that descriptor sees. It never
// closes the descriptor.
func readCachedSignatureFile(file *os.File) (CachedSignature, signatureReadStatus, error) {
	// Capture the regular-file facts that bind the cached signature.
	info, err := file.Stat()
	if err != nil {
		return CachedSignature{}, signatureReadMiss, fmt.Errorf("stat signature source failed, %w", err)
	}
	if !info.Mode().IsRegular() {
		return CachedSignature{}, signatureReadMiss, fmt.Errorf("signature source is not a regular file")
	}

	// Decode the managed xattr while preserving miss and failure semantics. A file system
	// without the attribute namespace is a miss, not a failure: there is simply no entry.
	encoded, err := readManagedXattr(file)
	if err != nil {
		if signatureXattrIgnorable(err) {
			return CachedSignature{}, signatureReadMiss, nil
		}
		return CachedSignature{}, signatureReadMiss, fmt.Errorf("read signature xattr failed, %w", err)
	}
	signature, err := DecodeCachedSignature(encoded)
	if err != nil {
		return CachedSignature{}, signatureReadMiss, err
	}

	// Revalidate metadata after the xattr read before declaring a cache hit.
	after, err := file.Stat()
	if err != nil {
		return CachedSignature{}, signatureReadMiss, fmt.Errorf("restat signature source failed, %w", err)
	}
	if !after.Mode().IsRegular() || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
		return signature, signatureReadStale, nil
	}
	if signature.Size != info.Size() || signature.MtimeNS != info.ModTime().UnixNano() {
		return signature, signatureReadStale, nil
	}
	return signature, signatureReadHit, nil
}

// signatureCache is the aggregate accounting of one run's cache activity. It owns no writer and
// no queue: an item reads its stored hash and publishes its computed one through the descriptor it
// already owns, so a cache operation never reopens a path and never outlives its item. Dropping a
// target's stale entry before that target is truncated is the one path-based cache step, and it
// happens before the target has a descriptor at all.
type signatureCache struct {
	lock    sync.Mutex
	summary SignatureCacheSummary
}

func newSignatureCache() *signatureCache {
	return &signatureCache{}
}

func newCachedSignature(hash []byte, indexed *stat) (CachedSignature, error) {
	if len(hash) != len(CachedSignature{}.SHA256) {
		return CachedSignature{}, fmt.Errorf("invalid SHA-256 size=%d", len(hash))
	}
	if indexed == nil {
		return CachedSignature{}, fmt.Errorf("signature metadata is missing")
	}

	signature := CachedSignature{
		Size:    indexed.size,
		MtimeNS: indexed.modTime.UnixNano(),
	}
	copy(signature.SHA256[:], hash)
	return signature, nil
}

func (c *signatureCache) lookup(file *os.File, path string, indexed *stat) ([]byte, bool) {
	// Treat every unusable cache read as a non-fatal miss with diagnostics.
	signature, status, err := readCachedSignatureFile(file)
	if err != nil {
		c.recordFailure(path, err)
		c.incrementMiss()
		return nil, false
	}

	// Distinguish metadata staleness from an absent entry for aggregate reporting.
	switch status {
	case signatureReadHit:
		if signature.Size != indexed.size || signature.MtimeNS != indexed.modTime.UnixNano() {
			c.incrementStale()
			return nil, false
		}
		c.incrementHit()
		return append([]byte(nil), signature.SHA256[:]...), true
	case signatureReadStale:
		c.incrementStale()
	default:
		c.incrementMiss()
	}
	return nil, false
}

func (c *StreamCopyer) invalidateSignature(file *os.File, path string) {
	// A file system that cannot store the managed attribute has no stale entry to drop.
	if err := removeManagedXattr(file); err != nil && !signatureXattrIgnorable(err) {
		if c.signatures != nil {
			c.signatures.recordFailure(path, fmt.Errorf("remove old signature xattr failed, %w", err))
		}
	}
}

func (c *StreamCopyer) invalidateSignaturePath(path string) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		if c.signatures != nil {
			c.signatures.recordFailure(path, fmt.Errorf("open old signature target failed, %w", err))
		}
		return
	}
	defer file.Close()
	c.invalidateSignature(file, path)
}

// refreshCacheEntry publishes the computed content signature of a finished item through the
// descriptor that owns one of that item's files. It publishes nothing when the item has no hash it
// computed itself or the policy does not refresh the cache, and a nil descriptor is an item that
// never opened the file it names rather than a failure.
//
// An entry states the size and modification time a later lookup compares, so it may only be
// published for the exact file version whose bytes produced the hash: an entry that binds that hash
// to other facts makes a later run report a hash for content it never read. The descriptor the item
// still owns is the evidence, and which evidence it can give depends on which of the item's files
// it is:
//
//   - The source descriptor must still show the size the item read and the modification time it
//     indexed. The run never wrote that file, so a source whose metadata moved after indexing cannot
//     be shown to be the version the run hashed, and it publishes nothing instead of a guess.
//   - A target descriptor is a file this run wrote with exactly the bytes that were hashed, and the
//     run stamps the item's metadata onto it after this publication, so its length is the evidence
//     available here.
//
// Every check runs through that descriptor, so a cache operation never reopens a path.
func (c *StreamCopyer) refreshCacheEntry(file *os.File, path string, job *baseJob, target bool) {
	if file == nil || c.signatures == nil || !c.hashPolicy.refreshesCache() {
		return
	}

	hash, ok := job.computedHash()
	if !ok {
		return
	}
	signature, err := newCachedSignature(hash, job.stat)
	if err != nil {
		c.signatures.recordFailure(path, err)
		return
	}

	info, err := file.Stat()
	if err != nil {
		c.signatures.recordFailure(path, fmt.Errorf("stat signature target failed, %w", err))
		return
	}
	if info.Size() != signature.Size || (!target && info.ModTime().UnixNano() != signature.MtimeNS) {
		return
	}

	c.signatures.refresh(file, path, signature)
}

// refresh publishes one computed signature through a descriptor that owns the file, unless that
// descriptor already stores it. Re-reading the entry is what makes a refresh cheap: a run that
// finds the same stored value writes nothing, which keeps an identical rewrite from costing a
// physical attribute block on copy-on-write file systems. A stored value that cannot be read is
// no stored value, so the entry is published.
func (c *signatureCache) refresh(file *os.File, path string, signature CachedSignature) {
	if file == nil {
		return
	}
	if stored, status, err := readCachedSignatureFile(file); err == nil && status == signatureReadHit && stored == signature {
		return
	}

	c.write(file, path, signature)
}

// write publishes one computed signature through the descriptor that owns the file. The snapshot
// is stored unchanged: a signature describes the version that was hashed, the reader treats an
// entry whose size or mtime no longer match as stale, and re-deriving it from live metadata would
// bind the hash to a version it never described. Every failure is an aggregate warning, because a
// cache that cannot store an entry never fails the item that computed it.
func (c *signatureCache) write(file *os.File, path string, signature CachedSignature) {
	// Reject anything a content signature cannot describe.
	info, err := file.Stat()
	if err != nil {
		c.recordFailure(path, fmt.Errorf("stat signature target failed, %w", err))
		return
	}
	if !info.Mode().IsRegular() {
		c.recordFailure(path, fmt.Errorf("signature target is not a regular file"))
		return
	}

	if err := writeManagedXattr(file, encodeCachedSignature(signature)); err != nil {
		// A file system without the managed attribute namespace cannot hold the cache at all:
		// that is a no-op, not a failure of this run.
		if signatureXattrIgnorable(err) {
			return
		}
		c.recordFailure(path, fmt.Errorf("write signature xattr failed, %w", err))
		return
	}

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
