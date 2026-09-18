package acp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCachedSignatureCodec(t *testing.T) {
	// Round-trip every signed and fixed-width field through the stable codec.
	want := CachedSignature{
		Size:    42,
		MtimeNS: -123456789,
		SHA256:  sha256.Sum256([]byte("fixture")),
	}
	encoded := encodeCachedSignature(want)
	got, err := DecodeCachedSignature(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("decoded signature = %#v, want %#v", got, want)
	}

	// Reject each independent header invariant without accepting partial data.
	tests := []struct {
		name   string
		mutate func([]byte) []byte
	}{
		{name: "size", mutate: func(value []byte) []byte { return value[:len(value)-1] }},
		{name: "magic", mutate: func(value []byte) []byte { value[0] ^= 0xff; return value }},
		{name: "version", mutate: func(value []byte) []byte { value[4]++; return value }},
		{name: "algorithm", mutate: func(value []byte) []byte { value[5]++; return value }},
		{name: "reserved", mutate: func(value []byte) []byte { value[6] = 1; return value }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			invalid := tt.mutate(append([]byte(nil), encoded...))
			if _, err := DecodeCachedSignature(invalid); err == nil {
				t.Fatal("DecodeCachedSignature() error = nil")
			}
		})
	}
}

func TestSignatureCacheWritePreservesACPSnapshot(t *testing.T) {
	// Build a signature from ACP's completed result rather than live file metadata.
	content := []byte("snapshot fixture")
	indexed := &stat{
		size:    int64(len(content)),
		modTime: time.Unix(100, 123),
	}
	hash := sha256.Sum256(content)
	want, err := newCachedSignature(hash[:], indexed)
	if err != nil {
		t.Fatal(err)
	}

	// Enqueueing must preserve that immutable snapshot even when the path differs.
	path := filepath.Join(t.TempDir(), "changed.bin")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), len(content)), 0o644); err != nil {
		t.Fatal(err)
	}
	changed := time.Unix(200, 456)
	if err := os.Chtimes(path, changed, changed); err != nil {
		t.Fatal(err)
	}
	cache := &signatureCache{queue: make(chan signatureWrite, 1)}
	cache.enqueue(path, want)
	write := <-cache.queue
	if write.signature != want {
		t.Fatalf("queued signature = %#v, want %#v", write.signature, want)
	}

	// Seed a decoy so the assertion below proves the writer replaced it, and
	// skip on filesystems that cannot store the managed attribute at all.
	seed, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	seedErr := writeSignatureXattr(seed, encodeCachedSignature(CachedSignature{Size: 7, MtimeNS: 7}))
	_ = seed.Close()
	if seedErr != nil {
		t.Skipf("temporary filesystem does not support signature xattrs: %v", seedErr)
	}

	// The writer publishes the queued snapshot as-is. Re-deriving it from live
	// metadata instead would bind this hash to an unrelated version of the file.
	cache.write(write)
	if cache.summary.Writes != 1 || cache.summary.Failures != 0 {
		t.Fatalf("signature summary = %#v", cache.summary)
	}

	// Inspect the raw attribute to confirm ACP's snapshot landed unchanged.
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	encoded, err := readSignatureXattr(file)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := DecodeCachedSignature(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if stored != want {
		t.Fatalf("stored signature = %#v, want %#v", stored, want)
	}

	// The entry is harmless because the reader owns staleness detection: a
	// snapshot whose size and mtime no longer match the file reads back as miss.
	_, valid, err := ReadCachedSignature(path)
	if err != nil {
		t.Fatal(err)
	}
	if valid {
		t.Fatal("snapshot for a changed file was read back as a cache hit")
	}
}

