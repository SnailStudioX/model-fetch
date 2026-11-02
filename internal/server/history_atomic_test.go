// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// download_history.json is a read-modify-write file: every completed download
// reads the whole list, appends one entry and writes it back. It used to be
// written with os.WriteFile, which truncates the file before the new bytes
// arrive, so a process killed in that window lost the entire history and every
// later append failed on the half-written file.

func historyTestJob(id string) *Job {
	started := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	return &Job{
		ID:        id,
		Repo:      "owner/" + id,
		Revision:  "main",
		Status:    JobStatusCompleted,
		StartedAt: &started,
		Progress:  JobProgress{TotalFiles: 1, TotalBytes: 10},
	}
}

func TestAppendHistoryKeepsEarlierEntriesReadable(t *testing.T) {
	setTestConfigHome(t, t.TempDir())

	for _, id := range []string{"first", "second"} {
		if err := AppendHistory(historyTestJob(id)); err != nil {
			t.Fatalf("append %s: %v", id, err)
		}
	}

	entries, err := LoadHistory()
	if err != nil {
		t.Fatalf("history written by this build cannot be read back: %v", err)
	}
	if len(entries) != 2 || entries[0].ID != "first" || entries[1].ID != "second" {
		t.Fatalf("unexpected history: %+v", entries)
	}
}

func TestFailedHistoryReplaceLeavesPreviousFileIntact(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "download_history.json")
	previous := `{"entries":[{"id":"first","repo":"owner/first"}]}`
	if err := os.WriteFile(path, []byte(previous), 0o644); err != nil {
		t.Fatal(err)
	}

	err := replaceStateFile(path, []byte(`{"entries":[{"id":"second"}]}`), failingRenameOps{historyStateFileOps{}}, "download history")
	if err == nil {
		t.Fatal("a history replace whose rename failed reported success")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != previous {
		t.Errorf("the failed write changed the history file: %s", data)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".download_history-") {
			t.Errorf("the failed write left a temp file behind: %s", entry.Name())
		}
	}
}

// The counterpart of the fix: a history file that cannot be parsed is not
// quietly restarted as empty. The append fails and says so, and the file stays
// where a person can recover it.
func TestAppendHistoryRejectsUnparseableFile(t *testing.T) {
	setTestConfigHome(t, t.TempDir())
	if err := os.MkdirAll(AppConfigDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(HistoryPath(), []byte(`{"entries": [`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := AppendHistory(historyTestJob("later")); err == nil {
		t.Fatal("an unparseable history file was accepted")
	}
	if _, err := os.ReadFile(HistoryPath()); err != nil {
		t.Fatalf("the unreadable history file is gone: %v", err)
	}
}
