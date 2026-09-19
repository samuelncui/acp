package acp

import "fmt"

// HashPolicy selects how an item's content hash is produced and how the stored hash cache is
// used. Hashing and the cache are one policy, because refreshing the cache requires a
// computed hash: a miss under a refresh-enabled value must read.
//
// A targetless item may reuse a stored hash, because it records content facts without writing
// the content anywhere. A transfer always reads its source and produces a computed hash, so
// the two reuse-only values are rejected for an item that requests targets.
type HashPolicy uint8

const (
	// HashOff produces no hash and leaves the cache untouched.
	HashOff HashPolicy = iota
	// HashCachedOnly reuses a stored hash for a targetless item; a miss leaves the item
	// without a hash. Rejected for an item with targets.
	HashCachedOnly
	// HashCachedOrRead reuses a stored hash for a targetless item; a miss reads and hashes
	// without refreshing. Rejected for an item with targets, because a copy always reads and
	// a reused hash would then describe content the transfer never computed.
	HashCachedOrRead
	// HashCachedOrReadRefresh reuses a stored hash for a targetless item; a miss reads,
	// hashes and refreshes. A transfer always reads, hashes and refreshes.
	HashCachedOrReadRefresh
	// HashRead always reads and hashes; the cache is not refreshed. A targetless item
	// ignores its stored hash, and a transfer hashes the content it copies.
	HashRead
	// HashReadRefresh always reads and hashes, and refreshes the cache.
	HashReadRefresh
)

func (p HashPolicy) String() string {
	switch p {
	case HashOff:
		return "off"
	case HashCachedOnly:
		return "cached-only"
	case HashCachedOrRead:
		return "cached-or-read"
	case HashCachedOrReadRefresh:
		return "cached-or-read-refresh"
	case HashRead:
		return "read"
	case HashReadRefresh:
		return "read-refresh"
	}
	return fmt.Sprintf("unknown(%d)", uint8(p))
}

// valid reports whether the value is one of the defined policies.
func (p HashPolicy) valid() bool {
	return p <= HashReadRefresh
}

// appliesToTransfer reports whether the policy can describe an item that requests a target. A
// transfer always reads its source and produces a computed hash, so a value that promises a
// stored hash where a transfer cannot use one is rejected instead of silently reading.
func (p HashPolicy) appliesToTransfer() bool {
	switch p {
	case HashCachedOnly, HashCachedOrRead:
		return false
	}
	return true
}

// readsContent reports whether the policy needs the source content.
func (p HashPolicy) readsContent() bool {
	switch p {
	case HashCachedOrRead, HashCachedOrReadRefresh, HashRead, HashReadRefresh:
		return true
	}
	return false
}

// producesHash reports whether a read item receives a content hash.
func (p HashPolicy) producesHash() bool {
	return p != HashOff
}

// usesCache reports whether the policy touches the stored cache at all: it either reuses a stored
// hash, refreshes one with a computed hash, or both. It is what decides whether a run needs a
// signature cache, and it is true for HashRead, which hashes content and writes nothing:
// reusesCache reports the lookup, refreshesCache reports the write.
func (p HashPolicy) usesCache() bool {
	return p != HashOff
}

// reusesCache reports whether a stored hash may replace the hash of an item that does not
// write its source.
func (p HashPolicy) reusesCache() bool {
	switch p {
	case HashCachedOnly, HashCachedOrRead, HashCachedOrReadRefresh:
		return true
	}
	return false
}

// refreshesCache reports whether a computed hash is written back to the cache.
func (p HashPolicy) refreshesCache() bool {
	switch p {
	case HashCachedOrReadRefresh, HashReadRefresh:
		return true
	}
	return false
}
