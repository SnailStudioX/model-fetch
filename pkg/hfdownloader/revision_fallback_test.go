// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// requestLog records the paths a mock hub was asked for. The scan issues
// requests from more than one goroutine, so the log is mutex-guarded.
type requestLog struct {
	mu    sync.Mutex
	paths []string
}

func (l *requestLog) add(path string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.paths = append(l.paths, path)
}

func (l *requestLog) count(match string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, path := range l.paths {
		if strings.Contains(path, match) {
			n++
		}
	}
	return n
}

// revisionHub serves exactly one revision. Repo-info and tree requests for any
// other revision answer 404, which is how a hub answers for a branch it never
// had, and the known revision serves one small file.
func revisionHub(t *testing.T, known string, body []byte) (*httptest.Server, *requestLog) {
	t.Helper()
	log := &requestLog{}
	files := []hfNode{{Type: "file", Path: "model.bin", Size: int64(len(body))}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.add(r.URL.Path)
		switch {
		case strings.Contains(r.URL.Path, "/revision/"+known):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(RepoInfo{SHA: "resolvedcommit"})
		case strings.Contains(r.URL.Path, "/revision/"):
			http.NotFound(w, r)
		case strings.Contains(r.URL.Path, "/tree/"+known):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(files)
		case strings.Contains(r.URL.Path, "/tree/"):
			http.NotFound(w, r)
		case strings.Contains(r.URL.Path, "/resolve/"), strings.Contains(r.URL.Path, "/raw/"):
			w.Header().Set("Content-Length", fmt.Sprint(len(body)))
			if r.Method != http.MethodHead {
				_, _ = w.Write(body)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, log
}

// TestPlanRepo_FallsBackToHubDefaultRevision is the regression test for the
// retry loop the user hit: a job created while ModelScope was selected stores
// "master", the hub source was later switched to Hugging Face, and every retry
// died on "tree API failed: 404 Not Found". The scan must re-read the same
// repository under this hub's default branch instead of failing.
func TestPlanRepo_FallsBackToHubDefaultRevision(t *testing.T) {
	body := []byte("payload")
	srv, log := revisionHub(t, "main", body)

	plan, err := PlanRepo(context.Background(), Job{Repo: "owner/repo", Revision: "master"}, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("PlanRepo with a revision from another hub: %v", err)
	}
	if plan.Revision != "main" {
		t.Errorf("plan.Revision = %q, want the hf default the files were read from", plan.Revision)
	}
	if len(plan.Items) != 1 || plan.Items[0].RelativePath != "model.bin" {
		t.Fatalf("plan items = %+v, want the single file listed under main", plan.Items)
	}
	if n := log.count("/tree/master"); n != 1 {
		t.Errorf("tree requests for the missing revision = %d, want exactly 1", n)
	}
	if n := log.count("/tree/main"); n != 1 {
		t.Errorf("tree requests for the fallback revision = %d, want exactly 1", n)
	}
}

// TestDownload_FallsBackToHubDefaultRevision drives the same recovery through
// the real download path, where the resolved revision also decides the cache
// ref that gets written.
func TestDownload_FallsBackToHubDefaultRevision(t *testing.T) {
	body := []byte("payload")
	srv, log := revisionHub(t, "main", body)

	cacheRoot := filepath.Join(t.TempDir(), "cache")
	cfg := Settings{CacheDir: cacheRoot, Endpoint: srv.URL, Concurrency: 1}
	if err := Download(context.Background(), Job{Repo: "owner/repo", Revision: "master"}, cfg, nil); err != nil {
		t.Fatalf("Download with a revision from another hub: %v", err)
	}
	if n := log.count("/tree/main"); n != 1 {
		t.Errorf("fallback tree requests = %d, want 1", n)
	}
	for _, path := range []string{"/raw/master/", "/resolve/master/"} {
		if n := log.count(path); n != 0 {
			t.Errorf("file requests under the missing revision (%s) = %d, want 0", path, n)
		}
	}

	hfCache, err := cfg.BuildHFCache()
	if err != nil {
		t.Fatalf("BuildHFCache: %v", err)
	}
	repoDir, err := hfCache.Repo("owner/repo", RepoTypeModel)
	if err != nil {
		t.Fatalf("Repo: %v", err)
	}
	for _, ref := range []string{"main", "master"} {
		commit, err := repoDir.ReadRef(ref)
		if err != nil {
			t.Fatalf("ReadRef(%q): %v", ref, err)
		}
		want := "resolvedcommit"
		if ref != "main" {
			want = ""
		}
		if commit != want {
			t.Errorf("refs/%s = %q, want %q", ref, commit, want)
		}
	}
}

// TestPlanRepo_UserChosenRevisionIsNotReplaced pins the other half of the rule:
// a branch, tag or commit the user selected is never swapped for the hub
// default, because that would silently download different content. The failure
// must instead name the hub and the revision that were tried.
func TestPlanRepo_UserChosenRevisionIsNotReplaced(t *testing.T) {
	body := []byte("payload")
	srv, log := revisionHub(t, "main", body)

	_, err := PlanRepo(context.Background(), Job{Repo: "owner/repo", Revision: "release-2026-01"}, Settings{Endpoint: srv.URL})
	if err == nil {
		t.Fatal("PlanRepo accepted a revision the hub does not have")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("errors.Is(err, ErrNotFound) = false for a 404 tree response: %v", err)
	}
	if !strings.Contains(err.Error(), "tree API failed: 404 Not Found") {
		t.Errorf("error lost the historical status message: %v", err)
	}
	if strings.Contains(err.Error(), "also tried") {
		t.Errorf("error suggests a fallback that was never attempted: %v", err)
	}
	if !strings.Contains(err.Error(), `"release-2026-01"`) || !strings.Contains(err.Error(), srv.URL) {
		t.Errorf("error does not name the revision and hub that failed: %v", err)
	}
	if n := log.count("/tree/main"); n != 0 {
		t.Errorf("fallback requests = %d, want 0 for a user-chosen revision", n)
	}
}

// TestPlanRepo_RevisionFallbackFailureNamesBothRevisions keeps the message
// useful when neither revision exists, which is the "the repository is not on
// this hub" case rather than the cross-hub default case.
func TestPlanRepo_RevisionFallbackFailureNamesBothRevisions(t *testing.T) {
	body := []byte("payload")
	srv, log := revisionHub(t, "only-on-some-other-hub", body)

	_, err := PlanRepo(context.Background(), Job{Repo: "owner/repo", Revision: "master"}, Settings{Endpoint: srv.URL})
	if err == nil {
		t.Fatal("PlanRepo accepted a repository that does not exist on this hub")
	}
	if !strings.Contains(err.Error(), `at revision "master"`) || !strings.Contains(err.Error(), `also tried "main"`) {
		t.Errorf("error does not name both attempted revisions: %v", err)
	}
	if n := log.count("/tree/master"); n != 1 {
		t.Errorf("tree requests for master = %d, want 1", n)
	}
	if n := log.count("/tree/main"); n != 1 {
		t.Errorf("tree requests for main = %d, want 1", n)
	}
}

// TestScanPlan_EventsCarryResolvedRevision keeps the dry-run preview honest:
// the files it lists came from the fallback revision, not the requested one.
func TestScanPlan_EventsCarryResolvedRevision(t *testing.T) {
	body := []byte("payload")
	srv, _ := revisionHub(t, "main", body)

	var revisions []string
	err := ScanPlan(context.Background(), Job{Repo: "owner/repo", Revision: "master"}, Settings{Endpoint: srv.URL},
		func(evt ProgressEvent) {
			if evt.Event == "plan_item" {
				revisions = append(revisions, evt.Revision)
			}
		})
	if err != nil {
		t.Fatalf("ScanPlan: %v", err)
	}
	if len(revisions) != 1 {
		t.Fatalf("plan_item events = %d, want 1", len(revisions))
	}
	if revisions[0] != "main" {
		t.Errorf("plan_item revision = %q, want the revision the files were read from", revisions[0])
	}
}
