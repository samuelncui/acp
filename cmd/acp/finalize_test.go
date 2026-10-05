package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samuelncui/acp"
	"github.com/sirupsen/logrus"
)

func TestReportFinishUsesOneSnapshot(t *testing.T) {
	// Capture diagnostics so the report-write fallback can be checked against the stored shape.
	var output bytes.Buffer
	logger := logrus.StandardLogger()
	previousOutput, previousFormatter, previousLevel := logger.Out, logger.Formatter, logger.GetLevel()
	t.Cleanup(func() {
		logger.SetOutput(previousOutput)
		logger.SetFormatter(previousFormatter)
		logger.SetLevel(previousLevel)
	})
	logger.SetOutput(&output)
	logger.SetFormatter(&logrus.JSONFormatter{DisableTimestamp: true})
	logger.SetLevel(logrus.InfoLevel)

	// Verify capture count for no report, saved report and failed report storage.
	for _, test := range []struct {
		name         string
		snapshot     *acp.Report
		noReport     bool
		writeFailure bool
		wantExit     int
	}{
		{name: "without a report", snapshot: &acp.Report{}, noReport: true},
		{
			name: "success",
			snapshot: &acp.Report{Jobs: []*acp.Job{{
				FullPath: "/source/a.txt", Base: "/source", Path: []string{"a.txt"},
				Status: acp.JobStatusFinished, SuccessTargets: []string{"/target/a.txt"},
			}}},
		},
		{
			name: "report write failure",
			snapshot: &acp.Report{Jobs: []*acp.Job{{
				FullPath: "/source/a.txt", Status: acp.JobStatusFinished,
				SuccessTargets: []string{"/target/a.txt"},
			}}},
			writeFailure: true, wantExit: 1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			// A file used as the report's parent gives a deterministic storage failure.
			path := ""
			if !test.noReport {
				path = filepath.Join(t.TempDir(), "report.json")
				if test.writeFailure {
					if err := os.WriteFile(path, []byte("existing file"), 0o600); err != nil {
						t.Fatal(err)
					}
					path = filepath.Join(path, "report.json")
				}
			}

			// The collector must be read once even when both storage and diagnostics need its rows.
			calls := 0
			collector := &report{getter: func() *acp.Report {
				calls++
				return test.snapshot
			}}
			output.Reset()
			if code := collector.finish(nil, path, false); code != test.wantExit {
				t.Fatalf("exit code = %d, want %d", code, test.wantExit)
			}
			if calls != 1 {
				t.Fatalf("terminal report captures = %d, want 1", calls)
			}

			// Both a failed and a successful run persist exactly the supplied report shape.
			if path != "" && !test.writeFailure {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("read terminal report: %v", err)
				}
				var compact bytes.Buffer
				if err := json.Compact(&compact, data); err != nil {
					t.Fatalf("decode terminal report: %v", err)
				}
				if want := test.snapshot.ToJSONString(false); compact.String() != want {
					t.Fatalf("stored report = %s, want %s", &compact, want)
				}
			}

			// A storage failure logs the same captured rows.
			decoder := json.NewDecoder(&output)
			var entry struct {
				Level string `json:"level"`
				Msg   string `json:"msg"`
			}
			if test.writeFailure {
				if err := decoder.Decode(&entry); err != nil {
					t.Fatalf("decode report-write warning: %v", err)
				}
				if entry.Level != "warning" || !strings.Contains(entry.Msg, path) {
					t.Fatalf("report-write warning = %#v", entry)
				}
				if err := decoder.Decode(&entry); err != nil {
					t.Fatalf("decode fallback report: %v", err)
				}
				if want := fmt.Sprintf("report= %q", test.snapshot.ToJSONString(false)); entry.Msg != want {
					t.Fatalf("fallback report = %q, want %q", entry.Msg, want)
				}
			}
			if decoder.More() {
				t.Fatal("unexpected extra diagnostics after finalizing the report")
			}
		})
	}
}
