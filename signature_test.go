package acp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

	// The writer must reject a path that no longer matches ACP's result.
	cache.write(write)
	if cache.summary.Writes != 0 || cache.summary.Failures != 1 {
		t.Fatalf("signature summary = %#v", cache.summary)
	}
}

func TestRunStreamSignatureCacheHitStaleAndForce(t *testing.T) {
	// Create one stable source whose cache lifecycle can be observed across runs.
	content := []byte("cache fixture")
	input := filepath.Join(t.TempDir(), "source.txt")
	if err := os.WriteFile(input, content, 0o644); err != nil {
		t.Fatal(err)
	}
	wantHash := sha256.Sum256(content)

	// The first hash populates the cache before RunStream returns.
	first, firstSummary := runSignatureHash(t, input, false)
	if first.SignatureCacheHit {
		t.Fatal("first hash unexpectedly hit the signature cache")
	}
	if first.SHA256 != hex.EncodeToString(wantHash[:]) {
		t.Fatalf("first SHA256 = %q, want %q", first.SHA256, hex.EncodeToString(wantHash[:]))
	}
	signature, valid, err := ReadCachedSignature(input)
	if err != nil {
		t.Fatal(err)
	}
	if !valid {
		t.Skip("temporary filesystem does not support signature xattrs")
	}
	if signature.SHA256 != wantHash || firstSummary.Writes != 1 {
		t.Fatalf("populated signature = %#v, summary = %#v", signature, firstSummary)
	}

	// Unchanged metadata uses the cached SHA-256 without reading content.
	second, secondSummary := runSignatureHash(t, input, false)
	if !second.SignatureCacheHit || second.SHA256 != first.SHA256 {
		t.Fatalf("cached job = %#v, want cache hit with SHA256 %q", second, first.SHA256)
	}
	if secondSummary.Hits != 1 || secondSummary.Writes != 0 {
		t.Fatalf("cache-hit summary = %#v", secondSummary)
	}

	// A metadata change makes the old signature stale and refreshes it.
	info, err := os.Stat(input)
	if err != nil {
		t.Fatal(err)
	}
	changed := info.ModTime().Add(2 * time.Second)
	if err := os.Chtimes(input, changed, changed); err != nil {
		t.Fatal(err)
	}
	stale, staleSummary := runSignatureHash(t, input, false)
	if stale.SignatureCacheHit || staleSummary.Stale != 1 || staleSummary.Writes != 1 {
		t.Fatalf("stale job = %#v, summary = %#v", stale, staleSummary)
	}

	// Force rehash bypasses the now-valid cache and still refreshes it.
	forced, forcedSummary := runSignatureHash(t, input, true)
	if forced.SignatureCacheHit || forcedSummary.Hits != 0 || forcedSummary.Writes != 1 {
		t.Fatalf("forced job = %#v, summary = %#v", forced, forcedSummary)
	}
}

