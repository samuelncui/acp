package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadStateRejectsLossyPaths(t *testing.T) {
	// Every persisted identity must remain distinct from a valid replacement-rune sibling.
	const invalid = "/source/file-\xff"
	states := map[string]*rewriteState{
		"root":          {Root: invalid},
		"pending":       {Pending: []rewriteEntry{{Path: invalid}}},
		"pending links": {Pending: []rewriteEntry{{Path: "/source/ok", Links: []string{invalid}}}},
		"busy":          {Busy: []rewriteEntry{{Path: invalid}}},
		"busy links":    {Busy: []rewriteEntry{{Path: "/source/ok", Links: []string{invalid}}}},
		"missing":       {Missing: []rewriteEntry{{Path: invalid}}},
		"missing links": {Missing: []rewriteEntry{{Path: "/source/ok", Links: []string{invalid}}}},
		"temporary":     {TmpFiles: []string{invalid}},
	}
	for name, state := range states {
		t.Run(name, func(t *testing.T) {
			// Write the raw unsupported bytes that a JSON encoder would otherwise normalize.
			path := filepath.Join(t.TempDir(), "state.json")
			data, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			data = []byte(strings.ReplaceAll(string(data), `\ufffd`, "\xff"))
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}

			// The complete document is rejected at load, before progress saves trust its paths.
			if _, err := loadState(path); err == nil || !strings.Contains(err.Error(), "UTF-8") {
				t.Fatalf("loadState = %v, want an explicit UTF-8 error", err)
			}
		})
	}
}
