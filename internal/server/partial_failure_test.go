// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestScanLocalCachedReposWarnsOnUnreadableRoot pins the partial-failure
// contract. A scan root that exists but cannot be listed used to be skipped, so
// GET /api/cache answered an empty repository list and the dashboard rendered
// "no cached models" for a directory it simply could not open. A regular file
// standing where a directory is expected fails the same way on every platform,
// without relying on POSIX permission bits that Windows does not enforce.
func TestScanLocalCachedReposWarnsOnUnreadableRoot(t *testing.T) {
	dir := t.TempDir()
	notADirectory := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(notADirectory, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	repos, warnings, err := scanLocalCachedReposWarned(dir, "", []string{notADirectory}, nil, false)
	if err != nil {
		t.Fatalf("a partly unreadable listing must stay a success: %v", err)
	}
	if len(repos) != 0 {
		t.Fatalf("unexpected repos: %+v", repos)
	}
	if len(warnings) == 0 {
		t.Fatal("an unreadable scan root produced no warning")
	}
	if !strings.Contains(warnings[0], notADirectory) {
		t.Errorf("warning does not name the path that failed: %q", warnings[0])
	}
}

// TestScanLocalCachedReposSilentWhenRootsAreReadable is the other half: an empty
// cache really is empty, and must not come back with warnings that would make
// the UI claim a read failure.
func TestScanLocalCachedReposSilentWhenRootsAreReadable(t *testing.T) {
	dir := t.TempDir()

	repos, warnings, err := scanLocalCachedReposWarned(dir, "", nil, nil, false)
	if err != nil || len(repos) != 0 {
		t.Fatalf("empty cache: repos=%v err=%v", repos, err)
	}
	if len(warnings) != 0 {
		t.Errorf("a readable empty cache produced warnings: %q", warnings)
	}
}

// TestScanHubForMirrorDistinguishesMissingFromUnreadable is the precondition the
// mirror diff and sync fixes rest on: a target that does not exist is an empty
// scan (a fresh mirror target is normal), while a target that cannot be read is
// an error the caller must report instead of treating as empty.
func TestScanHubForMirrorDistinguishesMissingFromUnreadable(t *testing.T) {
	missing, err := scanCacheForMirror(filepath.Join(t.TempDir(), "never-created"))
	if err != nil || len(missing) != 0 {
		t.Fatalf("a missing target should scan as empty, got entries=%v err=%v", missing, err)
	}

	// A file where a hub directory is expected: os.Stat succeeds, so the scan
	// reaches ReadDir and fails. (As an ancestor of the joined "hub" path that
	// same file reads as missing on Windows, where a path below a file is
	// reported as "path not found" rather than "not a directory".)
	dir := t.TempDir()
	notADirectory := filepath.Join(dir, "hub")
	if err := os.WriteFile(notADirectory, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := scanHubForMirror(notADirectory); err == nil {
		t.Fatal("an unreadable hub directory was reported as an empty one")
	}
}

// TestMirrorSyncRefusesUnreadableDestination is the half the diff fix could not
// cover. A diff may report a partly unreadable target and still be useful, but a
// sync writes: with the destination unreadable every source repo looks missing, so
// the copy would move data it cannot check is already there. A target that was
// never created stays the normal first mirror.
func TestMirrorSyncRefusesUnreadableDestination(t *testing.T) {
	dir := t.TempDir()
	srcHub := filepath.Join(dir, "src-hub")
	repoDir := filepath.Join(srcHub, "models--owner--repo")
	for _, sub := range []string{"blobs", "refs"} {
		if err := os.MkdirAll(filepath.Join(repoDir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(repoDir, "blobs", "weight.bin"), []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "refs", "main"), []byte("deadbeef\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	unreadable := filepath.Join(dir, "dst-hub")
	if err := os.WriteFile(unreadable, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := mirrorSyncHubs(srcHub, unreadable, mirrorSyncOptions{})
	if err == nil {
		t.Fatalf("a sync into an unreadable destination reported no error: %+v", result)
	}
	if !strings.Contains(err.Error(), unreadable) {
		t.Errorf("the error does not name the destination: %v", err)
	}

	fresh := filepath.Join(dir, "fresh-hub")
	result, err = mirrorSyncHubs(srcHub, fresh, mirrorSyncOptions{DryRun: true})
	if err != nil {
		t.Fatalf("a first mirror to a destination that does not exist must work: %v", err)
	}
	if result.Copied != 1 {
		t.Errorf("a first mirror should offer the one source repo, copied=%d repos=%v", result.Copied, result.Repos)
	}
}
