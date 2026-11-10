package acp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

type testContentItem struct {
	*SimpleJob
	source  ContentSource
	targets []ContentTarget
}

func (i *testContentItem) ContentSource() ContentSource    { return i.source }
func (i *testContentItem) ContentTargets() []ContentTarget { return i.targets }

type testContentSource func(context.Context) (fs.File, error)

func (s testContentSource) Open(ctx context.Context) (fs.File, error) {
	return s(ctx)
}

type testContentFile struct {
	io.ReadCloser
	metadata ContentMetadata
	stat     func() (fs.FileInfo, error)
	xattrs   func() (map[string][]byte, error)
}

func (f *testContentFile) Stat() (fs.FileInfo, error) {
	if f.stat != nil {
		return f.stat()
	}
	return f.metadata.Info, nil
}

func (f *testContentFile) Xattrs() (map[string][]byte, error) {
	if f.xattrs != nil {
		return f.xattrs()
	}
	return f.metadata.Xattrs, nil
}

type testContentTarget func(context.Context, ContentMetadata) (ContentWriter, error)

func (t testContentTarget) Open(ctx context.Context, metadata ContentMetadata) (ContentWriter, error) {
	return t(ctx, metadata)
}

type testContentReader struct {
	io.Reader
	close func() error
}

func (r *testContentReader) Close() error { return r.close() }

type testContentWriter struct {
	io.Writer
	finish func(error) error
}

func (w *testContentWriter) Finish(cause error) error { return w.finish(cause) }

func TestContentSourceObservedMetadataPrecedesNativeOutput(t *testing.T) {
	// The identifier is deliberately not a native path; only Open owns source observation.
	root := t.TempDir()
	data := []byte("observed content")
	mtime := time.Unix(1700000000, 0)
	closed := false
	item := &testContentItem{SimpleJob: &SimpleJob{Path: "member/a/../b", Dsts: []string{filepath.Join(root, "copy")}}}
	item.source = testContentSource(func(context.Context) (fs.File, error) {
		if _, err := os.Stat(item.Dsts[0]); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("output exists before source Open: %v", err)
		}
		return &testContentFile{
			ReadCloser: &testContentReader{Reader: bytes.NewReader(data), close: func() error { closed = true; return nil }},
			metadata: ContentMetadata{Info: contentForeignInfo{contentInfo{
				name: "b", size: int64(len(data)), mode: 0600, modTime: mtime,
			}}},
		}, nil
	})

	// Real native staging, allocation and publication must consume the observed size and mode.
	callback, batches := collectResults()
	if err := runStream(
		context.Background(),
		callback,
		[]Item{item},
		SetFromDevice(LinearDevice(true)),
		WithHashPolicy(HashReadRefresh),
	); err != nil {
		t.Fatal(err)
	}
	result := batches()[0][0]
	got, err := os.ReadFile(item.Dsts[0])
	if err != nil || !bytes.Equal(got, data) || !closed {
		t.Fatalf("output=%q, err=%v, source closed=%v", got, err, closed)
	}
	info, err := os.Stat(item.Dsts[0])
	if err != nil || info.Mode().Perm() != 0600 || !info.ModTime().Equal(mtime) {
		t.Fatalf("native metadata=%v, err=%v", info, err)
	}
	digest := sha256.Sum256(data)
	if result.Err != nil || result.Targets[0].Err != nil || result.Size != int64(len(data)) ||
		!bytes.Equal(result.SHA256, digest[:]) {
		t.Fatalf("result=%#v", result)
	}
}

