package acp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
)

type testContentFS func(string) (fs.File, error)

func (f testContentFS) Open(name string) (fs.File, error) { return f(name) }

func TestContentSourceFromFSObservesOpenedFileOnce(t *testing.T) {
	for _, data := range []string{"", "observed bytes"} {
		t.Run(data, func(t *testing.T) {
			// Binding is lazy, and filesystem names remain distinct from display identifiers.
			var lock sync.Mutex
			var events []string
			record := func(event string) []string {
				lock.Lock()
				defer lock.Unlock()
				events = append(events, event)
				return append([]string(nil), events...)
			}
			attrs := map[string][]byte{"user.binary": {0, 255}, "user.empty": {}, "acp.signature": {1}}
			var reader io.Reader = strings.NewReader(data)
			if data == "" {
				reader = contentReadFunc(func([]byte) (int, error) {
					t.Error("empty member attempted a payload read")
					return 0, errors.New("empty member has no payload")
				})
			}
			file := &testContentFile{
				ReadCloser: &testContentReader{Reader: reader, close: func() error {
					record("close")
					return nil
				}},
				stat: func() (fs.FileInfo, error) {
					record("stat")
					return contentForeignInfo{contentInfo{name: "actual", size: int64(len(data)), mode: 0640}}, nil
				},
				xattrs: func() (map[string][]byte, error) {
					record("xattrs")
					return attrs, nil
				},
			}
			source := SourceFromFS(testContentFS(func(name string) (fs.File, error) {
				record("open")
				if name != "docs/file" {
					t.Errorf("filesystem name=%q", name)
				}
				return file, nil
			}), "docs/file")
			if len(events) != 0 {
				t.Fatalf("binding opened filesystem: %v", events)
			}

			// The target receives observed portable facts and its own attribute byte copies.
			var output bytes.Buffer
			item := &testContentItem{SimpleJob: &SimpleJob{Path: "display/a/../file", Dsts: []string{"target"}},
				source: source}
			item.targets = []ContentTarget{testContentTarget(func(
				_ context.Context, metadata ContentMetadata,
			) (ContentWriter, error) {
				observed := record("target")
				if !reflect.DeepEqual(observed[:3], []string{"open", "stat", "xattrs"}) ||
					metadata.Info.Name() != "actual" || metadata.Info.Sys() != nil ||
					metadata.Info.Size() != int64(len(data)) || len(metadata.Xattrs) != 2 {
					t.Errorf("events=%v, metadata=%#v", observed, metadata)
				}
				metadata.Xattrs["user.binary"][0] = 9
				if attrs["user.binary"][0] != 0 {
					t.Error("target mutated source attributes")
				}
				return &testContentWriter{Writer: &output, finish: func(cause error) error {
					record("finish")
					if cause != nil {
						t.Errorf("Finish cause=%v", cause)
					}
					return nil
				}}, nil
			})}
			callback, batches := collectResults()
			if err := runStream(context.Background(), callback, []Item{item},
				SetFromDevice(LinearDevice(true)), SetToDevice(LinearDevice(true)), WithHashPolicy(HashRead)); err != nil {
				t.Fatal(err)
			}

			// Empty members still observe, close and finish; both sizes hash their actual bytes.
			digest := sha256.Sum256([]byte(data))
			result := batches()[0][0]
			if output.String() != data || result.Err != nil || result.Targets[0].Err != nil ||
				!bytes.Equal(result.SHA256, digest[:]) || result.SignatureCacheHit || len(events) != 6 ||
				events[5] != "finish" {
				t.Fatalf("output=%q, events=%v, result=%#v", output.String(), events, result)
			}
		})
	}
}

func TestContentSourceFromFSUsesStandardFiles(t *testing.T) {
	// A plain fs.File needs neither random access nor the optional xattr capability.
	root := t.TempDir()
	for _, name := range []string{"nested/source", "empty"} {
		data := []byte("standard filesystem")
		if name == "empty" {
			data = nil
		}
		fsys := fstest.MapFS{name: &fstest.MapFile{Data: data, Mode: 0600}}
		item := &testContentItem{SimpleJob: &SimpleJob{Path: "display " + name,
			Dsts: []string{filepath.Join(root, filepath.Base(name))}}, source: SourceFromFS(fsys, name)}
		callback, batches := collectResults()
		if err := runStream(context.Background(), callback, []Item{item},
			SetFromDevice(LinearDevice(true)), WithHashPolicy(HashRead)); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(item.Dsts[0])
		if result := batches()[0][0]; err != nil || !bytes.Equal(got, data) ||
			result.Err != nil || result.Targets[0].Err != nil || result.Size != int64(len(data)) {
			t.Fatalf("output=%q, err=%v, result=%#v", got, err, result)
		}
	}
}

