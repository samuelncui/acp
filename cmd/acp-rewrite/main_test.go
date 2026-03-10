package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestScanEntriesHardlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("hardlink scan unsupported on windows")
	}

	root := t.TempDir()
	statePath := filepath.Join(root, ".acp-rewrite-state.json")
	reportPath := filepath.Join(root, "report.json")

	if err := os.WriteFile(statePath, []byte("{}"), 0o644); err != nil {
		t.Fatalf("write state: %v", err)
	}
	if err := os.WriteFile(reportPath, []byte("{}"), 0o644); err != nil {
		t.Fatalf("write report: %v", err)
	}

	a := filepath.Join(root, "a.txt")
	b := filepath.Join(root, "b.txt")
	c := filepath.Join(root, "c.txt")

	if err := os.WriteFile(a, []byte("a"), 0o644); err != nil {
		t.Fatalf("write a: %v", err)
	}
	if err := os.Link(a, b); err != nil {
		t.Fatalf("link b: %v", err)
	}
	if err := os.WriteFile(c, []byte("c"), 0o644); err != nil {
		t.Fatalf("write c: %v", err)
	}

	entries, err := scanEntries(root, statePath, reportPath)
	if err != nil {
		t.Fatalf("scan entries: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries count = %d", len(entries))
	}

	var withLinks *rewriteEntry
	var single *rewriteEntry
	for i := range entries {
		if len(entries[i].Links) > 0 {
			withLinks = &entries[i]
		} else {
			single = &entries[i]
		}
	}
	if withLinks == nil || single == nil {
		t.Fatalf("unexpected entries: %+v", entries)
	}
	if withLinks.Path != a {
		t.Fatalf("hardlink entry path = %q", withLinks.Path)
	}
	if len(withLinks.Links) != 1 || withLinks.Links[0] != b {
		t.Fatalf("hardlink entry links = %+v", withLinks.Links)
	}
	if single.Path != c {
		t.Fatalf("single entry path = %q", single.Path)
	}
}

func TestRelinkOnePreserveLinkAttrs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("hardlink attrs unsupported on windows")
	}

	root := t.TempDir()
	src := filepath.Join(root, "src.txt")
	link := filepath.Join(root, "link.txt")
	tmp := filepath.Join(root, "tmp.txt")

	if err := os.WriteFile(src, []byte("old"), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	if err := os.Link(src, link); err != nil {
		t.Fatalf("link: %v", err)
	}

	oldTime := time.Unix(1700000000, 0)
	newTime := time.Unix(1700001000, 0)

	if err := os.Chmod(link, 0o600); err != nil {
		t.Fatalf("chmod link: %v", err)
	}
	if err := os.Chtimes(link, oldTime, oldTime); err != nil {
		t.Fatalf("chtimes link: %v", err)
	}

	if err := os.WriteFile(tmp, []byte("new"), 0o644); err != nil {
		t.Fatalf("write tmp: %v", err)
	}
	if err := os.Chtimes(tmp, newTime, newTime); err != nil {
		t.Fatalf("chtimes tmp: %v", err)
	}
	if err := os.Rename(tmp, src); err != nil {
		t.Fatalf("rename tmp to src: %v", err)
	}

	if err := relinkOne(src, link); err != nil {
		t.Fatalf("relinkOne: %v", err)
	}

	srcInfo, err := os.Stat(src)
	if err != nil {
		t.Fatalf("stat src: %v", err)
	}
	linkInfo, err := os.Stat(link)
	if err != nil {
		t.Fatalf("stat link: %v", err)
	}
	if !os.SameFile(srcInfo, linkInfo) {
		t.Fatalf("src and link not same file")
	}
	if srcInfo.Mode().Perm() != 0o600 || linkInfo.Mode().Perm() != 0o600 {
		t.Fatalf("mode mismatch src=%v link=%v", srcInfo.Mode().Perm(), linkInfo.Mode().Perm())
	}
	if srcInfo.ModTime().Unix() != oldTime.Unix() || linkInfo.ModTime().Unix() != oldTime.Unix() {
		t.Fatalf("modtime mismatch src=%v link=%v", srcInfo.ModTime(), linkInfo.ModTime())
	}
	data, err := os.ReadFile(link)
	if err != nil {
		t.Fatalf("read link: %v", err)
	}
	if string(data) != "new" {
		t.Fatalf("link content = %q", string(data))
	}
}
