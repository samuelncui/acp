package acp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samuelncui/acp/signature"
)

// entryFile is an encoded fixture file together with the facts of its fresh entries.
type entryFile struct {
	path  string
	plain []byte
	hash  []byte // SHA-256 of the plaintext
	disk  []byte // SHA-256 of the bytes on disk
}

func newEntryFile(t *testing.T) entryFile {
	t.Helper()
	requireSignatureXattrSupport(t)
	plain := runPlaintext()
	stored := rleEncode(t, plain)
	plainSum, diskSum := sha256.Sum256(plain), sha256.Sum256(stored)
	return entryFile{
		path:  writeSourceFile(t, t.TempDir(), "source.enc", stored),
		plain: plain, hash: plainSum[:], disk: diskSum[:],
	}
}

// fresh returns an entry bound to the file's current size and mtime.
func (f entryFile) fresh(t *testing.T, sha []byte, decoded *signature.Decoded) signature.Entry {
	t.Helper()
	info, err := os.Stat(f.path)
	if err != nil {
		t.Fatal(err)
	}
	return signature.Entry{Size: info.Size(), MtimeNS: info.ModTime().UnixNano(), SHA256: sha, Decoded: decoded}
}

func (f entryFile) decoded(sha []byte) *signature.Decoded {
	return &signature.Decoded{Encoding: "rle", Size: int64(len(f.plain)), SHA256: sha}
}

func (f entryFile) decodeItem() *decodedItem {
	return &decodedItem{SimpleJob: &SimpleJob{Path: f.path}, decode: newRLEDecoder, size: int64(len(f.plain))}
}

