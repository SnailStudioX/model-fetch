// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDismissJob_OnlyTerminalStates ensures DismissJob refuses to remove
// jobs that are still queued or running — a user can't accidentally wipe
// a live download by clicking the wrong button. Paused jobs are terminal
// from the "visible in list" standpoint and can be dismissed.
func TestDismissJob_OnlyTerminalStates(t *testing.T) {
	m := newTestJobManager(t, Config{}, nil)

	cases := []struct {
		status     JobStatus
		wantRemove bool
	}{
		{JobStatusQueued, false},
		{JobStatusRunning, false},
		{JobStatusPaused, true},
		{JobStatusCompleted, true},
		{JobStatusFailed, true},
		{JobStatusCancelled, true},
	}

	for _, tc := range cases {
		t.Run(string(tc.status), func(t *testing.T) {
			id := "job-" + string(tc.status)
			m.jobs[id] = &Job{
				ID:        id,
				Repo:      "owner/repo",
				Status:    tc.status,
				CreatedAt: time.Now(),
			}

			got := m.DismissJob(id)
			if got != tc.wantRemove {
				t.Errorf("DismissJob(%q) = %v, want %v", tc.status, got, tc.wantRemove)
			}
			_, present := m.jobs[id]
			if tc.wantRemove && present {
				t.Errorf("job %q still present after successful dismiss", id)
			}
			if !tc.wantRemove && !present {
				t.Errorf("job %q removed despite being in non-terminal state", id)
			}
			// Clean up for the next subtest.
			delete(m.jobs, id)
		})
	}
}

// TestDismissJob_SurvivesPageRefresh verifies the core #68-secondary
// guarantee: once a job is dismissed, it no longer appears in the list
// that sendInitialState uses to rehydrate a reconnecting browser.
func TestDismissJob_SurvivesPageRefresh(t *testing.T) {
	m := newTestJobManager(t, Config{}, nil)

	m.jobs["live"] = &Job{ID: "live", Status: JobStatusRunning, CreatedAt: time.Now()}
	m.jobs["gone"] = &Job{ID: "gone", Status: JobStatusCompleted, CreatedAt: time.Now()}

	if !m.DismissJob("gone") {
		t.Fatal("DismissJob(gone) returned false")
	}

	jobs := m.ListJobs()
	foundLive := false
	for _, j := range jobs {
		if j.ID == "gone" {
			t.Errorf("dismissed job %q still appears in ListJobs; refresh would repopulate the UI", j.ID)
		}
		if j.ID == "live" {
			foundLive = true
		}
	}
	if !foundLive {
		t.Error("live running job was unexpectedly removed")
	}
}

// TestDismissJobResult_ActiveJobReturnsSnapshot ensures the rejection
// path follows the JobManager snapshot contract instead of returning
// the live mutable job pointer.
func TestDismissJobResult_ActiveJobReturnsSnapshot(t *testing.T) {
	m := newTestJobManager(t, Config{}, nil)
	m.jobs["live"] = &Job{
		ID:        "live",
		Status:    JobStatusRunning,
		CreatedAt: time.Now(),
		Files: []JobFileProgress{{
			Path:   "model.bin",
			Status: "active",
		}},
	}

	res, snapshot := m.DismissJobResult("live")
	if res != DismissJobStillActive {
		t.Fatalf("DismissJobResult = %v, want DismissJobStillActive", res)
	}
	if snapshot == nil {
		t.Fatal("snapshot is nil")
	}
	snapshot.Status = JobStatusCancelled
	snapshot.Files[0].Status = "mutated"

	live := m.jobs["live"]
	if live.Status != JobStatusRunning {
		t.Errorf("live status mutated through returned snapshot: %s", live.Status)
	}
	if live.Files[0].Status != "active" {
		t.Errorf("live file status mutated through returned snapshot: %s", live.Files[0].Status)
	}
}