func TestRunHashPolicyMatrix(t *testing.T) {
	// Every policy reuses, reads, or ignores the stored hash, and only a refresh writes.
	// A seeded entry is metadata-valid but wrong, so a policy that reads content must
	// report the computed hash instead of the stored one.
	requireSignatureXattrSupport(t)

	content := []byte("policy fixture")
	stale := []byte("stale content")

	tests := []struct {
		name       string
		policy     HashPolicy
		seeded     bool
		wantHash   string
		wantHit    bool
		wantHits   int64
		wantMisses int64
		wantWrites int64
		wantStored string
	}{
		{name: "off", policy: HashOff, wantHash: "none", wantStored: "none"},
		{name: "off with stored entry", policy: HashOff, seeded: true, wantHash: "none", wantStored: "seeded"},
		{name: "cached only miss", policy: HashCachedOnly, wantHash: "none", wantMisses: 1, wantStored: "none"},
		{
			name: "cached only hit", policy: HashCachedOnly, seeded: true,
			wantHash: "seeded", wantHit: true, wantHits: 1, wantStored: "seeded",
		},
		{
			name: "cached or read miss", policy: HashCachedOrRead,
			wantHash: "computed", wantMisses: 1, wantStored: "none",
		},
		{
			name: "cached or read hit", policy: HashCachedOrRead, seeded: true,
			wantHash: "seeded", wantHit: true, wantHits: 1, wantStored: "seeded",
		},
		{
			name: "cached or read refresh miss", policy: HashCachedOrReadRefresh,
			wantHash: "computed", wantMisses: 1, wantWrites: 1, wantStored: "computed",
		},
		{
			name: "cached or read refresh hit", policy: HashCachedOrReadRefresh, seeded: true,
			wantHash: "seeded", wantHit: true, wantHits: 1, wantStored: "seeded",
		},
		{
			name: "read", policy: HashRead, seeded: true,
			wantHash: "computed", wantStored: "seeded",
		},
		{
			name: "read refresh", policy: HashReadRefresh, seeded: true,
			wantHash: "computed", wantWrites: 1, wantStored: "computed",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := writeSourceFile(t, t.TempDir(), "source.txt", content)
			var seeded CachedSignature
			if test.seeded {
				seeded = seedStaleSignature(t, input, stale)
			}

			result, summary := runSignatureHash(t, input, test.policy)

			switch test.wantHash {
			case "none":
				if len(result.SHA256) != 0 {
					t.Fatalf("SHA256 = %x, want none", result.SHA256)
				}
			case "seeded":
				if !bytes.Equal(result.SHA256, seeded.SHA256[:]) {
					t.Fatalf("SHA256 = %x, want the stored %x", result.SHA256, seeded.SHA256)
				}
			case "computed":
				want := sha256.Sum256(content)
				if !bytes.Equal(result.SHA256, want[:]) {
					t.Fatalf("SHA256 = %x, want %x", result.SHA256, want)
				}
			}
			if result.SignatureCacheHit != test.wantHit {
				t.Fatalf("SignatureCacheHit = %t, want %t", result.SignatureCacheHit, test.wantHit)
			}
			if summary.Hits != test.wantHits || summary.Misses != test.wantMisses ||
				summary.Writes != test.wantWrites || summary.Failures != 0 {
				t.Fatalf("summary = %#v, want hits=%d misses=%d writes=%d", summary, test.wantHits, test.wantMisses, test.wantWrites)
			}

			stored, valid := readStoredSignature(t, input)
			switch test.wantStored {
			case "none":
				if valid {
					t.Fatalf("stored signature = %#v, want none", stored)
				}
			case "seeded":
				if !valid || stored != seeded {
					t.Fatalf("stored signature = %#v, valid=%t, want %#v", stored, valid, seeded)
				}
			case "computed":
				want := sha256.Sum256(content)
				if !valid || stored.SHA256 != want {
					t.Fatalf("stored signature = %#v, valid=%t, want %x", stored, valid, want)
				}
			}
		})
	}
}

