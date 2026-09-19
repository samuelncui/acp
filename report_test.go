package acp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// TestErrorJSONMarshal pins the nil handling of the report error object: a missing error must
// encode as an absent message and decode back to nil instead of an error with an empty message.
func TestErrorJSONMarshal(t *testing.T) {
	t.Run("message round trip", func(t *testing.T) {
		want := &Error{Src: "src", Dst: "dst", Err: errors.New("copy 100% failed")}

		buf, err := json.Marshal(want)
		if err != nil {
			t.Fatalf("marshal error: %v", err)
		}
		if got := string(buf); got != `{"src":"src","dst":"dst","error":"copy 100% failed"}` {
			t.Fatalf("encoded error = %s", got)
		}

		var decoded Error
		if err := json.Unmarshal(buf, &decoded); err != nil {
			t.Fatalf("unmarshal error: %v", err)
		}
		if decoded.Src != want.Src || decoded.Dst != want.Dst {
			t.Fatalf("decoded error = %#v, want %#v", decoded, want)
		}
		if decoded.Err == nil || decoded.Err.Error() != want.Err.Error() {
			t.Fatalf("decoded error message = %v, want %q", decoded.Err, want.Err)
		}
	})

	t.Run("nil error stays nil", func(t *testing.T) {
		buf, err := json.Marshal(&Error{Src: "src"})
		if err != nil {
			t.Fatalf("marshal error: %v", err)
		}
		if got := string(buf); got != `{"src":"src"}` {
			t.Fatalf("encoded error = %s, want the message omitted", got)
		}

		var decoded Error
		if err := json.Unmarshal(buf, &decoded); err != nil {
			t.Fatalf("unmarshal error: %v", err)
		}
		if decoded.Err != nil {
			t.Fatalf("decoded error = %v, want nil", decoded.Err)
		}
	})

	t.Run("null message decodes to nil", func(t *testing.T) {
		var decoded Error
		if err := json.Unmarshal([]byte(`{"src":"src","error":null}`), &decoded); err != nil {
			t.Fatalf("unmarshal error: %v", err)
		}
		if decoded.Err != nil {
			t.Fatalf("decoded error = %v, want nil", decoded.Err)
		}
	})

	t.Run("nil receiver encodes as null", func(t *testing.T) {
		var missing *Error
		buf, err := missing.MarshalJSON()
		if err != nil {
			t.Fatalf("marshal nil error: %v", err)
		}
		if string(buf) != "null" {
			t.Fatalf("encoded nil error = %s, want null", buf)
		}
	})

	t.Run("message without an error", func(t *testing.T) {
		got := (&Error{Src: "src", Dst: "dst"}).Error()
		if !strings.Contains(got, "src") || !strings.Contains(got, "dst") {
			t.Fatalf("Error() = %q, want both paths", got)
		}
	})
}

// TestJobFailTargetsJSON pins the report row contract for target failures: an error travels as
// its message, a nil error stays null, and a decoded null never becomes a failure.
func TestJobFailTargetsJSON(t *testing.T) {
	job := &Job{
		FullPath:    "/source/a.txt",
		Base:        "/source",
		Path:        []string{"a.txt"},
		Status:      JobStatusFinished,
		FailTargets: map[string]error{"/target/b.txt": nil, "/target/c.txt": errors.New("no space")},
	}

	buf, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("marshal job: %v", err)
	}
	if got := string(buf); !strings.Contains(got, `"fail_target":{"/target/b.txt":null,"/target/c.txt":"no space"}`) {
		t.Fatalf("encoded job = %s", got)
	}

	var decoded Job
	if err := json.Unmarshal(buf, &decoded); err != nil {
		t.Fatalf("unmarshal job: %v", err)
	}
	if _, ok := decoded.FailTargets["/target/b.txt"]; ok {
		t.Fatalf("a null target failure came back as an error: %v", decoded.FailTargets["/target/b.txt"])
	}
	if got := decoded.FailTargets["/target/c.txt"]; got == nil || got.Error() != "no space" {
		t.Fatalf("decoded target failure = %v, want %q", got, "no space")
	}
	if decoded.FullPath != job.FullPath || decoded.Status != job.Status {
		t.Fatalf("decoded job = %#v, want %#v", decoded, job)
	}
}

// TestReportStandardLibraryRoundTrip pins the public report contract: the JSON ACP writes is
// decoded by encoding/json alone, with per-target failures preserved.
func TestReportStandardLibraryRoundTrip(t *testing.T) {
	const message = "copy 100% failed"
	want := &Report{
		Jobs: []*Job{{
			FullPath: "/source/a.txt",
			Base:     "/source",
			Path:     []string{"a.txt"},

			Status:         JobStatusFinished,
			SuccessTargets: []string{"/target/a.txt"},
			FailTargets:    map[string]error{"/target2/a.txt": errors.New(message), "": errors.New("source missing")},

			Size:      3,
			Mode:      0o644,
			ModTime:   time.Unix(1700000000, 0),
			WriteTime: time.Unix(1700000001, 0),
			SHA256:    "abc",

			SignatureCacheHit: true,
		}},
		Errors: []*Error{{Src: "src", Dst: "dst", Err: errors.New(message)}},
	}

	// Both encodings of ACP's own writer are readable by the standard library.
	for _, indent := range []bool{false, true} {
		encoded := want.ToJSONString(indent)
		if !json.Valid([]byte(encoded)) {
			t.Fatalf("report is not valid JSON (indent=%t): %s", indent, encoded)
		}

		var got Report
		if err := json.Unmarshal([]byte(encoded), &got); err != nil {
			t.Fatalf("decode report (indent=%t): %v\n%s", indent, err, encoded)
		}
		assertReportRoundTrip(t, &got)
	}

	// A report encoded by the standard library reads back through ACP's own writer.
	buf, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	var got Report
	if err := json.Unmarshal(buf, &got); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	assertReportRoundTrip(t, &got)
}

