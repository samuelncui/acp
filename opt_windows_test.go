//go:build windows

package acp

import "testing"

func TestSourceWindowsPath(t *testing.T) {
	job := Source(`C:\src\file.txt`)(new(wildcardJob))
	if len(job.src) != 1 {
		t.Fatalf("sources = %d", len(job.src))
	}

	src := job.src[0]
	if got := src.src(); got != `C:\src\file.txt` {
		t.Fatalf("source = %q", got)
	}
	if got := src.dst(`D:\target`); got != `D:\target\file.txt` {
		t.Fatalf("target = %q", got)
	}
}

func TestSourceWindowsRoot(t *testing.T) {
	job := Source(`C:\`)(new(wildcardJob))
	if len(job.src) != 1 {
		t.Fatalf("sources = %d", len(job.src))
	}

	src := job.src[0]
	if got := src.src(); got != `C:\` {
		t.Fatalf("source root = %q", got)
	}
	if got := src.append("file").src(); got != `C:\file` {
		t.Fatalf("child source = %q", got)
	}
}
