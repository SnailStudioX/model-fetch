//go:build windows

// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestWindowsReplaceJobsStateFileUsesNativeReplacement(t *testing.T) {
	t.Run("success replaces existing target", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "jobs.json")
		if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := replaceJobsStateFile(path, []byte("new"), defaultStateFileOps{}); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "new" {
			t.Fatalf("replacement content = %q, want new", got)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !info.Mode().IsRegular() {
			t.Fatalf("replacement is not a regular file: %v", info.Mode())
		}
		if info.Mode().Perm()&0o200 == 0 {
			t.Fatalf("replacement lost writable attribute: %v", info.Mode())
		}
		assertNoStateTemps(t, dir)
	})

	t.Run("pre-commit sync failure preserves target and removes temp", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "jobs.json")
		old := []byte("old")
		if err := os.WriteFile(path, old, 0o600); err != nil {
			t.Fatal(err)
		}
		want := errors.New("injected sync failure")
		err := replaceJobsStateFile(path, []byte("new"), failingSyncOps{defaultStateFileOps{}, want})
		if !errors.Is(err, want) {
			t.Fatalf("replacement error = %v, want sync error", err)
		}
		got, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if string(got) != string(old) {
			t.Fatalf("failed replacement changed target: %q", got)
		}
		assertNoStateTemps(t, dir)
	})
}

// openWithoutShareDelete is any reader of the destination: os.Open asks for
// FILE_SHARE_READ|FILE_SHARE_WRITE only, so the file cannot be renamed over for
// as long as that handle lives. A scan by the antivirus, a LoadState in another
// goroutine, or another process that just read the state looks the same.
func openWithoutShareDelete(t *testing.T, path string) *os.File {
	t.Helper()
	reader, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return reader
}

func TestWindowsRenameStateFileStagesTheFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "jobs_state.json")
	staged := filepath.Join(dir, ".jobs_state-staged.tmp")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}

	reader := openWithoutShareDelete(t, target)
	go func() {
		time.Sleep(80 * time.Millisecond)
		reader.Close()
	}()

	if err := renameStateFile(staged, target); err != nil {
		t.Fatalf("the replacement gave up on a target that was busy for a moment: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" {
		t.Fatalf("target content after the retry = %q, want new", got)
	}
}

func TestWindowsRenameStateFileGivesUpOnAPermanentlyBusyTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "jobs_state.json")
	staged := filepath.Join(dir, ".jobs_state-staged.tmp")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}

	reader := openWithoutShareDelete(t, target)
	defer reader.Close()

	started := time.Now()
	err := renameStateFile(staged, target)
	elapsed := time.Since(started)
	if !errors.Is(err, syscall.ERROR_ACCESS_DENIED) {
		t.Fatalf("replacement error = %v, want access denied", err)
	}
	// The wait is bounded: a save must not stall behind a handle that never
	// closes, and the caller keeps the temp file for its own cleanup.
	if elapsed > 2*time.Second {
		t.Fatalf("the retry waited %v before giving up", elapsed)
	}
	got, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != "old" {
		t.Fatalf("a failed replacement changed the target: %q", got)
	}
}
