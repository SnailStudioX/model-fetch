// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const lfsOID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// mockTreeHub answers the two endpoints a plan needs. The files it lists are LFS
// entries because filters only narrow those: a repository's small config files are
// always part of a download, so an LFS-only tree is what makes "my filter selected
// nothing" reachable.
func mockTreeHub(t *testing.T, tree string) *httptest.Server {
	t.Helper()
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/revision/main"):
			io.WriteString(w, `{"sha":"deadbeef"}`)
		case strings.Contains(r.URL.Path, "/tree/"):
			io.WriteString(w, tree)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(hub.Close)
	return hub
}

func waitForTerminalJob(t *testing.T, m *JobManager, id string) *Job {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if got, ok := m.GetJob(id); ok {
			if got.Status == JobStatusCompleted || got.Status == JobStatusFailed || got.Status == JobStatusCancelled {
				return got
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s never reached a terminal status", id)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A filtered download that selects nothing moves no bytes, and a job that ends
// `completed` after moving nothing tells the user the download happened. The Jobs
// page, the history and any retry decision read the status, so the empty
// selection has to arrive as a failure that says which pattern matched nothing.
func TestJobWithNoMatchingFilesEndsFailed(t *testing.T) {
	hub := mockTreeHub(t, `[{"type":"file","path":"model-Q4_K_M.gguf","size":100,"lfs":{"oid":"`+lfsOID+`","size":100}}]`)
	m := newLifecycleTestManager(t, Config{CacheDir: t.TempDir(), Endpoint: hub.URL, MaxActive: 1})

	job, _, err := m.CreateJob(DownloadRequest{Repo: "owner/model", Revision: "main", Filters: []string{"Q8_0"}})
	if err != nil {
		t.Fatal(err)
	}

	got := waitForTerminalJob(t, m, job.ID)
	if got.Status != JobStatusFailed {
		t.Fatalf("a selection that matched nothing ended %s (error %q)", got.Status, got.Error)
	}
	if !strings.Contains(got.Error, `"Q8_0"`) || !strings.Contains(got.Error, "1 file") {
		t.Errorf("the failure does not explain itself: %q", got.Error)
	}
}

// The other empty plan is a repository with no files: not the caller's mistake, so
// the job still completes. Where that case is reported is the downloader's warning
// event (see TestDownload_EmptyRepositoryWarnsInsteadOfPassingSilently); this
// pins the boundary, so a later change cannot quietly turn one case into the other.
func TestJobForEmptyRepositoryIsNotAFailure(t *testing.T) {
	hub := mockTreeHub(t, `[]`)
	m := newLifecycleTestManager(t, Config{CacheDir: t.TempDir(), Endpoint: hub.URL, MaxActive: 1})

	job, _, err := m.CreateJob(DownloadRequest{Repo: "owner/empty", Revision: "main"})
	if err != nil {
		t.Fatal(err)
	}

	got := waitForTerminalJob(t, m, job.ID)
	if got.Status != JobStatusCompleted {
		t.Fatalf("an empty repository ended %s (error %q), want the job to stay a no-op", got.Status, got.Error)
	}
}
