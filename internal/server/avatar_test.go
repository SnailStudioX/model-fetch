// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/bashrusakh/modelfetch/internal/hubsource"
)

// One lookup per unique author, cached across searches, and only successes stored:
// a search must never fail because a logo lookup failed, and a transient failure
// must be retried rather than remembered as "no logo".
func TestResolveAvatarsCachesPerAuthor(t *testing.T) {
	var mu sync.Mutex
	requests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		switch r.URL.Path {
		case "/api/users/manyeyes/avatar":
			io.WriteString(w, `{"avatarUrl":"https://huggingface.co/avatars/29a7ce.svg"}`)
		case "/api/organizations/iic/avatar":
			io.WriteString(w, `{"avatarUrl":"https://huggingface.co/avatars/org.svg"}`)
		default:
			// A repository author that is an organization 404s on the user route,
			// and an account that never set a logo 404s on both.
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	cfg := Config{Endpoint: upstream.URL, ModelsDir: t.TempDir(), CacheDir: t.TempDir()}
	s := newTestServerWithConfig(t, cfg)
	src := hubsource.Resolve(cfg.Source, cfg.Endpoint)

	hits := []hubsource.Hit{
		{ID: "manyeyes/model-a", Author: "manyeyes"},
		{ID: "manyeyes/model-b"},               // no author reported: derived from the id prefix
		{ID: "iic/model-c", Author: "iic"},     // user route 404s, organization route answers
		{ID: "ghost/model-d", Author: "ghost"}, // no logo: empty, and not cached
	}

	got := s.resolveAvatars(context.Background(), cfg, src, hits)
	if got["manyeyes"] != upstream.URL+"/avatars/29a7ce.svg" {
		t.Errorf("mirror host must be pinned for the proxied /avatars/ tree: got %q", got["manyeyes"])
	}
	if got["iic"] != upstream.URL+"/avatars/org.svg" {
		t.Errorf("organization fallback: got %q", got["iic"])
	}
	if got["ghost"] != "" {
		t.Errorf("missing logo must stay empty, not error: got %q", got["ghost"])
	}

	// manyeyes once, iic twice (user 404 then organization), ghost twice (both 404).
	if requests != 5 {
		t.Fatalf("first search made %d upstream requests, want 5", requests)
	}

	before := requests
	again := s.resolveAvatars(context.Background(), cfg, src, hits)
	if again["manyeyes"] != got["manyeyes"] || again["iic"] != got["iic"] {
		t.Errorf("cached authors changed between searches: %+v", again)
	}
	// Only the uncached author refetches (its two routes): a 404 here can be a
	// transient upstream failure, so it is not remembered as "no logo".
	if requests-before != 2 {
		t.Errorf("second search made %d new requests, want 2 for the uncached author only", requests-before)
	}

	// A search that touches only a cached author makes no upstream request at all.
	before = requests
	third := s.resolveAvatars(context.Background(), cfg, src, hits[:1])
	if third["manyeyes"] != got["manyeyes"] {
		t.Errorf("cache lost across a partial hit list: %+v", third)
	}
	if requests != before {
		t.Errorf("cached author refetched on a partial hit list: %d new requests", requests-before)
	}

	if s.resolveAvatars(context.Background(), cfg, src, nil) != nil {
		t.Error("no hits must return nothing, not an empty map")
	}
}

// The search response carries the logo per row, so the browser does not need its
// own lookup, and rows without one keep the initials placeholder.
func TestSearchResponseCarriesAvatar(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/models":
			// The Hub search API reports no author unless full=true, so the row's
			// author comes from the repository id prefix.
			io.WriteString(w, `[{"id":"manyeyes/model-a"},{"id":"ghost/model-b"}]`)
		case "/api/users/manyeyes/avatar":
			io.WriteString(w, `{"avatarUrl":"https://huggingface.co/avatars/29a7ce.svg"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	cfg := Config{Endpoint: upstream.URL, ModelsDir: t.TempDir(), CacheDir: t.TempDir()}
	s := newTestServerWithConfig(t, cfg)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/search?q=model", nil)
	s.handleSearch(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("search: %d %s", w.Code, w.Body.String())
	}
	var resp SearchResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 2 {
		t.Fatalf("results: %s", w.Body.String())
	}
	if resp.Results[0].Avatar != upstream.URL+"/avatars/29a7ce.svg" {
		t.Errorf("logo missing from search response: %+v", resp.Results[0])
	}
	if resp.Results[1].Avatar != "" {
		t.Errorf("unresolved author must report no logo: %+v", resp.Results[1])
	}
}

// ModelScope has no anonymous per-author route, so its logo comes from the
// repository detail.
func TestResolveAvatarsModelScopeDetailRoute(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/models/iic/model-a" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, `{"Data":{"Name":"iic/model-a","Organization":{"Name":"iic","Avatar":"https://resouces.modelscope.cn/avatar/86fc4b9c.png"}}}`)
	}))
	defer upstream.Close()

	cfg := Config{Source: "modelscope", Endpoint: upstream.URL, ModelsDir: t.TempDir(), CacheDir: t.TempDir()}
	s := newTestServerWithConfig(t, cfg)
	src := hubsource.Resolve(cfg.Source, cfg.Endpoint)

	got := s.resolveAvatars(context.Background(), cfg, src, []hubsource.Hit{{ID: "iic/model-a", Author: "iic"}})
	if got["iic"] != "https://resouces.modelscope.cn/avatar/86fc4b9c.png" {
		t.Errorf("modelscope logo: got %q", got["iic"])
	}
}
