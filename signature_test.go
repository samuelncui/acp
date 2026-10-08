package acp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sync"
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
	want, err := newCachedSignature(hash[:], indexed)
	if err != nil {
		t.Fatal(err)
	}

	// Open the descriptor the item would own, seed a decoy so the assertion below proves the
	// writer replaced it, and skip on filesystems that cannot store the attribute at all.
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := writeSignatureXattr(file, encodeCachedSignature(CachedSignature{Size: 7, MtimeNS: 7})); err != nil {
		t.Skipf("temporary filesystem does not support signature xattrs: %v", err)
	}

	// Publish the stable file's hash through its owned descriptor.
	cache := newSignatureCache()
	cache.write(file, path, want)
	if cache.summary.Writes != 1 || cache.summary.Failures != 0 {
		t.Fatalf("signature summary = %#v", cache.summary)
	}

	// Inspect the raw attribute to confirm ACP's snapshot landed unchanged.
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
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	// A later modification leaves the completed entry stale for the next lookup.
	changed := time.Unix(200, 456)
	if err := os.Chtimes(path, changed, changed); err != nil {
		t.Fatal(err)
	}
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
	// Prepare a cache-capable source before asserting publication behavior.
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
		t.Fatal("the first refresh published no valid signature")
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

func TestRunSignatureCacheAccessCounts(t *testing.T) {
	// Pin source opens and managed-xattr calls independently of filesystem xattr support.
	tests := []struct {
		name         string
		policy       HashPolicy
		entry        string
		targets      int
		opens        int
		reads        int
		sourceWrites int
		targetWrites int
		failures     int64
	}{
		{name: "off", policy: HashOff},
		{name: "read", policy: HashRead, opens: 1},
		{name: "reuse hit", policy: HashCachedOnly, entry: "equal", opens: 1, reads: 1},
		{name: "reuse miss", policy: HashCachedOrRead, opens: 1, reads: 1},
		{name: "reuse refresh hit", policy: HashCachedOrReadRefresh, entry: "equal", opens: 1, reads: 1},
		{name: "reuse refresh miss", policy: HashCachedOrReadRefresh, opens: 1, reads: 1, sourceWrites: 1},
		{
			name: "reuse refresh corrupt", policy: HashCachedOrReadRefresh, entry: "corrupt",
			opens: 1, reads: 1, sourceWrites: 1, failures: 1,
		},
		{name: "read refresh miss", policy: HashReadRefresh, opens: 1, reads: 1, sourceWrites: 1},
		{name: "read refresh equal", policy: HashReadRefresh, entry: "equal", opens: 1, reads: 1},
		{
			name: "read refresh corrupt", policy: HashReadRefresh, entry: "corrupt",
			opens: 1, reads: 1, sourceWrites: 1,
		},
		{
			name: "read refresh unreadable", policy: HashReadRefresh, entry: "unreadable",
			opens: 1, reads: 1, sourceWrites: 1,
		},
		{name: "transfer off", policy: HashOff, targets: 2, opens: 1},
		{
			name: "transfer refresh equal source", policy: HashReadRefresh, entry: "equal",
			targets: 2, opens: 1, reads: 1, targetWrites: 2,
		},
		{
			name: "transfer refresh missing source", policy: HashCachedOrReadRefresh,
			targets: 2, opens: 1, reads: 1, sourceWrites: 1, targetWrites: 2,
		},
	}
	for _, mode := range []ReadMode{ReadBuffered, ReadMapped} {
		for _, test := range tests {
			t.Run(mode.String()+"/"+test.name, func(t *testing.T) {
				// Declare all content, target paths and stored metadata before opening the stream.
				root := t.TempDir()
				content := []byte("single cache read fixture")
				input := writeSourceFile(t, root, "source.txt", content)
				info, err := os.Stat(input)
				if err != nil {
					t.Fatal(err)
				}
				want := CachedSignature{Size: info.Size(), MtimeNS: info.ModTime().UnixNano(), SHA256: sha256.Sum256(content)}
				var targets []string
				for index := 0; index < test.targets; index++ {
					targets = append(targets, filepath.Join(root, fmt.Sprintf("target-%d.txt", index)))
				}

				// Track the one source descriptor and every cache access, including target writers.
				previousOpen, previousRead, previousWrite := openSourceContent, readManagedXattr, writeManagedXattr
				previousPathWrite := writeManagedPathXattr
				var lock sync.Mutex
				var sourceFile *os.File
				var opens, reads, sourceWrites, targetWrites int
				openSourceContent = func(path string, mode ReadMode, info os.FileInfo) (itemSource, error) {
					source, err := previousOpen(path, mode, info)
					lock.Lock()
					opens++
					sourceFile = source.file
					lock.Unlock()
					return source, err
				}
				readManagedXattr = func(file *os.File) ([]byte, error) {
					// A fresh target must never read a cache entry, and a source must use its owner.
					lock.Lock()
					reads++
					isSource := file == sourceFile
					lock.Unlock()
					if !isSource {
						t.Errorf("cache read through %q instead of the source descriptor", file.Name())
					}

					// Refresh-only reads suppress unusable entries; reuse reports their diagnostics.
					switch test.entry {
					case "equal":
						return encodeCachedSignature(want), nil
					case "corrupt":
						return []byte("corrupt"), nil
					case "unreadable":
						return nil, errors.New("read fixture")
					default:
						return nil, errSignatureXattrUnsupported
					}
				}
				writeManagedXattr = func(file *os.File, value []byte) error {
					// Every publication describes the completed source content using indexed facts.
					got, err := DecodeCachedSignature(value)
					if err != nil || got != want {
						t.Errorf("published signature = %#v / %v, want %#v", got, err, want)
					}

					// Source and target writes have separate counts because only sources skip equals.
					lock.Lock()
					defer lock.Unlock()
					if file == sourceFile {
						sourceWrites++
					} else {
						targetWrites++
					}
					return nil
				}
				writeManagedPathXattr = func(path string, value []byte) error {
					// The asynchronous source cache describes the same stable file after Close.
					got, err := DecodeCachedSignature(value)
					if path != input || err != nil || got != want {
						t.Errorf("path cache = %q / %#v / %v", path, got, err)
					}
					lock.Lock()
					sourceWrites++
					lock.Unlock()
					return nil
				}
				t.Cleanup(func() {
					openSourceContent, readManagedXattr, writeManagedXattr = previousOpen, previousRead, previousWrite
					writeManagedPathXattr = previousPathWrite
				})

				// Wait for the full item so the counts include publication and descriptor release.
				var summary SignatureCacheSummary
				handler := func(event Event) {
					if update, ok := event.(*EventSignatureCacheSummary); ok {
						summary = update.Summary
					}
				}
				item := newFixtureItem(input, targets...)
				if err := runFixture(
					context.Background(), newStreamFixture(item), []Item{item}, WithHashPolicy(test.policy),
					SetFromDevice(WithReadMode(mode)), WithEventHandler(handler),
				); err != nil {
					t.Fatal(err)
				}
				if _, err := item.terminal(t); err != nil {
					t.Fatal(err)
				}

				// A read-refresh corrupt entry is replaced without adding reuse misses or warnings.
				lock.Lock()
				defer lock.Unlock()
				if opens != test.opens || reads != test.reads || sourceWrites != test.sourceWrites || targetWrites != test.targetWrites {
					t.Fatalf("opens/reads/source writes/target writes = %d/%d/%d/%d, want %d/%d/%d/%d",
						opens, reads, sourceWrites, targetWrites, test.opens, test.reads, test.sourceWrites, test.targetWrites)
				}
				if summary.Writes != int64(sourceWrites+targetWrites) || summary.Failures != test.failures {
					t.Fatalf("summary = %#v, want writes=%d failures=%d", summary, sourceWrites+targetWrites, test.failures)
				}
				if test.policy == HashReadRefresh && (summary.Hits != 0 || summary.Misses != 0 || summary.Stale != 0) {
					t.Fatalf("read-refresh recorded reuse activity: %#v", summary)
				}
			})
		}
	}
}