func TestContentSourceOpenAndCloseFailuresDoNotPublish(t *testing.T) {
	for _, stage := range []string{"open", "close"} {
		t.Run(stage, func(t *testing.T) {
			// A failed member must not leave an output that a subsequent attempt could adopt.
			root := t.TempDir()
			failure := errors.New(stage + " failure")
			closes := 0
			item := &testContentItem{SimpleJob: &SimpleJob{Path: "member", Dsts: []string{filepath.Join(root, "copy")}}}
			item.source = testContentSource(func(context.Context) (fs.File, error) {
				if stage == "open" {
					return nil, failure
				}
				return &testContentFile{
					ReadCloser: &testContentReader{
						Reader: strings.NewReader("abc"), close: func() error { closes++; return failure },
					},
					metadata: ContentMetadata{Info: contentInfo{name: "member", size: 3, mode: 0600}},
				}, nil
			})

			// The exact Close cause reaches both the item and its uncommitted native target.
			callback, batches := collectResults()
			err := runStream(
				context.Background(),
				callback,
				[]Item{item},
				SetFromDevice(LinearDevice(true)),
				WithHashPolicy(HashRead),
			)
			result := batches()[0][0]
			if !errors.Is(err, failure) || !errors.Is(result.Err, failure) || !errors.Is(result.Targets[0].Err, failure) {
				t.Fatalf("run=%v, result=%#v", err, result)
			}
			if _, err := os.Stat(item.Dsts[0]); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed member was published: %v", err)
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 0 || (stage == "close" && closes != 1) {
				t.Fatalf("owned output cleanup: entries=%v, err=%v, closes=%d", entries, err, closes)
			}
		})
	}
}

func TestContentEndpointsSerializeThroughCloseAndFinish(t *testing.T) {
	// Synthetic stream members record lifecycle order without filesystem identifiers.
	var lock sync.Mutex
	var events []string
	record := func(event string) { lock.Lock(); events = append(events, event); lock.Unlock() }
	var items []Item
	for _, name := range []string{"first", "second", "third"} {
		item := &testContentItem{SimpleJob: &SimpleJob{Path: name, Dsts: []string{"stream/a/../" + name}}}
		item.source = testContentSource(func(context.Context) (fs.File, error) {
			record(name + " open source")
			return &testContentFile{ReadCloser: &testContentReader{Reader: strings.NewReader(name), close: func() error {
				record(name + " close source")
				return nil
			}}, metadata: ContentMetadata{Info: contentInfo{name: name, size: int64(len(name)), mode: 0640}}}, nil
		})
		item.targets = []ContentTarget{testContentTarget(func(
			_ context.Context, metadata ContentMetadata,
		) (ContentWriter, error) {
			record(name + " open target")
			if metadata.Info.Size() != int64(len(name)) {
				t.Errorf("observed metadata size=%d", metadata.Info.Size())
			}
			return &testContentWriter{Writer: io.Discard, finish: func(cause error) error {
				record(name + " finish target")
				if cause != nil {
					t.Errorf("Finish cause=%v", cause)
				}
				return nil
			}}, nil
		})}
		items = append(items, item)
	}

	// Both endpoint sides use one lane even though preparation has multiple workers.
	callback, batches := collectResults()
	if err := runStream(
		context.Background(),
		callback,
		items,
		SetFromDevice(LinearDevice(true)),
		SetToDevice(LinearDevice(true)),
		WithHashPolicy(HashRead),
	); err != nil {
		t.Fatal(err)
	}
	if len(events) != 12 {
		t.Fatalf("events=%v", events)
	}
	for i, name := range []string{"first", "second", "third"} {
		member := events[i*4 : (i+1)*4]
		if member[0] != name+" open source" || member[3] != name+" finish target" {
			t.Fatalf("member lifecycle=%v", member)
		}
		middle := []string{name + " open target", name + " close source"}
		if !reflect.DeepEqual(member[1:3], middle) && !reflect.DeepEqual(member[1:3], []string{middle[1], middle[0]}) {
			t.Fatalf("member lifecycle=%v", member)
		}
	}
	for _, batch := range batches() {
		for _, result := range batch {
			if result.Err != nil || result.Targets[0].Err != nil || result.Targets[0].Path != result.Job.Targets()[0] {
				t.Fatalf("result=%#v", result)
			}
		}
	}
}

