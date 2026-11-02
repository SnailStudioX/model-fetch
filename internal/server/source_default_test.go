// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// ModelFetch starts on ModelScope: it is the hub reachable from the networks this
// project is usually used on, and a first run that lands on huggingface.co answers
// every search with a connection error. A saved config must still win over that
// default, and a legacy config that named only an endpoint must keep the source
// that endpoint implies - otherwise the new default turns a working install into a
// source/endpoint mismatch on every request.
func TestDefaultConfigSelectsModelScope(t *testing.T) {
	if got := DefaultConfig().Source; got != "modelscope" {
		t.Fatalf("DefaultConfig().Source = %q, want modelscope", got)
	}
}

func TestApplyConfigToServerSourcePrecedence(t *testing.T) {
	for _, tc := range []struct{ name, endpoint, fileSource, want string }{
		{"empty file keeps the built-in default", "", "", "modelscope"},
		{"legacy huggingface endpoint", "https://huggingface.co", "", "hf"},
		{"legacy huggingface mirror", "https://hf-mirror.com", "", "hf"},
		{"modelscope endpoint", "https://modelscope.cn", "", "modelscope"},
		{"explicit source beats the endpoint", "https://modelscope.cn", "hf", "hf"},
		{"explicit alias is stored as written", "https://modelscope.cn", "ms", "ms"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateTokenConfig(t)
			if err := SaveConfigFile(&ConfigFile{Endpoint: tc.endpoint, Source: tc.fileSource}); err != nil {
				t.Fatal(err)
			}
			cfg := DefaultConfig()
			if err := ApplyConfigToServer(&cfg); err != nil {
				t.Fatal(err)
			}
			if cfg.Source != tc.want {
				t.Errorf("Source = %q, want %q (endpoint=%q fileSource=%q)", cfg.Source, tc.want, tc.endpoint, tc.fileSource)
			}
			if cfg.Endpoint != tc.endpoint {
				t.Errorf("Endpoint = %q, want the saved %q", cfg.Endpoint, tc.endpoint)
			}
		})
	}
}

// The UI has to tell a stale endpoint left behind by a switch from a mirror the user
// chose, and only the server knows which host belongs to which source. Without this
// table a Hugging Face mirror survives a switch to ModelScope, the settings POST is
// rejected, and the selector reads as one that does nothing.
func TestSettingsReportsSourceHosts(t *testing.T) {
	isolateTokenConfig(t)
	s := New(DefaultConfig())

	w := cacheRequest(t, s, "GET", "/api/settings", "")
	if w.Code != http.StatusOK {
		t.Fatalf("settings status %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		SourceHosts    map[string][]string `json:"source_hosts"`
		SourceDefaults map[string]string   `json:"source_defaults"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{"hf": "hf-mirror.com", "modelscope": "modelscope.cn"} {
		if !slices.Contains(body.SourceHosts[id], want) {
			t.Errorf("source_hosts[%q] = %v, want it to contain %q", id, body.SourceHosts[id], want)
		}
	}
	for id, want := range body.SourceDefaults {
		host := strings.TrimPrefix(want, "https://")
		if !slices.Contains(body.SourceHosts[id], host) {
			t.Errorf("source_hosts[%q] = %v, want it to contain the default host %q", id, body.SourceHosts[id], host)
		}
	}
}

// The source id is fed straight back into the UI pickers, so an accepted alias must
// be stored as the canonical id: "ms" would match no option the selectors offer.
func TestUpdateSettingsStoresCanonicalSourceID(t *testing.T) {
	isolateTokenConfig(t)
	s := New(DefaultConfig())

	w := postTokenSettings(s, `{"source":"ms"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("settings status %d: %s", w.Code, w.Body.String())
	}
	if got := s.snapshotConfig(); got.Source != "modelscope" {
		t.Fatalf("stored source = %q, want modelscope", got.Source)
	}

	w = postTokenSettings(s, `{"source":"huggingface","endpoint":"https://hf-mirror.com"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("settings status %d: %s", w.Code, w.Body.String())
	}
	if got := s.snapshotConfig(); got.Source != "hf" || got.Endpoint != "https://hf-mirror.com" {
		t.Fatalf("stored pair = source %q endpoint %q, want hf and the typed mirror", got.Source, got.Endpoint)
	}
}
