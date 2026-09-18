//go:build windows

package acp

import (
	"path/filepath"
	"testing"
)

func TestSourceWindowsPath(t *testing.T) {
	base, name := filepath.Split(`C:\src\file.txt`)
	src := &source{base: base, path: name}

	if got := src.src(); got != `C:\src\file.txt` {
		t.Fatalf("source = %q", got)
	}
	if got := src.dst(`D:\target`); got != `D:\target\file.txt` {
		t.Fatalf("target = %q", got)
	}
}

func TestSourceWindowsRoot(t *testing.T) {
	base, name := filepath.Split(filepath.Clean(`C:\`))
	src := &source{base: base, path: name}

	if got := src.src(); got != `C:\` {
		t.Fatalf("source root = %q", got)
	}
	if got := src.append("file").src(); got != `C:\file` {
		t.Fatalf("child source = %q", got)
	}
}
