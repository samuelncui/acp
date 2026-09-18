package acp

import (
	"context"
	"path/filepath"
	"sort"
	"strings"
	"testing"
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

func TestRunRejectsNilBatchSource(t *testing.T) {
	// Run owns the pipeline lifecycle, so it must reject an input it cannot read.
	if err := Run(context.Background(), nil); err == nil {
		t.Fatal("Run() error = nil, want a rejected batch source")
	}
}

func TestRunRejectsInvalidOptions(t *testing.T) {
	// Read mode belongs to the source device, so a target read mode must not start a run.
	err := Run(context.Background(), newSliceSource(), SetToDevice(WithReadMode(ReadMapped)))
	if err == nil || !strings.Contains(err.Error(), "read mode is a source option") {
		t.Fatalf("Run() error = %v, want a rejected target read mode", err)
	}
}