// TestHandleDismissJob_EndToEnd drives the HTTP endpoint, asserts the
// dismissed job is gone, and confirms the response envelope. Exercises
// the full route wiring so we catch regressions in server.go registration.
func TestHandleDismissJob_EndToEnd(t *testing.T) {
	hub := NewWSHub()
	go hub.Run()

	srv := &Server{
		config: Config{},
		jobs:   newTestJobManager(t, Config{}, hub),
		wsHub:  hub,
	}
	srv.jobs.jobs["done1"] = &Job{
		ID:        "done1",
		Repo:      "owner/repo",
		Status:    JobStatusCompleted,
		CreatedAt: time.Now(),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/jobs/{id}/dismiss", srv.handleDismissJob)

	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/jobs/done1/dismiss", "application/json", nil)
	if err != nil {
		t.Fatalf("POST dismiss: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["success"] != true {
		t.Errorf("response success = %v, want true", body["success"])
	}

	// Attempting to dismiss a running job must fail with 409 so the UI
	// can distinguish "not yet dismissable" from "not found".
	srv.jobs.jobs["busy"] = &Job{ID: "busy", Status: JobStatusRunning, CreatedAt: time.Now()}
	resp2, err := http.Post(ts.URL+"/api/jobs/busy/dismiss", "application/json", nil)
	if err != nil {
		t.Fatalf("POST dismiss (busy): %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusConflict {
		t.Errorf("busy dismiss status = %d, want 409", resp2.StatusCode)
	}

	// 404 for unknown job.
	resp3, err := http.Post(ts.URL+"/api/jobs/nope/dismiss", "application/json", nil)
	if err != nil {
		t.Fatalf("POST dismiss (nope): %v", err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusNotFound {
		t.Errorf("nope dismiss status = %d, want 404", resp3.StatusCode)
	}
}

// TestDismissJob_BroadcastsRemoval pins the tombstone. Dismissal deletes the
// job server-side and returns no snapshot, so every connected client has to be
// told to forget the card; otherwise it stays on screen and its next action
// answers 404 "Job not found".
func TestDismissJob_BroadcastsRemoval(t *testing.T) {
	hub := NewWSHub()
	m := newTestJobManager(t, Config{}, hub)
	m.jobs["gone"] = &Job{
		ID:        "gone",
		Repo:      "owner/repo",
		Status:    JobStatusFailed,
		CreatedAt: time.Now(),
	}

	if res, _ := m.DismissJobResult("gone"); res != DismissJobOK {
		t.Fatalf("DismissJobResult = %v, want DismissJobOK", res)
	}

	// The hub is deliberately not running: its buffered channel is where the
	// frames would go, so the test can read them directly.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case frame := <-hub.broadcast:
			var msg WSMessage
			if err := json.Unmarshal(frame, &msg); err != nil {
				t.Fatalf("unmarshal WS frame: %v", err)
			}
			if msg.Type != "job_removed" {
				continue
			}
			data, ok := msg.Data.(map[string]any)
			if !ok || data["id"] != "gone" {
				t.Fatalf("job_removed payload = %#v, want id \"gone\"", msg.Data)
			}
			return
		case <-deadline:
			t.Fatal("dismiss did not broadcast a job_removed frame")
		}
	}
}

// TestDismissJob_RejectedDismissalSendsNothing ensures the 409/404 paths stay
// silent: a job that still exists must not be announced as removed.
func TestDismissJob_RejectedDismissalSendsNothing(t *testing.T) {
	hub := NewWSHub()
	m := newTestJobManager(t, Config{}, hub)
	m.jobs["busy"] = &Job{ID: "busy", Status: JobStatusRunning, CreatedAt: time.Now()}

	if res, _ := m.DismissJobResult("busy"); res != DismissJobStillActive {
		t.Fatalf("DismissJobResult = %v, want DismissJobStillActive", res)
	}
	if res, _ := m.DismissJobResult("absent"); res != DismissJobNotFound {
		t.Fatalf("DismissJobResult = %v, want DismissJobNotFound", res)
	}

	select {
	case frame := <-hub.broadcast:
		t.Fatalf("unexpected WS frame for a rejected dismissal: %s", frame)
	case <-time.After(150 * time.Millisecond):
	}
}

// waitForStateWithout waits until the state file no longer contains needle.
// DismissJobResult dispatches its persist instead of blocking the dismiss behind
// it (an in-flight job-state write used to hold up the HTTP request), so the
// write this test asserts has no completion point the caller can join — the file
// is the only observable one. Reading it is safe because the state file is
// replaced by rename, never truncated in place.
func waitForStateWithout(t *testing.T, path, needle string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last []byte
	for {
		if data, err := os.ReadFile(path); err == nil {
			last = data
			if !strings.Contains(string(data), needle) {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never left the state file; last read: %s", needle, last)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestDismissJob_PersistedRemovalSurvivesRestart is the stronger half of the
// #68-secondary guarantee: the dismissed job must be gone from the state file,
// not only from the live map. Without the save it reappears on the next start,
// which is what the user reported as "the job can't be deleted".
func TestDismissJob_PersistedRemovalSurvivesRestart(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "jobs_state.json")

	first := newJobManagerWithStatePath(Config{}, nil, statePath)
	registerTestJobManagerCleanup(t, first)
	first.jobs["gone"] = &Job{ID: "gone", Repo: "owner/repo", Status: JobStatusFailed, CreatedAt: time.Now()}
	first.jobs["kept"] = &Job{ID: "kept", Repo: "owner/other", Status: JobStatusCompleted, CreatedAt: time.Now()}
	if err := first.saveState(); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	if res, _ := first.DismissJobResult("gone"); res != DismissJobOK {
		t.Fatalf("DismissJobResult = %v, want DismissJobOK", res)
	}
	waitForStateWithout(t, statePath, `"gone"`)

	restored := newJobManagerWithStatePath(Config{}, nil, statePath)
	registerTestJobManagerCleanup(t, restored)
	restored.LoadState()

	if _, ok := restored.GetJob("gone"); ok {
		t.Error("dismissed job came back from the state file after restart")
	}
	if _, ok := restored.GetJob("kept"); !ok {
		t.Error("undismissed job was lost from the state file")
	}
}
