package acp

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/sirupsen/logrus"
)

// FileEntry describes one source file and the targets its source-relative path maps to.
type FileEntry struct {
	// Base is the directory the relative path is resolved against.
	Base string
	// Path is the source path relative to Base.
	Path string
	// Source is the resolved source file path.
	Source string
	// Targets holds one resolved target path per requested target directory.
	Targets []string
}

// SelectFiles walks every source, keeps regular files, maps each source-relative path
// onto every target directory, and returns the entries in physical order. A source may
// be a directory or a regular file, and every target must exist and be a directory.
//
// The result is what the acp command line copies: repeated relative paths are removed so
// one source tree cannot overwrite another's file, and the order follows the platform's
// path ordering so a linear target receives files in the order the medium stores them.
func SelectFiles(sources, targets []string) ([]FileEntry, error) {
	dsts := make([]string, 0, len(targets))
	for _, target := range targets {
		target = filepath.Clean(target)
		if target == "" {
			continue
		}

		info, err := os.Stat(target)
		if err != nil {
			return nil, fmt.Errorf("check dst path '%s', %w", target, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("dst path is not a dir, path= '%s'", target)
		}

		dsts = append(dsts, target)
	}
	if len(sources) == 0 {
		return nil, fmt.Errorf("source path not found")
	}

	roots := make([]*source, 0, len(sources))
	for _, path := range sources {
		path = filepath.Clean(path)
		base, name := filepath.Split(path)
		roots = append(roots, &source{base: base, path: name})
	}
	sort.Slice(roots, func(i, j int) bool {
		return comparePath(roots[i].path, roots[j].path) < 0
	})
	for _, root := range roots {
		if _, err := os.Stat(root.src()); err != nil {
			return nil, fmt.Errorf("check src path '%s', %w", root.src(), err)
		}
	}

	entries := make([]FileEntry, 0, 64)
	var walk func(src *source)
	walk = func(src *source) {
		path := src.src()

		info, err := os.Stat(path)
		if err != nil {
			logrus.WithError(err).Errorf("walk get stat, path= '%s'", path)
			return
		}

		mode := info.Mode()
		if mode.IsRegular() {
			targets := make([]string, 0, len(dsts))
			for _, dst := range dsts {
				targets = append(targets, src.dst(dst))
			}
			entries = append(entries, FileEntry{Base: src.base, Path: src.path, Source: path, Targets: targets})
			return
		}
		if mode&UnexpectFileMode != 0 {
			return
		}

		files, err := os.ReadDir(path)
		if err != nil {
			logrus.WithError(err).Errorf("walk read dir, path= '%s'", path)
			return
		}
		for _, file := range files {
			walk(src.append(file.Name()))
		}
	}
	for _, root := range roots {
		walk(root)
	}

	sort.Slice(entries, func(i, j int) bool {
		return comparePath(entries[i].Path, entries[j].Path) < 0
	})

	// Repeated relative paths would write the same target twice, so keep the first.
	filtered := entries[:0]
	previous := ""
	for index, entry := range entries {
		if index > 0 && entry.Path == previous {
			logrus.Errorf("same relative path, ignored, path= '%s'", entry.Source)
			continue
		}

		filtered = append(filtered, entry)
		previous = entry.Path
	}

	return filtered, nil
}
