package acp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/samuelncui/acp/signature"
)

func TestSignatureCacheWriteRetainsPriorRunMetadata(t *testing.T) {
	// Prepare one stable file and its metadata for a completed cache publication.
	content := []byte("snapshot fixture")
	path := writeSourceFile(t, t.TempDir(), "source.bin", content)
	modified := time.Unix(100, 123)
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}
	indexed := &stat{
		size:    int64(len(content)),
		modTime: modified,
	}
	hash := sha256.Sum256(content)
	entry := signature.Entry{Size: indexed.size, MtimeNS: indexed.modTime.UnixNano(), SHA256: hash[:]}

	// Open the descriptor the item would own, seed a decoy so the assertion below proves the
	// writer replaced it, and skip on filesystems that cannot store the attribute at all.
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := signature.WriteFile(file, signature.Entry{Size: 7, MtimeNS: 7, SHA256: sha("decoy")}); err != nil {
		t.Skipf("temporary filesystem does not support signature xattrs: %v", err)
	}

	// Publish the stable file's hash through its owned descriptor.
	copyer := newTestStream(t, WithHashPolicy(HashReadRefresh))
	copyer.cacheStats = &cacheStats{}
	copyer.writeSignature(file, path, entry)
	if summary := copyer.cacheStats.snapshot(); summary.Writes != 1 || summary.Failures != 0 {
		t.Fatalf("signature summary = %#v", summary)
	}

	// Read the attribute back to confirm ACP's snapshot landed unchanged.
	stored, status, err := signature.ReadFile(file, mustStat(t, file))
	if err != nil || status != signature.Fresh || !stored.Equal(entry) {
		t.Fatalf("stored signature = %#v %v / %v, want a fresh %#v", stored, status, err, entry)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	// A later modification leaves the completed entry stale for the next lookup.
	changed := time.Unix(200, 456)
	if err := os.Chtimes(path, changed, changed); err != nil {
		t.Fatal(err)
	}
	_, valid, err := signature.ReadCached(path)
	if err != nil {
		t.Fatal(err)
	}
	if valid {
		t.Fatal("snapshot for a changed file was read back as a cache hit")
	}
}

func TestOverwriteDropsOldSignatureWithoutCache(t *testing.T) {
	// Give old and new content identical metadata so retaining the old cache would look valid.
	root := t.TempDir()
	source := writeSourceFile(t, root, "source.txt", []byte("new"))
	target := writeSourceFile(t, root, "target.txt", []byte("old"))
	modified := time.Unix(100, 123)
	for _, path := range []string{source, target} {
		if err := os.Chtimes(path, modified, modified); err != nil {
			t.Fatal(err)
		}
	}

	// Attach the old signature after the shared metadata has been fixed.
	seeded := signature.Entry{Size: 3, MtimeNS: modified.UnixNano(), SHA256: sha("old")}
	if err := signature.WritePath(target, seeded); err != nil {
		t.Skipf("temporary filesystem does not support signature xattrs: %v", err)
	}

	// Replacing the target must discard its old signature even without a hash policy.
	item := newFixtureItem(source, target)
	if err := runFixture(
		context.Background(),
		newStreamFixture(item),
		[]Item{item},
		Overwrite(true),
	); err != nil {
		t.Fatal(err)
	}
	if signature, valid, err := signature.ReadCached(target); err != nil {
		t.Fatal(err)
	} else if valid {
		t.Fatalf("overwritten target retained a valid signature: %#v", signature)
	}
}

func TestRunCorruptSignatureIsWarning(t *testing.T) {
	// Probe support before requiring malformed and oversized attributes to exercise fallback.
	requireSignatureXattrSupport(t)
	for name, value := range map[string][]byte{
		"short":     []byte("corrupt"),
		"one extra": append(legacyLayout(signature.Entry{SHA256: make([]byte, sha256.Size)}), 0),
		"oversized": make([]byte, 112),
		"too long":  bytes.Repeat([]byte{0x0a, 0x00}, 512),
	} {
		t.Run(name, func(t *testing.T) {
			// A bounded xattr read must reject malformed data without accepting a valid prefix.
			input := writeSourceFile(t, t.TempDir(), "source.txt", nil)
			file, err := os.Open(input)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			if err := writeRawSignature(file, value); err != nil {
				t.Fatal(err)
			}

			// Fall back to content hashing while surfacing only an aggregate warning.
			result, summary := runSignatureHash(t, input, HashCachedOrReadRefresh)
			want := sha256.Sum256(nil)
			if result.SignatureCacheHit || !bytes.Equal(result.SHA256, want[:]) {
				t.Fatalf("result = %#v, want the computed hash %x", result, want)
			}
			if summary.Failures != 1 || summary.Misses != 1 || summary.Writes != 1 {
				t.Fatalf("summary = %#v", summary)
			}
			if summary.FirstError == "" || len(summary.Samples) != 1 || summary.Samples[0] != input {
				t.Fatalf("warning details = %#v", summary)
			}
		})
	}
}

