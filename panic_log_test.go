package acp

import (
	"context"
	"io"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

// TestRecoveredCallerPanicIsLoggedOnceWithStack checks that a panic in caller-implemented code is
// logged once through the run's logger, at the point of recovery, with the stack of the panic.
func TestRecoveredCallerPanicIsLoggedOnceWithStack(t *testing.T) {
	tests := []struct {
		name    string
		what    string
		item    func(root string) Item
		options []Option
	}{
		{
			name: "ContentSource.Open",
			what: "ContentSource.Open panicked",
			item: func(root string) Item {
				item := &testContentItem{SimpleJob: &SimpleJob{Path: "member", Dsts: []string{filepath.Join(root, "copy")}}}
				item.source = testContentSource(func(context.Context) (fs.File, error) { panic("injected source panic") })
				return item
			},
			options: []Option{SetFromDevice(LinearDevice(true))},
		},
		{
			name: "encoder",
			what: "item target encoder panicked",
			item: func(root string) Item {
				source := writeSourceFile(t, root, "source", []byte("payload"))
				return &encodedItem{
					SimpleJob: &SimpleJob{Path: source, Dsts: []string{filepath.Join(root, "copy.enc")}},
					encode:    func(io.Writer) (io.WriteCloser, error) { panic("injected encoder panic") },
				}
			},
		},
		{
			name: "ContentTarget.Open",
			what: "ContentTarget.Open panicked",
			item: func(root string) Item {
				source := writeSourceFile(t, root, "source", []byte("payload"))
				item := &testContentItem{SimpleJob: &SimpleJob{Path: source, Dsts: []string{"member"}}}
				item.targets = []ContentTarget{testContentTarget(func(context.Context, ContentMetadata) (ContentWriter, error) {
					panic("injected target panic")
				})}
				return item
			},
			options: []Option{SetToDevice(LinearDevice(true))},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger, hook := logtest.NewNullLogger()
			options := append([]Option{WithHashPolicy(HashRead), WithLogger(logger)}, tt.options...)
			onResults, snapshot := collectResults()
			item := tt.item(t.TempDir())
			_ = runStream(context.Background(), onResults, []Item{item}, options...)
			if result := snapshot()[0][0]; result.Err == nil && result.Targets[0].Err == nil {
				t.Fatalf("panic was not reported, result=%#v", result)
			}

			var withStack []*logrus.Entry
			for _, entry := range hook.AllEntries() {
				if strings.Contains(entry.Message, "goroutine ") {
					withStack = append(withStack, entry)
				}
			}
			if len(withStack) != 1 {
				t.Fatalf("entries with a stack = %d, want 1, entries=%v", len(withStack), hook.AllEntries())
			}
			message := withStack[0].Message
			if withStack[0].Level != logrus.ErrorLevel || !strings.Contains(message, tt.what) || !strings.Contains(message, "panic_log_test.go") {
				t.Fatalf("panic entry = %q, want %q with the stack through this test file", message, tt.what)
			}
		})
	}
}
