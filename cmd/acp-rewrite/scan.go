package main

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/samuelncui/acp/internal/fileio"
)

func scanEntries(ctx context.Context, root, statePath, reportPath string, ignorePaths []string) ([]rewriteEntry, error) {
	groups := make(map[fileIdentity]int)
	entries := make([]rewriteEntry, 0, 128)

	// Walk regular files while respecting cancellation, explicit ignores and command documents.
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		// Ignore excluded entries before collecting metadata or persisted identities.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return err
		}
		if shouldIgnorePath(p, ignorePaths) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if p == statePath || (reportPath != "" && p == reportPath) {
			return nil
		}
		if d.IsDir() {
			return nil
		}

		// One observation supplies regular-file selection, hardlink grouping, and the rewrite.
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if err := fileio.CheckJSONPaths(p); err != nil {
			return err
		}

		// Retain one metadata observation and every path for each scanned hardlink identity.
		id, linked := checkFileLinked(info)
		if linked {
			if index, exists := groups[id]; exists {
				entry := &entries[index]
				if p < entry.Path {
					entry.Path, p = p, entry.Path
					entry.Info = info
				}
				entry.Links = append(entry.Links, p)
				return nil
			}
			groups[id] = len(entries)
		}
		entries = append(entries, rewriteEntry{Path: p, Info: info})
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Stable entry and link order makes saved work and retries deterministic.
	for index := range entries {
		sort.Strings(entries[index].Links)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, nil
}

func normalizeIgnorePaths(root string, ignores []string) ([]string, error) {
	if len(ignores) == 0 {
		return nil, nil
	}
	normalized := make([]string, 0, len(ignores))
	for _, p := range ignores {
		if p == "" {
			continue
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(root, p)
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, err
		}
		normalized = append(normalized, filepath.Clean(abs))
	}
	return normalized, nil
}

func shouldIgnorePath(p string, ignores []string) bool {
	if len(ignores) == 0 {
		return false
	}
	for _, ig := range ignores {
		if ig == "" {
			continue
		}
		if p == ig {
			return true
		}
		if strings.HasPrefix(p, ig+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}
