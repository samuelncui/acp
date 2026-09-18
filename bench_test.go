package acp

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// benchSource supplies one targetless item, which is the shape a hash-only run uses.
type benchSource struct {
	path string
	done bool
}

func (s *benchSource) Next(context.Context) ([]Item, error) {
	if s.done {
		return nil, io.EOF
	}
	s.done = true
	return []Item{&benchItem{source: s}}, nil
}

type benchItem struct {
	source   *benchSource
	failures int
}

func (i *benchItem) Source() string    { return i.source.path }
func (i *benchItem) Targets() []string { return nil }
func (i *benchItem) Completed(*Result) {}
func (i *benchItem) Failed(error)      { i.failures++ }

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
		source := &benchSource{path: path}
		if err := Run(context.Background(), source, WithHashPolicy(HashRead), SetFromDevice(WithReadMode(ReadBuffered))); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkReadMapped(b *testing.B) {
	path := benchFile(b, 64<<20)
	b.SetBytes(64 << 20)
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		source := &benchSource{path: path}
		if err := Run(context.Background(), source, WithHashPolicy(HashRead), SetFromDevice(WithReadMode(ReadMapped))); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRefreshSignatureUnchanged and BenchmarkRefreshSignatureChanged measure the
// cache publication path: an unchanged stored hash is compared and left alone, a changed
// one is written back. The difference is the physical write a refresh would otherwise cost
// on every scanned file.
func BenchmarkRefreshSignatureUnchanged(b *testing.B) {
	path := benchFile(b, 4096)
	// Seed the cache once so the measured runs compare against a stored value.
	if err := Run(context.Background(), &benchSource{path: path}, WithHashPolicy(HashReadRefresh)); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if err := Run(context.Background(), &benchSource{path: path}, WithHashPolicy(HashReadRefresh)); err != nil {
			b.Fatal(err)
		}
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
		if err := Run(context.Background(), &benchSource{path: path}, WithHashPolicy(HashReadRefresh)); err != nil {
			b.Fatal(err)
		}
	}
}
