// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A mirror push or pull that writes no longer runs inside the request: the
// handler starts a task and answers, and the caller follows it. These tests pin
// the task contract (start, progress, finish, refuse a second one, cancel) and the
// guarantee that a cancelled sync never leaves a half-copied repository behind.

// startMirrorTaskAndWait posts a write sync and waits for its task to leave the
// running state, returning the finished task.
func startMirrorTaskAndWait(t *testing.T, s *Server, path, body string) *MirrorTask {
	t.Helper()

	w := cacheRequest(t, s, http.MethodPost, path, body)
	if w.Code != http.StatusAccepted {
		t.Fatalf("starting %s: status=%d body=%s", path, w.Code, w.Body.String())
	}
	var started struct {
		Task MirrorTask `json:"task"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	if started.Task.ID == "" {
		t.Fatalf("no task id in %s", w.Body.String())
	}
	if started.Task.State != mirrorStateRunning {
		t.Fatalf("a started task reports state %q", started.Task.State)
	}

	deadline := time.Now().Add(30 * time.Second)
	for {
		w := cacheRequest(t, s, http.MethodGet, "/api/mirror/task/"+started.Task.ID, "")
		if w.Code == http.StatusOK {
			var task MirrorTask
			if err := json.Unmarshal(w.Body.Bytes(), &task); err != nil {
				t.Fatal(err)
			}
			if task.State != mirrorStateRunning {
				return &task
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %s never finished", started.Task.ID)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// writeMirrorHubRepo builds one repository the way scanHubForMirror expects to
// find it: a models--owner--name directory with blobs and a refs/main.
func writeMirrorHubRepo(t *testing.T, hubDir, repoName string, files map[string]string) string {
	t.Helper()

	repoDir := filepath.Join(hubDir, "models--"+strings.ReplaceAll(repoName, "/", "--"))
	blobsDir := filepath.Join(repoDir, "blobs")
	if err := os.MkdirAll(blobsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(blobsDir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(repoDir, "refs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "refs", "main"), []byte("deadbeef\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return repoDir
}

func TestMirrorPushRunsAsTaskAndReportsProgress(t *testing.T) {
	cacheDir := t.TempDir()
	hubDir := filepath.Join(cacheDir, "hub")
	writeMirrorHubRepo(t, hubDir, "owner/one", map[string]string{"weight.bin": "one"})
	writeMirrorHubRepo(t, hubDir, "owner/two", map[string]string{"weight.bin": "two"})
	target := t.TempDir()

	s := New(Config{CacheDir: cacheDir})
	task := startMirrorTaskAndWait(t, s, "/api/mirror/push", `{"target":"`+filepath.ToSlash(target)+`"}`)

	if task.State != mirrorStateCompleted {
		t.Fatalf("task state = %q, want %q (error=%q result=%+v)", task.State, mirrorStateCompleted, task.Error, task.Result)
	}
	if task.Result == nil || task.Result.Copied != 2 || !task.Result.Success {
		t.Fatalf("result reports the wrong work: %+v", task.Result)
	}
	if task.TotalRepos != 2 || task.DoneRepos != 2 {
		t.Errorf("progress = %d/%d, want 2/2", task.DoneRepos, task.TotalRepos)
	}
	if task.Mode != "push" || task.FinishedAt == nil {
		t.Errorf("task is missing how it ended: %+v", task)
	}
	for _, repo := range []string{"owner/one", "owner/two"} {
		dir := filepath.Join(target, "hub", "models--"+strings.ReplaceAll(repo, "/", "--"), "blobs", "weight.bin")
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("%s did not reach the destination: %v", repo, err)
		}
	}

	// The page reloads: the most recent task has to be findable by itself.
	w := cacheRequest(t, s, http.MethodGet, "/api/mirror/task", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/mirror/task: %d %s", w.Code, w.Body.String())
	}
	var current struct {
		Task MirrorTask `json:"task"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &current); err != nil {
		t.Fatal(err)
	}
	if current.Task.ID != task.ID || current.Task.State != mirrorStateCompleted {
		t.Fatalf("the running or last task is not reportable: %+v", current.Task)
	}

	if w := cacheRequest(t, s, http.MethodGet, "/api/mirror/task/mirror-never-started", ""); w.Code != http.StatusNotFound {
		t.Errorf("unknown task id status = %d, want 404", w.Code)
	}
}

