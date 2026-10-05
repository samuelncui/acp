//go:build darwin || linux

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/samuelncui/acp"
)

func TestRewriteBusyEntryRetainsItsQueueAndContinues(t *testing.T) {
	// Hold a real advisory lock while an independent entry remains writable.
	root := t.TempDir()
	busy, good := filepath.Join(root, "busy"), filepath.Join(root, "good")
	for _, path := range []string{busy, good} {
		if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	file, err := os.Open(busy)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	state := &rewriteState{Root: root, Pending: []rewriteEntry{{Path: busy}, {Path: good}}}
	statePath := filepath.Join(root, "state.json")
	jobs := map[string]*acp.Job{}
	var reportErrors []*acp.Error
	err = runQueue(context.Background(), state, statePath, "", false, false, jobs, &reportErrors)
	if !errors.Is(err, errRewriteBusy) {
		t.Fatalf("busy outcome=%v", err)
	}

	// Keep the established busy classification and the good entry's success, then resume after unlock.
	saved, err := loadState(statePath)
	if err != nil || len(saved.Pending) != 0 || len(saved.Busy) != 1 || saved.Busy[0].Path != busy {
		t.Fatalf("state=%+v / %v", saved, err)
	}
	if len(jobs[good].SuccessTargets) != 1 {
		t.Fatal("independent rewrite did not succeed")
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if err := runQueue(context.Background(), saved, statePath, "", false, false, jobs, &reportErrors); err != nil {
		t.Fatal(err)
	}
}

func TestRewriteBusyEntryReportFailureRetainsOneRetry(t *testing.T) {
	// A report failure must retain the current entry and tail without also queueing a busy retry.
	root := t.TempDir()
	busy, tail := filepath.Join(root, "busy"), filepath.Join(root, "tail")
	for _, path := range []string{busy, tail} {
		if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	file, err := os.Open(busy)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	oldReport := writeReport
	sentinel := errors.New("report persistence failed")
	writeReport = func(string, bool, map[string]*acp.Job, []*acp.Error) error { return sentinel }
	t.Cleanup(func() { writeReport = oldReport })
	state := &rewriteState{Root: root, Pending: []rewriteEntry{{Path: busy}, {Path: tail}}}
	statePath := filepath.Join(root, "state.json")
	jobs := map[string]*acp.Job{}
	var reportErrors []*acp.Error
	err = runQueue(context.Background(), state, statePath, "", false, false, jobs, &reportErrors)
	if !errors.Is(err, sentinel) || !errors.Is(err, errRewriteBusy) {
		t.Fatalf("combined failure=%v", err)
	}

	// The saved queue owns each entry once, and its unaccepted tail has no report row.
	saved, err := loadState(statePath)
	if err != nil || len(saved.Pending) != 2 || saved.Pending[0].Path != busy || saved.Pending[1].Path != tail || len(saved.Busy) != 0 {
		t.Fatalf("state=%+v / %v", saved, err)
	}
	if jobs[tail] != nil {
		t.Fatal("tail started after report failure")
	}
	writeReport = oldReport
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if err := runQueue(context.Background(), saved, statePath, "", false, false, jobs, &reportErrors); err != nil {
		t.Fatal(err)
	}
}
