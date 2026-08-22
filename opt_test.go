package acp

import (
	"path/filepath"
	"sort"
	"testing"
)

func TestSourceRoot(t *testing.T) {
	root := string(filepath.Separator)
	job := Source(root)(new(wildcardJob))
	if len(job.src) != 1 {
		t.Fatalf("sources = %d", len(job.src))
	}

	src := job.src[0]
	if got := src.src(); got != root {
		t.Fatalf("source root = %q, want %q", got, root)
	}

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
