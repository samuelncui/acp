package acp

import (
	"crypto/sha256"
	"fmt"
	"os"
	"sync"

	"github.com/samuelncui/acp/signature"
	"github.com/sirupsen/logrus"
)

const signatureSampleLimit = 5

// SignatureCacheSummary describes the signature-cache activity of one run.
type SignatureCacheSummary struct {
	Hits       int64    `json:"hits"`
	Misses     int64    `json:"misses"`
	Stale      int64    `json:"stale"`
	Writes     int64    `json:"writes"`
	Failures   int64    `json:"failures"`
	FirstError string   `json:"first_error,omitempty"`
	Samples    []string `json:"samples,omitempty"`
}

// cacheOutcome names the counters of a run's signature-cache statistics.
type cacheOutcome uint8

const (
	cacheHit cacheOutcome = iota
	cacheMiss
	cacheStale
	cacheWrite
)

// cacheStats is the aggregate accounting of one run's cache activity; it only counts. An item
// reads its stored hash and publishes its computed one through the descriptor it already owns, so
// a cache operation never reopens a path and never outlives its item.
type cacheStats struct {
	lock    sync.Mutex
	summary SignatureCacheSummary
}

func (s *cacheStats) count(outcome cacheOutcome) {
	s.lock.Lock()
	defer s.lock.Unlock()
	switch outcome {
	case cacheHit:
		s.summary.Hits++
	case cacheMiss:
		s.summary.Misses++
	case cacheStale:
		s.summary.Stale++
	case cacheWrite:
		s.summary.Writes++
	}
}

func (s *cacheStats) recordFailure(path string, err error) {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.summary.Failures++
	if s.summary.FirstError == "" {
		s.summary.FirstError = err.Error()
	}
	if len(s.summary.Samples) < signatureSampleLimit {
		s.summary.Samples = append(s.summary.Samples, path)
	}
}

// snapshot returns an isolated diagnostic snapshot of the run's cache activity. Every write
// happens inside the item that caused it, so there is nothing left to drain when it is read.
func (s *cacheStats) snapshot() SignatureCacheSummary {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.summary.Samples = append([]string(nil), s.summary.Samples...)
	return s.summary
}