func assertReportRoundTrip(t *testing.T, got *Report) {
	t.Helper()

	if len(got.Jobs) != 1 {
		t.Fatalf("report jobs = %d, want 1", len(got.Jobs))
	}
	row := got.Jobs[0]
	if row.FullPath != "/source/a.txt" || row.Base != "/source" || len(row.Path) != 1 || row.Path[0] != "a.txt" {
		t.Fatalf("row identity = %#v", row)
	}
	if row.Status != JobStatusFinished || row.Size != 3 || row.Mode != 0o644 || row.SHA256 != "abc" || !row.SignatureCacheHit {
		t.Fatalf("row facts = %#v", row)
	}
	if !row.ModTime.Equal(time.Unix(1700000000, 0)) || !row.WriteTime.Equal(time.Unix(1700000001, 0)) {
		t.Fatalf("row times = %#v", row)
	}
	if len(row.SuccessTargets) != 1 || row.SuccessTargets[0] != "/target/a.txt" {
		t.Fatalf("row success targets = %v", row.SuccessTargets)
	}
	if len(row.FailTargets) != 2 {
		t.Fatalf("row fail targets = %v, want two failures", row.FailTargets)
	}
	for target, message := range map[string]string{"/target2/a.txt": "copy 100% failed", "": "source missing"} {
		if err := row.FailTargets[target]; err == nil || err.Error() != message {
			t.Fatalf("failure %q = %v, want %q", target, row.FailTargets[target], message)
		}
	}
	if len(got.Errors) != 1 || got.Errors[0].Err == nil || got.Errors[0].Err.Error() != "copy 100% failed" {
		t.Fatalf("report errors = %#v", got.Errors)
	}
}

// TestReportToJSONStringIndentUsesSpaces pins the indented report: a JSON encoder accepts only
// spaces, and the indentation must reach every nested row.
func TestReportToJSONStringIndentUsesSpaces(t *testing.T) {
	report := &Report{
		Jobs: []*Job{{
			FullPath:    "/source/a.txt",
			FailTargets: map[string]error{"/target/a.txt": errors.New("boom")},
		}},
	}

	indented := report.ToJSONString(true)
	if strings.Contains(indented, "\t") {
		t.Fatalf("indented report contains a tab: %q", indented)
	}
	for _, snippet := range []string{"\n  \"files\"", "\n    {", "\n      \"fail_target\""} {
		if !strings.Contains(indented, snippet) {
			t.Fatalf("indented report does not contain %q:\n%s", snippet, indented)
		}
	}

	var decoded Report
	if err := json.Unmarshal([]byte(indented), &decoded); err != nil {
		t.Fatalf("decode indented report: %v", err)
	}
	if len(decoded.Jobs) != 1 || decoded.Jobs[0].FailTargets["/target/a.txt"] == nil {
		t.Fatalf("decoded report = %#v", decoded)
	}
}

// TestRunReportsAFailedItemAsATerminalRow pins the report row of an item ACP could not process:
// consumers resolve an item by its row, and the failure travels under the empty target key.
func TestRunReportsAFailedItemAsATerminalRow(t *testing.T) {
	root := t.TempDir()
	source := writeSourceFile(t, root, "unreadable.txt", []byte("fixture"))

	// The item must exist while the shell enumerates it and fail only once ACP reads it, so
	// revoke the read permission instead of removing the file: enumeration stats the source,
	// the content stage opens it.
	if err := os.Chmod(source, 0o000); err != nil {
		t.Fatalf("revoke read permission: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(source, 0o644) })

	if file, err := os.Open(source); err == nil {
		_ = file.Close()
		t.Skip("the test host can read a mode-000 file")
	}

	handler, getter := NewReportGetter()
	// The run has to read the source to fail on it: a targetless item under the default policy
	// is described without being opened, so the hash policy is what makes the open failure
	// reach the pipeline as an item outcome.
	copyer, err := New(
		context.Background(),
		WildcardJob(Source(source)),
		WithHash(true),
		WithEventHandler(handler),
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := copyer.WaitErr(); err != nil {
		t.Fatalf("WaitErr() error = %v, want nil: a failed item is an item outcome", err)
	}

	report := getter()
	if len(report.Errors) != 0 {
		t.Fatalf("report errors = %v, want none", report.Errors)
	}
	if len(report.Jobs) != 1 {
		t.Fatalf("report jobs = %d, want one terminal row", len(report.Jobs))
	}

	row := report.Jobs[0]
	if row.Status != JobStatusFinished {
		t.Fatalf("row status = %q, want %q", row.Status, JobStatusFinished)
	}
	if row.FullPath != source {
		t.Fatalf("row full path = %q, want %q", row.FullPath, source)
	}
	if len(row.FailTargets) != 1 || row.FailTargets[""] == nil {
		t.Fatalf("row fail targets = %v, want the item failure under the empty key", row.FailTargets)
	}
}
