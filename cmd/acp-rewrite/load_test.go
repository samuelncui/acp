package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadRejectsInvalidDocuments(t *testing.T) {
	// Loading must refuse ambiguous or lossy documents before startup can touch saved paths.
	for _, kind := range []string{"state", "report"} {
		for name, document := range map[string]string{
			"null": "null", "trailing object": "{} {}", "trailing garbage": "{} broken",
			"invalid UTF-8": "{\"root\":\"/bad-\xff\"}",
		} {
			t.Run(kind+"/"+name, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), kind+".json")
				if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
					t.Fatal(err)
				}

				// Invalid bytes in unknown fields are also rejected before decoding can replace them.
				var err error
				if kind == "state" {
					_, err = loadState(path)
				} else {
					_, _, err = loadReport(path)
				}
				if err == nil {
					t.Fatalf("accepted %q", document)
				}
			})
		}
	}
}

func TestLoadRejectsInvalidRows(t *testing.T) {
	// Every queued file and accumulated report row needs an unambiguous path.
	for _, test := range []struct{ kind, document string }{
		{"state", `{"root":"/root","pending":[null]}`},
		{"state", `{"root":"/root","busy":[{}]}`},
		{"state", `{"root":"/root","missing":[{}]}`},
		{"state", `{"root":"/root","pending":[{"path":"/root/a","links":[""]}]}`},
		{"state", `{"root":"/root","tmp_files":[""]}`},
		{"report", `{"files":[null]}`},
		{"report", `{"files":[{}]}`},
		{"report", `{"files":[{"full_path":"/root/a"},{"full_path":"/root/a"}]}`},
		{"report", `{"errors":[null]}`},
	} {
		t.Run(fmt.Sprintf("%s/%s", test.kind, test.document), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), test.kind+".json")
			if err := os.WriteFile(path, []byte(test.document), 0o600); err != nil {
				t.Fatal(err)
			}

			// A rejected row is an ordinary load error, never a panic or silent data loss.
			var err error
			if test.kind == "state" {
				_, err = loadState(path)
			} else {
				_, _, err = loadReport(path)
			}
			if err == nil {
				t.Fatalf("accepted %s", test.document)
			}
		})
	}
}

func TestLoadKeepsValidReplacementRuneAndWhitespace(t *testing.T) {
	// A literal replacement rune is a valid filename character, and trailing whitespace is JSON.
	path := filepath.Join(t.TempDir(), "document.json")
	const name = "/root/file-\ufffd"
	document := `{"root":"/root","pending":[{"path":"` + name + `"}],"files":[{"full_path":"` + name + `"}]}`
	if err := os.WriteFile(path, []byte(document+"\n\t "), 0o600); err != nil {
		t.Fatal(err)
	}

	// Both consumers retain the exact identity while ignoring the other's unrelated fields.
	state, err := loadState(path)
	if err != nil || len(state.Pending) != 1 || state.Pending[0].Path != name {
		t.Fatalf("state = %+v / %v", state, err)
	}
	jobs, _, err := loadReport(path)
	if err != nil || len(jobs) != 1 || jobs[name] == nil {
		t.Fatalf("report = %+v / %v", jobs, err)
	}
}
