package acp

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestSourceRoot(t *testing.T) {
	// The file-system root splits into an empty relative path, which must still resolve as
	// the root itself and join children onto it.
	root := string(filepath.Separator)
	base, name := filepath.Split(filepath.Clean(root))
	if got := (&source{base: base, path: name}).src(); got != root {
		t.Fatalf("source root = %q, want %q", got, root)
	}

	src := &source{base: base, path: name}
	target := filepath.Join(root, "target")
	child := src.append("file")
	if got := child.src(); got != filepath.Join(root, "file") {
		t.Fatalf("child source = %q", got)
	}
	if got := child.dst(target); got != filepath.Join(target, "file") {
		t.Fatalf("child target = %q", got)
	}
}

func TestComparePath(t *testing.T) {
	paths := []string{
		"b",
		"a-b",
		filepath.Join("a", "b", "c"),
		"a",
		filepath.Join("a", "b"),
	}
	sort.Slice(paths, func(i, j int) bool { return comparePath(paths[i], paths[j]) < 0 })

	want := []string{
		"a",
		filepath.Join("a", "b"),
		filepath.Join("a", "b", "c"),
		"a-b",
		"b",
	}
	for idx := range want {
		if paths[idx] != want[idx] {
			t.Fatalf("paths[%d] = %q, want %q", idx, paths[idx], want[idx])
		}
	}
}

func TestLinearDeviceOnlySerializesItsOwnStage(t *testing.T) {
	// Cover each directional linear constraint independently.
	tests := []struct {
		name            string
		options         []Option
		wantFromThreads int
		wantToThreads   int
	}{
		{
			name: "linear target",
			options: []Option{
				SetToDevice(LinearDevice(true)),
			},
			wantFromThreads: 8,
			wantToThreads:   1,
		},
		{
			name: "linear source",
			options: []Option{
				SetFromDevice(LinearDevice(true)),
			},
			wantFromThreads: 1,
			wantToThreads:   8,
		},
	}

	// Keep the unconstrained side parallel while serializing only the linear stage.
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			option := newOption()
			for _, apply := range test.options {
				option = apply(option)
			}

			if err := option.check(); err != nil {
				t.Fatal(err)
			}
			if option.fromDevice.threads != test.wantFromThreads {
				t.Fatalf("source threads = %d, want %d", option.fromDevice.threads, test.wantFromThreads)
			}
			if option.toDevice.threads != test.wantToThreads {
				t.Fatalf("target threads = %d, want %d", option.toDevice.threads, test.wantToThreads)
			}
		})
	}
}

func TestNewRejectsNegativeDeviceThreads(t *testing.T) {
	tests := []struct {
		name   string
		option Option
		want   string
	}{
		{
			name:   "source",
			option: SetFromDevice(DeviceThreads(-1)),
			want:   "check source device failed",
		},
		{
			name:   "target",
			option: SetToDevice(DeviceThreads(-1)),
			want:   "check target device failed",
		},
		{
			name:   "linear source",
			option: SetFromDevice(DeviceThreads(-1), LinearDevice(true)),
			want:   "check source device failed",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			copyer, err := New(context.Background(), test.option)
			if err == nil {
				copyer.Wait()
				t.Fatal("New() error = nil")
			}
			if !strings.Contains(err.Error(), test.want) || !strings.Contains(err.Error(), "threads=-1") {
				t.Fatalf("New() error = %q, want %q and thread count", err, test.want)
			}
		})
	}
}

func TestNewRejectsInvalidOptions(t *testing.T) {
	tests := []struct {
		name   string
		option Option
		want   string
	}{
		{
			name:   "empty read buffer",
			option: WithReadBuffer(0),
			want:   "read buffer must be at least one item",
		},
		{
			name:   "target read mode",
			option: SetToDevice(WithReadMode(ReadMapped)),
			want:   "read mode is a source option",
		},
		{
			name:   "unknown hash policy",
			option: WithHashPolicy(HashPolicy(0xff)),
			want:   "unknown hash policy",
		},
		{
			name:   "empty result buffer",
			option: WithResultBuffer(0),
			want:   "result buffer must be at least one item",
		},
		{
			name:   "empty result batch",
			option: WithResultBatch(0),
			want:   "result batch must be at least one item",
		},
		{
			name:   "short result flush interval",
			option: WithResultFlushInterval(time.Millisecond),
			want:   "result flush interval must be at least",
		},
		{
			name:   "unknown source read mode",
			option: SetFromDevice(WithReadMode(ReadMode(9))),
			want:   "unknown read mode",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			copyer, err := New(context.Background(), test.option)
			if err == nil {
				copyer.Wait()
				t.Fatal("New() error = nil")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("New() error = %q, want %q", err, test.want)
			}
		})
	}
}

func TestNewStreamRejectsNilResultsCallback(t *testing.T) {
	// NewStream owns the pipeline lifecycle, so it must reject an input it cannot report to.
	if _, err := NewStream(context.Background(), nil); err == nil {
		t.Fatal("NewStream() error = nil, want a rejected results callback")
	}
}

func TestNewStreamRejectsInvalidOptions(t *testing.T) {
	// Read mode belongs to the source device, so a target read mode must not start a run.
	_, err := NewStream(
		context.Background(),
		func([]Result) error { return nil },
		SetToDevice(WithReadMode(ReadMapped)),
	)
	if err == nil || !strings.Contains(err.Error(), "read mode is a source option") {
		t.Fatalf("NewStream() error = %v, want a rejected target read mode", err)
	}
}