// Two syncs writing the same destination would race over the same repository
// directories, so a second one is refused while the first is running.
func TestMirrorTaskRefusesASecondRunningSync(t *testing.T) {
	s := New(Config{CacheDir: t.TempDir()})

	s.mirrorTaskMu.Lock()
	s.mirrorTask = &MirrorTask{ID: "mirror-busy", Mode: "push", State: mirrorStateRunning}
	s.mirrorTaskMu.Unlock()

	w := cacheRequest(t, s, http.MethodPost, "/api/mirror/push", `{"target":"`+filepath.ToSlash(t.TempDir())+`"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("second sync while one runs: status=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "mirror-busy") {
		t.Errorf("the conflict does not name the running task: %s", w.Body.String())
	}
}

// A dry run reads two trees and writes nothing, so it stays a plain answer: there
// is nothing to supervise, and the diff panel needs the result in the response.
func TestMirrorDryRunStaysSynchronous(t *testing.T) {
	cacheDir := t.TempDir()
	writeMirrorHubRepo(t, filepath.Join(cacheDir, "hub"), "owner/one", map[string]string{"weight.bin": "one"})
	target := t.TempDir()

	s := New(Config{CacheDir: cacheDir})
	w := cacheRequest(t, s, http.MethodPost, "/api/mirror/push", `{"target":"`+filepath.ToSlash(target)+`","dryRun":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("dry run: status=%d body=%s", w.Code, w.Body.String())
	}
	var result MirrorSyncResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.DryRun || result.Copied != 1 {
		t.Fatalf("dry run reported the wrong thing: %+v", result)
	}
	if entries, err := os.ReadDir(filepath.Join(target, "hub")); err == nil && len(entries) > 0 {
		t.Errorf("a dry run wrote the destination: %v", entries)
	}
}

// A cancelled sync must not leave a repository half copied: the destination would
// then look synchronized while it is not.
func TestMirrorSyncCancellationLeavesNoPartialRepo(t *testing.T) {
	src := t.TempDir()
	srcHub := filepath.Join(src, "hub")
	writeMirrorHubRepo(t, srcHub, "owner/one", map[string]string{"weight.bin": "one"})
	writeMirrorHubRepo(t, srcHub, "owner/two", map[string]string{"weight.bin": "two"})
	dstHub := filepath.Join(t.TempDir(), "hub")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var seen []mirrorProgress
	result, err := mirrorSyncHubs(srcHub, dstHub, mirrorSyncOptions{
		Ctx: ctx,
		Progress: func(p mirrorProgress) {
			seen = append(seen, p)
			// Stop after the first repository: the second must not be started at
			// all, so the destination holds either a complete repository or none.
			if p.Done >= 1 {
				cancel()
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Success {
		t.Error("a cancelled sync reported success")
	}
	if !strings.Contains(result.Message, "cancelled") {
		t.Errorf("the message does not say it was cancelled: %q", result.Message)
	}
	if len(seen) == 0 || seen[0].Total != 2 {
		t.Errorf("the first progress report should name both repositories: %+v", seen)
	}

	copied := 0
	for _, repo := range []string{"owner/one", "owner/two"} {
		dir := filepath.Join(dstHub, "models--"+strings.ReplaceAll(repo, "/", "--"))
		switch _, err := os.Stat(dir); {
		case err == nil:
			for _, name := range []string{filepath.Join("blobs", "weight.bin"), filepath.Join("refs", "main")} {
				if _, statErr := os.Stat(filepath.Join(dir, name)); statErr != nil {
					t.Errorf("%s is present without %s: a partial repository was left behind", repo, name)
				}
			}
			copied++
		case os.IsNotExist(err):
			// Not started, which is the other allowed outcome.
		default:
			t.Fatal(err)
		}
	}
	if copied == 0 {
		t.Error("the first repository was reported copied but is not in the destination")
	}
	if copied == 2 {
		t.Error("the cancellation did not stop the second repository")
	}

	// The result counts what reached the destination, not what the plan intended:
	// a stopped sync that reported the plan would read as a completed one.
	if result.Copied != copied {
		t.Errorf("result reports %d copied, the destination holds %d", result.Copied, copied)
	}
	var onDisk int64
	for _, repo := range []string{"owner/one", "owner/two"} {
		// Sizes are counted over blobs, the same way the scanner does.
		blobs := filepath.Join(dstHub, "models--"+strings.ReplaceAll(repo, "/", "--"), "blobs")
		if err := filepath.WalkDir(blobs, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			info, statErr := d.Info()
			if statErr == nil {
				onDisk += info.Size()
			}
			return nil
		}); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	if result.CopiedSize != onDisk {
		t.Errorf("result reports %d bytes copied, the destination holds %d", result.CopiedSize, onDisk)
	}
	if result.Deleted != 0 {
		t.Errorf("nothing was deleted, result says %d", result.Deleted)
	}
}

// With Force the destination already had a complete-enough repository, so
// removing the interrupted re-copy removes that one too. The removal is the right
// thing - a half-written re-copy would read as synchronized - but it is the one
// interruption that discards work that was already there, so the result has to
// name it rather than let the caller assume the destination still holds what it
// held before.
func TestMirrorCancelledForceRecopySaysWhatItRemoved(t *testing.T) {
	srcHub := filepath.Join(t.TempDir(), "hub")
	writeMirrorHubRepo(t, srcHub, "owner/one", map[string]string{"weight.bin": "one", "extra.bin": "not in the destination"})
	writeMirrorHubRepo(t, srcHub, "owner/two", map[string]string{"weight.bin": "two"})

	dstHub := filepath.Join(t.TempDir(), "hub")
	oldRepo := writeMirrorHubRepo(t, dstHub, "owner/one", map[string]string{"weight.bin": "one"})
	oldBlob := filepath.Join(oldRepo, "blobs", "weight.bin")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result, err := mirrorSyncHubs(srcHub, dstHub, mirrorSyncOptions{
		Force: true,
		Ctx:   ctx,
		Progress: func(p mirrorProgress) {
			// The report naming a repository is the one just before it is copied,
			// so the sync enters owner/one and stops at its first file.
			if p.Repo == "owner/one" {
				cancel()
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(oldBlob); !os.IsNotExist(statErr) {
		t.Errorf("the interrupted re-copy left the destination holding half of %s: %v", "owner/one", statErr)
	}

	var saysSo bool
	for _, e := range result.Errors {
		if strings.Contains(e, "owner/one") && strings.Contains(e, "already had") {
			saysSo = true
		}
	}
	if !saysSo {
		t.Errorf("a cancelled force re-copy did not report that it removed the destination's own copy: %v", result.Errors)
	}
}

// A sync into a destination that had nothing to lose stays quiet about removals:
// reporting a loss that did not happen is as misleading as hiding one that did.
func TestMirrorCancelledFreshCopyReportsNoLostCopy(t *testing.T) {
	srcHub := filepath.Join(t.TempDir(), "hub")
	writeMirrorHubRepo(t, srcHub, "owner/one", map[string]string{"weight.bin": "one"})

	dstHub := filepath.Join(t.TempDir(), "hub")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result, err := mirrorSyncHubs(srcHub, dstHub, mirrorSyncOptions{
		Ctx: ctx,
		Progress: func(p mirrorProgress) {
			if p.Repo == "owner/one" {
				cancel()
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range result.Errors {
		if strings.Contains(e, "already had") {
			t.Errorf("a copy with no previous destination copy claimed a loss: %q", e)
		}
	}
}
