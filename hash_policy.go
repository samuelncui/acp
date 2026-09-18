package acp

import "fmt"

// HashPolicy selects how an item's content hash is produced and how the stored hash
// cache is used. Hashing and the cache are one policy, because refreshing the cache
// requires a computed hash: a miss under a refresh-enabled value must read.
type HashPolicy uint8

const (
	// HashOff produces no hash and leaves the cache untouched.
	HashOff HashPolicy = iota
	// HashCachedOnly reuses a stored hash; a miss leaves the item without one.
	HashCachedOnly
	// HashCachedOrRead reuses a stored hash; a miss reads and hashes without refreshing.
	HashCachedOrRead
	// HashCachedOrReadRefresh reuses a stored hash; a miss reads, hashes and refreshes.
	HashCachedOrReadRefresh
	// HashRead always reads and hashes; the cache is not refreshed.
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

// usesCache reports whether stored hashes are read at all.
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