func TestContentTargetAcceptsNativeMappedSourcesInRequestOrder(t *testing.T) {
	// Native sources preserve configured mappings and observed metadata with a single endpoint target.
	root := t.TempDir()
	var opened []string
	var items []Item
	for _, name := range []string{"first", "second", "third"} {
		source := writeSourceFile(t, root, name, []byte(name))
		item := &testContentItem{SimpleJob: &SimpleJob{Path: source, Dsts: []string{"display/a/../" + name}}}
		item.targets = []ContentTarget{testContentTarget(func(
			_ context.Context, metadata ContentMetadata,
		) (ContentWriter, error) {
			opened = append(opened, name)
			if metadata.Info.Size() != int64(len(name)) {
				t.Errorf("size=%d", metadata.Info.Size())
			}
			return &testContentWriter{Writer: io.Discard, finish: func(error) error { return nil }}, nil
		})}
		items = append(items, item)
	}

	// Native source mappings and stream result identifiers remain independent.
	callback, batches := collectResults()
	if err := runStream(
		context.Background(),
		callback,
		items,
		SetFromDevice(WithReadMode(ReadMapped)),
		SetToDevice(LinearDevice(true)),
		WithHashPolicy(HashRead),
	); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(opened, []string{"first", "second", "third"}) {
		t.Fatalf("Open order=%v", opened)
	}
	for _, batch := range batches() {
		for _, result := range batch {
			if result.Err != nil || result.Targets[0].Err != nil {
				t.Fatalf("result=%#v", result)
			}
		}
	}
}

func TestContentFailuresFinishExactlyOnceAndStopTheLane(t *testing.T) {
	for _, stage := range []string{
		"short read", "read", "short write", "write", "close", "finish", "open target", "open target handle",
	} {
		t.Run(stage, func(t *testing.T) {
			// Inject the boundary failure into the first member; the second must remain unopened.
			failure := errors.New(stage)
			if stage == "short read" {
				failure = io.ErrUnexpectedEOF
			}
			if stage == "short write" {
				failure = io.ErrShortWrite
			}
			var opens, finishes int
			var finishCause error
			var items []Item
			for range 2 {
				item := &testContentItem{SimpleJob: &SimpleJob{Path: "source", Dsts: []string{"target"}}}
				item.source = testContentSource(func(context.Context) (fs.File, error) {
					opens++
					var reader io.Reader = strings.NewReader("abc")
					if stage == "read" {
						reader = contentErrorReader{err: failure}
					}
					size := int64(3)
					if stage == "short read" {
						size = 4
					}
					return &testContentFile{ReadCloser: &testContentReader{Reader: reader, close: func() error {
						if stage == "close" {
							return failure
						}
						return nil
					}}, metadata: ContentMetadata{Info: contentInfo{name: "source", size: size, mode: 0600}}}, nil
				})
				item.targets = []ContentTarget{testContentTarget(func(context.Context, ContentMetadata) (ContentWriter, error) {
					if stage == "open target" {
						return nil, failure
					}
					var writer io.Writer = io.Discard
					if stage == "write" {
						writer = contentErrorWriter{err: failure}
					}
					if stage == "short write" {
						writer = contentErrorWriter{}
					}
					var openErr error
					if stage == "open target handle" {
						openErr = failure
					}
					return &testContentWriter{Writer: writer, finish: func(cause error) error {
						finishes++
						finishCause = cause
						if stage == "finish" {
							return failure
						}
						return nil
					}}, openErr
				})}
				items = append(items, item)
			}

			// Finish observes read, write and Close failures and is never retried by cleanup.
			callback, batches := collectResults()
			err := runStream(
				context.Background(),
				callback,
				items,
				SetFromDevice(LinearDevice(true)),
				SetToDevice(LinearDevice(true)),
				WithHashPolicy(HashRead),
			)
			wantFinishes := 1
			if stage == "open target" {
				wantFinishes = 0
			}
			if !errors.Is(err, failure) || opens != 1 || finishes != wantFinishes {
				t.Fatalf("run=%v, opens=%d, finishes=%d", err, opens, finishes)
			}
			if stage != "finish" && stage != "open target" && !errors.Is(finishCause, failure) {
				t.Fatalf("Finish cause=%v, want=%v", finishCause, failure)
			}
			if len(batches()) != 2 || !errors.Is(batches()[0][0].Targets[0].Err, failure) ||
				!errors.Is(batches()[1][0].Err, failure) {
				t.Fatalf("results=%v", batches())
			}
		})
	}
}

type contentErrorReader struct{ err error }

func (r contentErrorReader) Read([]byte) (int, error) { return 0, r.err }

type contentErrorWriter struct{ err error }

func (w contentErrorWriter) Write([]byte) (int, error) { return 0, w.err }