func storedEntry(t *testing.T, path string) signature.Entry {
	t.Helper()
	entry, _, err := signature.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

// runOne runs one item and returns its result and the cache summary.
func runOne(t *testing.T, item Item, opts ...Option) (Result, SignatureCacheSummary) {
	t.Helper()
	var summary SignatureCacheSummary
	handler := func(event Event) {
		if update, ok := event.(*EventSignatureCacheSummary); ok {
			summary = update.Summary
		}
	}
	onResults, snapshot := collectResults()
	if err := runStream(context.Background(), onResults, []Item{item}, append(opts, WithEventHandler(handler))...); err != nil {
		t.Fatal(err)
	}
	result := resultsByItem(t, snapshot())[item]
	if result.Err != nil {
		t.Fatalf("item failed: %v", result.Err)
	}
	return result, summary
}

func TestLegacyEntryIsReadAndRewritten(t *testing.T) {
	requireSignatureXattrSupport(t)
	path := writeSourceFile(t, t.TempDir(), "plain.bin", []byte("legacy fixture"))
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	legacy := signature.Entry{Size: info.Size(), MtimeNS: info.ModTime().UnixNano(), SHA256: sha("not the content")}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	err = writeRawSignature(file, legacyLayout(legacy))
	_ = file.Close()
	if err != nil {
		t.Fatal(err)
	}

	// The legacy layout is a plain hit.
	result, _ := runOne(t, &SimpleJob{Path: path}, WithHashPolicy(HashCachedOnly))
	if !result.SignatureCacheHit || !bytes.Equal(result.SHA256, legacy.SHA256) {
		t.Fatalf("legacy entry result = %#v", result)
	}

	// A refresh that computes another hash rewrites it in the Protobuf form.
	runOne(t, &SimpleJob{Path: path}, WithHashPolicy(HashReadRefresh))
	file, err = os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := readRawSignature(file)
	_ = file.Close()
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256([]byte("legacy fixture"))
	entry, status, err := signature.Read(path)
	if err != nil || status != signature.Fresh || strings.HasPrefix(string(raw), "ACPS") || !bytes.Equal(entry.SHA256, want[:]) {
		t.Fatalf("rewritten entry = %x / %#v %v / %v", raw, entry, status, err)
	}
}

func TestPlainAndDecodingRefreshesKeepEachOthersHash(t *testing.T) {
	f := newEntryFile(t)

	// A plain refresh from a missing entry writes only what it computed.
	runOne(t, &SimpleJob{Path: f.path}, WithHashPolicy(HashReadRefresh))
	if got := storedEntry(t, f.path); got.Decoded != nil || !bytes.Equal(got.SHA256, f.disk) {
		t.Fatalf("plain refresh entry = %#v", got)
	}

	// A decoding refresh keeps the fresh outer hash and adds the decoded part.
	runOne(t, f.decodeItem(), WithHashPolicy(HashReadRefresh))
	want := f.fresh(t, f.disk, f.decoded(f.hash))
	if got := storedEntry(t, f.path); !got.Equal(want) {
		t.Fatalf("decoding refresh entry = %#v, want %#v", got, want)
	}

	// A plain refresh over a changed outer hash keeps the fresh decoded part.
	seedEntry(t, f.path, f.fresh(t, sha("decoy"), f.decoded(f.hash)))
	runOne(t, &SimpleJob{Path: f.path}, WithHashPolicy(HashReadRefresh))
	if got := storedEntry(t, f.path); !got.Equal(want) {
		t.Fatalf("plain refresh entry = %#v, want %#v", got, want)
	}

	// A decoding refresh over a changed decoded hash keeps the fresh outer hash.
	seedEntry(t, f.path, f.fresh(t, f.disk, f.decoded(sha("decoy"))))
	runOne(t, f.decodeItem(), WithHashPolicy(HashReadRefresh))
	if got := storedEntry(t, f.path); !got.Equal(want) {
		t.Fatalf("decoding refresh entry = %#v, want %#v", got, want)
	}

	// An unchanged entry is not written again.
	_, summary := runOne(t, f.decodeItem(), WithHashPolicy(HashReadRefresh))
	if summary.Writes != 0 {
		t.Fatalf("unchanged decoding refresh wrote %d entries", summary.Writes)
	}

	// A stale entry contributes nothing to the refresh.
	stale := f.fresh(t, sha("decoy"), f.decoded(sha("decoy")))
	stale.MtimeNS++
	seedEntry(t, f.path, stale)
	runOne(t, f.decodeItem(), WithHashPolicy(HashReadRefresh))
	if got := storedEntry(t, f.path); !got.Equal(f.fresh(t, nil, f.decoded(f.hash))) {
		t.Fatalf("refresh from a stale entry = %#v", got)
	}
}

func TestReuseTrustsOnlyItsOwnHash(t *testing.T) {
	f := newEntryFile(t)
	outer, inner := sha("outer"), sha("inner")
	both := func(t *testing.T) signature.Entry { return f.fresh(t, outer, f.decoded(inner)) }
	tests := []struct {
		name    string
		decode  bool
		entry   func(t *testing.T) signature.Entry
		want    []byte
		stale   bool
		encoder string
	}{
		{name: "plain reuses the outer hash", entry: both, want: outer},
		{name: "plain misses a decoded-only entry", entry: func(t *testing.T) signature.Entry { return f.fresh(t, nil, f.decoded(inner)) }},
		{name: "decoding reuses the decoded hash", decode: true, entry: both, want: inner},
		{name: "decoding misses an outer-only entry", decode: true, entry: func(t *testing.T) signature.Entry { return f.fresh(t, outer, nil) }},
		{name: "decoding misses a missing decoded hash", decode: true, entry: func(t *testing.T) signature.Entry { return f.fresh(t, outer, f.decoded(nil)) }},
		{name: "decoding misses another encoding", decode: true, entry: func(t *testing.T) signature.Entry {
			entry := both(t)
			entry.Decoded.Encoding = "zstd"
			return entry
		}},
		{name: "decoding misses another declared size", decode: true, entry: func(t *testing.T) signature.Entry {
			entry := both(t)
			entry.Decoded.Size++
			return entry
		}},
		{name: "plain misses a stale entry", stale: true, entry: both},
		{name: "decoding misses a stale entry", decode: true, stale: true, entry: both},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			entry := test.entry(t)
			if test.stale {
				entry.MtimeNS++
			}
			seedEntry(t, f.path, entry)
			var item Item = &SimpleJob{Path: f.path}
			if test.decode {
				item = f.decodeItem()
			}
			result, summary := runOne(t, item, WithHashPolicy(HashCachedOnly))
			if test.want != nil {
				if !result.SignatureCacheHit || !bytes.Equal(result.SHA256, test.want) || summary.Hits != 1 {
					t.Fatalf("result = %#v summary = %#v, want a hit on %x", result, summary, test.want)
				}
				return
			}
			if result.SignatureCacheHit || result.SHA256 != nil || summary.Hits != 0 || summary.Misses+summary.Stale != 1 {
				t.Fatalf("result = %#v summary = %#v, want a miss", result, summary)
			}
			if test.stale != (summary.Stale == 1) {
				t.Fatalf("stale = %d, want stale=%t", summary.Stale, test.stale)
			}
		})
	}
}