func TestRunStreamTransferAlwaysHashesAndRefreshesTargets(t *testing.T) {
	// Prepare an existing target so the transfer exercises overwrite invalidation.
	content := []byte("transfer fixture")
	root := t.TempDir()
	input := filepath.Join(root, "source.txt")
	target := filepath.Join(root, "target.txt")
	if err := os.WriteFile(input, content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Seed a metadata-valid but incorrect cache entry to prove transfer ignores it.
	info, err := os.Stat(input)
	if err != nil {
		t.Fatal(err)
	}
	wrong := CachedSignature{Size: info.Size(), MtimeNS: info.ModTime().UnixNano(), SHA256: sha256.Sum256([]byte("wrong"))}
	file, err := os.Open(input)
	if err != nil {
		t.Fatal(err)
	}
	err = writeSignatureXattr(file, encodeCachedSignature(wrong))
	_ = file.Close()
	if err != nil {
		t.Skipf("temporary filesystem does not support signature xattrs: %v", err)
	}
	targetInfo, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	targetFile, err := os.Open(target)
	if err != nil {
		t.Fatal(err)
	}
	err = writeSignatureXattr(targetFile, encodeCachedSignature(CachedSignature{
		Size: targetInfo.Size(), MtimeNS: targetInfo.ModTime().UnixNano(), SHA256: sha256.Sum256([]byte("old")),
	}))
	_ = targetFile.Close()
	if err != nil {
		t.Skipf("temporary filesystem does not support target signature xattrs: %v", err)
	}

	// Copy real content while the source advertises a deliberately incorrect cache entry.
	sink := new(collectingStreamSink)
	var summary SignatureCacheSummary
	handler := func(event Event) {
		if update, ok := event.(*EventSignatureCacheSummary); ok {
			summary = update.Summary
		}
	}
	err = RunStream(
		context.Background(),
		&sliceStreamSource{requests: []*StreamRequest{{ID: 1, Source: input, Targets: []string{target}}}},
		sink,
		Overwrite(true),
		WithSignatureCache(true),
		WithEventHandler(handler),
	)
	if err != nil {
		t.Fatal(err)
	}
	wantHash := sha256.Sum256(content)
	job := sink.results[0].Job
	if job.SignatureCacheHit || job.SHA256 != hex.EncodeToString(wantHash[:]) {
		t.Fatalf("transfer job = %#v", job)
	}

	// Verify both bytes and refreshed source and target signature facts.
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("target content = %q, want %q", got, content)
	}
	for _, path := range []string{input, target} {
		signature, valid, err := ReadCachedSignature(path)
		if err != nil {
			t.Fatal(err)
		}
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
	source := filepath.Join(root, "source.txt")
	target := filepath.Join(root, "target.txt")
	modified := time.Unix(100, 123)
	if err := os.WriteFile(source, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{source, target} {
		if err := os.Chtimes(path, modified, modified); err != nil {
			t.Fatal(err)
		}
	}

	// Attach the old signature after the shared metadata has been fixed.
	file, err := os.Open(target)
	if err != nil {
		t.Fatal(err)
	}
	err = writeSignatureXattr(file, encodeCachedSignature(CachedSignature{
		Size: 3, MtimeNS: modified.UnixNano(), SHA256: sha256.Sum256([]byte("old")),
	}))
	_ = file.Close()
	if err != nil {
		t.Skipf("temporary filesystem does not support signature xattrs: %v", err)
	}

	// Overwrite without enabling cache writes and require the old xattr to disappear.
	err = RunStream(
		context.Background(),
		&sliceStreamSource{requests: []*StreamRequest{{ID: 1, Source: source, Targets: []string{target}}}},
		new(collectingStreamSink),
		Overwrite(true),
	)
	if err != nil {
		t.Fatal(err)
	}
	if signature, valid, err := ReadCachedSignature(target); err != nil {
		t.Fatal(err)
	} else if valid {
		t.Fatalf("overwritten target retained a valid signature: %#v", signature)
	}
}

func TestRunStreamCorruptSignatureIsWarning(t *testing.T) {
	// Seed an undecodable managed xattr on an otherwise valid source.
	input := filepath.Join(t.TempDir(), "source.txt")
	if err := os.WriteFile(input, nil, 0o644); err != nil {
		t.Fatal(err)
	}
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
	job, summary := runSignatureHash(t, input, false)
	if job.SignatureCacheHit || summary.Failures != 1 || summary.Misses != 1 || summary.Writes != 1 {
		t.Fatalf("job = %#v, summary = %#v", job, summary)
	}
	if summary.FirstError == "" || len(summary.Samples) != 1 || summary.Samples[0] != input {
		t.Fatalf("warning details = %#v", summary)
	}
}

func TestRunStreamSignatureCacheZeroLength(t *testing.T) {
	// Populate the valid SHA-256 signature of an empty file.
	input := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(input, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	// Hash the empty content and require the asynchronous cache write to drain.
	first, _ := runSignatureHash(t, input, false)
	if first.SHA256 != hex.EncodeToString(sha256.New().Sum(nil)) {
		t.Fatalf("empty SHA256 = %q", first.SHA256)
	}
	_, valid, err := ReadCachedSignature(input)
	if err != nil {
		t.Fatal(err)
	}
	if !valid {
		t.Skip("temporary filesystem does not support signature xattrs")
	}

	// Reuse the zero-length cache entry without reopening content.
	second, summary := runSignatureHash(t, input, false)
	if !second.SignatureCacheHit || summary.Hits != 1 {
		t.Fatalf("empty cache job = %#v, summary = %#v", second, summary)
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

func TestRunStreamSignatureCacheDrainsConcurrentWrites(t *testing.T) {
	// Build more sources than the writer pool can process at once.
	root := t.TempDir()
	source := new(sliceStreamSource)
	want := make(map[string][32]byte)
	for idx := 0; idx < 32; idx++ {
		path := filepath.Join(root, fmt.Sprintf("%02d.bin", idx))
		content := []byte(fmt.Sprintf("concurrent signature %d", idx))
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}
		source.requests = append(source.requests, &StreamRequest{ID: int64(idx + 1), Source: path})
		want[path] = sha256.Sum256(content)
	}

	// Run concurrent hashing and require every asynchronous xattr write to drain.
	sink := new(collectingStreamSink)
	if err := RunStream(
		context.Background(), source, sink,
		WithSignatureCache(true),
		SetFromDevice(DeviceThreads(4)),
	); err != nil {
		t.Fatal(err)
	}
	if len(sink.results) != len(want) {
		t.Fatalf("received %d results, want %d", len(sink.results), len(want))
	}

	// Confirm all cache writes are visible after RunStream returns.
	for path, expected := range want {
		signature, valid, err := ReadCachedSignature(path)
		if err != nil {
			t.Fatal(err)
		}
		if !valid {
			t.Skip("temporary filesystem does not support signature xattrs")
		}
		if signature.SHA256 != expected {
			t.Fatalf("signature for %q = %x, want %x", path, signature.SHA256, expected)
		}
	}
}

type cancelingSignatureSink struct {
	cancel context.CancelFunc
	path   string
}

func (s *cancelingSignatureSink) Write(_ context.Context, result *StreamResult) error {
	s.path = result.Job.FullPath
	s.cancel()
	return nil
}

func (*cancelingSignatureSink) Flush(context.Context) error {
	return nil
}

func TestRunStreamSignatureCacheDrainsAfterCancellation(t *testing.T) {
	// Cancel only after the first completed result has entered the Sink.
	path := filepath.Join(t.TempDir(), "cancellation.bin")
	if err := os.WriteFile(path, []byte("cancellation fixture"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Run a blocking source so the Sink controls the cancellation boundary.
	ctx, cancel := context.WithCancel(context.Background())
	sink := &cancelingSignatureSink{cancel: cancel}
	err := RunStream(ctx, &blockingStreamSource{
		request: &StreamRequest{ID: 1, Source: path},
	}, sink, WithSignatureCache(true))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunStream() error = %v, want context cancellation", err)
	}

	// The completed result's cache write must drain despite caller cancellation.
	if sink.path == "" {
		t.Fatal("cancellation sink received no completed result")
	}
	_, valid, err := ReadCachedSignature(sink.path)
	if err != nil {
		t.Fatal(err)
	}
	if !valid {
		t.Skip("temporary filesystem does not support signature xattrs")
	}
}

func TestRunStreamSignatureCacheIsBestEffortForReadOnlyFile(t *testing.T) {
	// Hash content successfully even if the filesystem rejects the cache write.
	path := filepath.Join(t.TempDir(), "readonly.bin")
	content := []byte("read-only signature fixture")
	if err := os.WriteFile(path, content, 0o444); err != nil {
		t.Fatal(err)
	}
	job, summary := runSignatureHash(t, path, true)
	want := sha256.Sum256(content)
	if job.SHA256 != hex.EncodeToString(want[:]) {
		t.Fatalf("SHA256 = %q, want %q", job.SHA256, hex.EncodeToString(want[:]))
	}

	// Accept either a valid owner-writable xattr or a non-fatal warning.
	signature, valid, err := ReadCachedSignature(path)
	if err != nil {
		t.Fatal(err)
	}
	if valid && signature.SHA256 != want {
		t.Fatalf("signature = %x, want %x", signature.SHA256, want)
	}
	if !valid && summary.Failures == 0 {
		t.Fatalf("read-only cache miss had no warning: %#v", summary)
	}
}

func runSignatureHash(t *testing.T, input string, force bool) (*Job, SignatureCacheSummary) {
	t.Helper()

	// Capture the aggregate event while collecting the single stream result.
	sink := new(collectingStreamSink)
	var summary SignatureCacheSummary
	handler := func(event Event) {
		if update, ok := event.(*EventSignatureCacheSummary); ok {
			summary = update.Summary
		}
	}

	// Run one targetless hash with the requested cache-read policy.
	if err := RunStream(
		context.Background(),
		&sliceStreamSource{requests: []*StreamRequest{{ID: 1, Source: input}}},
		sink,
		WithSignatureCache(true),
		ForceRehash(force),
		WithEventHandler(handler),
	); err != nil {
		t.Fatal(err)
	}
	if len(sink.results) != 1 {
		t.Fatalf("received %d results, want 1", len(sink.results))
	}

	// Return only after RunStream has drained cache writes and emitted its summary.
	return sink.results[0].Job, summary
}
