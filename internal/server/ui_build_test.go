// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SnailStudioX/model-fetch/internal/assets/webui"
)

// The frontend is one decision point: the built Svelte app when a build is
// embedded, and a clear plain-text answer when it is not, so a Go-only build
// never serves an empty tree that looks like a broken dashboard.
func TestUIFilesystemChoice(t *testing.T) {
	srv := New(DefaultConfig())
	h := srv.uiHandler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	data := rec.Body.Bytes()

	if webui.HasBuild() {
		if rec.Code != http.StatusOK {
			t.Fatalf("GET / with an embedded build = %d, want 200", rec.Code)
		}
		if !bytes.Contains(data, []byte(`id="app"`)) {
			t.Errorf("HasBuild is true but the served index.html is not the built frontend")
		}
		return
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("GET / without a frontend build = %d, want 503", rec.Code)
	}
	if !bytes.Contains(data, []byte("npm run build")) {
		t.Errorf("the no-build answer does not say how to build the frontend: %q", data)
	}
}

// A page left open across a restart must not keep running the previous bundle,
// so index.html is always revalidated while content-hashed build assets are
// cached for a year.
func TestStaticCacheHeaders(t *testing.T) {
	if !webui.HasBuild() {
		t.Skip("no frontend build embedded; cache headers only exist for the built UI")
	}
	srv := New(DefaultConfig())
	srvURL := httptest.NewServer(srv.uiHandler()).URL

	resp, err := http.Get(srvURL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Cache-Control"); got != "no-cache" {
		t.Errorf("index.html Cache-Control = %q, want no-cache", got)
	}

	entries, err := fs.ReadDir(webui.StaticFS(), "assets")
	if err != nil {
		t.Fatalf("read assets dir: %v", err)
	}
	var asset string
	for _, e := range entries {
		if !e.IsDir() {
			asset = "assets/" + e.Name()
			break
		}
	}
	if asset == "" {
		t.Fatal("build is embedded but assets/ is empty")
	}

	resp2, err := http.Get(srvURL + "/" + asset)
	if err != nil {
		t.Fatalf("GET %s: %v", asset, err)
	}
	defer resp2.Body.Close()
	if !strings.Contains(resp2.Header.Get("Cache-Control"), "immutable") {
		t.Errorf("%s Cache-Control = %q, want immutable", asset, resp2.Header.Get("Cache-Control"))
	}
}

// The build ships more than .js and .css. The handler used to choose the type by
// matching path suffixes itself, so every other asset — the icons at the build
// root among them — was labelled text/html, and each request read the whole file
// into memory just to learn whether it existed. Serving through http.FileServer
// also answers Range, Last-Modified and ETag, which that branch could not.
func TestStaticContentTypesComeFromTheFileServer(t *testing.T) {
	if !webui.HasBuild() {
		t.Skip("no frontend build embedded; content types only exist for the built UI")
	}
	srv := New(DefaultConfig())
	srvURL := httptest.NewServer(srv.uiHandler()).URL

	resp, err := http.Get(srvURL + "/apple-touch-icon.png")
	if err != nil {
		t.Fatalf("GET icon: %v", err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Content-Type"); got != "image/png" {
		t.Errorf("icon Content-Type = %q, want image/png", got)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) == 0 {
		t.Fatal("the icon body is empty")
	}

	// A browser asks for a byte range when it resumes or re-validates; a 200 with
	// the whole file in answer to a Range request is what used to happen.
	rangeRequest, err := http.NewRequest(http.MethodGet, srvURL+"/apple-touch-icon.png", nil)
	if err != nil {
		t.Fatal(err)
	}
	rangeRequest.Header.Set("Range", "bytes=0-15")
	partial, err := http.DefaultClient.Do(rangeRequest)
	if err != nil {
		t.Fatalf("ranged GET: %v", err)
	}
	defer partial.Body.Close()
	if partial.StatusCode != http.StatusPartialContent {
		t.Fatalf("ranged GET status = %d, want %d", partial.StatusCode, http.StatusPartialContent)
	}
	got, err := io.ReadAll(partial.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 16 {
		t.Errorf("ranged body = %d bytes, want 16", len(got))
	}
}

// With --port 0 the configured port is 0, so the desktop shell must read the
// address the listener actually bound.
func TestResolvedAddrReportsChosenPort(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Addr = "127.0.0.1"
	cfg.Port = 0
	srv := New(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.ListenAndServe(ctx) }()

	var addr string
	for i := 0; i < 50 && addr == ""; i++ {
		addr = srv.Addr()
		if addr == "" {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if addr == "" {
		t.Fatal("server did not report a bound address")
	}

	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("reported address %q is not host:port", addr)
	}
	if port == "0" {
		t.Errorf("reported address %q still carries the configured port 0", addr)
	}
}