func TestContentSourceHashPoliciesIgnoreForeignCache(t *testing.T) {
	for _, policy := range []HashPolicy{
		HashOff, HashCachedOnly, HashCachedOrRead, HashCachedOrReadRefresh, HashRead, HashReadRefresh,
	} {
		t.Run(policy.String(), func(t *testing.T) {
			// A valid decoy matches the observed facts but cannot opt a foreign file into cache reuse.
			data := []byte("actual content")
			mtime := time.Unix(1700000000, 0)
			decoy := encodeCachedSignature(CachedSignature{
				Size: int64(len(data)), MtimeNS: mtime.UnixNano(), SHA256: [32]byte{1},
			})
			opens, closes := 0, 0
			item := &testContentItem{
				SimpleJob: &SimpleJob{Path: "stream member"},
				source: testContentSource(func(context.Context) (fs.File, error) {
					opens++
					return &testContentFile{
						ReadCloser: &testContentReader{Reader: bytes.NewReader(data), close: func() error {
							closes++
							return nil
						}},
						metadata: ContentMetadata{
							Info:   contentInfo{name: "member", size: int64(len(data)), mode: 0600, modTime: mtime},
							Xattrs: map[string][]byte{"acp.signature": decoy, "user.acp.signature": decoy},
						},
					}, nil
				}),
			}

			// Content policies hash actual bytes; off and cached-only observe nothing and invent no hash.
			callback, batches := collectResults()
			if err := runStream(context.Background(), callback, []Item{item},
				SetFromDevice(LinearDevice(true)), WithHashPolicy(policy)); err != nil {
				t.Fatal(err)
			}
			var want []byte
			wantOpens := 0
			if policy != HashOff && policy != HashCachedOnly {
				digest := sha256.Sum256(data)
				want, wantOpens = digest[:], 1
			}
			result := batches()[0][0]
			if result.Err != nil || !bytes.Equal(result.SHA256, want) || result.SignatureCacheHit ||
				opens != wantOpens || closes != wantOpens {
				t.Fatalf("opens=%d, closes=%d, result=%#v", opens, closes, result)
			}
		})
	}
}

func TestContentCancellationClosesWithoutDraining(t *testing.T) {
	// A reader with a large remaining payload cancels after its first buffer, without draining it.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var reads atomic.Int64
	closes := 0
	item := &testContentItem{SimpleJob: &SimpleJob{Path: "member", Dsts: []string{"target"}}}
	item.source = testContentSource(func(context.Context) (fs.File, error) {
		reader := contentCancelReader{cancel: cancel, reads: &reads}
		return &testContentFile{
			ReadCloser: &testContentReader{Reader: reader, close: func() error { closes++; return nil }},
			metadata:   ContentMetadata{Info: contentInfo{name: "member", size: 1 << 40, mode: 0600}},
		}, nil
	})
	finishes := 0
	item.targets = []ContentTarget{testContentTarget(func(context.Context, ContentMetadata) (ContentWriter, error) {
		return &testContentWriter{Writer: io.Discard, finish: func(cause error) error {
			finishes++
			if !errors.Is(cause, context.Canceled) {
				t.Errorf("Finish cause=%v", cause)
			}
			return nil
		}}, nil
	})}

	// Cancellation keeps its original cause and joins every endpoint handle exactly once.
	callback, batches := collectResults()
	err := runStream(
		ctx,
		callback,
		[]Item{item},
		SetFromDevice(LinearDevice(true)),
		SetToDevice(LinearDevice(true)),
		WithHashPolicy(HashRead),
	)
	if !errors.Is(err, context.Canceled) || reads.Load() != 1 || closes != 1 || finishes != 1 {
		t.Fatalf("run=%v, reads=%d, closes=%d, finishes=%d, results=%v", err, reads.Load(), closes, finishes, batches())
	}
}

type contentCancelReader struct {
	cancel context.CancelFunc
	reads  *atomic.Int64
}

func (r contentCancelReader) Read(data []byte) (int, error) {
	r.reads.Add(1)
	r.cancel()
	clear(data)
	return len(data), nil
}

type contentForeignInfo struct{ contentInfo }

func (contentForeignInfo) Sys() any { return "endpoint-owned metadata" }

