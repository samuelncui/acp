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
	signatureQueueSize   = 128
	signatureSampleLimit = 5
)

var errSignatureXattrUnsupported = errors.New("signature xattr is unsupported")

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
	if isSignatureXattrUnsupported(err) {
		return CachedSignature{}, false, nil
	}
	return signature, status == signatureReadHit, err
}

func readCachedSignature(path string) (CachedSignature, signatureReadStatus, error) {
	// Bind metadata and xattr reads to one regular-file descriptor.
	file, err := os.Open(path)
	if err != nil {
		return CachedSignature{}, signatureReadMiss, fmt.Errorf("open signature source failed, %w", err)
	}
	defer file.Close()

	// Capture the regular-file facts that bind the cached signature.
	info, err := file.Stat()
	if err != nil {
		return CachedSignature{}, signatureReadMiss, fmt.Errorf("stat signature source failed, %w", err)
	}
	if !info.Mode().IsRegular() {
		return CachedSignature{}, signatureReadMiss, fmt.Errorf("signature source is not a regular file")
	}

	// Decode the managed xattr while preserving miss and failure semantics.
	encoded, err := readSignatureXattr(file)
	if err != nil {
		if isSignatureXattrMissing(err) {
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

type signatureWrite struct {
	path      string
	signature CachedSignature
}

type signatureCache struct {
	queue chan signatureWrite
	wg    sync.WaitGroup

	lock    sync.Mutex
	summary SignatureCacheSummary
}

func newSignatureCache(workers int) *signatureCache {
	// Start the bounded writer pool before any copy stage can enqueue work.
	cache := &signatureCache{queue: make(chan signatureWrite, signatureQueueSize)}
	cache.wg.Add(workers)
	for idx := 0; idx < workers; idx++ {
		go func() {
			defer cache.wg.Done()
			for write := range cache.queue {
				cache.write(write)
			}
		}()
	}
	return cache
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

func (c *signatureCache) lookup(path string, indexed *stat) ([]byte, bool) {
	// Treat every unusable cache read as a non-fatal miss with diagnostics.
	signature, status, err := readCachedSignature(path)
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

func (c *signatureCache) enqueue(path string, signature CachedSignature) {
	// Queue the ACP result unchanged for a bounded writer to publish as-is.
	c.queue <- signatureWrite{path: path, signature: signature}
}

func (c *Copyer) invalidateSignature(file *os.File, path string) {
	if err := removeSignatureXattr(file); err != nil && !isSignatureXattrMissing(err) {
		if c.signatures != nil {
			c.signatures.recordFailure(path, fmt.Errorf("remove old signature xattr failed, %w", err))
		}
	}
}

func (c *Copyer) invalidateSignaturePath(path string) {
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

func (c *signatureCache) write(write signatureWrite) {
	// Open the target and reject anything a content signature cannot describe.
	file, err := os.Open(write.path)
	if err != nil {
		c.recordFailure(write.path, fmt.Errorf("open signature target failed, %w", err))
		return
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		c.recordFailure(write.path, fmt.Errorf("stat signature target failed, %w", err))
		return
	}
	if !info.Mode().IsRegular() {
		c.recordFailure(write.path, fmt.Errorf("signature target is not a regular file"))
		return
	}

	// Publish the queued snapshot unchanged. Rechecking live metadata here would
	// prove nothing: the reader compares the stored size and mtime against the
	// file and treats a mismatch as stale, so an entry queued for a version that
	// has since changed can never be read back as a hit. Rewriting it from live
	// metadata is not an option either, because the hash belongs to the hashed
	// version only.
	if err := writeSignatureXattr(file, encodeCachedSignature(write.signature)); err != nil {
		c.recordFailure(write.path, fmt.Errorf("write signature xattr failed, %w", err))
		return
	}

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

func (c *signatureCache) closeAndWait() SignatureCacheSummary {
	// Stop accepting writes and drain every queued filesystem operation.
	close(c.queue)
	c.wg.Wait()

	// Return an isolated diagnostic snapshot after all workers have stopped.
	c.lock.Lock()
	defer c.lock.Unlock()
	c.summary.Samples = append([]string(nil), c.summary.Samples...)
	return c.summary
}

func (c *Copyer) finishSignatureCache() {
	if c.signatures == nil {
		return
	}

	// Drain all best-effort writes before reporting the final aggregate summary.
	summary := c.signatures.closeAndWait()
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

func signatureWorkers(from, to *deviceOption) int {
	if from.linear || to.linear {
		return 1
	}
	if from.threads > to.threads {
		return from.threads
	}
	return to.threads
}

func signatureCacheKey(key string) bool {
	return key == "acp.signature" || key == "user.acp.signature"
}
