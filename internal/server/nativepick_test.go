// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// The dialog opens on the machine that runs the server, so a browser somewhere else
// must not be able to drive it. These cases pin the guard itself, not the platform
// helper: no test here may open a real dialog.
func TestIsLoopbackRequest(t *testing.T) {
	cases := []struct {
		remoteAddr string
		want       bool
	}{
		{"127.0.0.1:54321", true},
		{"127.0.0.1", true},
		{"[::1]:8787", true},
		{"::1", true},
		{"10.0.0.5:1234", false},
		{"192.168.1.20:1", false},
		{"8.8.8.8:53", false},
		{"", false},
		{"not-an-ip:1", false},
	}
	for _, tc := range cases {
		req := httptest.NewRequest("POST", "/api/browse/pick", nil)
		req.RemoteAddr = tc.remoteAddr
		if got := isLoopbackRequest(req); got != tc.want {
			t.Errorf("RemoteAddr %q: got %v want %v", tc.remoteAddr, got, tc.want)
		}
	}
}

func TestNativePickRejectsRemoteBrowser(t *testing.T) {
	srv := newTestServerWithConfig(t, Config{CacheDir: t.TempDir()})

	req := httptest.NewRequest("POST", "/api/browse/pick", nil)
	req.RemoteAddr = "10.0.0.5:1234"
	w := httptest.NewRecorder()
	srv.handleNativePick(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status %d want 403: %s", w.Code, w.Body.String())
	}
}

// A headless server (Docker, CI, a bare VM) has no desktop to show a dialog on. The
// kill switch is what makes that state testable, and the answer must be a clean
// "not available" rather than a hung request.
func TestNativePickUnavailableWhenDisabled(t *testing.T) {
	t.Setenv("MODELFETCH_NO_NATIVE_PICK", "1")
	srv := newTestServerWithConfig(t, Config{CacheDir: t.TempDir()})

	req := httptest.NewRequest("POST", "/api/browse/pick", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	w := httptest.NewRecorder()
	srv.handleNativePick(w, req)

	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status %d want 501: %s", w.Code, w.Body.String())
	}
	if nativePickAvailable() {
		t.Fatal("nativePickAvailable should be false while the kill switch is set")
	}
}

// The listing carries the availability flag so the UI can hide a button that cannot
// work, instead of offering one that fails after the click.
func TestBrowseReportsNativePickAvailability(t *testing.T) {
	root := t.TempDir()

	t.Setenv("MODELFETCH_NO_NATIVE_PICK", "1")
	srv := newTestServerWithConfig(t, Config{CacheDir: root})
	w := httptest.NewRecorder()
	srv.handleBrowse(w, httptest.NewRequest("GET", "/api/browse?path="+url.QueryEscape(root), nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var disabled browseResponse
	if err := json.NewDecoder(w.Body).Decode(&disabled); err != nil {
		t.Fatal(err)
	}
	if disabled.NativePick {
		t.Fatal("native_pick should be false while the kill switch is set")
	}

	t.Setenv("MODELFETCH_NO_NATIVE_PICK", "")
	w = httptest.NewRecorder()
	srv.handleBrowse(w, httptest.NewRequest("GET", "/api/browse?path="+url.QueryEscape(root), nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var enabled browseResponse
	if err := json.NewDecoder(w.Body).Decode(&enabled); err != nil {
		t.Fatal(err)
	}
	if enabled.NativePick != nativePickSupported() {
		t.Fatalf("native_pick=%v but this host reports support=%v", enabled.NativePick, nativePickSupported())
	}
}