func TestContentOptionsFailBeforeOpeningEitherSide(t *testing.T) {
	for _, tc := range []struct {
		name    string
		from    []DeviceOption
		to      []DeviceOption
		source  bool
		targets int
		aligned bool
	}{
		{name: "random source", source: true, from: []DeviceOption{DeviceThreads(1)}},
		{name: "mapped endpoint source", source: true, from: []DeviceOption{LinearDevice(true), WithReadMode(ReadMapped)}},
		{name: "random target", targets: 1, to: []DeviceOption{DeviceThreads(1)}},
		{name: "multiple stream targets", targets: 2, to: []DeviceOption{LinearDevice(true)}},
		{name: "misaligned targets", targets: 1, aligned: true, to: []DeviceOption{LinearDevice(true)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Invalid endpoint configuration is refused during indexing, before native stat or Open.
			item := &testContentItem{SimpleJob: &SimpleJob{Path: "not a native source"}}
			if tc.source {
				item.source = testContentSource(func(context.Context) (fs.File, error) {
					t.Error("source opened")
					return nil, nil
				})
			}
			for range tc.targets {
				item.Dsts = append(item.Dsts, "target")
				item.targets = append(item.targets, testContentTarget(func(
					context.Context, ContentMetadata,
				) (ContentWriter, error) {
					t.Error("target opened")
					return nil, nil
				}))
			}
			if tc.aligned {
				item.Dsts = append(item.Dsts, "native target")
			}

			// Item failures retain the stream's accepted-result contract.
			callback, batches := collectResults()
			if err := runStream(
				context.Background(),
				callback,
				[]Item{item},
				SetFromDevice(tc.from...),
				SetToDevice(tc.to...),
			); err != nil {
				t.Fatal(err)
			}
			result := batches()[0][0]
			if result.Err == nil || strings.Contains(result.Err.Error(), "stat") {
				t.Fatalf("validation result=%#v", result)
			}
		})
	}
}

// finishOutput records one native output's completion steps for endpoint overlap checks.
type finishOutput struct {
	*pipelineOutput
	sync      func() error
	publish   func() error
	discard   func()
	discarded atomic.Int32
}

func (o *finishOutput) Sync() error       { return o.sync() }
func (o *finishOutput) Commit(bool) error { return o.publish() }
func (o *finishOutput) Discard() error {
	o.discarded.Add(1)
	if o.discard != nil {
		o.discard()
	}
	return nil
}

// endpointMember builds one prepared content-source member with a native target.
func endpointMember(c *StreamCopyer, order uint64, target string, source ContentSource) *writeJob {
	name := fmt.Sprintf("member-%d", order)
	job := newWriteJob(&baseJob{copyer: c, item: &SimpleJob{Path: name, Dsts: []string{target}},
		path: name, order: order, targets: []string{target},
		outputs: []targetSpec{{name: target, path: target, device: "fixture"}}}, nil)
	job.contentSource, job.endpoints = source, true
	return job
}

func memberSource(size int64, read func(), opened func(), closeErr error) ContentSource {
	return testContentSource(func(context.Context) (fs.File, error) {
		opened()
		return &testContentFile{
			ReadCloser: &testContentReader{
				Reader: &generatedReader{remaining: size, read: func(int) { read() }},
				close:  func() error { return closeErr },
			},
			metadata: ContentMetadata{Info: contentInfo{name: "member", size: size, mode: 0600}},
		}, nil
	})
}

