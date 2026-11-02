// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// What a hub answers when it refuses a search is only useful if the caller can
// read it. Hugging Face answers this API's own error shape; ModelScope answers an
// envelope with no "error" key, which used to reach the client as a bare status
// code.

func TestSearchReportsReasonForModelScopeError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, `{"Code":500,"Message":"search backend unavailable","Success":false}`)
	}))
	defer upstream.Close()

	cfg := Config{Source: "modelscope", Endpoint: upstream.URL, ModelsDir: t.TempDir(), CacheDir: t.TempDir()}
	s := newTestServerWithConfig(t, cfg)

	w := httptest.NewRecorder()
	s.handleSearch(w, httptest.NewRequest("GET", "/api/search?q=qwen", nil))

	// The upstream status is kept: a rejected request must not look like a local 500.
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want the upstream status: %s", w.Code, w.Body.String())
	}
	var resp ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("not this API's error shape: %v %s", err, w.Body)
	}
	if resp.ErrorDetail == nil {
		t.Fatalf("no structured error: %s", w.Body)
	}
	if !strings.Contains(resp.ErrorDetail.Details, "search backend unavailable") {
		t.Errorf("the upstream reason was lost: %+v", resp.ErrorDetail)
	}
	if !strings.Contains(resp.ErrorDetail.Details, "modelscope") {
		t.Errorf("the error must name the source it came from: %+v", resp.ErrorDetail)
	}
}

// A hub that speaks this API's error shape explains itself better than a wrapper
// would, and clients already read it. Its body is passed through unchanged.
func TestSearchPassesThroughHubErrorShape(t *testing.T) {
	body := `{"error":"You are sending too many requests, please try again later."}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, body)
	}))
	defer upstream.Close()

	cfg := Config{Endpoint: upstream.URL, ModelsDir: t.TempDir(), CacheDir: t.TempDir()}
	s := newTestServerWithConfig(t, cfg)

	w := httptest.NewRecorder()
	s.handleSearch(w, httptest.NewRequest("GET", "/api/search?q=qwen", nil))

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: %s", w.Code, w.Body.String())
	}
	if strings.TrimSpace(w.Body.String()) != body {
		t.Errorf("hub error was rewritten instead of passed through: %s", w.Body.String())
	}
}

// A long HTML error page has no business being quoted in full in the UI.
func TestSearchErrorExcerptIsBounded(t *testing.T) {
	long := "<html><body>" + strings.Repeat("gateway problem ", 400) + "</body></html>"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
		io.WriteString(w, long)
	}))
	defer upstream.Close()

	cfg := Config{Source: "modelscope", Endpoint: upstream.URL, ModelsDir: t.TempDir(), CacheDir: t.TempDir()}
	s := newTestServerWithConfig(t, cfg)

	w := httptest.NewRecorder()
	s.handleSearch(w, httptest.NewRequest("GET", "/api/search?q=qwen", nil))

	var resp ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("not this API's error shape: %v %s", err, w.Body)
	}
	details := resp.ErrorDetail.Details
	if len(details) > 500 {
		t.Errorf("error details carry %d characters of upstream body", len(details))
	}
	if !strings.Contains(details, "gateway problem") {
		t.Errorf("the excerpt dropped the part that explains it: %q", details)
	}
}

// An order the source does not offer still answers 200 with rows, so the response
// has to name the order the rows actually came in. Otherwise a search that asked
// for trending and got downloads is indistinguishable from one that asked for
// downloads.
func TestSearchReportsSubstitutedSort(t *testing.T) {
	var askedSort, askedPageSize string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only the search call answers for the search; the per-author logo lookups
		// hit this same server and carry no sort.
		if r.URL.Path == "/openapi/v1/models" {
			askedSort = r.URL.Query().Get("sort")
			askedPageSize = r.URL.Query().Get("page_size")
		}
		io.WriteString(w, `{"data":{"models":[{"id":"owner/model-a"}]}}`)
	}))
	defer upstream.Close()

	cfg := Config{Source: "modelscope", Endpoint: upstream.URL, ModelsDir: t.TempDir(), CacheDir: t.TempDir()}
	s := newTestServerWithConfig(t, cfg)

	w := httptest.NewRecorder()
	s.handleSearch(w, httptest.NewRequest("GET", "/api/search?q=model&sort=trendingScore&limit=10", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("search: %d %s", w.Code, w.Body.String())
	}
	// The fallback itself is deliberate - an unsupported order returns zero rows -
	// so only its visibility is in question here.
	if askedSort != "downloads" {
		t.Fatalf("upstream sort = %q, want the substituted order to be requested", askedSort)
	}
	var resp SearchResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Warnings) == 0 {
		t.Fatalf("a substituted order was not reported: %s", w.Body.String())
	}
	joined := strings.Join(resp.Warnings, "\n")
	if !strings.Contains(joined, "trendingScore") || !strings.Contains(joined, "downloads") {
		t.Errorf("warning does not name both what was asked and what was used: %q", joined)
	}
	// A page under the cap is sent as asked; only the capped one is reported.
	if askedPageSize != "10" {
		t.Errorf("upstream page_size = %q, want the requested limit", askedPageSize)
	}
}

// A source that caps a page answers a short page without running out of results.
// Reading that as the end would strand the rest, so the page link has to be judged
// against the size actually requested, and the cap has to be said.
func TestSearchKeepsPaginationAcrossPageCap(t *testing.T) {
	hits := make([]string, 0, 50)
	for i := 0; i < 50; i++ {
		hits = append(hits, fmt.Sprintf(`{"id":"owner/model-%02d"}`, i))
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":{"models":[`+strings.Join(hits, ",")+`]}}`)
	}))
	defer upstream.Close()

	cfg := Config{Source: "modelscope", Endpoint: upstream.URL, ModelsDir: t.TempDir(), CacheDir: t.TempDir()}
	s := newTestServerWithConfig(t, cfg)

	w := httptest.NewRecorder()
	s.handleSearch(w, httptest.NewRequest("GET", "/api/search?q=model&limit=100", nil))

	var resp SearchResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("%v %s", err, w.Body)
	}
	if len(resp.Results) != 50 {
		t.Fatalf("results = %d, want the 50 the source caps a page to", len(resp.Results))
	}
	if resp.NextURL == "" {
		t.Errorf("a full page under the cap reported no next page: %s", w.Body.String())
	}
	joined := strings.Join(resp.Warnings, "\n")
	if !strings.Contains(joined, "50") || !strings.Contains(joined, "100") {
		t.Errorf("the page cap was not reported: %q", joined)
	}
}

// A source that honours everything asked of it must stay quiet.
func TestSearchAddsNoWarningsForHF(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[{"id":"owner/model-a"}]`)
	}))
	defer upstream.Close()

	cfg := Config{Endpoint: upstream.URL, ModelsDir: t.TempDir(), CacheDir: t.TempDir()}
	s := newTestServerWithConfig(t, cfg)

	w := httptest.NewRecorder()
	s.handleSearch(w, httptest.NewRequest("GET", "/api/search?q=model&sort=downloads&limit=10", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("search: %d %s", w.Code, w.Body.String())
	}
	var resp SearchResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Warnings) != 0 {
		t.Errorf("a search that was answered as asked produced warnings: %q", resp.Warnings)
	}
}
