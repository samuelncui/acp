// Package acp_test keeps a compile-level guard for the af05f05c public API. ACP is a public
// library: every function, method and struct of that commit keeps its name, and the report row
// keeps its field names, types and JSON tags, so a caller written against af05f05c still
// compiles and runs without changing a line.
//
// The declarations and the run below only use the exported surface, exactly like a caller does,
// so a break of the old shape fails this build instead of a downstream one.
package acp_test

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/samuelncui/acp"
	"github.com/sirupsen/logrus"
)

// The af05f05c functions, options and methods with their af05f05c signatures. WaitErr is the
// additive companion of Wait: it is what a caller reads the run's outcome from.
var (
	_ func(context.Context, ...acp.Option) (*acp.Copyer, error) = acp.New
	_ func(*acp.Copyer)                                         = (*acp.Copyer).Wait
	_ func(*acp.Copyer) error                                   = (*acp.Copyer).WaitErr

	_ func(string, []string) acp.Option               = acp.AccurateJob
	_ func(...acp.WildcardJobOption) acp.Option       = acp.WildcardJob
	_ func(...string) acp.WildcardJobOption           = acp.Source
	_ func(string, ...[]string) acp.WildcardJobOption = acp.AccurateSource
	_ func(...string) acp.WildcardJobOption           = acp.Target

	_ func(...acp.DeviceOption) acp.Option = acp.SetFromDevice
	_ func(...acp.DeviceOption) acp.Option = acp.SetToDevice
	_ func(bool) acp.DeviceOption          = acp.LinearDevice
	_ func(int) acp.DeviceOption           = acp.DeviceThreads

	_ func(bool) acp.Option                       = acp.Overwrite
	_ func(bool) acp.Option                       = acp.WithHash
	_ func(*logrus.Logger) acp.Option             = acp.WithLogger
	_ func(acp.EventHandler) acp.Option           = acp.WithEventHandler
	_ func() acp.Option                           = acp.WithProgressBar
	_ func() acp.EventHandler                     = acp.NewProgressBar
	_ func() (acp.EventHandler, acp.ReportGetter) = acp.NewReportGetter
	_ func(*acp.Report, bool) string              = (*acp.Report).ToJSONString

	_ func(func(string) int) func(string) int = acp.Cache[string, int]

	_ acp.ReportGetter = func() *acp.Report { return new(acp.Report) }
	_ acp.EventHandler = func(acp.Event) {}
	_ error            = (*acp.Error)(nil)
)

// The af05f05c report row: every field with the type and the JSON tag of that commit. A field
// that changed type, such as Path, breaks this literal.
func af05f05cReportRow() *acp.Job {
	return &acp.Job{
		Base: "/source",
		Path: []string{"nested", "data.txt"},

		Status:         acp.JobStatusFinished,
		SuccessTargets: []string{"/target/nested/data.txt"},
		FailTargets:    map[string]error{"/target/other.txt": errors.New("file exists")},

		Size:      int64(6),
		Mode:      fs.FileMode(0o644),
		ModTime:   time.Unix(1700000000, 0),
		WriteTime: time.Unix(1700000001, 0),
		SHA256:    "abc",
	}
}

// The af05f05c events, including the row event the report machinery consumes.
func af05f05cEvents() []acp.Event {
	return []acp.Event{
		&acp.EventUpdateJob{Job: af05f05cReportRow()},
		&acp.EventUpdateCount{Bytes: 6, Files: 1, Finished: true},
		&acp.EventUpdateProgress{Bytes: 6, Files: 1, Finished: true},
		&acp.EventReportError{Error: &acp.Error{Src: "src", Dst: "dst", Err: errors.New("boom")}},
		&acp.EventFinished{},
	}
}

// af05f05cJobs is the job vocabulary of that commit: an exact pair and a wildcard tree that
// uses both a plain source and a segmented one.
func af05f05cJobs(treeSource, exactSource, exactTarget, mappedTarget string) []acp.Option {
	return []acp.Option{
		acp.AccurateJob(exactSource, []string{exactTarget}),
		acp.WildcardJob(
			acp.Source(treeSource),
			acp.AccurateSource(filepath.Dir(exactSource), []string{filepath.Base(exactSource)}),
			acp.Target(mappedTarget),
		),
	}
}

// TestAf05f05cRowJSONShape pins the old document: base is a string, path is an array, and the
// fields the old release wrote keep their names and order-independent values.
func TestAf05f05cRowJSONShape(t *testing.T) {
	encoded, err := json.Marshal(af05f05cReportRow())
	if err != nil {
		t.Fatalf("marshal af05f05c row: %v", err)
	}

	var shape struct {
		Base    string            `json:"base"`
		Path    []string          `json:"path"`
		Status  string            `json:"status"`
		Size    int64             `json:"size"`
		Mode    uint32            `json:"mode"`
		SHA256  string            `json:"sha256"`
		Success []string          `json:"success_target"`
		Fail    map[string]string `json:"fail_target"`
	}
	if err := json.Unmarshal(encoded, &shape); err != nil {
		t.Fatalf("decode af05f05c row: %v\n%s", err, encoded)
	}
	if shape.Base != "/source" || !slices.Equal(shape.Path, []string{"nested", "data.txt"}) {
		t.Fatalf("row identity = %s", encoded)
	}
	if shape.Status != acp.JobStatusFinished || shape.Size != 6 || shape.Mode != 0o644 || shape.SHA256 != "abc" {
		t.Fatalf("row facts = %s", encoded)
	}
	if len(shape.Success) != 1 || shape.Success[0] != "/target/nested/data.txt" {
		t.Fatalf("row success targets = %s", encoded)
	}
	if shape.Fail["/target/other.txt"] != "file exists" {
		t.Fatalf("row fail targets = %s", encoded)
	}

	// The status vocabulary and the walk mask of that commit stay exported.
	for _, status := range []string{
		acp.JobStatusPending, acp.JobStatusPreparing, acp.JobStatusCopying,
		acp.JobStatusFinishing, acp.JobStatusFinished,
	} {
		if status == "" {
			t.Fatal("empty job status constant")
		}
	}
	if acp.UnexpectFileMode == 0 {
		t.Fatal("UnexpectFileMode is empty")
	}
}