func TestContentNativeTargetCompletionOverlapsNextMember(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Every Sync blocks; later members must still open and read until the in-flight bound.
		c := newTestStream(t, WithHashPolicy(HashOff), SetFromDevice(LinearDevice(true)))
		release := make(chan struct{})
		var lock sync.Mutex
		var events []string
		record := func(event string) { lock.Lock(); defer lock.Unlock(); events = append(events, event) }
		outputs := make(map[uint64]*finishOutput)
		c.filesystem = pipelineFilesystem{create: func(job *writeJob, spec targetSpec) (transferOutput, error) {
			output := &finishOutput{
				pipelineOutput: &pipelineOutput{write: func(data []byte) (int, error) { return len(data), nil }},
				sync:           func() error { <-release; return nil },
				publish:        func() error { record(fmt.Sprintf("commit-%d", job.order)); return nil },
			}
			lock.Lock()
			outputs[job.order] = output
			lock.Unlock()
			return output, nil
		}}
		prepared := make(chan *writeJob, endpointMembers+1)
		for i := range uint64(endpointMembers + 1) {
			order := i
			prepared <- endpointMember(c, order, fmt.Sprintf("target-%d", order), memberSource(2*batchSize, func() {},
				func() { record(fmt.Sprintf("open-%d", order)) }, nil))
		}
		close(prepared)
		out := c.copy(context.Background(), prepared)

		// Members open while earlier ones sync, and the member beyond the bound waits for a completion.
		synctest.Wait()
		lock.Lock()
		got := slices.Clone(events)
		lock.Unlock()
		if !slices.Equal(got, []string{"open-0", "open-1", "open-2", "open-3"}) {
			t.Fatalf("events before any Sync completed=%v", got)
		}

		close(release)
		results := 0
		for job := range out {
			results++
			if err := job.result().Targets[0].Err; err != nil {
				t.Errorf("member %d: %v", job.order, err)
			}
		}
		if results != endpointMembers+1 || !slices.Contains(events, "open-4") || !slices.Contains(events, "commit-0") {
			t.Fatalf("results=%d events=%v", results, events)
		}
		for order, output := range outputs {
			if output.discarded.Load() != 1 {
				t.Errorf("member %d discards=%d", order, output.discarded.Load())
			}
		}
	})
}

func TestContentOverlappedCompletionKeepsPublicationOrderAndFailures(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Two members publish one path; the later one must not commit before the delayed earlier one.
		c := newTestStream(t, WithHashPolicy(HashOff), SetFromDevice(LinearDevice(true)))
		release := make(chan struct{})
		syncErr := errors.New("sync failed")
		closeErr := errors.New("member close failed")
		var lock sync.Mutex
		var commits []uint64
		c.filesystem = pipelineFilesystem{create: func(job *writeJob, spec targetSpec) (transferOutput, error) {
			return &finishOutput{
				pipelineOutput: &pipelineOutput{write: func(data []byte) (int, error) { return len(data), nil }},
				sync: func() error {
					if job.order == 0 {
						<-release
					}
					if job.order == 2 {
						return syncErr
					}
					return nil
				},
				publish: func() error { lock.Lock(); defer lock.Unlock(); commits = append(commits, job.order); return nil },
			}, nil
		}}

		// A native completion failure stays per-item; a source Close failure still stops later members.
		opened := make([]atomic.Bool, 5)
		prepared := make(chan *writeJob, 5)
		for i, target := range []string{"same", "same", "failed-sync", "close-fails", "never-opened"} {
			var err error
			if i == 3 {
				err = closeErr
			}
			prepared <- endpointMember(c, uint64(i), target, memberSource(batchSize, func() {}, func() { opened[i].Store(true) }, err))
		}
		close(prepared)
		out := c.copy(context.Background(), prepared)
		synctest.Wait()
		lock.Lock()
		premature := slices.Contains(commits, 1)
		lock.Unlock()
		if premature || !opened[3].Load() {
			t.Fatalf("same-path member published before its predecessor: commits=%v opened=%v", commits, opened[3].Load())
		}
		close(release)

		errs := make(map[uint64]error)
		for job := range out {
			result := job.result()
			errs[job.order] = errors.Join(result.Err, result.Targets[0].Err)
		}
		if !slices.Equal(commits, []uint64{0, 1}) {
			t.Fatalf("commits=%v", commits)
		}
		if errs[0] != nil || errs[1] != nil || !errors.Is(errs[2], syncErr) || !errors.Is(errs[3], closeErr) ||
			!errors.Is(errs[4], closeErr) || opened[4].Load() {
			t.Fatalf("errors=%v unstarted member opened=%v", errs, opened[4].Load())
		}
	})
}