func TestRunRejectsSignatureChangedBetweenRuns(t *testing.T) {
	// Size and modification time independently make an earlier run's cache entry stale.
	requireSignatureXattrSupport(t)
	for _, changed := range []string{"size", "mtime"} {
		t.Run(changed, func(t *testing.T) {
			// Publish an entry in a complete first run before changing any source facts.
			content := []byte("between runs")
			input := writeSourceFile(t, t.TempDir(), "source.txt", content)
			if _, summary := runSignatureHash(t, input, HashReadRefresh); summary.Writes != 1 {
				t.Fatalf("initial summary = %#v, want one stored entry", summary)
			}
			info, err := os.Stat(input)
			if err != nil {
				t.Fatal(err)
			}

			// Change only the selected validation fact before starting the next run.
			modified := info.ModTime().Add(time.Second)
			if changed == "size" {
				content = append(content, '!')
				if err := os.WriteFile(input, content, 0o644); err != nil {
					t.Fatal(err)
				}
				modified = info.ModTime()
			}
			if err := os.Chtimes(input, modified, modified); err != nil {
				t.Fatal(err)
			}

			// Both standalone lookup and pipeline reuse reject the old entry using fresh metadata.
			if _, valid, err := ReadCachedSignature(input); err != nil || valid {
				t.Fatalf("standalone lookup = valid=%t error=%v, want a stale miss", valid, err)
			}
			result, summary := runSignatureHash(t, input, HashCachedOrRead)
			want := sha256.Sum256(content)
			if result.SignatureCacheHit || !bytes.Equal(result.SHA256, want[:]) {
				t.Fatalf("result = %#v, want a content read with hash %x", result, want)
			}
			if summary.Stale != 1 || summary.Hits != 0 || summary.Misses != 0 || summary.Failures != 0 {
				t.Fatalf("summary = %#v, want one stale entry", summary)
			}
		})
	}
}