func TestContentSourceObservationFailuresCloseAcquiredFiles(t *testing.T) {
	for _, stage := range []string{
		"open", "stat", "xattrs", "nil info", "directory", "negative size", "oversized attrs",
		"stat panic", "xattrs panic", "info panic", "stat cancel", "xattrs cancel", "fs open cancel",
	} {
		t.Run(stage, func(t *testing.T) {
			// Every failed observation retains a handle for one Close and never starts a target.
			captureLogs(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			failure, closeFailure := errors.New(stage), errors.New("source close failed")
			closes, reads, targetOpens := 0, 0, 0
			info := fs.FileInfo(contentInfo{name: "member", size: 1, mode: 0600})
			if stage == "info panic" {
				info = contentPanicInfo{contentInfo: contentInfo{name: "member", mode: 0600}, cause: failure}
			}
			file := &testContentFile{ReadCloser: &testContentReader{
				Reader: contentReadFunc(func([]byte) (int, error) { reads++; return 0, io.EOF }),
				close:  func() error { closes++; return closeFailure },
			}}
			file.stat = func() (fs.FileInfo, error) {
				switch stage {
				case "stat":
					return nil, failure
				case "stat panic":
					panic(failure)
				case "stat cancel":
					cancel()
					return nil, failure
				case "nil info":
					return nil, nil
				case "directory":
					return contentInfo{mode: fs.ModeDir}, nil
				case "negative size":
					return contentInfo{size: -1}, nil
				}
				return info, nil
			}
			file.xattrs = func() (map[string][]byte, error) {
				switch stage {
				case "xattrs":
					return nil, failure
				case "xattrs panic":
					panic(failure)
				case "xattrs cancel":
					cancel()
					return nil, failure
				case "oversized attrs":
					return map[string][]byte{"attr": make([]byte, ContentMaxXattrValueBytes+1)}, nil
				}
				return nil, nil
			}
			fsys := testContentFS(func(string) (fs.File, error) {
				if stage == "open" || stage == "fs open cancel" {
					if stage == "fs open cancel" {
						cancel()
					}
					return file, failure
				}
				return file, nil
			})
			root := t.TempDir()
			item := &testContentItem{SimpleJob: &SimpleJob{Path: "member", Dsts: []string{
				filepath.Join(root, "native"), "custom",
			}}, source: SourceFromFS(fsys, "member"), targets: []ContentTarget{nil,
				testContentTarget(func(context.Context, ContentMetadata) (ContentWriter, error) {
					targetOpens++
					return nil, errors.New("unexpected target Open")
				}),
			}}

			// Failed observation and failed cleanup remain identifiable on the terminal item.
			callback, batches := collectResults()
			stream, err := NewStream(ctx, callback, SetFromDevice(LinearDevice(true)),
				SetToDevice(LinearDevice(true)), WithHashPolicy(HashRead))
			if err != nil {
				t.Fatal(err)
			}
			_ = stream.Submit(item)
			_ = stream.Close()
			err = stream.Wait()
			result := batches()[0][0]
			if err == nil || result.Err == nil || !errors.Is(result.Err, closeFailure) ||
				closes != 1 || reads != 0 || targetOpens != 0 {
				t.Fatalf("run=%v, result=%#v, closes=%d, reads=%d, targets=%d",
					err, result, closes, reads, targetOpens)
			}
			if strings.Contains(stage, "cancel") && !errors.Is(result.Err, context.Canceled) {
				t.Fatalf("cancellation lost: %v", result.Err)
			}
			if stage != "nil info" && stage != "directory" && stage != "negative size" &&
				stage != "oversized attrs" && !errors.Is(result.Err, failure) {
				t.Fatalf("observation cause lost: %v", result.Err)
			}
			if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
				t.Fatalf("output created before observation: %v, %v", entries, err)
			}
			select {
			case <-stream.HardStopped():
				t.Fatal("caller-owned metadata observation became a fatal pipeline failure")
			default:
			}
		})
	}
}

type contentReadFunc func([]byte) (int, error)

func (f contentReadFunc) Read(data []byte) (int, error) { return f(data) }

type contentPanicInfo struct {
	contentInfo
	cause error
}

func (i contentPanicInfo) Size() int64 { panic(i.cause) }

func TestContentSourceFromFSBindingErrorsDoNotOpen(t *testing.T) {
	// Logical paths follow io/fs without cleaning or converting native path syntax.
	for _, name := range []string{"", "/absolute", "a/../b", "a//b", "bad\xff"} {
		source := SourceFromFS(testContentFS(func(string) (fs.File, error) {
			t.Error("invalid binding opened filesystem")
			return nil, nil
		}), name)
		file, err := source.Open(context.Background())
		if file != nil || !errors.Is(err, fs.ErrInvalid) {
			t.Fatalf("name=%q, file=%v, err=%v", name, file, err)
		}
	}
	if _, err := SourceFromFS(nil, "valid").Open(context.Background()); !errors.Is(err, fs.ErrInvalid) {
		t.Fatalf("nil filesystem error=%v", err)
	}

	// Already-cancelled contexts never call the standard filesystem.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	source := SourceFromFS(testContentFS(func(string) (fs.File, error) {
		t.Error("cancelled binding opened filesystem")
		return nil, nil
	}), "valid")
	if _, err := source.Open(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Open=%v", err)
	}
}
