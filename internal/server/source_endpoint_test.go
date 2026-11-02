// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/bashrusakh/modelfetch/internal/hubsource"
)

// An endpoint is a base URL that fits exactly one source's API shape. A stale
// endpoint left behind when the source is switched answers every call with an HTML
// error page, so the contradiction is reported before the request goes out.
func TestSourceEndpointMismatchFlagsCrossSourceEndpoint(t *testing.T) {
	msg := sourceEndpointMismatch(Config{Source: "modelscope", Endpoint: "https://hf-mirror.com"})
	if msg == "" {
		t.Fatal("cross-source endpoint not flagged")
	}
	if !strings.Contains(msg, "modelscope") || !strings.Contains(msg, "https://modelscope.cn") {
		t.Fatalf("message must name the source and its default endpoint: %s", msg)
	}

	// Legitimate configurations must not be flagged: per-source defaults, mirrors,
	// and self-hosted instances on hosts no registered source claims.
	for _, cfg := range []Config{
		{},
		{Source: "modelscope"},
		{Source: "modelscope", Endpoint: "https://modelscope.cn"},
		{Source: "modelscope", Endpoint: "https://modelscope.cn:8443"},
		{Source: "hf", Endpoint: "https://hf-mirror.com"},
		{Source: "hf", Endpoint: "http://127.0.0.1:9801"},
	} {
		if msg := sourceEndpointMismatch(cfg); msg != "" {
			t.Errorf("legitimate config flagged: %+v -> %s", cfg, msg)
		}
	}
}

func TestSearchRejectsSourceEndpointMismatchBeforeRequest(t *testing.T) {
	s := newTestServerWithConfig(t, Config{Source: "modelscope", Endpoint: "https://hf-mirror.com"})
	w := cacheRequest(t, s, "GET", "/api/search?q=qwen", "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "https://modelscope.cn") {
		t.Fatalf("body = %s", w.Body.String())
	}
}

