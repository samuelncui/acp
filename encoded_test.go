package acp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samuelncui/acp/signature"
)

// rleEncoder is a trivial reversible test codec: (count, value) pairs ending with a (0, 0) pair.
// It buffers pairs, so the final run and the terminator reach dst only through Close.
type rleEncoder struct {
	dst      io.Writer
	buf      []byte
	value    byte
	count    int
	closeErr error
}

func newRLEEncoder(dst io.Writer) (io.WriteCloser, error) { return &rleEncoder{dst: dst}, nil }

func (e *rleEncoder) Write(data []byte) (int, error) {
	for _, b := range data {
		if e.count > 0 && (b != e.value || e.count == 255) {
			e.buf = append(e.buf, byte(e.count), e.value)
			e.count = 0
		}
		e.value = b
		e.count++
	}
	if len(e.buf) >= 4096 {
		if _, err := e.dst.Write(e.buf); err != nil {
			return 0, err
		}
		e.buf = e.buf[:0]
	}
	return len(data), nil
}

func (e *rleEncoder) Close() error {
	if e.count > 0 {
		e.buf = append(e.buf, byte(e.count), e.value)
	}
	e.buf = append(e.buf, 0, 0)
	if _, err := e.dst.Write(e.buf); err != nil {
		return err
	}
	return e.closeErr
}

var errRLETrailingData = errors.New("rle: data after terminator")

// rleDecoder expands rleEncoder output. A missing terminator is io.ErrUnexpectedEOF.
type rleDecoder struct {
	src   *bufio.Reader
	value byte
	count int
	done  bool
}

func newRLEDecoder(src io.Reader) (io.ReadCloser, error) {
	return io.NopCloser(&rleDecoder{src: bufio.NewReader(src)}), nil
}

func (d *rleDecoder) Read(data []byte) (int, error) {
	n := 0
	for n < len(data) && !d.done {
		if d.count == 0 {
			var pair [2]byte
			if _, err := io.ReadFull(d.src, pair[:]); err != nil {
				if err == io.EOF {
					err = io.ErrUnexpectedEOF
				}
				return n, err
			}
			if pair[0] == 0 {
				if _, err := d.src.ReadByte(); err != io.EOF {
					if err == nil {
						err = errRLETrailingData
					}
					return n, err
				}
				d.done = true
				break
			}
			d.value, d.count = pair[1], int(pair[0])
		}
		data[n] = d.value
		n++
		d.count--
	}
	if n == 0 && d.done {
		return 0, io.EOF
	}
	return n, nil
}