func TestRunRefreshWritesOnlyWhenStoredHashDiffers(t *testing.T) {
	content := []byte("refresh fixture")
	input := writeSourceFile(t, t.TempDir(), "source.txt", content)
	requireSignatureXattrSupport(t)

	// The first refresh stores the computed hash.
	result, summary := runSignatureHash(t, input, HashReadRefresh)
	want := sha256.Sum256(content)
	if !bytes.Equal(result.SHA256, want[:]) || summary.Writes != 1 {
		t.Fatalf("first refresh = %x / %#v", result.SHA256, summary)
	}
	stored, valid := readStoredSignature(t, input)
	if !valid {
		t.Skip("temporary filesystem does not support signature xattrs")
	}

	// An identical rewrite leaves the stored entry untouched.
	if _, summary = runSignatureHash(t, input, HashReadRefresh); summary.Writes != 0 {
		t.Fatalf("identical refresh wrote %d signatures, want 0", summary.Writes)
	}
	if again, valid := readStoredSignature(t, input); !valid || again != stored {
		t.Fatalf("stored signature changed to %#v, want %#v", again, stored)
	}

	// A changed source content produces a different hash, which must be republished.
	changed := []byte("changed fixture")
	if len(changed) != len(content) {
		t.Fatalf("test fixture requires equal content lengths, got %d and %d", len(changed), len(content))
	}
	if err := os.WriteFile(input, changed, 0o644); err != nil {
		t.Fatal(err)
	}
	result, summary = runSignatureHash(t, input, HashReadRefresh)
	want = sha256.Sum256(changed)
	if !bytes.Equal(result.SHA256, want[:]) || summary.Writes != 1 {
		t.Fatalf("changed refresh = %x / %#v", result.SHA256, summary)
	}
}

func TestRunTransferAlwaysReadsAndRefreshesTargets(t *testing.T) {
	// Prepare an existing target so the transfer exercises overwrite invalidation.
	content := []byte("transfer fixture")
	root := t.TempDir()
	input := writeSourceFile(t, root, "source.txt", content)
	target := writeSourceFile(t, root, "target.txt", []byte("old"))

	// Seed metadata-valid but incorrect entries to prove a transfer ignores them.
	seedStaleSignature(t, input, []byte("wrong source"))
	seedStaleSignature(t, target, []byte("wrong target"))

	// A request with targets always reads its source, so reuse cannot apply there.
	item := newFixtureItem(input, target)
	var summary SignatureCacheSummary
	handler := func(event Event) {
		if update, ok := event.(*EventSignatureCacheSummary); ok {
			summary = update.Summary
		}
	}
	if err := Run(
		context.Background(),
		newSliceSource(item),
		WithHashPolicy(HashCachedOrReadRefresh),
		SetToDevice(Overwrite(true)),
		WithEventHandler(handler),
	); err != nil {
		t.Fatal(err)
	}

	wantHash := sha256.Sum256(content)
	result, err := item.terminal(t)
	if err != nil {
		t.Fatalf("item failed: %v", err)
	}
	if result.SignatureCacheHit || !bytes.Equal(result.SHA256, wantHash[:]) {
		t.Fatalf("transfer result = %#v, want the computed hash %x", result, wantHash)
	}

	// Both bytes and refreshed source and target signatures must be present.
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("target content = %q, want %q", got, content)
	}
	for _, path := range []string{input, target} {
		signature, valid := readStoredSignature(t, path)
		if !valid || signature.SHA256 != wantHash {
			t.Fatalf("signature for %q = %#v, valid=%t", path, signature, valid)
		}
	}
	if summary.Hits != 0 || summary.Writes != 2 {
		t.Fatalf("transfer summary = %#v", summary)
	}
}