// A 400 written inside the withConfig closure cannot abort the handler, so source
// validation must happen before any mutation. Otherwise a rejected value is still
// stored, and the config file disagrees with the response.
func TestUpdateSettingsRejectsBadSourceWithoutMutating(t *testing.T) {
	s := newTestServerWithConfig(t, Config{Source: "modelscope", Endpoint: "https://modelscope.cn"})

	w := cacheRequest(t, s, "POST", "/api/settings", `{"source":"bogus"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d body = %s", w.Code, w.Body.String())
	}
	if got := s.snapshotConfig(); got.Source != "modelscope" {
		t.Fatalf("rejected source stored: %q", got.Source)
	}

	w = cacheRequest(t, s, "POST", "/api/settings", `{"source":"modelscope","endpoint":"https://hf-mirror.com"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d body = %s", w.Code, w.Body.String())
	}
	if got := s.snapshotConfig(); got.Source != "modelscope" || got.Endpoint != "https://modelscope.cn" {
		t.Fatalf("rejected pair stored: source=%q endpoint=%q", got.Source, got.Endpoint)
	}
}

// A hub that serves a single-page app answers unknown API paths with HTML. The
// search handler must turn that into a readable error instead of passing the page
// through, which is what a mismatched source and endpoint look like to the UI.
func TestSearchReportsHTMLResponseAsReadableError(t *testing.T) {
	if !looksLikeHTML([]byte("  \n<!doctype html>\n<html></html>")) {
		t.Fatal("HTML body not detected")
	}
	if looksLikeHTML([]byte(`{"data":{"models":[]}}`)) {
		t.Fatal("JSON body misdetected as HTML")
	}
	if looksLikeHTML(nil) {
		t.Fatal("empty body misdetected as HTML")
	}
}

// The UI's "open in hub" links must follow the selected source instead of always
// pointing at Hugging Face, so the server reports the page URL shape with the repo
// left as a placeholder.
func TestSourcePageTemplatesFollowTheSelectedSource(t *testing.T) {
	tmpl := sourcePageTemplates(hubsource.Resolve("modelscope", ""))
	if tmpl["model"] != "https://modelscope.cn/{repo}" {
		t.Errorf("modelscope model template = %q, want https://modelscope.cn/{repo}", tmpl["model"])
	}
	if tmpl["dataset"] != "https://modelscope.cn/datasets/{repo}" {
		t.Errorf("modelscope dataset template = %q, want https://modelscope.cn/datasets/{repo}", tmpl["dataset"])
	}

	tmpl = sourcePageTemplates(hubsource.Resolve("hf", ""))
	// Page links follow the configured endpoint, and the default endpoint is
	// hf-mirror.com (see hubsource.DefaultEndpoint), not huggingface.co.
	if tmpl["model"] != hubsource.DefaultEndpoint+"/{repo}" || tmpl["dataset"] != hubsource.DefaultEndpoint+"/datasets/{repo}" {
		t.Errorf("hf templates = %+v, want page shapes built from the default endpoint", tmpl)
	}
}

// Search pagination must be reported as an API-relative URL. An upstream cursor is
// opaque and cross-origin, so the browser has to come back through this handler
// instead of fetching the advertised URL directly.
func TestSearchNextURLIsRelativeForCounterSources(t *testing.T) {
	src := hubsource.Resolve("modelscope", "")
	apiURL, err := url.Parse("/api/search?q=qwen&sort=downloads&limit=40&filter=gguf&page=1")
	if err != nil {
		t.Fatal(err)
	}
	empty := &http.Response{Header: http.Header{}}

	// A full page is the only signal that more results exist: neither API exposes
	// a total count.
	if got := searchNextURL(apiURL, src, make([]hubsource.Hit, 40), empty, 1, 40); got != "/api/search?filter=gguf&limit=40&page=2&q=qwen&sort=downloads" {
		t.Errorf("next = %s, want the page-2 URL with the original params preserved", got)
	}
	if got := searchNextURL(apiURL, src, make([]hubsource.Hit, 39), empty, 1, 40); got != "" {
		t.Errorf("short page next = %s, want empty", got)
	}
	if got := searchNextURL(apiURL, src, make([]hubsource.Hit, 40), empty, maxSearchPage, 40); got != "" {
		t.Errorf("capped page next = %s, want empty", got)
	}
}

func TestSearchNextURLFollowsLinkCursorForHF(t *testing.T) {
	src := hubsource.Resolve("hf", "")
	apiURL, err := url.Parse("/api/search?q=gguf&sort=downloads&limit=40&page=1")
	if err != nil {
		t.Fatal(err)
	}

	resp := &http.Response{Header: http.Header{"Link": []string{`<https://huggingface.co/api/models?q=gguf&sort=downloads&limit=40&cursor=abc>; rel="next"`}}}
	// The cursor advances results; the page number is carried so the response still
	// reports which page the client is on.
	if got := searchNextURL(apiURL, src, make([]hubsource.Hit, 40), resp, 1, 40); got != "/api/search?cursor=abc&limit=40&page=2&q=gguf&sort=downloads" {
		t.Errorf("next = %s, want the cursor replayed through this handler with the page counter", got)
	}

	// Absence of rel="next" is exhaustion, not an error.
	if got := searchNextURL(apiURL, src, make([]hubsource.Hit, 40), &http.Response{Header: http.Header{}}, 1, 40); got != "" {
		t.Errorf("no cursor next = %s, want empty", got)
	}

	// A next link that carries no cursor cannot be replayed here; reporting no more
	// results is honest rather than silently wrong.
	resp = &http.Response{Header: http.Header{"Link": []string{`<https://huggingface.co/api/models?limit=40>; rel="next"`}}}
	if got := searchNextURL(apiURL, src, make([]hubsource.Hit, 40), resp, 1, 40); got != "" {
		t.Errorf("cursorless next = %s, want empty", got)
	}
}