// TestShellSurfaceRunsThroughTheOldCalls drives the af05f05c shell exactly as the old callers
// did: New plus job options plus WaitErr, with the report read through NewReportGetter and the
// old events delivered to an old handler.
func TestShellSurfaceRunsThroughTheOldCalls(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.MkdirAll(filepath.Join(source, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "nested", "data.txt"), []byte("nested"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "target")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}

	// Every event shape of af05f05c still satisfies the exported interface.
	events := af05f05cEvents()
	if len(events) != 5 {
		t.Fatalf("events = %d, want 5", len(events))
	}
	if event, ok := events[0].(*acp.EventUpdateJob); !ok || event.Job.Base != "/source" {
		t.Fatalf("row event = %#v", events[0])
	}

	cached := acp.Cache(func(in string) int { return len(in) })
	if got := cached("acp"); got != 3 {
		t.Fatalf("Cache() = %d, want 3", got)
	}

	handler, getter := acp.NewReportGetter()
	options := af05f05cJobs(
		source,
		filepath.Join(source, "nested", "data.txt"),
		filepath.Join(root, "exact.txt"),
		target,
	)
	options = append(options,
		acp.SetFromDevice(acp.LinearDevice(false), acp.DeviceThreads(1)),
		acp.SetToDevice(acp.DeviceThreads(1)),
		acp.Overwrite(false),
		acp.WithHash(true),
		acp.WithLogger(logrus.StandardLogger()),
		acp.WithEventHandler(handler),
	)

	copyer, err := acp.New(context.Background(), options...)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := copyer.WaitErr(); err != nil {
		t.Fatalf("WaitErr() error = %v, want nil", err)
	}
	copyer.Wait()

	report := getter()
	if report == nil || len(report.Errors) != 0 {
		t.Fatalf("report = %#v, want rows without pipeline errors", report)
	}
	if len(report.Jobs) == 0 {
		t.Fatal("report has no rows")
	}
	for _, row := range report.Jobs {
		if row.Base == "" || len(row.Path) == 0 {
			t.Fatalf("row identity = %#v, want a base and a segmented path", row)
		}
		if row.FullPath == "" {
			t.Fatalf("row = %#v, want the additive whole path", row)
		}
	}
	if _, err := os.Stat(filepath.Join(target, "source", "nested", "data.txt")); err != nil {
		t.Fatalf("copied file: %v", err)
	}
}

// TestShellPublishesTheOldBaseAndSegmentedPath pins the row of a nested source file: the shell
// reports the source base and the source-relative path segments af05f05c wrote, and the report
// document carries that path as an array.
func TestShellPublishesTheOldBaseAndSegmentedPath(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.MkdirAll(filepath.Join(source, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(source, "nested", "data.txt")
	if err := os.WriteFile(nested, []byte("nested"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "target")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}

	handler, getter := acp.NewReportGetter()
	copyer, err := acp.New(
		context.Background(),
		acp.WildcardJob(acp.Source(source), acp.Target(target)),
		acp.WithEventHandler(handler),
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := copyer.WaitErr(); err != nil {
		t.Fatalf("WaitErr() error = %v, want nil", err)
	}

	report := getter()
	if len(report.Jobs) != 1 {
		t.Fatalf("report rows = %d, want 1", len(report.Jobs))
	}

	row := report.Jobs[0]
	// The enumeration starts at the source itself, so the relative path keeps the source name,
	// which is the row af05f05c published for /root/source/nested/data.txt.
	base, name := filepath.Split(source)
	wantPath := []string{name, "nested", "data.txt"}
	if row.Base != base || !slices.Equal(row.Path, wantPath) {
		t.Fatalf("row identity = base %q path %#v, want %q and %#v", row.Base, row.Path, base, wantPath)
	}
	if row.FullPath != nested {
		t.Fatalf("row full path = %q, want %q", row.FullPath, nested)
	}

	encoded, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("marshal row: %v", err)
	}
	if want := `"path":["` + name + `","nested","data.txt"]`; !strings.Contains(string(encoded), want) {
		t.Fatalf("row JSON = %s, want %s", encoded, want)
	}
}

// TestReportGetterKeysRowsByTheOldRelativePath pins the af05f05c lookup: rows that share a
// relative path are one entry, because that is the key the old report getter used.
func TestReportGetterKeysRowsByTheOldRelativePath(t *testing.T) {
	handler, getter := acp.NewReportGetter()
	handler(&acp.EventUpdateJob{Job: &acp.Job{Base: "/first/", Path: []string{"tree", "a.txt"}}})
	handler(&acp.EventUpdateJob{Job: &acp.Job{Base: "/second/", Path: []string{"tree", "a.txt"}}})

	report := getter()
	if len(report.Jobs) != 1 {
		t.Fatalf("report rows = %d, want one row per relative path", len(report.Jobs))
	}
	if report.Jobs[0].Base != "/second/" {
		t.Fatalf("report row base = %q, want the last row of the key", report.Jobs[0].Base)
	}
}
