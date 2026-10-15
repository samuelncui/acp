package acp

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
)

func TestHardStoppedNotifiesBeforeFeedCloseAndSourceSettlement(t *testing.T) {
	// A fatal reader panic notifies a source waiter while physical Close still owns its handle.
	synctest.Test(t, func(t *testing.T) {
		captureLogs(t)
		failure := errors.New("fatal reader failure")
		closing, settled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		unblock := func() { releaseOnce.Do(func() { close(release) }) }
		stream, err := NewStream(context.Background(), func([]Result) error { return nil },
			SetFromDevice(LinearDevice(true)), WithHashPolicy(HashRead))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { unblock(); _ = stream.Close() })
		item := &testContentItem{SimpleJob: &SimpleJob{Path: "stream member"}}
		item.source = testContentSource(func(context.Context) (fs.File, error) {
			return &testContentFile{ReadCloser: &testContentReader{Reader: hardStoppedPanicReader{failure}, close: func() error {
				close(closing)
				<-release
				close(settled)
				return nil
			}}, metadata: ContentMetadata{Info: contentInfo{name: "member", size: 1, mode: 0600}}}, nil
		})

		// Notification must be observable before the caller closes the feed or releases the source.
		if err := stream.Submit(item); err != nil {
			t.Fatal(err)
		}
		<-stream.HardStopped()
		<-closing
		select {
		case <-settled:
			t.Fatal("fatal notification was delayed until source settlement")
		default:
		}
		closed := make(chan error, 1)
		go func() { closed <- stream.Close() }()
		synctest.Wait()
		select {
		case <-closed:
			t.Fatal("Close returned before source settlement")
		default:
		}

		// Close still settles the owned source, and Wait still returns the original fatal cause.
		unblock()
		if err := <-closed; err != nil {
			t.Fatal(err)
		}
		if err := stream.Wait(); !errors.Is(err, failure) {
			t.Fatalf("Wait = %v, want fatal cause %v", err, failure)
		}
		select {
		case <-settled:
		default:
			t.Fatal("Wait returned before source settlement")
		}
	})
}

type hardStoppedPanicReader struct{ cause error }

func (r hardStoppedPanicReader) Read([]byte) (int, error) { panic(r.cause) }

func TestHardStoppedStaysOpenForNormalAndNonfatalCompletion(t *testing.T) {
	for _, mode := range []string{
		"normal", "source failure", "target failure", "callback failure", "cancellation", "source close failure",
	} {
		t.Run(mode, func(t *testing.T) {
			// Exercise existing normal, per-item/per-target and graceful-stop outcomes through the stream.
			captureLogs(t)
			root := t.TempDir()
			source := writeSourceFile(t, root, "source", []byte("content"))
			var item Item = &SimpleJob{Path: source}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			failure := errors.New(mode)
			var want error
			opts := []Option{WithResultBatch(1)}
			switch mode {
			case "source failure":
				item = &SimpleJob{Path: filepath.Join(root, "missing")}
			case "target failure":
				item = &SimpleJob{Path: source, Dsts: []string{source}}
			case "callback failure":
				want = failure
			case "cancellation":
				cancel()
				want = context.Canceled
			case "source close failure":
				item = &testContentItem{
					SimpleJob: &SimpleJob{Path: "stream member"},
					source: testContentSource(func(context.Context) (fs.File, error) {
						return &testContentFile{
							ReadCloser: &testContentReader{
								Reader: strings.NewReader("a"), close: func() error { return failure },
							},
							metadata: ContentMetadata{Info: contentInfo{name: "member", size: 1, mode: 0600}},
						}, nil
					}),
				}
				opts = append(opts, SetFromDevice(LinearDevice(true)), WithHashPolicy(HashRead))
				want = failure
			}
			var results []Result
			stream, err := NewStream(ctx, func(batch []Result) error {
				results = append(results, batch...)
				if mode == "callback failure" {
					return failure
				}
				return nil
			}, opts...)
			if err != nil {
				t.Fatal(err)
			}
			_ = stream.Submit(item)

			// The terminal error remains available from Wait without turning graceful stops into fatal ones.
			if err := stream.Wait(); !errors.Is(err, want) {
				t.Fatalf("Wait = %v, want %v", err, want)
			}
			select {
			case <-stream.HardStopped():
				t.Fatal("normal or nonfatal completion signaled a fatal stop")
			default:
			}
			if mode != "cancellation" && len(results) != 1 {
				t.Fatalf("results = %d, want one accepted-item outcome", len(results))
			}
			if mode == "source failure" && results[0].Err == nil {
				t.Fatal("source failure did not reach the item result")
			}
			if mode == "target failure" && results[0].Targets[0].Err == nil {
				t.Fatal("target failure did not reach its target result")
			}
		})
	}
}