func TestOverwriteInvalidatesSignatureWithoutCache(t *testing.T) {
	// Give the old and new content identical metadata so only explicit invalidation is safe.
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
	seeded := CachedSignature{Size: 3, MtimeNS: modified.UnixNano(), SHA256: sha256.Sum256([]byte("old"))}
	file, err := os.Open(target)
	if err != nil {
		t.Fatal(err)
	}
	err = writeSignatureXattr(file, encodeCachedSignature(seeded))
	_ = file.Close()
	if err != nil {
		t.Skipf("temporary filesystem does not support signature xattrs: %v", err)
	}

	// A run without a hash policy must still drop the stale stored signature.
	item := newFixtureItem(source, target)
	if err := Run(context.Background(), newSliceSource(item), SetToDevice(Overwrite(true))); err != nil {
		t.Fatal(err)
	}
	if signature, valid, err := ReadCachedSignature(target); err != nil {
		t.Fatal(err)
	} else if valid {
		t.Fatalf("overwritten target retained a valid signature: %#v", signature)
	}
}

func TestRunCorruptSignatureIsWarning(t *testing.T) {
	// Seed an undecodable managed xattr on an otherwise valid source.
	input := writeSourceFile(t, t.TempDir(), "source.txt", nil)
	file, err := os.Open(input)
	if err != nil {
		t.Fatal(err)
	}
	err = writeSignatureXattr(file, []byte("corrupt"))
	_ = file.Close()
	if err != nil {
		t.Skipf("temporary filesystem does not support signature xattrs: %v", err)
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
}

func TestRunSignatureCacheZeroLength(t *testing.T) {
	// Populate the valid SHA-256 signature of an empty file.
	input := writeSourceFile(t, t.TempDir(), "empty", nil)

	// Hash the empty content and require the asynchronous cache write to drain.
	first, summary := runSignatureHash(t, input, HashCachedOrReadRefresh)
	if want := sha256.Sum256(nil); !bytes.Equal(first.SHA256, want[:]) {
		t.Fatalf("empty SHA256 = %x, want %x", first.SHA256, want)
	}
	if summary.Writes != 1 {
		t.Fatalf("empty cache summary = %#v", summary)
	}
	if _, valid := readStoredSignature(t, input); !valid {
		t.Skip("temporary filesystem does not support signature xattrs")
	}

	// Reuse the zero-length cache entry without reopening content.
	second, summary := runSignatureHash(t, input, HashCachedOrReadRefresh)
	if !second.SignatureCacheHit || summary.Hits != 1 {
		t.Fatalf("empty cache result = %#v, summary = %#v", second, summary)
	}
}

func TestSignatureCacheWarningSamplesAreBounded(t *testing.T) {
	// Inject more failures than the diagnostic sample limit.
	cache := newSignatureCache(1)
	for idx := 0; idx < signatureSampleLimit+3; idx++ {
		cache.recordFailure(fmt.Sprintf("path-%d", idx), errors.New("injected failure"))
	}
	summary := cache.closeAndWait()

	// Preserve the aggregate count while bounding retained paths.
	if summary.Failures != signatureSampleLimit+3 {
		t.Fatalf("failures = %d, want %d", summary.Failures, signatureSampleLimit+3)
	}
	if len(summary.Samples) != signatureSampleLimit {
		t.Fatalf("samples = %d, want %d", len(summary.Samples), signatureSampleLimit)
	}
}

func TestRunSignatureCacheDrainsConcurrentWrites(t *testing.T) {
	// Build more sources than the writer pool can process at once.
	root := t.TempDir()
	items := make([]Item, 0, 32)
	want := make(map[string][32]byte)
	for idx := 0; idx < 32; idx++ {
		path := writeSourceFile(t, root, fmt.Sprintf("%02d.bin", idx), []byte(fmt.Sprintf("concurrent signature %d", idx)))
		items = append(items, newFixtureItem(path))
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		want[path] = sha256.Sum256(content)
	}

	// Run concurrent hashing and require every asynchronous xattr write to drain.
	if err := Run(
		context.Background(),
		newSliceSource(items...),
		WithHashPolicy(HashReadRefresh),
		SetFromDevice(DeviceThreads(4)),
	); err != nil {
		t.Fatal(err)
	}

	// Confirm all cache writes are visible after Run returns.
	for path, expected := range want {
		signature, valid := readStoredSignature(t, path)
		if !valid {
			t.Skip("temporary filesystem does not support signature xattrs")
		}
		if signature.SHA256 != expected {
			t.Fatalf("signature for %q = %x, want %x", path, signature.SHA256, expected)
		}
	}
}

func TestRunSignatureCacheDrainsAfterCancellation(t *testing.T) {
	// Cancel only after the first completed item has left the pipeline.
	content := []byte("cancellation fixture")
	path := writeSourceFile(t, t.TempDir(), "cancellation.bin", content)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	item := newFixtureItem(path)
	item.hook = func(*Result, error) { cancel() }
	source := &blockingSource{batch: []Item{item}, endErr: nil}

	err := Run(ctx, source, WithHashPolicy(HashReadRefresh))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context cancellation", err)
	}

	// The completed item's cache write must drain despite caller cancellation.
	result, terminalErr := item.terminal(t)
	if terminalErr != nil {
		t.Fatalf("item failed: %v", terminalErr)
	}
	signature, valid := readStoredSignature(t, path)
	if !valid {
		t.Skip("temporary filesystem does not support signature xattrs")
	}
	if signature.SHA256 != sha256.Sum256(content) || !bytes.Equal(result.SHA256, signature.SHA256[:]) {
		t.Fatalf("signature = %x, result = %x", signature.SHA256, result.SHA256)
	}
}