func TestContentHardStopDuringOverlappedCompletionDiscardsOutputs(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Stop the pipeline while member 0 syncs and member 1 is reading.
		c := newTestStream(t, WithHashPolicy(HashOff), SetFromDevice(LinearDevice(true)))
		syncing, reading := make(chan struct{}), make(chan struct{})
		var lock sync.Mutex
		var commits []uint64
		outputs := make(map[uint64]*finishOutput)
		c.filesystem = pipelineFilesystem{create: func(job *writeJob, spec targetSpec) (transferOutput, error) {
			output := &finishOutput{
				pipelineOutput: &pipelineOutput{write: func(data []byte) (int, error) { return len(data), nil }},
				sync: func() error {
					if job.order == 0 {
						close(syncing)
						<-c.hardStop
						return errors.New("sync interrupted")
					}
					return nil
				},
				publish: func() error { lock.Lock(); defer lock.Unlock(); commits = append(commits, job.order); return nil },
			}
			lock.Lock()
			outputs[job.order] = output
			lock.Unlock()
			return output, nil
		}}
		var once sync.Once
		var opens, closed atomic.Int32
		block := func() {
			once.Do(func() { close(reading) })
			<-c.hardStop
		}
		prepared := make(chan *writeJob, 3)
		for i := range 3 {
			read := func() {}
			if i == 1 {
				read = block
			}
			source := memberSource(4*batchSize, read, func() { opens.Add(1) }, nil)
			prepared <- endpointMember(c, uint64(i), fmt.Sprintf("target-%d", i), testContentSource(
				func(ctx context.Context) (fs.File, error) {
					file, err := source.Open(ctx)
					file.(*testContentFile).ReadCloser.(*testContentReader).close = func() error { closed.Add(1); return nil }
					return file, err
				}))
		}
		close(prepared)
		out := c.copy(context.Background(), prepared)
		<-syncing
		<-reading
		c.stopHard()
		for range out {
		}

		// A hard stop may still race one later Open, as before; nothing publishes and every
		// acquired source and created output is released exactly once.
		if len(commits) != 0 || opens.Load() < 2 || closed.Load() != opens.Load() || len(outputs) < 2 {
			t.Fatalf("commits=%v opens=%d closes=%d outputs=%d", commits, opens.Load(), closed.Load(), len(outputs))
		}
		for order, output := range outputs {
			if output.discarded.Load() != 1 {
				t.Errorf("member %d discards=%d", order, output.discarded.Load())
			}
		}
	})
}

// eventLog records pipeline steps from concurrent completion workers.
type eventLog struct {
	lock   sync.Mutex
	events []string
}

func (l *eventLog) add(format string, args ...any) {
	l.lock.Lock()
	defer l.lock.Unlock()
	l.events = append(l.events, fmt.Sprintf(format, args...))
}

func (l *eventLog) snapshot() []string {
	l.lock.Lock()
	defer l.lock.Unlock()
	return slices.Clone(l.events)
}

// loggedFilesystem creates outputs whose Sync waits for release when blocked reports their
// order, and whose Commit and Discard are logged.
func loggedFilesystem(log *eventLog, blocked func(uint64) bool, release <-chan struct{}) pipelineFilesystem {
	return pipelineFilesystem{create: func(job *writeJob, spec targetSpec) (transferOutput, error) {
		log.add("create-%d", job.order)
		return &finishOutput{
			pipelineOutput: &pipelineOutput{write: func(data []byte) (int, error) { return len(data), nil }},
			sync: func() error {
				if blocked(job.order) {
					<-release
				}
				return nil
			},
			publish: func() error { log.add("commit-%d", job.order); return nil },
			discard: func() { log.add("discard-%d", job.order) },
		}, nil
	}}
}

func TestContentRandomSourceKeepsSamePathMembersInLane(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Without ordered publication, a same-path member must not open while its predecessor completes.
		c := newTestStream(t, WithHashPolicy(HashOff), Overwrite(true))
		if c.ordersPublication() {
			t.Fatal("default devices unexpectedly order publication")
		}
		release := make(chan struct{})
		log := new(eventLog)
		c.filesystem = loggedFilesystem(log, func(uint64) bool { return true }, release)
		prepared := make(chan *writeJob, 2)
		for i := range uint64(2) {
			prepared <- endpointMember(c, i, "same", memberSource(batchSize, func() {}, func() { log.add("open-%d", i) }, nil))
		}
		close(prepared)
		out := c.copy(context.Background(), prepared)
		synctest.Wait()
		if got := log.snapshot(); !slices.Equal(got, []string{"open-0", "create-0"}) {
			t.Errorf("events before member 0 completed=%v", got)
		}
		close(release)
		for job := range out {
			if err := job.result().Targets[0].Err; err != nil {
				t.Errorf("member %d: %v", job.order, err)
			}
		}
		commits := slices.DeleteFunc(log.snapshot(), func(event string) bool { return !strings.HasPrefix(event, "commit-") })
		if !slices.Equal(commits, []string{"commit-0", "commit-1"}) {
			t.Fatalf("commits=%v", commits)
		}
	})
}

