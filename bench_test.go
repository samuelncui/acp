package acp

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// benchItem is the benchmark's caller-owned item: one source and no target, which is the shape a
// hash-only run uses. The push engine reads it once per submission.
type benchItem struct {
	path     string
	failures int
}

func (i *benchItem) Source() string    { return i.path }
func (i *benchItem) Targets() []string { return nil }

// Failed records one item the run could not process. It is a benchmark helper, not part of Item.
func (i *benchItem) Failed(error) { i.failures++ }

// benchHash hashes one source through a push run and fails the benchmark when the run reported a
// failure, so a broken measurement never looks like a fast one.
func benchHash(b *testing.B, path string, opts ...Option) {
	b.Helper()

	item := &benchItem{path: path}
	onResults := func(results []Result) error {
		for _, result := range results {
			if result.Err != nil {
				item.Failed(result.Err)
			}
		}
		return nil
	}

	if err := runStream(context.Background(), onResults, []Item{item}, opts...); err != nil {
		b.Fatal(err)
	}
	if item.failures != 0 {
		b.Fatalf("hash-only run failed %d items", item.failures)
	}
}

func benchFile(b *testing.B, size int64) string {
	b.Helper()

	path := filepath.Join(b.TempDir(), "content.bin")
	file, err := os.Create(path)
	if err != nil {
		b.Fatal(err)
	}
	defer file.Close()

	block := make([]byte, 1<<20)
	for index := range block {
		block[index] = byte(index)
	}
	for written := int64(0); written < size; written += int64(len(block)) {
		if _, err := file.Write(block); err != nil {
			b.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		b.Fatal(err)
	}
	return path
}

// BenchmarkReadBuffered and BenchmarkReadMapped measure the same hash-only read through
// each source read mode, which is the comparison behind reading every source buffered.
func BenchmarkReadBuffered(b *testing.B) {
	path := benchFile(b, 64<<20)
	b.SetBytes(64 << 20)
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		benchHash(b, path, WithHashPolicy(HashRead), SetFromDevice(WithReadMode(ReadBuffered)))
	}
}

func BenchmarkReadMapped(b *testing.B) {
	path := benchFile(b, 64<<20)
	b.SetBytes(64 << 20)
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		benchHash(b, path, WithHashPolicy(HashRead), SetFromDevice(WithReadMode(ReadMapped)))
	}
}

// BenchmarkRefreshSignatureUnchanged and BenchmarkRefreshSignatureChanged measure the
// cache publication path: an unchanged stored hash is compared and left alone, a changed
// one is written back. The difference is the physical write a refresh would otherwise cost
// on every scanned file.
func BenchmarkRefreshSignatureUnchanged(b *testing.B) {
	path := benchFile(b, 4096)
	// Seed the cache once so the measured runs compare against a stored value.
	benchHash(b, path, WithHashPolicy(HashReadRefresh))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		benchHash(b, path, WithHashPolicy(HashReadRefresh))
	}
}

func BenchmarkRefreshSignatureChanged(b *testing.B) {
	path := benchFile(b, 4096)
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		// A different mtime makes the stored entry stale, so the run rewrites it.
		info, err := os.Stat(path)
		if err != nil {
			b.Fatal(err)
		}
		if err := os.Chtimes(path, info.ModTime(), info.ModTime().Add(1)); err != nil {
			b.Fatal(err)
		}
		benchHash(b, path, WithHashPolicy(HashReadRefresh))
	}
}