func TestRunSignatureCacheIsBestEffortForReadOnlyFile(t *testing.T) {
	// Hash content successfully even if the filesystem rejects the cache write.
	path := writeSourceFile(t, t.TempDir(), "readonly.bin", []byte("read-only signature fixture"))
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(content)

	result, summary := runSignatureHash(t, path, HashReadRefresh)
	if !bytes.Equal(result.SHA256, want[:]) {
		t.Fatalf("SHA256 = %x, want %x", result.SHA256, want)
	}

	// Accept either a valid owner-writable xattr or a non-fatal warning.
	signature, valid := readStoredSignature(t, path)
	if valid && signature.SHA256 != want {
		t.Fatalf("signature = %x, want %x", signature.SHA256, want)
	}
	if !valid && summary.Failures == 0 {
		t.Fatalf("read-only cache miss had no warning: %#v", summary)
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
	if err := Run(
		context.Background(),
		newSliceSource(item),
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
func seedStaleSignature(t *testing.T, path string, content []byte) CachedSignature {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	stale := CachedSignature{
		Size:    info.Size(),
		MtimeNS: info.ModTime().UnixNano(),
		SHA256:  sha256.Sum256(content),
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	err = writeSignatureXattr(file, encodeCachedSignature(stale))
	_ = file.Close()
	if err != nil {
		t.Skipf("temporary filesystem does not support signature xattrs: %v", err)
	}
	return stale
}

// requireSignatureXattrSupport skips a test when the temporary filesystem cannot store the
// managed attribute at all.
func requireSignatureXattrSupport(t *testing.T) {
	t.Helper()

	path := writeSourceFile(t, t.TempDir(), "probe", []byte("probe"))
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	err = writeSignatureXattr(file, encodeCachedSignature(CachedSignature{Size: 5, MtimeNS: 1}))
	_ = file.Close()
	if err != nil {
		t.Skipf("temporary filesystem does not support signature xattrs: %v", err)
	}
}

// readStoredSignature returns the signature ACP currently stores for path.
func readStoredSignature(t *testing.T, path string) (CachedSignature, bool) {
	t.Helper()

	signature, valid, err := ReadCachedSignature(path)
	if err != nil {
		t.Fatalf("read cached signature for %q: %v", path, err)
	}
	return signature, valid
}
