//go:build linux

package mmap

import (
	"errors"
	"os"
	"syscall"
	"testing"
)

// recordPrefetch replaces the madvise syscall with a spy that records every advice the prefetch
// issues, so a test can pin the advice sequence and the error path without syscall injection.
func recordPrefetch(t *testing.T, fail error) *[]int {
	t.Helper()

	previous := madvise
	advices := new([]int)
	madvise = func(_ []byte, advice int) error {
		*advices = append(*advices, advice)
		return fail
	}
	t.Cleanup(func() { madvise = previous })

	return advices
}

// TestPrefetchIssuesEveryAdvice pins the Linux prefetch. An madvise call takes exactly one advice:
// these are values, not flag bits, so passing `MADV_SEQUENTIAL|MADV_WILLNEED` keeps only the
// prefetch and silently drops the sequential hint, which is why the two hints are two calls.
func TestPrefetchIssuesEveryAdvice(t *testing.T) {
	if syscall.MADV_SEQUENTIAL|syscall.MADV_WILLNEED != syscall.MADV_WILLNEED {
		t.Fatalf("this test assumes the two constants are not flag bits on this platform")
	}

	advices := recordPrefetch(t, nil)
	reader, err := Open(writeFixture(t, "prefetch.bin", []byte("0123456789")))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = reader.Close() })

	if len(*advices) != len(linuxPrefetchAdvice) {
		t.Fatalf("prefetch issued advice %v, want %v", *advices, linuxPrefetchAdvice)
	}
	for index, want := range linuxPrefetchAdvice {
		if (*advices)[index] != want {
			t.Fatalf("prefetch issued advice %v, want %v", *advices, linuxPrefetchAdvice)
		}
	}
}

// TestPrefetchSkipsAFileAboveTheWindow pins the documented threshold: a mapping above
// prefetchMaxSize receives no advice at all, so a large copy does not fault in its whole content.
func TestPrefetchSkipsAFileAboveTheWindow(t *testing.T) {
	advices := recordPrefetch(t, nil)

	path := writeFixture(t, "large.bin", nil)
	if err := os.Truncate(path, prefetchMaxSize+1); err != nil {
		t.Fatalf("grow the fixture: %v", err)
	}
	reader, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = reader.Close() })

	if len(*advices) != 0 {
		t.Fatalf("a file above the prefetch window received advice %v", *advices)
	}
}

// TestPrefetchFailureReleasesTheMapping pins the documented error path: a mapping whose madvise
// failed is released before the error returns, and the descriptor opened for it is closed with it,
// so a host that refuses the advice cannot leak either.
func TestPrefetchFailureReleasesTheMapping(t *testing.T) {
	failure := errors.New("madvise fixture")
	recordPrefetch(t, failure)

	previousUnmap := munmap
	unmapped := 0
	munmap = func(data []byte) error {
		unmapped++
		return previousUnmap(data)
	}
	t.Cleanup(func() { munmap = previousUnmap })

	path := writeFixture(t, "advice-failure.bin", []byte("0123456789"))
	before := descriptorCount(t)

	const attempts = 8
	for index := 0; index < attempts; index++ {
		reader, err := Open(path)
		if reader != nil || err == nil {
			t.Fatalf("Open with a failing madvise = (%v, %v), want a failure", reader, err)
		}
		if !errors.Is(err, failure) {
			t.Fatalf("Open error = %v, want %v", err, failure)
		}
	}
	if unmapped != attempts {
		t.Fatalf("released %d mappings, want one per failed open", unmapped)
	}
	if after := descriptorCount(t); after > before {
		t.Fatalf("descriptors leaked: %d -> %d", before, after)
	}
}

// descriptorCount counts this process's open descriptors.
func descriptorCount(t *testing.T) int {
	t.Helper()

	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("cannot count descriptors: %v", err)
	}
	return len(entries)
}
