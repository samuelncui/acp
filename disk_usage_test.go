package acp

import (
	"errors"
	"math"
	"syscall"
	"testing"
)

// TestMappingErrorIdentities pins the identity of every mapped target failure. An I/O error
// describes one target, not a device that turned read-only, so it must not abort the target.
func TestMappingErrorIdentities(t *testing.T) {
	tests := []struct {
		name      string
		from      error
		want      error
		wantAbort bool
	}{
		{name: "no space", from: syscall.ENOSPC, want: ErrTargetNoSpace, wantAbort: true},
		{name: "read-only device", from: syscall.EROFS, want: ErrTargetDropToReadonly, wantAbort: true},
		{name: "io error", from: syscall.EIO, want: ErrTargetIO, wantAbort: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mapped := mappingError(tt.from)
			if !errors.Is(mapped, tt.want) {
				t.Fatalf("mappingError(%v) = %v, want %v", tt.from, mapped, tt.want)
			}
			if got := checkErrorAbort(mapped); got != tt.wantAbort {
				t.Fatalf("checkErrorAbort(%v) = %t, want %t", mapped, got, tt.wantAbort)
			}
		})
	}

	// A target I/O error must never be classified as a device that dropped to read-only.
	if errors.Is(mappingError(syscall.EIO), ErrTargetDropToReadonly) {
		t.Fatal("an I/O error is still classified as a read-only device")
	}
	if err := mappingError(nil); err != nil {
		t.Fatalf("mappingError(nil) = %v, want nil", err)
	}
}

// TestDiskUsageRefreshKeepsInflightReservations pins the reservation accounting across a
// refresh: a new measurement replaces the capacity estimate, never the commitments already
// made against it on the same mount point.
func TestDiskUsageRefreshKeepsInflightReservations(t *testing.T) {
	cache := newDiskUsageCache(t.TempDir(), math.MaxInt64)
	if err := cache.check(1); err != nil {
		t.Fatalf("first check: %v", err)
	}

	cache.lock.Lock()
	// An in-flight copy has already reserved everything the device reported as free.
	cache.used = cache.freeSpace + 1
	cache.lock.Unlock()

	// The next check refreshes, so the measurement can prove nothing landed; the reservation
	// of the copy still in flight must reject the new one.
	if err := cache.check(0); !errors.Is(err, ErrTargetNoSpace) {
		t.Fatalf("check() = %v, want %v", err, ErrTargetNoSpace)
	}
}

// TestDiskUsageAccountsLandedBytes pins the other half of the refresh: bytes that landed since
// the previous measurement have already consumed the measured capacity, so they stop holding a
// reservation of their own instead of being counted twice.
func TestDiskUsageAccountsLandedBytes(t *testing.T) {
	cache := newDiskUsageCache("unused", math.MaxInt64)

	// 800 bytes are reserved against a 1000-byte estimate, and 250 bytes landed since.
	cache.freeSpace = 1000
	cache.used = 800
	if err := cache.account(750, 200); err != nil {
		t.Fatalf("account() = %v, want nil", err)
	}
	if cache.freeSpace != 750 || cache.used != 550 {
		t.Fatalf("cache = free %d used %d, want free 750 used 550", cache.freeSpace, cache.used)
	}

	// The request asking for space right now keeps its own reservation even when everything
	// counted before it has landed.
	cache.freeSpace = 1000
	cache.used = 10
	if err := cache.account(999, 100); err != nil {
		t.Fatalf("account() = %v, want nil", err)
	}
	if cache.used != 100 {
		t.Fatalf("used = %d, want the current request's 100", cache.used)
	}

	// A reservation that survives the refresh is still checked against the new capacity.
	cache.freeSpace = 100
	cache.used = 900
	if err := cache.account(50, 200); !errors.Is(err, ErrTargetNoSpace) {
		t.Fatalf("account() = %v, want %v", err, ErrTargetNoSpace)
	}
}
