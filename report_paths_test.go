package acp

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestReportJSONRejectsLossyPaths(t *testing.T) {
	// Source, successful targets and failed targets all carry filesystem identities.
	const invalid = "/source/file-\xff"
	values := map[string]any{
		"base":         &Job{Base: invalid},
		"path":         &Job{Path: []string{invalid}},
		"full":         &Job{FullPath: invalid},
		"success":      &Job{SuccessTargets: []string{invalid}},
		"failure":      &Job{FailTargets: map[string]error{invalid: errors.New("failed")}},
		"src":          &Error{Src: invalid},
		"dst":          &Error{Dst: invalid},
		"report value": Report{Jobs: []*Job{{FullPath: invalid}}},
		"report error": Report{Errors: []*Error{{Src: invalid}}},
	}
	for name, value := range values {
		t.Run(name, func(t *testing.T) {
			if encoded, err := json.Marshal(value); err == nil || !strings.Contains(err.Error(), "UTF-8") {
				t.Fatalf("JSON = %q / %v, want an explicit UTF-8 error", encoded, err)
			}
		})
	}
}

func TestReportJSONPreservesUTF8Paths(t *testing.T) {
	// A literal replacement rune is valid text, as are backslashes and controls.
	const name = "/source/日本語-\ufffd\\\t\n "
	want := &Job{Base: name, Path: []string{name}, FullPath: name, SuccessTargets: []string{name}}
	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}

	// Decode with the supported standard-library surface without normalizing names.
	var got Job
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(&got, want) {
		t.Fatalf("decoded = %#v, want %#v", &got, want)
	}
}