func TestContentLaneMemberWaitsForOverlappedCompletion(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Two overlapped members sync while a ContentTarget member, which keeps its lane, follows.
		c := newTestStream(t, WithHashPolicy(HashOff), SetFromDevice(LinearDevice(true)))
		release := make(chan struct{})
		log := new(eventLog)
		c.filesystem = loggedFilesystem(log, func(uint64) bool { return true }, release)
		prepared := make(chan *writeJob, 3)
		for i := range uint64(2) {
			prepared <- endpointMember(c, i, fmt.Sprintf("target-%d", i),
				memberSource(batchSize, func() {}, func() { log.add("open-%d", i) }, nil))
		}
		member := endpointMember(c, 2, "stream", memberSource(batchSize, func() {}, func() { log.add("open-2") }, nil))
		member.outputs[0].content = testContentTarget(func(context.Context, ContentMetadata) (ContentWriter, error) {
			log.add("target-open-2")
			return &testContentWriter{Writer: io.Discard, finish: func(error) error { log.add("finish-2"); return nil }}, nil
		})
		if c.finishesOffLane(member) {
			t.Fatal("ContentTarget member must keep its lane")
		}
		prepared <- member
		close(prepared)
		out := c.copy(context.Background(), prepared)
		synctest.Wait()
		if before := log.snapshot(); !slices.Equal(before, []string{"open-0", "create-0", "open-1", "create-1"}) {
			t.Errorf("events before overlapped completion=%v", before)
		}
		close(release)
		results := 0
		for job := range out {
			results++
			if err := job.result().Targets[0].Err; err != nil {
				t.Errorf("job %d: %v", job.order, err)
			}
		}

		// The lane member opens only after every earlier completion committed and discarded.
		events := log.snapshot()
		opened := slices.Index(events, "open-2")
		for _, event := range []string{"commit-0", "discard-0", "commit-1", "discard-1"} {
			if at := slices.Index(events, event); at < 0 || at > opened {
				t.Fatalf("%s not before open-2: %v", event, events)
			}
		}
		if results != 3 || slices.Index(events, "target-open-2") < opened || !slices.Contains(events, "finish-2") {
			t.Fatalf("results=%d events=%v", results, events)
		}
	})
}

func TestContentNativeItemAfterOverlappedMemberPublishesInOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Only member 0's Sync blocks; the native item's own completion is ready at once.
		c := newTestStream(t, WithHashPolicy(HashOff), SetFromDevice(LinearDevice(true)))
		release := make(chan struct{})
		log := new(eventLog)
		c.filesystem = loggedFilesystem(log, func(order uint64) bool { return order == 0 }, release)
		prepared := make(chan *writeJob, 2)
		prepared <- endpointMember(c, 0, "same", memberSource(batchSize, func() {}, func() { log.add("open-0") }, nil))
		prepared <- preparedFixture(c, 1, batchSize, "same", &generatedReader{remaining: batchSize})
		close(prepared)
		out := c.copy(context.Background(), prepared)

		// The native item runs during member 0's completion; only publication order holds its commit.
		synctest.Wait()
		if before := log.snapshot(); !slices.Contains(before, "create-1") || slices.Contains(before, "commit-1") {
			t.Errorf("events before member 0 completed=%v", before)
		}
		close(release)
		for job := range out {
			if err := job.result().Targets[0].Err; err != nil {
				t.Errorf("job %d: %v", job.order, err)
			}
		}
		commits := slices.DeleteFunc(log.snapshot(), func(event string) bool { return !strings.HasPrefix(event, "commit-") })
		if !slices.Equal(commits, []string{"commit-0", "commit-1"}) {
			t.Fatalf("commits=%v", commits)
		}
	})
}
