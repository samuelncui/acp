package acp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/samuelncui/acp/signature"
)

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
			var seeded signature.Entry
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
				if !bytes.Equal(result.SHA256, seeded.SHA256) {
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
				if !valid || !stored.Equal(seeded) {
					t.Fatalf("stored signature = %#v, valid=%t, want %#v", stored, valid, seeded)
				}
			case "computed":
				want := sha256.Sum256(content)
				if !valid || !bytes.Equal(stored.SHA256, want[:]) {
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
	if again, valid := readStoredSignature(t, input); !valid || !again.Equal(stored) {
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
				sum := sha256.Sum256(content)
				want := signature.Entry{Size: info.Size(), MtimeNS: info.ModTime().UnixNano(), SHA256: sum[:]}
				var targets []string
				for index := 0; index < test.targets; index++ {
					targets = append(targets, filepath.Join(root, fmt.Sprintf("target-%d.txt", index)))
				}

				// Track the one source descriptor and every cache access, including target writers.
				var hooks fsHooks
				var lock sync.Mutex
				var sourceFile *os.File
				var opens, reads, sourceWrites, targetWrites int
				hooks.open = func(path string, mode ReadMode, info os.FileInfo) (itemSource, error) {
					source, err := openNativeSource(path, mode, info)
					lock.Lock()
					opens++
					sourceFile = source.file
					lock.Unlock()
					return source, err
				}
				hooks.readEntry = func(file *os.File, _ os.FileInfo) (signature.Entry, signature.Status, error) {
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
						return want, signature.Fresh, nil
					case "corrupt":
						err := errors.New("decode signature entry failed")
						return signature.Entry{}, signature.Missing, err
					case "unreadable":
						return signature.Entry{}, signature.Missing, errors.New("read fixture")
					default:
						// A platform without the attribute reads as a missing entry.
						return signature.Entry{}, signature.Missing, nil
					}
				}
				hooks.writeEntry = func(file *os.File, entry signature.Entry) error {
					// Every publication describes the completed source content using indexed facts.
					if !entry.Equal(want) {
						t.Errorf("published signature = %#v, want %#v", entry, want)
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
				hooks.writeEntryPath = func(path string, entry signature.Entry) error {
					// The asynchronous source cache describes the same stable file after Close.
					if path != input || !entry.Equal(want) {
						t.Errorf("path cache = %q / %#v, want %q / %#v", path, entry, input, want)
					}
					lock.Lock()
					sourceWrites++
					lock.Unlock()
					return nil
				}

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
					SetFromDevice(WithReadMode(mode)), WithEventHandler(handler), withFS(hooks),
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
			if _, valid, err := signature.ReadCached(input); err != nil || valid {
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
		if !valid || !bytes.Equal(signature.SHA256, wantHash[:]) {
			t.Fatalf("signature for %q = %#v, valid=%t", path, signature, valid)
		}
	}
	if summary.Hits != 0 || summary.Writes != 2 {
		t.Fatalf("transfer summary = %#v", summary)
	}
}

func TestRunRefreshesOnlyTheSidesThatAllowIt(t *testing.T) {
	// A refreshing transfer writes each side's signature unless that device opted out.
	requireSignatureXattrSupport(t)
	content := []byte("side fixture")
	wantHash := sha256.Sum256(content)
	for _, test := range []struct {
		name           string
		options        []Option
		source, target bool
	}{
		{"source off", []Option{SetFromDevice(RefreshSignatureCache(false))}, false, true},
		{"target off", []Option{SetToDevice(RefreshSignatureCache(false))}, true, false},
		{"both off", []Option{SetFromDevice(RefreshSignatureCache(false)), SetToDevice(RefreshSignatureCache(false))}, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			input := writeSourceFile(t, root, "source.txt", content)
			target := filepath.Join(root, "target.txt")
			item := newFixtureItem(input, target)
			options := append([]Option{WithHashPolicy(HashReadRefresh)}, test.options...)
			if err := runFixture(context.Background(), newStreamFixture(item), []Item{item}, options...); err != nil {
				t.Fatal(err)
			}
			result, err := item.terminal(t)
			if err != nil || !bytes.Equal(result.SHA256, wantHash[:]) {
				t.Fatalf("result = %#v, err = %v", result, err)
			}
			for path, want := range map[string]bool{input: test.source, target: test.target} {
				if _, valid := readStoredSignature(t, path); valid != want {
					t.Fatalf("signature on %q present=%t, want %t", path, valid, want)
				}
			}
		})
	}
}