func TestTargetlessDecodingPolicies(t *testing.T) {
	f := newEntryFile(t)
	decode := func(opts ...Option) func(HashPolicy) (Result, SignatureCacheSummary) {
		return func(policy HashPolicy) (Result, SignatureCacheSummary) {
			return runOne(t, f.decodeItem(), append(opts, WithHashPolicy(policy))...)
		}
	}
	run := decode()

	// A read policy without refresh computes the hash and leaves the file alone.
	result, _ := run(HashCachedOrRead)
	if result.SignatureCacheHit || !bytes.Equal(result.SHA256, f.hash) {
		t.Fatalf("cached-or-read result = %#v", result)
	}
	if _, status, _ := signature.Read(f.path); status != signature.Missing {
		t.Fatalf("cached-or-read stored an entry, status = %v", status)
	}

	// The refreshing policy reads on a miss, stores the decoded part, then hits.
	result, summary := run(HashCachedOrReadRefresh)
	if result.SignatureCacheHit || !bytes.Equal(result.SHA256, f.hash) || summary.Writes != 1 {
		t.Fatalf("refresh result = %#v summary = %#v", result, summary)
	}
	if got := storedEntry(t, f.path); !got.Equal(f.fresh(t, nil, f.decoded(f.hash))) {
		t.Fatalf("stored entry = %#v", got)
	}
	for _, policy := range []HashPolicy{HashCachedOnly, HashCachedOrRead, HashCachedOrReadRefresh} {
		result, summary := run(policy)
		if !result.SignatureCacheHit || !bytes.Equal(result.SHA256, f.hash) || summary.Hits != 1 || summary.Writes != 0 {
			t.Fatalf("%s result = %#v summary = %#v, want a hit", policy, result, summary)
		}
	}

	// A decoding read never reuses under the read-always policies, and the source side can opt out.
	if result, _ := run(HashRead); result.SignatureCacheHit {
		t.Fatalf("read policy reused a stored hash: %#v", result)
	}
	seedEntry(t, f.path, f.fresh(t, nil, f.decoded(sha("decoy"))))
	optOut := decode(SetFromDevice(RefreshSignatureCache(false)))
	if result, summary := optOut(HashReadRefresh); !bytes.Equal(result.SHA256, f.hash) || summary.Writes != 0 {
		t.Fatalf("opted-out result = %#v summary = %#v", result, summary)
	}
	if got := storedEntry(t, f.path); !bytes.Equal(got.Decoded.SHA256, sha("decoy")) {
		t.Fatalf("an opted-out source refreshed its entry: %#v", got)
	}

	// A hit reports the stored hash even when the stored copy changed since: only metadata decides.
	if err := os.WriteFile(f.path, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, summary := run(HashCachedOnly); summary.Stale != 1 {
		t.Fatalf("changed file summary = %#v, want stale", summary)
	}
}

// markerRun copies the encoded plaintext once and reports where each managed write happened.
type markerRun struct {
	root, out  string
	source     string
	target     string
	lock       sync.Mutex
	written    []string // paths the entry write named, with whether the final path existed
	finalSeen  []bool
	storedSize int64
	storedHash []byte
}

func runEncodedTarget(t *testing.T, linear bool, opts ...Option) *markerRun {
	t.Helper()
	requireSignatureXattrSupport(t)
	plain := runPlaintext()
	run := &markerRun{root: t.TempDir(), out: t.TempDir()}
	run.source = writeSourceFile(t, run.root, "source", plain)
	modTime := time.Unix(1_600_000_000, 987_654_321)
	if err := os.Chtimes(run.source, modTime, modTime); err != nil {
		t.Fatal(err)
	}
	run.target = filepath.Join(run.out, "source.enc")

	// Observe every managed write: the entry must exist before the file reaches its final name.
	record := func(path string) {
		run.lock.Lock()
		defer run.lock.Unlock()
		_, err := os.Lstat(run.target)
		run.written = append(run.written, path)
		run.finalSeen = append(run.finalSeen, err == nil)
	}
	opts = append(opts, withFS(fsHooks{
		writeEntry: func(file *os.File, entry signature.Entry) error {
			record(file.Name())
			return signature.WriteFile(file, entry)
		},
		writeEntryPath: func(path string, entry signature.Entry) error {
			record(path)
			return signature.WritePath(path, entry)
		},
	}))

	item := &encodedItem{SimpleJob: &SimpleJob{Path: run.source, Dsts: []string{run.target}}, encode: newRLEEncoder}
	if linear {
		opts = append(opts, SetToDevice(LinearDevice(true)))
	}
	result, _ := runOne(t, item, opts...)
	run.storedSize, run.storedHash = result.Targets[0].StoredSize, result.Targets[0].StoredSHA256
	if result.Targets[0].Err != nil {
		t.Fatalf("target failed: %v", result.Targets[0].Err)
	}
	return run
}

func TestEncodedTargetEntryIsWrittenBeforePublication(t *testing.T) {
	plain := runPlaintext()
	plainSum := sha256.Sum256(plain)
	for _, device := range []struct {
		name   string
		linear bool
	}{{"random", false}, {"linear", true}} {
		for _, test := range []struct {
			name string
			opts []Option
			// hashed expects both hashes in the entry and the stored hash in the result: the marker
			// holds whatever the transfer computed, whatever the refresh policy.
			hashed bool
		}{
			{"refreshing policy", []Option{WithHashPolicy(HashReadRefresh)}, true},
			{"default policy", nil, false},
			{"read policy", []Option{WithHashPolicy(HashRead)}, true},
			{"target opted out", []Option{WithHashPolicy(HashReadRefresh), SetToDevice(RefreshSignatureCache(false))}, true},
		} {
			t.Run(device.name+"/"+test.name, func(t *testing.T) {
				run := runEncodedTarget(t, device.linear, test.opts...)
				info, err := os.Stat(run.target)
				if err != nil {
					t.Fatal(err)
				}
				content, err := os.ReadFile(run.target)
				if err != nil {
					t.Fatal(err)
				}
				storedSum := sha256.Sum256(content)
				if test.hashed != (run.storedHash != nil) || (test.hashed && !bytes.Equal(run.storedHash, storedSum[:])) {
					t.Fatalf("stored hash = %x, want the hash of the file on disk %x: %t", run.storedHash, storedSum, test.hashed)
				}

				// The entry on the final file carries the marker and the stored facts.
				want := signature.Entry{
					Size: run.storedSize, MtimeNS: info.ModTime().UnixNano(),
					Decoded: &signature.Decoded{Encoding: "rle", Size: int64(len(plain))},
				}
				if test.hashed {
					want.SHA256 = storedSum[:]
					want.Decoded.SHA256 = plainSum[:]
				}
				got, status, err := signature.Read(run.target)
				if err != nil || status != signature.Fresh || !got.Equal(want) || info.Size() != run.storedSize {
					t.Fatalf("entry = %#v %v / %v, want %#v", got, status, err, want)
				}
				sourceInfo, err := os.Stat(run.source)
				if err != nil || want.MtimeNS != sourceInfo.ModTime().UnixNano() {
					t.Fatalf("entry mtime %d is not the source mtime, %v", want.MtimeNS, err)
				}

				// Exactly the marker was published, on the temporary file and before the rename.
				var targetWrites int
				out, err := filepath.EvalSymlinks(run.out)
				if err != nil {
					t.Fatal(err)
				}
				for i, path := range run.written {
					if resolved, err := filepath.EvalSymlinks(filepath.Dir(path)); err != nil || resolved != out {
						continue
					}
					targetWrites++
					if path == run.target || run.finalSeen[i] {
						t.Fatalf("entry written to %q after the final name existed", path)
					}
				}
				if targetWrites != 1 {
					t.Fatalf("target entry writes = %d, want 1 (%v)", targetWrites, run.written)
				}
			})
		}
	}
}

func TestEncodedTargetMarkerFailureFailsTheTarget(t *testing.T) {
	requireSignatureXattrSupport(t)
	for _, failure := range []error{errors.New("injected xattr failure"), errors.ErrUnsupported} {
		for _, device := range []struct {
			name   string
			linear bool
		}{{"random", false}, {"linear", true}} {
			t.Run(device.name+"/"+failure.Error(), func(t *testing.T) {
				root, out := t.TempDir(), t.TempDir()
				source := writeSourceFile(t, root, "source", runPlaintext())

				encoded := &encodedItem{SimpleJob: &SimpleJob{Path: source, Dsts: []string{filepath.Join(out, "encoded")}}, encode: newRLEEncoder}
				opts := []Option{withFS(fsHooks{
					writeEntry:     func(*os.File, signature.Entry) error { return failure },
					writeEntryPath: func(string, signature.Entry) error { return failure },
				})}
				if device.linear {
					opts = append(opts, SetToDevice(LinearDevice(true)))
				}
				onResults, snapshot := collectResults()
				if err := runStream(context.Background(), onResults, []Item{encoded}, opts...); err != nil {
					t.Fatal(err)
				}
				result := resultsByItem(t, snapshot())[encoded]
				if result.Err != nil || len(result.Targets) != 1 || !errors.Is(result.Targets[0].Err, failure) {
					t.Fatalf("result = %#v, want the target to fail with %v", result, failure)
				}
				assertOnlyFiles(t, out)
			})
		}
	}
}

func TestEncodingNameIsRequired(t *testing.T) {
	root, out := t.TempDir(), t.TempDir()
	source := writeSourceFile(t, root, "source", []byte("content"))
	for name, encoding := range map[string]string{"empty": "", "too long": strings.Repeat("x", signature.MaxEncoding+1)} {
		t.Run(name, func(t *testing.T) {
			encoded := &encodedItem{SimpleJob: &SimpleJob{Path: source, Dsts: []string{filepath.Join(out, name)}}, encode: newRLEEncoder}
			decoded := &decodedItem{SimpleJob: &SimpleJob{Path: source}, decode: newRLEDecoder, size: 1}
			onResults, snapshot := collectResults()
			// The helper's empty name means "rle", so name the invalid one through a wrapper.
			items := []Item{&namedEncoder{encodedItem: encoded, name: encoding}, &namedDecoder{decodedItem: decoded, name: encoding}}
			if err := runStream(context.Background(), onResults, items); err != nil {
				t.Fatal(err)
			}
			for _, item := range items {
				if result := resultsByItem(t, snapshot())[item]; result.Err == nil {
					t.Fatalf("item %T with encoding %q did not fail", item, encoding)
				}
			}
			assertOnlyFiles(t, out)
		})
	}
}

type namedEncoder struct {
	*encodedItem
	name string
}

func (i *namedEncoder) TargetEncoder() (string, func(io.Writer) (io.WriteCloser, error)) {
	return i.name, i.encode
}

type namedDecoder struct {
	*decodedItem
	name string
}

func (i *namedDecoder) SourceDecoder() (string, func(io.Reader) (io.ReadCloser, error), int64) {
	return i.name, i.decode, i.size
}

func TestFailedDecoderCloseRefreshesNothing(t *testing.T) {
	// A decoder that yields the declared plaintext and then rejects it in Close must not leave a
	// decoded hash that a later reuse policy would report as verified content.
	f := newEntryFile(t)
	failure := errors.New("decoder rejected the plaintext")
	item := &decodedItem{SimpleJob: &SimpleJob{Path: f.path}, size: int64(len(f.plain)),
		decode: func(src io.Reader) (io.ReadCloser, error) {
			decoder, err := newRLEDecoder(src)
			if err != nil {
				return nil, err
			}
			return closeErrorDecoder{Reader: decoder, err: failure}, nil
		}}
	onResults, snapshot := collectResults()
	if err := runStream(context.Background(), onResults, []Item{item}, WithHashPolicy(HashReadRefresh)); err != nil {
		t.Fatal(err)
	}
	if result := resultsByItem(t, snapshot())[item]; !errors.Is(result.Err, failure) {
		t.Fatalf("failed decode result = %#v", result)
	}
	if _, status, err := signature.Read(f.path); err != nil || status != signature.Missing {
		t.Fatalf("failed decode stored an entry, status = %v err = %v", status, err)
	}
	if result, summary := runOne(t, f.decodeItem(), WithHashPolicy(HashCachedOnly)); result.SignatureCacheHit || summary.Hits != 0 {
		t.Fatalf("cached-only after a failed decode = %#v summary = %#v, want a miss", result, summary)
	}
}