func rleEncode(t *testing.T, plain []byte) []byte {
	t.Helper()
	var encoded bytes.Buffer
	encoder, _ := newRLEEncoder(&encoded)
	if _, err := encoder.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := encoder.Close(); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func rleDecodeFile(t *testing.T, path string) []byte {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	decoder, _ := newRLEDecoder(file)
	plain, err := io.ReadAll(decoder)
	if err != nil {
		t.Fatalf("decode %q: %v", path, err)
	}
	return plain
}

// runPlaintext spans several chunks and shrinks under the test codec.
func runPlaintext() []byte {
	plain := make([]byte, 0, 2*batchSize+batchSize/2)
	for i := 0; len(plain) < cap(plain); i++ {
		plain = append(plain, bytes.Repeat([]byte{byte('a' + i%26)}, min(1000+i%7, cap(plain)-len(plain)))...)
	}
	return plain
}

// encodedItem encodes its native targets.
type encodedItem struct {
	*SimpleJob
	encode func(io.Writer) (io.WriteCloser, error)
	// encoding names the codec; empty means "rle".
	encoding string
}

func (i *encodedItem) TargetEncoder() (string, func(io.Writer) (io.WriteCloser, error)) {
	return codecName(i.encoding), i.encode
}

func codecName(name string) string {
	if name == "" {
		return "rle"
	}
	return name
}

func hasManagedSignature(t *testing.T, path string) bool {
	t.Helper()
	_, status, err := signature.Read(path)
	if err != nil {
		t.Fatalf("read managed signature of %q: %v", path, err)
	}
	return status != signature.Missing
}

func assertOnlyFiles(t *testing.T, dir string, names ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, entry := range entries {
		got = append(got, entry.Name())
	}
	if fmt.Sprint(got) != fmt.Sprint(names) {
		t.Fatalf("files in %q = %v, want %v", dir, got, names)
	}
}

func resultsByItem(t *testing.T, batches [][]Result) map[Item]Result {
	t.Helper()
	results := make(map[Item]Result)
	for _, batch := range batches {
		for _, result := range batch {
			if _, ok := results[result.Job]; ok {
				t.Fatalf("duplicate result for %v", result.Job)
			}
			results[result.Job] = result
		}
	}
	return results
}

var targetDevices = []struct {
	name    string
	options []Option
}{
	{"random", nil},
	{"linear", []Option{SetToDevice(LinearDevice(true))}},
}

func TestEncodedTargetStoresEncodedFile(t *testing.T) {
	// An encoded target hashes plaintext, stores encoded bytes and keeps staged publication.
	requireSignatureXattrSupport(t)
	plain := runPlaintext()
	want := sha256.Sum256(plain)
	modTime := time.Unix(1_600_000_000, 123_456_789)
	for _, device := range targetDevices {
		t.Run(device.name, func(t *testing.T) {
			root, out := t.TempDir(), t.TempDir()
			source := writeSourceFile(t, root, "source", plain)
			if err := os.Chmod(source, 0o640); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(source, modTime, modTime); err != nil {
				t.Fatal(err)
			}
			sourceInfo, err := os.Stat(source)
			if err != nil {
				t.Fatal(err)
			}
			var allocations atomic.Int32
			countAllocations := withFS(fsHooks{allocate: func(file *os.File, size int64) error {
				allocations.Add(1)
				return truncate(file, size)
			}})

			encodedPath := filepath.Join(out, "source.enc")
			plainPath := filepath.Join(out, "plain")
			encoded := &encodedItem{SimpleJob: &SimpleJob{Path: source, Dsts: []string{encodedPath}}, encode: newRLEEncoder}
			plainItem := &SimpleJob{Path: source, Dsts: []string{plainPath}}
			onResults, snapshot := collectResults()
			options := append([]Option{WithHashPolicy(HashReadRefresh), countAllocations}, device.options...)
			if err := runStream(context.Background(), onResults, []Item{encoded, plainItem}, options...); err != nil {
				t.Fatal(err)
			}
			results := resultsByItem(t, snapshot())

			// Hash and Size describe plaintext; StoredSize and StoredSHA256 are the encoded file on disk.
			result := results[encoded]
			if result.Err != nil || len(result.Targets) != 1 || result.Targets[0].Err != nil {
				t.Fatalf("encoded result = %#v", result)
			}
			if result.Size != int64(len(plain)) || !bytes.Equal(result.SHA256, want[:]) {
				t.Fatalf("encoded facts size=%d hash=%x", result.Size, result.SHA256)
			}
			stored, err := os.Stat(encodedPath)
			if err != nil {
				t.Fatal(err)
			}
			wantEncoded := rleEncode(t, plain)
			if result.Targets[0].StoredSize != stored.Size() || stored.Size() != int64(len(wantEncoded)) {
				t.Fatalf("stored size = %d, file size = %d, encoded size = %d", result.Targets[0].StoredSize, stored.Size(), len(wantEncoded))
			}
			onDisk, err := os.ReadFile(encodedPath)
			if err != nil {
				t.Fatal(err)
			}
			storedSum := sha256.Sum256(onDisk)
			if !bytes.Equal(result.Targets[0].StoredSHA256, storedSum[:]) {
				t.Fatalf("stored hash = %x, want the hash of the file on disk %x", result.Targets[0].StoredSHA256, storedSum)
			}
			if !bytes.Equal(rleDecodeFile(t, encodedPath), plain) {
				t.Fatal("encoded target does not decode to the source")
			}

			// Metadata belongs to the stored file; its entry names the encoding and describes both views,
			// so a plain read of the stored bytes can reuse their hash.
			if stored.Mode() != sourceInfo.Mode() || !stored.ModTime().Equal(sourceInfo.ModTime()) {
				t.Fatalf("encoded metadata mode=%s mtime=%s, want %s %s", stored.Mode(), stored.ModTime(), sourceInfo.Mode(), sourceInfo.ModTime())
			}
			entry, status, err := signature.Read(encodedPath)
			wantEntry := signature.Entry{
				Size: stored.Size(), MtimeNS: sourceInfo.ModTime().UnixNano(), SHA256: storedSum[:],
				Decoded: &signature.Decoded{Encoding: "rle", Size: int64(len(plain)), SHA256: want[:]},
			}
			if err != nil || status != signature.Fresh || !entry.Equal(wantEntry) {
				t.Fatalf("encoded entry = %#v %v / %v, want %#v", entry, status, err, wantEntry)
			}
			if signature, valid, err := signature.ReadCached(encodedPath); err != nil || !valid || !bytes.Equal(signature.SHA256, storedSum[:]) {
				t.Fatalf("plain view of the encoded file = %#v valid=%t / %v", signature, valid, err)
			}

			// The unencoded item keeps preallocation, the cache entry, and StoredSize and StoredSHA256
			// equal to Size and SHA256.
			plainResult := results[plainItem]
			if plainResult.Err != nil || plainResult.Targets[0].Err != nil || plainResult.Targets[0].StoredSize != int64(len(plain)) ||
				!bytes.Equal(plainResult.Targets[0].StoredSHA256, plainResult.SHA256) || len(plainResult.SHA256) == 0 {
				t.Fatalf("plain result = %#v", plainResult)
			}
			if !hasManagedSignature(t, plainPath) || !hasManagedSignature(t, source) {
				t.Fatal("unencoded target or source lost its managed signature")
			}
			wantAllocations := int32(1)
			if device.name == "linear" {
				wantAllocations = 0
			}
			if got := allocations.Load(); got != wantAllocations {
				t.Fatalf("allocations = %d, want %d (only the unencoded random target)", got, wantAllocations)
			}
			assertOnlyFiles(t, out, "plain", "source.enc")

			// One item with several targets stores the hash of each file on disk; an empty source still
			// stores its encoder terminator; a failed target beside successful ones has none.
			multiOut := t.TempDir()
			blocker := filepath.Join(multiOut, "blocker")
			if err := os.WriteFile(blocker, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			emptySource := writeSourceFile(t, root, "empty", nil)
			multi := &encodedItem{SimpleJob: &SimpleJob{
				Path: source, Dsts: []string{filepath.Join(multiOut, "a.enc"), filepath.Join(blocker, "failed.enc"), filepath.Join(multiOut, "b.enc")},
			}, encode: newRLEEncoder}
			empty := &encodedItem{SimpleJob: &SimpleJob{Path: emptySource, Dsts: []string{filepath.Join(multiOut, "empty.enc")}}, encode: newRLEEncoder}
			multiResults, multiSnapshot := collectResults()
			if err := runStream(context.Background(), multiResults, []Item{multi, empty}, options...); err != nil {
				t.Fatal(err)
			}
			byItem := resultsByItem(t, multiSnapshot())
			for _, item := range []*encodedItem{multi, empty} {
				result := byItem[item]
				if result.Err != nil || len(result.Targets) != len(item.Dsts) {
					t.Fatalf("result of %q = %#v", item.Path, result)
				}
				for _, target := range result.Targets {
					if filepath.Base(target.Path) == "failed.enc" {
						if target.Err == nil || target.StoredSHA256 != nil || target.StoredSize != 0 {
							t.Fatalf("failed target = %#v", target)
						}
						continue
					}
					content, err := os.ReadFile(target.Path)
					if err != nil || target.Err != nil {
						t.Fatalf("target %q err=%v read=%v", target.Path, target.Err, err)
					}
					sum := sha256.Sum256(content)
					if len(content) == 0 || target.StoredSize != int64(len(content)) || !bytes.Equal(target.StoredSHA256, sum[:]) {
						t.Fatalf("target %q stored size=%d hash=%x, want size %d hash %x", target.Path, target.StoredSize, target.StoredSHA256, len(content), sum)
					}
				}
			}
		})
	}
}

func TestEncodedTargetFailureLeavesNoTarget(t *testing.T) {
	// A refused encoder or a failed encoder Close fails only that target and publishes nothing.
	sentinel := errors.New("encoder failed")
	for _, failure := range []struct {
		name   string
		encode func(io.Writer) (io.WriteCloser, error)
	}{
		{"start", func(io.Writer) (io.WriteCloser, error) { return nil, sentinel }},
		{"close", func(dst io.Writer) (io.WriteCloser, error) { return &rleEncoder{dst: dst, closeErr: sentinel}, nil }},
		{"close panic", func(dst io.Writer) (io.WriteCloser, error) {
			return &panicCloser{WriteCloser: &rleEncoder{dst: dst}, value: sentinel}, nil
		}},
	} {
		for _, device := range targetDevices {
			t.Run(failure.name+"/"+device.name, func(t *testing.T) {
				root, out := t.TempDir(), t.TempDir()
				source := writeSourceFile(t, root, "source", runPlaintext())
				failed := &encodedItem{SimpleJob: &SimpleJob{Path: source, Dsts: []string{filepath.Join(out, "failed")}}, encode: failure.encode}
				next := &SimpleJob{Path: source, Dsts: []string{filepath.Join(out, "next")}}
				onResults, snapshot := collectResults()
				options := append([]Option{WithHashPolicy(HashRead)}, device.options...)
				if err := runStream(context.Background(), onResults, []Item{failed, next}, options...); err != nil {
					t.Fatal(err)
				}
				results := resultsByItem(t, snapshot())
				target := results[failed].Targets[0]
				if results[failed].Err != nil || !errors.Is(target.Err, sentinel) || target.StoredSize != 0 || target.StoredSHA256 != nil {
					t.Fatalf("failed result = %#v", results[failed])
				}
				if results[next].Err != nil || results[next].Targets[0].Err != nil {
					t.Fatalf("next result = %#v", results[next])
				}
				assertOnlyFiles(t, out, "next")
			})
		}
	}
}

type panicCloser struct {
	io.WriteCloser
	value error
}

func (c *panicCloser) Close() error {
	_ = c.WriteCloser.Close()
	panic(c.value)
}

// decodedItem decodes its native source with a declared plaintext size.
type decodedItem struct {
	*SimpleJob
	decode func(io.Reader) (io.ReadCloser, error)
	size   int64
	// encoding names the codec; empty means "rle".
	encoding string
}

func (i *decodedItem) SourceDecoder() (string, func(io.Reader) (io.ReadCloser, error), int64) {
	return codecName(i.encoding), i.decode, i.size
}

var sourceDevices = []struct {
	name    string
	options []Option
}{
	{"buffered", nil},
	{"mapped", []Option{SetFromDevice(WithReadMode(ReadMapped))}},
	{"linear", []Option{SetFromDevice(LinearDevice(true))}},
}

func TestDecodedSourceRoundTrip(t *testing.T) {
	// An encoded copy reads back through its decoder with plaintext facts.
	requireSignatureXattrSupport(t)
	plain := runPlaintext()
	want := sha256.Sum256(plain)
	modTime := time.Unix(1_600_000_000, 123_456_789)
	for _, device := range sourceDevices {
		t.Run(device.name, func(t *testing.T) {
			root, out := t.TempDir(), t.TempDir()
			source := writeSourceFile(t, root, "source", plain)
			if err := os.Chmod(source, 0o640); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(source, modTime, modTime); err != nil {
				t.Fatal(err)
			}
			sourceInfo, err := os.Stat(source)
			if err != nil {
				t.Fatal(err)
			}
			encodedPath := filepath.Join(root, "source.enc")
			encoded := &encodedItem{SimpleJob: &SimpleJob{Path: source, Dsts: []string{encodedPath}}, encode: newRLEEncoder}
			onResults, snapshot := collectResults()
			if err := runStream(context.Background(), onResults, []Item{encoded}, WithHashPolicy(HashReadRefresh)); err != nil {
				t.Fatal(err)
			}
			written := resultsByItem(t, snapshot())[encoded]
			if written.Err != nil || written.Targets[0].Err != nil {
				t.Fatalf("encode result = %#v", written)
			}
			stored, err := os.Stat(encodedPath)
			if err != nil {
				t.Fatal(err)
			}
			if written.Targets[0].StoredSize != stored.Size() || stored.Size() >= int64(len(plain)) {
				t.Fatalf("stored size = %d, file size = %d, plaintext size = %d", written.Targets[0].StoredSize, stored.Size(), len(plain))
			}

			// Restore to a plain target and verify without a target in one refreshing run.
			restoredPath := filepath.Join(out, "restored")
			restore := &decodedItem{SimpleJob: &SimpleJob{Path: encodedPath, Dsts: []string{restoredPath}}, decode: newRLEDecoder, size: int64(len(plain))}
			verify := &decodedItem{SimpleJob: &SimpleJob{Path: encodedPath}, decode: newRLEDecoder, size: int64(len(plain))}
			onResults, snapshot = collectResults()
			options := append([]Option{WithHashPolicy(HashReadRefresh)}, device.options...)
			if err := runStream(context.Background(), onResults, []Item{restore, verify}, options...); err != nil {
				t.Fatal(err)
			}
			results := resultsByItem(t, snapshot())
			for _, item := range []Item{restore, verify} {
				result := results[item]
				if result.Err != nil || result.Size != int64(len(plain)) || !bytes.Equal(result.SHA256, want[:]) || result.SignatureCacheHit {
					t.Fatalf("decoded result = %#v", result)
				}
				if !bytes.Equal(result.SHA256, written.SHA256) {
					t.Fatalf("decoded hash %x differs from written hash %x", result.SHA256, written.SHA256)
				}
			}
			target := results[restore].Targets[0]
			if target.Err != nil || target.StoredSize != int64(len(plain)) {
				t.Fatalf("restored target = %#v", target)
			}
			restored, err := os.ReadFile(restoredPath)
			if err != nil || !bytes.Equal(restored, plain) {
				t.Fatalf("restored content differs, err = %v", err)
			}

			// The stored copy's metadata travels to the plaintext; only the plaintext target is cached.
			info, err := os.Stat(restoredPath)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode() != sourceInfo.Mode() || !info.ModTime().Equal(sourceInfo.ModTime()) {
				t.Fatalf("restored metadata mode=%s mtime=%s, want %s %s", info.Mode(), info.ModTime(), sourceInfo.Mode(), sourceInfo.ModTime())
			}
			// The decoding refresh keeps the stored-bytes hash the encoding write recorded.
			if entry, status, err := signature.Read(encodedPath); err != nil || status != signature.Fresh ||
				entry.Decoded == nil || !bytes.Equal(entry.Decoded.SHA256, want[:]) ||
				len(entry.SHA256) == 0 || !bytes.Equal(entry.SHA256, written.Targets[0].StoredSHA256) {
				t.Fatalf("encoded copy entry = %#v %v / %v", entry, status, err)
			}
			if signature, valid := readStoredSignature(t, restoredPath); !valid || !bytes.Equal(signature.SHA256[:], want[:]) {
				t.Fatalf("restored signature = %#v valid=%t", signature, valid)
			}
		})
	}
}

type closeErrorDecoder struct {
	io.Reader
	err error
}

func (d closeErrorDecoder) Close() error { return d.err }

func TestDecodedSourceFailures(t *testing.T) {
	// Decoder failures wrap ErrEncodedContent, native read errors keep their identity, and each
	// fails only its own item while the rest of the run completes.
	plain := runPlaintext()
	encoded := rleEncode(t, plain)
	sentinel := errors.New("decoder failed")
	readErr := errors.New("native read failed")
	size := int64(len(plain))
	for _, failure := range []struct {
		name    string
		stored  []byte
		decode  func(io.Reader) (io.ReadCloser, error)
		size    int64
		readErr bool
		want    error
	}{
		{"start", encoded, func(io.Reader) (io.ReadCloser, error) { return nil, sentinel }, size, false, sentinel},
		{"trailing data", append(append([]byte(nil), encoded...), 1), newRLEDecoder, size, false, errRLETrailingData},
		{"missing terminator", encoded[:len(encoded)-2], newRLEDecoder, size, false, io.ErrUnexpectedEOF},
		{"short plaintext", encoded, newRLEDecoder, size + 1, false, ErrEncodedContent},
		{"long plaintext", encoded, newRLEDecoder, size - 1, false, ErrEncodedContent},
		{"close", encoded, func(src io.Reader) (io.ReadCloser, error) {
			decoder, _ := newRLEDecoder(src)
			return closeErrorDecoder{Reader: decoder, err: sentinel}, nil
		}, size, false, sentinel},
		{"native read", encoded, newRLEDecoder, size, true, readErr},
		{"native read at start", encoded, func(src io.Reader) (io.ReadCloser, error) {
			// A decoder that reads a header and drops the cause still reports the native error.
			if _, err := src.Read(make([]byte, 1)); err != nil {
				return nil, fmt.Errorf("read header: %v", err)
			}
			return newRLEDecoder(src)
		}, size, true, readErr},
	} {
		t.Run(failure.name, func(t *testing.T) {
			root, out := t.TempDir(), t.TempDir()
			bad := writeSourceFile(t, root, "bad.enc", failure.stored)
			good := writeSourceFile(t, root, "good.enc", encoded)
			plainSource := writeSourceFile(t, root, "plain", plain)
			options := []Option{WithHashPolicy(HashRead)}
			if failure.readErr {
				options = append(options, withFS(fsHooks{open: func(path string, mode ReadMode, info os.FileInfo) (itemSource, error) {
					source, err := openNativeSource(path, mode, info)
					if err == nil && path == bad {
						source.reader = failingReader{ReadCloser: source.reader, err: readErr}
					}
					return source, err
				}}))
			}
			badVerify := &decodedItem{SimpleJob: &SimpleJob{Path: bad}, decode: failure.decode, size: failure.size}
			badRestore := &decodedItem{SimpleJob: &SimpleJob{Path: bad, Dsts: []string{filepath.Join(out, "bad")}}, decode: failure.decode, size: failure.size}
			goodRestore := &decodedItem{SimpleJob: &SimpleJob{Path: good, Dsts: []string{filepath.Join(out, "good")}}, decode: newRLEDecoder, size: size}
			plainCopy := &SimpleJob{Path: plainSource, Dsts: []string{filepath.Join(out, "plain")}}
			negative := &decodedItem{SimpleJob: &SimpleJob{Path: good}, decode: newRLEDecoder, size: -1}
			onResults, snapshot := collectResults()
			items := []Item{badVerify, badRestore, goodRestore, plainCopy, negative}
			if err := runStream(context.Background(), onResults, items, options...); err != nil {
				t.Fatal(err)
			}
			results := resultsByItem(t, snapshot())

			wantEncoded := !failure.readErr
			if err := results[badVerify].Err; !errors.Is(err, failure.want) || errors.Is(err, ErrEncodedContent) != wantEncoded {
				t.Fatalf("targetless error = %v, want %v (encoded=%t)", err, failure.want, wantEncoded)
			}
			if err := results[badRestore].Targets[0].Err; !errors.Is(err, failure.want) || errors.Is(err, ErrEncodedContent) != wantEncoded {
				t.Fatalf("target error = %v, want %v (encoded=%t)", err, failure.want, wantEncoded)
			}
			if err := results[negative].Err; err == nil || errors.Is(err, ErrEncodedContent) {
				t.Fatalf("negative size error = %v", err)
			}
			for _, item := range []Item{goodRestore, plainCopy} {
				if result := results[item]; result.Err != nil || result.Targets[0].Err != nil {
					t.Fatalf("independent result = %#v", result)
				}
			}
			assertOnlyFiles(t, out, "good", "plain")
		})
	}
}

// failingReader is a native source whose every read fails.
type failingReader struct {
	io.ReadCloser
	err error
}

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

func TestPlaintextTargetStoredHash(t *testing.T) {
	for _, policy := range []HashPolicy{HashOff, HashRead} {
		t.Run(policy.String(), func(t *testing.T) {
			root := t.TempDir()
			source := writeSourceFile(t, root, "source", []byte("plaintext target"))
			result, _ := runOne(t, &SimpleJob{Path: source, Dsts: []string{filepath.Join(root, "target")}}, WithHashPolicy(policy))
			if result.Err != nil || result.Targets[0].Err != nil {
				t.Fatalf("result = %#v", result)
			}

			// A hashing transfer reports the content hash as the stored hash; unknown stays nil.
			want := result.SHA256
			if policy == HashOff {
				want = nil
			}
			if len(want) != len(result.Targets[0].StoredSHA256) || !bytes.Equal(result.Targets[0].StoredSHA256, want) ||
				(policy == HashRead && len(want) == 0) {
				t.Fatalf("stored hash = %x, SHA256 = %x", result.Targets[0].StoredSHA256, result.SHA256)
			}
		})
	}
}