// storedHash returns the stored hash an item may reuse from a fresh entry. A plain read trusts
// only the hash of the bytes on disk; a decoding read trusts only the decoded hash, and only for
// the encoding and declared size the item names now.
func (j *baseJob) storedHash(entry *signature.Entry) []byte {
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
func (c *StreamCopyer) lookupSignature(file *os.File, path string, job *baseJob) (*signature.Entry, []byte) {
	// Treat every unusable cache read as a non-fatal miss with diagnostics.
	entry, status, err := c.fs().ReadEntry(file, job.stat.info)
	if err != nil {
		c.cacheStats.recordFailure(path, err)
		c.cacheStats.count(cacheMiss)
		return nil, nil
	}

	// Distinguish metadata staleness from an absent entry for aggregate reporting.
	switch status {
	case signature.Fresh:
		if hash := job.storedHash(&entry); hash != nil {
			c.cacheStats.count(cacheHit)
			return &entry, append([]byte(nil), hash...)
		}
		c.cacheStats.count(cacheMiss)
		return &entry, nil
	case signature.Stale:
		c.cacheStats.count(cacheStale)
	default:
		c.cacheStats.count(cacheMiss)
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
		c.writeSignature(file, path, entry)
	}
}

// refreshCachePath publishes through a stable, caller-owned path after its descriptor closes.
// The caller keeps the path unchanged until publication and any target replacement finish.
func (c *StreamCopyer) refreshCachePath(path string, job *baseJob, target bool) {
	if entry, ok := c.cacheEntry(path, job, target); ok {
		c.recordSignatureWrite(path, c.fs().WriteEntryPath(path, entry))
	}
}

// refreshHash returns the hash this run computed when the policy refreshes the cache and the
// side has not opted out of writing it.
func (c *StreamCopyer) refreshHash(job *baseJob, target bool) ([]byte, bool) {
	// Only a completed content hash under a refresh policy can publish a hash.
	if c.cacheStats == nil || !c.hashPolicy.refreshesCache() {
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

func (c *StreamCopyer) cacheEntry(path string, job *baseJob, target bool) (signature.Entry, bool) {
	hash, ok := c.refreshHash(job, target)
	if !ok {
		return signature.Entry{}, false
	}

	// A fresh target has no entry to merge with or skip.
	if target {
		return signature.Entry{Size: job.stat.size, MtimeNS: job.stat.modTime.UnixNano(), SHA256: hash}, true
	}

	// A source keeps the half of its fresh entry this read did not compute, and skips an unchanged write.
	entry := signature.Entry{Size: job.stat.info.Size(), MtimeNS: job.stat.info.ModTime().UnixNano()}
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
		entry.Decoded = &signature.Decoded{Encoding: job.decodeEncoding, Size: job.stat.size, SHA256: hash}
	}
	if existing != nil && existing.Equal(entry) {
		return signature.Entry{}, false
	}
	return entry, true
}

// encodedEntry describes an encoded target: the stored file's own size and the source mtime that
// the metadata restore applies, with the plaintext facts as the encoded-file marker. The hashes of
// the stored bytes and of the plaintext are the only optional parts. The marker is written under
// every hash policy; it holds both hashes whenever the transfer computed them and none otherwise.
func (c *StreamCopyer) encodedEntry(job *baseJob, stored int64, storedHash []byte) signature.Entry {
	entry := signature.Entry{
		Size:    stored,
		MtimeNS: job.stat.modTime.UnixNano(),
		Decoded: &signature.Decoded{Encoding: job.encodeEncoding, Size: job.stat.size},
	}
	if hash, ok := job.computedHash(); ok && len(hash) == sha256.Size && len(storedHash) == sha256.Size {
		entry.SHA256 = storedHash
		entry.Decoded.SHA256 = hash
	}
	return entry
}

// writeEncodedMarker publishes the entry that marks a target as encoded, through the descriptor
// of a random output or, when the descriptor is already closed (nil file), the owned temporary
// path of a linear one. Unlike a plain cache write, its failure is the caller's error, including
// a file system without the attribute.
func (c *StreamCopyer) writeEncodedMarker(file *os.File, path string, job *baseJob, stored int64, storedHash []byte) error {
	entry := c.encodedEntry(job, stored, storedHash)
	var err error
	if file == nil {
		err = c.fs().WriteEntryPath(path, entry)
	} else {
		err = c.fs().WriteEntry(file, entry)
	}
	if err != nil {
		return err
	}
	if c.cacheStats != nil {
		c.cacheStats.count(cacheWrite)
	}
	return nil
}

// write publishes one computed entry through the descriptor that owns the file. The snapshot
// is stored unchanged: an entry describes the version that was hashed, the reader treats an
// entry whose size or mtime no longer match as stale, and re-deriving it from live metadata would
// bind the hash to a version it never described. Every failure is an aggregate warning, because a
// cache that cannot store an entry never fails the item that computed it.
func (c *StreamCopyer) writeSignature(file *os.File, path string, entry signature.Entry) {
	c.recordSignatureWrite(path, c.fs().WriteEntry(file, entry))
}

func (c *StreamCopyer) recordSignatureWrite(path string, err error) {
	// Descriptor and path publication share unsupported handling and aggregate warnings.
	if err != nil {
		if signature.Ignorable(err) {
			return
		}
		c.cacheStats.recordFailure(path, fmt.Errorf("write signature xattr failed, %w", err))
		return
	}

	// Count only entries accepted by the file system.
	c.cacheStats.count(cacheWrite)
}

func (c *StreamCopyer) reportCacheStats() {
	if c.cacheStats == nil {
		return
	}

	// Every item has published its own entry by the time the pipeline ends, so the aggregate
	// summary is complete here.
	summary := c.cacheStats.snapshot()
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