// TestWithEventHandlerIsLastWins pins the option rule for event handlers: the last registration
// replaces the earlier ones, and a nil handler clears the registration instead of leaving a
// handler that fails the run when it is called.
func TestWithEventHandlerIsLastWins(t *testing.T) {
	root := t.TempDir()
	input := writeSourceFile(t, root, "source.txt", []byte("fixture"))

	var first, second, third int
	firstHandler := func(Event) { first++ }
	secondHandler := func(Event) { second++ }
	thirdHandler := func(Event) { third++ }

	// The handler registered last is the only one that receives events.
	item := newFixtureItem(input)
	if err := runFixture(
		context.Background(),
		newStreamFixture(item),
		[]Item{item},
		WithEventHandler(firstHandler),
		WithEventHandler(secondHandler),
		WithEventHandler(thirdHandler),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := item.terminal(t); err != nil {
		t.Fatalf("item failed: %v", err)
	}
	if first != 0 || second != 0 {
		t.Fatalf("replaced handlers received events: first=%d second=%d", first, second)
	}
	if third == 0 {
		t.Fatal("the handler registered last received no event")
	}

	// WithProgressBar installs its handler through the same option, so a later handler replaces
	// the bar instead of running beside it.
	silenceStderr(t)
	option := newOption()
	option = WithProgressBar()(option)
	option = WithEventHandler(secondHandler)(option)
	if len(option.eventHandlers) != 1 {
		t.Fatalf("handlers after WithProgressBar and WithEventHandler = %d, want the bar replaced", len(option.eventHandlers))
	}

	// A nil handler clears the registration, so nothing is called and the run still succeeds.
	item = newFixtureItem(input)
	if err := runFixture(
		context.Background(),
		newStreamFixture(item),
		[]Item{item},
		WithEventHandler(firstHandler),
		WithEventHandler(nil),
	); err != nil {
		t.Fatalf("run error = %v, want nil after clearing the handler", err)
	}
	if first != 0 {
		t.Fatalf("a cleared handler received %d events", first)
	}
}

// silenceStderr points the process stderr at the null device while a progress bar is created,
// because the bar renders its blank state as soon as it exists.
func silenceStderr(t *testing.T) {
	t.Helper()

	previous := os.Stderr
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	os.Stderr = devNull
	t.Cleanup(func() {
		os.Stderr = previous
		_ = devNull.Close()
	})
}

// TestNewDoesNotWriteIntoTheCallersOptionSlice pins that New applies the caller's options
// without appending into the spare capacity past the caller's slice length.
func TestNewDoesNotWriteIntoTheCallersOptionSlice(t *testing.T) {
	root := t.TempDir()
	input := writeSourceFile(t, root, "source.txt", []byte("fixture"))

	// The caller hands over the first element of a slice whose spare capacity holds a sentinel,
	// and the shell enumerates its job from that element.
	options := make([]Option, 1, 2)
	options[0] = WildcardJob(Source(input))
	full := append(options, WithHashPolicy(HashRead))

	copyer, err := New(context.Background(), full[:1]...)
	if err != nil {
		t.Fatal(err)
	}
	copyer.Wait()

	// The sentinel must still be the option the caller stored, not one New appended.
	option := newOption()
	option = full[1](option)
	if option.hashPolicy != HashRead {
		t.Fatalf("New() overwrote the caller's spare capacity: hash policy = %s", option.hashPolicy)
	}
}

// TestNewStreamRejectsJobOptions pins the option boundary between the push engine and the
// compatibility shell: a job option describes what the shell enumerates, so the engine refuses it
// instead of silently running an empty stream.
func TestNewStreamRejectsJobOptions(t *testing.T) {
	input := writeSourceFile(t, t.TempDir(), "source.txt", []byte("fixture"))

	if _, err := NewStream(context.Background(), func([]Result) error { return nil }, AccurateJob(input, nil)); err == nil {
		t.Fatal("NewStream() error = nil, want a rejected job option")
	} else if !strings.Contains(err.Error(), "job options describe the compatibility shell") {
		t.Fatalf("NewStream() error = %v, want the shell boundary", err)
	}
	if _, err := NewStream(context.Background(), func([]Result) error { return nil }, WildcardJob(Source(filepath.Dir(input)))); err == nil {
		t.Fatal("NewStream() error = nil, want a rejected wildcard job option")
	}
}

// TestValueOptionsAreLastWins pins the option rule for the run-level values: a repeated option
// replaces the earlier value instead of combining with it.
func TestValueOptionsAreLastWins(t *testing.T) {
	option, err := buildOption(
		WithHashPolicy(HashRead), WithHashPolicy(HashOff),
		WithReadBuffer(1), WithReadBuffer(4),
		WithResultBuffer(1), WithResultBuffer(8),
		WithResultBatch(2), WithResultBatch(16),
		WithResultFlushInterval(time.Second), WithResultFlushInterval(time.Minute),
		WithHash(true), WithHashPolicy(HashCachedOnly),
	)
	if err != nil {
		t.Fatal(err)
	}

	if option.hashPolicy != HashCachedOnly {
		t.Fatalf("hash policy = %s, want %s", option.hashPolicy, HashCachedOnly)
	}
	if option.readBuffer != 4 {
		t.Fatalf("read buffer = %d, want 4", option.readBuffer)
	}
	if option.resultBuffer != 8 {
		t.Fatalf("result buffer = %d, want 8", option.resultBuffer)
	}
	if option.resultBatch != 16 {
		t.Fatalf("result batch = %d, want 16", option.resultBatch)
	}
	if option.resultFlushInterval != time.Minute {
		t.Fatalf("result flush interval = %s, want %s", option.resultFlushInterval, time.Minute)
	}
}
