// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// readJSON decoded whatever the client sent, so an arbitrarily large request body
// was an arbitrarily large allocation. The biggest body this API legitimately
// accepts — a settings POST with hub sources, type routes and long paths — is a
// few kilobytes.

func TestBodyLimitCapsTheRequestBody(t *testing.T) {
	s := New(DefaultConfig())

	var readErr error
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, readErr = io.ReadAll(r.Body)
	})

	body := `{"cache-dir":"` + strings.Repeat("x", maxRequestBodyBytes+1) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/settings", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.bodyLimitMiddleware(inner).ServeHTTP(rec, req)

	var tooLarge *http.MaxBytesError
	if !errors.As(readErr, &tooLarge) {
		t.Fatalf("the body was not capped: read error = %v", readErr)
	}
}

// The cap has to leave ordinary requests alone: every handler reads its body
// through readJSON, so a limit that misfires would break the API entirely.
func TestBodyLimitLeavesNormalRequestsAlone(t *testing.T) {
	s := New(DefaultConfig())

	var decoded string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			CacheDir string `json:"cache-dir"`
		}
		if err := readJSON(r, &payload); err != nil {
			t.Errorf("decoding a normal-sized body: %v", err)
			return
		}
		decoded = payload.CacheDir
	})

	body := `{"cache-dir":"` + strings.Repeat("x", 64<<10) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/settings", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.bodyLimitMiddleware(inner).ServeHTTP(rec, req)

	if len(decoded) != 64<<10 {
		t.Fatalf("decoded cache-dir length = %d, want %d", len(decoded), 64<<10)
	}
}