func TestRunSignatureCacheZeroLength(t *testing.T) {
	// Populate the valid SHA-256 signature of an empty file.
	requireSignatureXattrSupport(t)
	input := writeSourceFile(t, t.TempDir(), "empty", nil)

	// Hash the empty content; the item publishes its entry through its own descriptor, so the
	// write has already happened when the run returns.
	first, summary := runSignatureHash(t, input, HashCachedOrReadRefresh)
	if want := sha256.Sum256(nil); !bytes.Equal(first.SHA256, want[:]) {
		t.Fatalf("empty SHA256 = %x, want %x", first.SHA256, want)
	}
	if summary.Writes != 1 {
		t.Fatalf("empty cache summary = %#v", summary)
	}
	if _, valid := readStoredSignature(t, input); !valid {
		t.Fatal("the empty file's signature was not published")
	}

	// Reuse the zero-length cache entry without reopening content.
	second, summary := runSignatureHash(t, input, HashCachedOrReadRefresh)
	if !second.SignatureCacheHit || summary.Hits != 1 {
		t.Fatalf("empty cache result = %#v, summary = %#v", second, summary)
	}
}

func TestSignatureCacheWarningSamplesAreBounded(t *testing.T) {
	// Inject more failures than the diagnostic sample limit.
	cache := &cacheStats{}
	for idx := 0; idx < signatureSampleLimit+3; idx++ {
		cache.recordFailure(fmt.Sprintf("path-%d", idx), errors.New("injected failure"))
	}
	summary := cache.snapshot()

	// Preserve the aggregate count while bounding retained paths.
	if summary.Failures != signatureSampleLimit+3 {
		t.Fatalf("failures = %d, want %d", summary.Failures, signatureSampleLimit+3)
	}
	if len(summary.Samples) != signatureSampleLimit {
		t.Fatalf("samples = %d, want %d", len(summary.Samples), signatureSampleLimit)
	}
}

// runSignatureHash runs one targetless item with the requested policy and returns its
// result together with the aggregate cache summary.
func runSignatureHash(t *testing.T, input string, policy HashPolicy) (*Result, SignatureCacheSummary) {
	t.Helper()

	var summary SignatureCacheSummary
	handler := func(event Event) {
		if update, ok := event.(*EventSignatureCacheSummary); ok {
			summary = update.Summary
		}
	}

	item := newFixtureItem(input)
	if err := runFixture(
		context.Background(),
		newStreamFixture(item),
		[]Item{item},
		WithHashPolicy(policy),
		WithEventHandler(handler),
	); err != nil {
		t.Fatal(err)
	}
	result, err := item.terminal(t)
	if err != nil {
		t.Fatalf("item failed: %v", err)
	}
	return result, summary
}

// seedStaleSignature stores a metadata-valid signature for content the file does not hold,
// so a policy that reads content reports a different hash than the stored one.
func seedStaleSignature(t *testing.T, path string, content []byte) signature.Entry {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	stale := signature.Entry{Size: info.Size(), MtimeNS: info.ModTime().UnixNano(), SHA256: sha(string(content))}
	if err := signature.WritePath(path, stale); err != nil {
		t.Skipf("temporary filesystem does not support signature xattrs: %v", err)
	}
	return stale
}

// requireSignatureXattrSupport skips a test when the temporary filesystem cannot store the
// managed attribute at all.
func requireSignatureXattrSupport(t *testing.T) {
	t.Helper()

	path := writeSourceFile(t, t.TempDir(), "probe", []byte("probe"))
	if err := signature.WritePath(path, signature.Entry{Size: 5, MtimeNS: 1}); err != nil {
		t.Skipf("temporary filesystem does not support signature xattrs: %v", err)
	}
}

// readStoredSignature returns the entry ACP currently stores for path and whether it is a fresh
// plain hash.
func readStoredSignature(t *testing.T, path string) (signature.Entry, bool) {
	t.Helper()

	entry, valid, err := signature.ReadCached(path)
	if err != nil {
		t.Fatalf("read cached signature for %q: %v", path, err)
	}
	return entry, valid
}

// legacyLayout returns the legacy fixed-size entry layout that earlier releases wrote. The
// signature package reads it but never writes it, so a test that seeds one needs the raw bytes.
func legacyLayout(value signature.Entry) []byte {
	encoded := make([]byte, 56)
	copy(encoded[:4], "ACPS")
	encoded[4] = 1
	encoded[5] = 1
	binary.BigEndian.PutUint64(encoded[8:16], uint64(value.Size))
	binary.BigEndian.PutUint64(encoded[16:24], uint64(value.MtimeNS))
	copy(encoded[24:], value.SHA256)
	return encoded
}

// mustStat returns the metadata of an open file.
func mustStat(t *testing.T, file *os.File) os.FileInfo {
	t.Helper()
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func sha(text string) []byte {
	sum := sha256.Sum256([]byte(text))
	return sum[:]
}

// seedEntry stores an entry exactly as given on a file.
func seedEntry(t *testing.T, path string, entry signature.Entry) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := signature.WriteFile(file, entry); err != nil {
		t.Fatal(err)
	}
}