func TestRunTransferAlwaysReadsAndRefreshesTargets(t *testing.T) {
	// Prepare an existing target whose replacement must receive the computed signature.
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
	if err := runFixture(
		context.Background(),
		newStreamFixture(item),
		[]Item{item},
		WithHashPolicy(HashCachedOrReadRefresh),
		Overwrite(true),
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
	if signature, valid, err := ReadCachedSignature(target); err != nil {
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
		"one extra": append(encodeCachedSignature(CachedSignature{}), 0),
		"oversized": make([]byte, signatureEncodedSize*2),
	} {
		t.Run(name, func(t *testing.T) {
			// A bounded xattr read must reject malformed data without accepting a valid prefix.
			input := writeSourceFile(t, t.TempDir(), "source.txt", nil)
			file, err := os.Open(input)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			if err := writeSignatureXattr(file, value); err != nil {
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

func TestTargetSignaturePrecedesReadOnlyMetadata(t *testing.T) {
	// The source's final mode must not prevent publishing a fresh target's cache entry.
	requireSignatureXattrSupport(t)
	root := t.TempDir()
	source := writeSourceFile(t, root, "source", []byte("content"))
	if err := os.Chmod(source, 0o444); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "target")
	previous := writeManagedXattr
	t.Cleanup(func() { writeManagedXattr = previous })
	writeManagedXattr = func(file *os.File, value []byte) error {
		// Linux user attributes require current write permission even through an already-open fd.
		if file.Name() != source {
			info, err := file.Stat()
			if err != nil {
				return err
			}
			if info.Mode().Perm()&0o200 == 0 {
				return os.ErrPermission
			}
		}
		return previous(file, value)
	}

	// Final content, permissions and the cache must all survive the ordinary completion path.
	item := newFixtureItem(source, target)
	if err := runFixture(context.Background(), newStreamFixture(item), []Item{item}, WithHashPolicy(HashReadRefresh)); err != nil {
		t.Fatal(err)
	}
	result, err := item.terminal(t)
	if err != nil || len(result.Targets) != 1 || result.Targets[0].Err != nil {
		t.Fatalf("copy outcome=%+v / %v", result, err)
	}
	if _, found, err := ReadCachedSignature(target); err != nil || !found {
		t.Fatalf("target signature found=%v / %v", found, err)
	}
	if info, err := os.Stat(target); err != nil || info.Mode().Perm() != 0o444 {
		t.Fatalf("target mode=%v / %v", info, err)
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
	cache := newSignatureCache()
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

func TestRunWritesEveryItemCacheEntry(t *testing.T) {
	// Build more sources than one item at a time, so every item publishes its own entry.
	requireSignatureXattrSupport(t)
	root := t.TempDir()
	items := make([]Item, 0, 32)
	fixtures := make([]*fixtureItem, 0, 32)
	want := make(map[string][32]byte)
	for idx := 0; idx < 32; idx++ {
		path := writeSourceFile(t, root, fmt.Sprintf("%02d.bin", idx), []byte(fmt.Sprintf("concurrent signature %d", idx)))
		item := newFixtureItem(path)
		items = append(items, item)
		fixtures = append(fixtures, item)
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		want[path] = sha256.Sum256(content)
	}

	// Run concurrent hashing and require every item to have published its own entry.
	if err := runFixture(
		context.Background(),
		newStreamFixture(fixtures...),
		items,
		WithHashPolicy(HashReadRefresh),
		SetFromDevice(DeviceThreads(4)),
	); err != nil {
		t.Fatal(err)
	}

	// Confirm every item published before the run returned: no write outlives its item.
	for path, expected := range want {
		signature, valid := readStoredSignature(t, path)
		if !valid {
			t.Fatalf("item %q published no valid signature", path)
		}
		if signature.SHA256 != expected {
			t.Fatalf("signature for %q = %x, want %x", path, signature.SHA256, expected)
		}
	}
}

func TestRunSignatureCacheSurvivesCancellation(t *testing.T) {
	// Cancel only after the first completed item has left the pipeline, which is where a caller
	// observes that the run is working.
	requireSignatureXattrSupport(t)
	content := []byte("cancellation fixture")
	path := writeSourceFile(t, t.TempDir(), "cancellation.bin", content)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	item := newFixtureItem(path)
	fixture := newStreamFixture(item)

	// A result batch of one delivers the completed item immediately, so the callback stops the
	// run while the pipeline is still open instead of at Close.
	cancelled := make(chan struct{}, 1)
	fixture.hook = func([]Result) error {
		cancel()
		select {
		case cancelled <- struct{}{}:
		default:
		}
		return nil
	}

	stream, err := NewStream(ctx, fixture.onResults, WithHashPolicy(HashReadRefresh), WithResultBuffer(1), WithResultBatch(1))
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Submit(item); err != nil {
		t.Fatal(err)
	}

	// Submit is the run's single cancellation checkpoint, so the batch after the callback
	// observes the stop and is refused.
	select {
	case <-cancelled:
	case <-time.After(10 * time.Second):
		t.Fatal("the run never delivered the completed item")
	}
	if err := stream.Submit(newFixtureItem(path)); !errors.Is(err, context.Canceled) {
		t.Fatalf("Submit() after cancellation error = %v, want context cancellation", err)
	}
	if err := stream.Wait(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait() error = %v, want context cancellation", err)
	}

	// The completed item published its entry before its result, so caller cancellation cannot
	// lose it.
	result, terminalErr := item.terminal(t)
	if terminalErr != nil {
		t.Fatalf("item failed: %v", terminalErr)
	}
	signature, valid := readStoredSignature(t, path)
	if !valid {
		t.Fatal("the completed item published no valid signature before cancellation")
	}
	if signature.SHA256 != sha256.Sum256(content) || !bytes.Equal(result.SHA256, signature.SHA256[:]) {
		t.Fatalf("signature = %x, result = %x", signature.SHA256, result.SHA256)
	}
}

func TestRunSignatureCacheIsBestEffortForReadOnlyFile(t *testing.T) {
	// Hash content successfully even if the filesystem rejects the cache write.
	requireSignatureXattrSupport(t)
	path := writeSourceFile(t, t.TempDir(), "readonly.bin", []byte("read-only signature fixture"))
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(content)

	// Reading and hashing must succeed independently of a writable cache attribute.
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

// TestRunIgnoresUnsupportedSignatureXattr pins the no-op policy for a file system without the
// managed attribute namespace: the cache is simply absent, so a run neither fails nor records a
// cache failure. A real xattr failure is still reported.
func TestRunIgnoresUnsupportedSignatureXattr(t *testing.T) {
	// Simulate a filesystem without the managed namespace, regardless of this host's support.
	previousRead, previousWrite, previousPath := readManagedXattr, writeManagedXattr, writeManagedPathXattr
	t.Cleanup(func() {
		readManagedXattr, writeManagedXattr, writeManagedPathXattr = previousRead, previousWrite, previousPath
	})
	readManagedXattr = func(*os.File) ([]byte, error) { return nil, errSignatureXattrUnsupported }
	writeManagedXattr = func(*os.File, []byte) error { return errSignatureXattrUnsupported }
	writeManagedPathXattr = func(string, []byte) error { return errSignatureXattrUnsupported }

	// Keep an existing target so replacement also exercises the no-cache policy.
	root := t.TempDir()
	content := []byte("unsupported xattr fixture")
	input := writeSourceFile(t, root, "source.txt", content)
	target := writeSourceFile(t, root, "target.txt", []byte("old"))

	// A transfer refreshes both the source and its fresh target, exercising cache reads and writes.
	run := func() (*fixtureItem, SignatureCacheSummary) {
		t.Helper()

		// Collect the summary after the transfer has completed both files.
		item := newFixtureItem(input, target)
		var summary SignatureCacheSummary
		handler := func(event Event) {
			if update, ok := event.(*EventSignatureCacheSummary); ok {
				summary = update.Summary
			}
		}
		if err := runFixture(
			context.Background(),
			newStreamFixture(item),
			[]Item{item},
			WithHashPolicy(HashCachedOrReadRefresh),
			Overwrite(true),
			WithEventHandler(handler),
		); err != nil {
			t.Fatalf("runFixture() error = %v, want nil: an unsupported xattr is not a failure", err)
		}
		return item, summary
	}

	// Unsupported attributes leave the transfer successful without cache failures.
	item, summary := run()
	result, err := item.terminal(t)
	if err != nil {
		t.Fatalf("item failed: %v", err)
	}
	if want := sha256.Sum256(content); !bytes.Equal(result.SHA256, want[:]) {
		t.Fatalf("SHA256 = %x, want %x", result.SHA256, want)
	}
	if summary.Failures != 0 || summary.Writes != 0 {
		t.Fatalf("summary = %#v, want no recorded failure and no write", summary)
	}

	// A broken attribute is still a failure, so the no-op policy cannot swallow real ones.
	writeManagedXattr = func(*os.File, []byte) error { return errors.New("xattr is broken") }
	writeManagedPathXattr = func(string, []byte) error { return errors.New("xattr is broken") }
	if _, summary := run(); summary.Failures == 0 {
		t.Fatalf("summary = %#v, want the broken attribute recorded", summary)
	}
}

// TestRunReadsTheStoredHashThroughTheItemDescriptor pins where a stored hash is read: through the
// descriptor the item opened for its content, never by reopening the stable source path.
func TestRunReadsTheStoredHashThroughTheItemDescriptor(t *testing.T) {
	for _, mode := range []ReadMode{ReadBuffered, ReadMapped} {
		t.Run(mode.String(), func(t *testing.T) {
			// Seed a reusable entry before the run while keeping the source stable throughout it.
			requireSignatureXattrSupport(t)
			content := []byte("descriptor cache read fixture")
			input := writeSourceFile(t, t.TempDir(), "source.txt", content)
			seeded := seedStaleSignature(t, input, []byte("stored value"))

			// Observe descriptor identity directly without changing the file or its path.
			previousOpen, previousRead := openSourceContent, readManagedXattr
			var opened *os.File
			var opens, reads int
			openSourceContent = func(path string, mode ReadMode, info os.FileInfo) (itemSource, error) {
				source, err := previousOpen(path, mode, info)
				opens++
				opened = source.file
				return source, err
			}
			readManagedXattr = func(file *os.File) ([]byte, error) {
				reads++
				if file != opened {
					t.Errorf("cache read descriptor = %p, want the source descriptor %p", file, opened)
				}
				return previousRead(file)
			}
			t.Cleanup(func() { openSourceContent, readManagedXattr = previousOpen, previousRead })

			// A reusable entry bypasses content without opening or reading the cache again.
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
				WithHashPolicy(HashCachedOrRead),
				SetFromDevice(WithReadMode(mode)),
				WithEventHandler(handler),
			); err != nil {
				t.Fatal(err)
			}

			// The result and summary reflect one descriptor-based cache lookup.
			result, err := item.terminal(t)
			if err != nil {
				t.Fatalf("item failed: %v", err)
			}
			if !result.SignatureCacheHit || !bytes.Equal(result.SHA256, seeded.SHA256[:]) {
				t.Fatalf("result = %#v, want the stored hash %x of the file the item opened", result, seeded.SHA256)
			}
			if summary.Hits != 1 || summary.Misses != 0 || summary.Failures != 0 {
				t.Fatalf("summary = %#v, want one hit read through the item's descriptor", summary)
			}
			if opens != 1 || reads != 1 {
				t.Fatalf("source opens=%d cache reads=%d, want one each", opens, reads)
			}
		})
	}
}

// TestRunWritesComputedCacheAfterSourceClose pins where a computed hash is published:
// through the stable source path after the content descriptor has closed.
func TestRunWritesComputedCacheAfterSourceClose(t *testing.T) {
	for _, mode := range []ReadMode{ReadBuffered, ReadMapped} {
		t.Run(mode.String(), func(t *testing.T) {
			// A stored entry that differs from the computed hash requires one source publication.
			requireSignatureXattrSupport(t)
			content := []byte("descriptor cache write fixture")
			input := writeSourceFile(t, t.TempDir(), "source.txt", content)
			seedStaleSignature(t, input, []byte("stale stored value"))

			// Observe one content open and path publication after descriptor closure.
			previousOpen, previousWrite := openSourceContent, writeManagedPathXattr
			var opened *os.File
			var opens, writes int
			openSourceContent = func(path string, mode ReadMode, info os.FileInfo) (itemSource, error) {
				source, err := previousOpen(path, mode, info)
				opens++
				opened = source.file
				return source, err
			}
			writeManagedPathXattr = func(path string, value []byte) error {
				writes++
				if path != input {
					t.Errorf("cache path = %q, want %q", path, input)
				}
				if _, err := opened.Stat(); !errors.Is(err, os.ErrClosed) {
					t.Errorf("cache publication preceded source Close: %v", err)
				}
				return previousWrite(path, value)
			}
			t.Cleanup(func() { openSourceContent, writeManagedPathXattr = previousOpen, previousWrite })

			// Run a content refresh and retain the aggregate warning and write counts.
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
				WithHashPolicy(HashReadRefresh),
				SetFromDevice(WithReadMode(mode)),
				WithEventHandler(handler),
			); err != nil {
				t.Fatal(err)
			}

			// Only the completed content hash is published, exactly once through the owned source.
			result, err := item.terminal(t)
			if err != nil {
				t.Fatalf("item failed: %v", err)
			}
			want := sha256.Sum256(content)
			if !bytes.Equal(result.SHA256, want[:]) {
				t.Fatalf("SHA256 = %x, want %x: the item hashes the content it opened", result.SHA256, want)
			}
			if summary.Hits != 0 || summary.Misses != 0 || summary.Writes != 1 || summary.Failures != 0 {
				t.Fatalf("summary = %#v, want one write and no reuse accounting", summary)
			}
			if opens != 1 || writes != 1 {
				t.Fatalf("source opens=%d cache writes=%d, want one each", opens, writes)
			}
			stored, valid := readStoredSignature(t, input)
			if !valid || stored.SHA256 != want {
				t.Fatalf("stored signature for the item's file = %#v, valid=%t, want %x", stored, valid, want)
			}
			if _, err := opened.Stat(); !errors.Is(err, os.ErrClosed) {
				t.Fatalf("source descriptor after the run = %v, want closed", err)
			}
		})
	}
}

// TestRunPublishesTargetCacheThroughTheOpenTargetDescriptor pins where a target's cache entry is
// published: through the descriptor the writer wrote that target with, after the item's content
// hash is complete and while the descriptor is still open. A descriptor reopened by path would sit
// at offset zero and would know nothing about the item's hash.
func TestRunPublishesTargetCacheThroughTheOpenTargetDescriptor(t *testing.T) {
	// Prepare stable source content and two fresh final target paths.
	requireSignatureXattrSupport(t)
	content := []byte("target descriptor fixture")
	root := t.TempDir()
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	input := writeSourceFile(t, root, "source.txt", content)
	targets := []string{filepath.Join(root, "target-1.txt"), filepath.Join(root, "target-2.txt")}

	// Publication must retain the source metadata observed before the run.
	info, err := os.Stat(input)
	if err != nil {
		t.Fatal(err)
	}
	wantHash := sha256.Sum256(content)
	want, err := newCachedSignature(wantHash[:], &stat{size: int64(len(content)), modTime: info.ModTime()})
	if err != nil {
		t.Fatal(err)
	}

	// Record the descriptor the item reads its source through, so every published entry can be
	// attributed to the file that owns it.
	previousOpen := openSourceContent
	var sourceFile *os.File
	openSourceContent = func(path string, mode ReadMode, info os.FileInfo) (itemSource, error) {
		source, err := previousOpen(path, mode, info)
		if err == nil && path == input {
			sourceFile = source.file
		}
		return source, err
	}
	t.Cleanup(func() { openSourceContent = previousOpen })

	// Record each publication's descriptor, position and content facts.
	type publication struct {
		source    bool
		name      string
		offset    int64
		open      bool
		signature CachedSignature
	}
	previousWrite := writeManagedXattr
	var (
		lock         sync.Mutex
		publications []publication
	)
	writeManagedXattr = func(file *os.File, value []byte) error {
		// Reject any publication that cannot describe a complete signature.
		signature, err := DecodeCachedSignature(value)
		if err != nil {
			t.Errorf("published an undecodable signature: %v", err)
			return previousWrite(file, value)
		}

		// The descriptor that read or wrote the whole content stands at its end; a descriptor
		// reopened by path would start at offset zero.
		offset, offsetErr := file.Seek(0, io.SeekCurrent)
		if offsetErr != nil {
			t.Errorf("read the publishing descriptor offset: %v", offsetErr)
		}
		_, statErr := file.Stat()

		// Record the descriptor before the real attribute write and eventual close.
		lock.Lock()
		publications = append(publications, publication{
			source:    file == sourceFile,
			name:      file.Name(),
			offset:    offset,
			open:      statErr == nil,
			signature: signature,
		})
		lock.Unlock()

		// Preserve the real xattr write so the final files can be checked independently.
		return previousWrite(file, value)
	}
	t.Cleanup(func() { writeManagedXattr = previousWrite })

	// Wait for the stream to publish and close every target descriptor.
	item := newFixtureItem(input, targets...)
	if err := runFixture(
		context.Background(),
		newStreamFixture(item),
		[]Item{item},
		WithHashPolicy(HashReadRefresh),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := item.terminal(t); err != nil {
		t.Fatalf("item failed: %v", err)
	}

	// Every file publishes its own entry, through its own open descriptor, carrying the hash this
	// item computed from the whole source.
	lock.Lock()
	defer lock.Unlock()
	if len(publications) != len(targets) {
		t.Fatalf("published %d cache entries, want one per file: %#v", len(publications), publications)
	}
	var sourcePublished, targetsPublished int
	for _, published := range publications {
		if !published.open {
			t.Fatalf("published %#v through a closed descriptor", published)
		}
		if published.signature != want {
			t.Fatalf("published signature = %#v, want the computed %#v", published.signature, want)
		}
		if published.offset != int64(len(content)) {
			t.Fatalf(
				"published through a descriptor at offset %d, want %d: the entry must travel through the descriptor that handled the whole content",
				published.offset, len(content),
			)
		}
		if published.source {
			sourcePublished++
			continue
		}
		for _, target := range targets {
			if published.name == filepath.Join(resolvedRoot, filepath.Base(target)) {
				t.Fatalf("cache published through final path %q, want the temporary writer", target)
			}
		}
		if filepath.Dir(published.name) != resolvedRoot {
			t.Fatalf("target descriptor path = %q, want a temporary file beside the target", published.name)
		}
		targetsPublished++
	}
	if sourcePublished != 0 || targetsPublished != len(targets) {
		t.Fatalf("publications = %#v, want one descriptor publication per target", publications)
	}

	// Every entry describes its own file afterwards, which is the whole point of publishing it.
	for _, path := range append([]string{input}, targets...) {
		stored, valid := readStoredSignature(t, path)
		if !valid || stored != want {
			t.Fatalf("stored signature for %q = %#v, valid=%t, want %#v", path, stored, valid, want)
		}
	}
}

// blockingHash keeps the first content hash of an item incomplete until the test releases it, so a
// test can observe what an item does while its hash is still unknown.
type blockingHash struct {
	hash.Hash

	started chan struct{}
	release chan struct{}

	startOnce *sync.Once
}

func (h *blockingHash) Write(p []byte) (int, error) {
	h.startOnce.Do(func() { close(h.started) })
	<-h.release

	return h.Hash.Write(p)
}

// holdContentHash makes the run's hash consumer wait on the returned release function before it
// hashes its first chunk. It reports when the consumer has started, so a test knows the item's hash
// is being held back, and it restores the pool when the test ends.
func holdContentHash(t *testing.T) (started <-chan struct{}, released <-chan struct{}, release func()) {
	t.Helper()

	// Every concurrent file owns its own digest; only the observation and release gates are shared.
	previous := sha256Pool
	first, gate := make(chan struct{}), make(chan struct{})
	var startOnce sync.Once
	sha256Pool = &sync.Pool{New: func() interface{} {
		return &blockingHash{Hash: sha256.New(), started: first, release: gate, startOnce: &startOnce}
	}}

	// Restore the original pool only after tests have joined their transfer workers.
	unblock := onceRelease(gate)
	t.Cleanup(func() { unblock(); sha256Pool = previous })
	return first, gate, unblock
}

// TestRunHoldsTargetCacheUntilTheHashIsComplete pins the ordering a target's cache entry depends
// on: the writer keeps its descriptor open until the item's hash is complete, so the entry always
// describes the whole content. The run is held back at the hash, and an item that completed anyway
// would have published an entry it could not describe.
func TestRunHoldsTargetCacheUntilTheHashIsComplete(t *testing.T) {
	requireSignatureXattrSupport(t)

	content := bytes.Repeat([]byte{'c'}, 4096)
	root := t.TempDir()
	input := writeSourceFile(t, root, "source.txt", content)
	target := filepath.Join(root, "target.txt")
	started, released, release := holdContentHash(t)

	// A linear target skips the durability sync, so its writer has nothing left to do but publish
	// by the time the hash is held back.
	item := newFixtureItem(input, target)
	done := make(chan error, 1)
	go func() {
		done <- runFixture(
			context.Background(),
			newStreamFixture(item),
			[]Item{item},
			WithHashPolicy(HashReadRefresh),
			SetToDevice(LinearDevice(true)),
		)
	}()

	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the hash consumer never took the held hash")
	}

	// The item cannot finish while its hash is unknown, and the writer must be waiting instead of
	// publishing the entry it cannot describe yet.
	select {
	case err := <-done:
		t.Fatalf("the run finished while the item's hash was held back: %v", err)
	case <-time.After(250 * time.Millisecond):
	}
	select {
	case <-released:
		t.Fatal("the held hash was released early")
	default:
	}

	// Releasing the hash lets the item finish and publish the entry of every file it handled.
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run error = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the run never finished after the hash was released")
	}

	if _, err := item.terminal(t); err != nil {
		t.Fatalf("item failed: %v", err)
	}
	for _, path := range []string{input, target} {
		signature, valid := readStoredSignature(t, path)
		if !valid || signature.SHA256 != sha256.Sum256(content) {
			t.Fatalf("stored signature for %q = %#v, valid=%t, want %x", path, signature, valid, sha256.Sum256(content))
		}
	}
}

// TestRunRefreshKeepsLinearTargetsWorking pins the cache refresh against a linear target: the
// writer waits for the item's hash before it closes, so a linear run still completes every item and
// publishes the entry of every file it wrote.
func TestRunRefreshKeepsLinearTargetsWorking(t *testing.T) {
	requireSignatureXattrSupport(t)

	root := t.TempDir()
	const total = 4
	items := make([]Item, 0, total)
	fixtures := make([]*fixtureItem, 0, total)
	want := make(map[string][32]byte, total)
	for idx := 0; idx < total; idx++ {
		content := []byte(fmt.Sprintf("linear cache fixture %d", idx))
		source := writeSourceFile(t, root, fmt.Sprintf("source-%02d.bin", idx), content)
		target := filepath.Join(root, "target", fmt.Sprintf("%02d.bin", idx))
		item := newFixtureItem(source, target)
		items = append(items, item)
		fixtures = append(fixtures, item)
		want[target] = sha256.Sum256(content)
	}

	if err := runFixture(
		context.Background(),
		newStreamFixture(fixtures...),
		items,
		WithHashPolicy(HashReadRefresh),
		SetToDevice(LinearDevice(true)),
	); err != nil {
		t.Fatalf("run error = %v, want nil", err)
	}

	for _, item := range fixtures {
		result, err := item.terminal(t)
		if err != nil {
			t.Fatalf("item %q failed: %v", item.source, err)
		}
		if len(result.Targets) != 1 || result.Targets[0].Err != nil {
			t.Fatalf("targets of %q = %#v, want one written target", item.source, result.Targets)
		}
		target := result.Targets[0].Path
		stored, valid := readStoredSignature(t, target)
		if !valid || stored.SHA256 != want[target] {
			t.Fatalf("target %q signature = %x, valid=%t, want %x", target, stored.SHA256, valid, want[target])
		}
	}
}

// TestRunPublishesTheCacheEntryBeforeTheResult pins that nothing about cache writing survives the
// item: by the time an item's result reaches the caller, the entry it computed is already stored.
func TestRunPublishesTheCacheEntryBeforeTheResult(t *testing.T) {
	requireSignatureXattrSupport(t)

	content := []byte("publication order fixture")
	input := writeSourceFile(t, t.TempDir(), "source.txt", content)
	want := sha256.Sum256(content)

	var (
		lock      sync.Mutex
		delivered []Result
		observed  []CachedSignature
		valid     []bool
	)
	onResults := func(results []Result) error {
		for _, result := range results {
			// The callback is the caller's first view of the item, and the entry is already
			// there: the item published it before it completed.
			signature, ok, err := ReadCachedSignature(input)
			lock.Lock()
			delivered = append(delivered, result)
			observed = append(observed, signature)
			valid = append(valid, ok && err == nil)
			lock.Unlock()
		}
		return nil
	}

	// A result batch of one delivers the item as soon as it is finished, instead of when the run
	// closes, so the observation happens while the pipeline is still open.
	item := newFixtureItem(input)
	stream, err := NewStream(context.Background(), onResults, WithHashPolicy(HashReadRefresh), WithResultBuffer(1), WithResultBatch(1))
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Submit(item); err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := stream.Wait(); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}

	lock.Lock()
	defer lock.Unlock()
	if len(delivered) != 1 || delivered[0].Err != nil {
		t.Fatalf("delivered %#v, want one completed result", delivered)
	}
	if len(valid) != 1 || !valid[0] || observed[0].SHA256 != want {
		t.Fatalf("the entry observed with the result = %#v, valid=%v, want %x", observed, valid, want)
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
