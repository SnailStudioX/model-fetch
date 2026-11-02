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

// hubEntryToken reads one entry's credential out of a list, masked or not.
func hubEntryToken(list []HubSource, id string) string {
	entry := findHubSource(list, id)
	if entry == nil {
		return "<missing:" + id + ">"
	}
	return entry.Token
}

// hubSettingsGET reads the masked hub list and the active id from GET /api/settings.
func hubSettingsGET(t *testing.T, s *Server) ([]HubSource, string) {
	t.Helper()
	w := cacheRequest(t, s, http.MethodGet, "/api/settings", "")
	if w.Code != http.StatusOK {
		t.Fatalf("settings status %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		HubSources      []HubSource `json:"hub_sources"`
		ActiveHubSource string      `json:"active_hub_source"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.HubSources, body.ActiveHubSource
}

// A fresh install has to arrive with the two hubs the user can reach, and the
// Hugging Face one has to point at the mirror: huggingface.co answers nothing on
// the networks this project is used on, so an empty tab means a search that fails
// with a connection error until the user finds the endpoint field.
func TestNewSeedsDefaultHubSources(t *testing.T) {
	isolateTokenConfig(t)
	cfg := New(DefaultConfig()).snapshotConfig()

	if cfg.ActiveHub != "modelscope" {
		t.Errorf("ActiveHub = %q, want modelscope", cfg.ActiveHub)
	}
	ids := make([]string, 0, len(cfg.HubSources))
	for _, entry := range cfg.HubSources {
		ids = append(ids, entry.ID)
	}
	if !slices.Equal(ids, []string{"modelscope", "hf"}) {
		t.Fatalf("hub source ids = %v, want [modelscope hf]", ids)
	}
	hf := findHubSource(cfg.HubSources, "hf")
	if hf == nil || hf.Endpoint != DefaultHFHubEndpoint {
		t.Errorf("hf entry = %+v, want endpoint %s", hf, DefaultHFHubEndpoint)
	}
	// The seeded ModelScope tab means "the source's own default", so a fresh install
	// keeps the empty endpoint it has always started with instead of pinning a host
	// nobody typed.
	if cfg.Endpoint != "" {
		t.Errorf("Endpoint = %q, want the empty value a fresh install always had", cfg.Endpoint)
	}
	if cfg.Source != "modelscope" {
		t.Errorf("Source = %q, want modelscope", cfg.Source)
	}
}

// A config written by an older version keeps its credential at the top level. It
// must land on the entry that describes the live pair, or the first hub switch
// would drop the only credential the install ever had.
func TestApplyConfigToServerAttachesLegacyTokenToActiveHub(t *testing.T) {
	isolateTokenConfig(t)
	if err := SaveConfigFile(&ConfigFile{Token: "hf_legacy_credential", Endpoint: DefaultHFHubEndpoint}); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	if err := ApplyConfigToServer(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.ActiveHub != "hf" {
		t.Errorf("ActiveHub = %q, want the entry for the saved mirror", cfg.ActiveHub)
	}
	if got := hubEntryToken(cfg.HubSources, cfg.ActiveHub); got != "hf_legacy_credential" {
		t.Errorf("active entry token = %q, want the legacy top-level credential", got)
	}
	if cfg.Token != "hf_legacy_credential" {
		t.Errorf("live token = %q, want the legacy credential", cfg.Token)
	}
}

// The credential belongs to the hub, not to the session. Without this, switching
// from a Hugging Face mirror to ModelScope keeps sending the Hugging Face token to
// ModelScope, and the hub that has its own token is used without it.
func TestSwitchHubSourceMovesCredential(t *testing.T) {
	isolateTokenConfig(t)
	s := New(DefaultConfig())

	requireTokenSettingsOK(t, s, `{"token":"hf_tab_credential"}`)
	requireTokenState(t, s, "hf_tab_credential", "hf_tab_credential")

	entries, active := hubSettingsGET(t, s)
	if active != "modelscope" {
		t.Fatalf("active_hub_source = %q, want modelscope", active)
	}
	if got := hubEntryToken(entries, "modelscope"); got != redactToken("hf_tab_credential") {
		t.Errorf("active entry token = %q, want the display mask", got)
	}

	requireTokenSettingsOK(t, s, `{"activeHubSource":"hf"}`)
	requireTokenState(t, s, "", "")
	live := s.snapshotConfig()
	if live.Source != "hf" || live.Endpoint != DefaultHFHubEndpoint {
		t.Errorf("live pair = source %q endpoint %q, want the selected tab's pair", live.Source, live.Endpoint)
	}
	entries, active = hubSettingsGET(t, s)
	if active != "hf" {
		t.Errorf("active_hub_source = %q, want hf", active)
	}
	if got := hubEntryToken(entries, "modelscope"); got != redactToken("hf_tab_credential") {
		t.Errorf("the tab left behind lost its credential: %q", got)
	}
	if got := hubEntryToken(entries, "hf"); got != "" {
		t.Errorf("hf entry token = %q, want no credential of its own", got)
	}

	// Switching back brings the credential with the hub.
	requireTokenSettingsOK(t, s, `{"activeHubSource":"modelscope"}`)
	requireTokenState(t, s, "hf_tab_credential", "hf_tab_credential")
}

// Multiple hubs with different endpoints and credentials is the point of the
// feature: the list, the active id and each entry's credential all have to survive
// a settings POST and a server restart.
func TestHubSourcesRoundTripThroughConfigFile(t *testing.T) {
	isolateTokenConfig(t)
	s := New(DefaultConfig())

	const body = `{"hubSources":[` +
		`{"id":"modelscope","name":"ModelScope","source":"modelscope","endpoint":"https://modelscope.cn"},` +
		`{"id":"hf","name":"HF mirror","source":"hf","endpoint":"https://hf-mirror.com","token":"hf_mirror_credential"},` +
		`{"id":"hf-2","name":"Office mirror","source":"hf","endpoint":"https://hf.internal.example","token":"office_credential"}]}`
	requireTokenSettingsOK(t, s, body)
	requireTokenSettingsOK(t, s, `{"activeHubSource":"hf-2"}`)

	live := s.snapshotConfig()
	if live.ActiveHub != "hf-2" {
		t.Errorf("ActiveHub = %q, want hf-2", live.ActiveHub)
	}
	if live.Source != "hf" || live.Endpoint != "https://hf.internal.example" {
		t.Errorf("live pair = source %q endpoint %q, want the active tab's pair", live.Source, live.Endpoint)
	}
	if live.Token != "office_credential" {
		t.Errorf("live token = %q, want the active tab's credential", live.Token)
	}

	file, err := LoadConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	if file.ActiveHub != "hf-2" || len(file.HubSources) != 3 {
		t.Fatalf("stored list = %d entries active %q, want 3 and hf-2", len(file.HubSources), file.ActiveHub)
	}
	if got := hubEntryToken(file.HubSources, "hf"); got != "hf_mirror_credential" {
		t.Errorf("stored hf token = %q, want the credential saved for that tab", got)
	}
	if file.Token != "office_credential" {
		t.Errorf("stored top-level token = %q, want it to mirror the live credential", file.Token)
	}

	// A restart reads the same list back, so the tabs and the Models selector show
	// the hubs the user saved rather than the seeded pair.
	restarted := DefaultConfig()
	if err := ApplyConfigToServer(&restarted); err != nil {
		t.Fatal(err)
	}
	if restarted.ActiveHub != "hf-2" || len(restarted.HubSources) != 3 {
		t.Fatalf("restarted list = %d entries active %q, want 3 and hf-2", len(restarted.HubSources), restarted.ActiveHub)
	}
	if restarted.Source != "hf" || restarted.Endpoint != "https://hf.internal.example" || restarted.Token != "office_credential" {
		t.Errorf("restarted live triple = source %q endpoint %q token %q, want the saved active tab", restarted.Source, restarted.Endpoint, restarted.Token)
	}
}

// The UI echoes the display mask back on every save, so a mask must never become a
// credential, an omitted token field must preserve the stored one, and only an
// explicit empty string may clear it.
func TestHubSourcesPreserveStoredTokens(t *testing.T) {
	isolateTokenConfig(t)
	s := New(DefaultConfig())

	requireTokenSettingsOK(t, s, `{"hubSources":[{"id":"modelscope","source":"modelscope"},{"id":"hf","source":"hf","endpoint":"https://hf-mirror.com","token":"hf_stored_credential"}]}`)
	if got := hubEntryToken(s.snapshotConfig().HubSources, "hf"); got != "hf_stored_credential" {
		t.Fatalf("stored hf token = %q, want the authored credential", got)
	}

	requireTokenSettingsOK(t, s, `{"hubSources":[{"id":"modelscope","source":"modelscope","token":"********1234"},{"id":"hf","source":"hf","endpoint":"https://hf-mirror.com","token":"********dential"}]}`)
	live := s.snapshotConfig()
	if got := hubEntryToken(live.HubSources, "hf"); got != "hf_stored_credential" {
		t.Errorf("hf token after echoing the mask = %q, want the stored credential", got)
	}
	if got := hubEntryToken(live.HubSources, "modelscope"); got != "" {
		t.Errorf("modelscope token after echoing the mask = %q, want it to stay empty", got)
	}
	if live.Token != "hf_stored_credential" && live.ActiveHub == "hf" {
		t.Errorf("live token = %q, want the active tab's credential", live.Token)
	}

	requireTokenSettingsOK(t, s, `{"hubSources":[{"id":"modelscope","source":"modelscope"},{"id":"hf","source":"hf","endpoint":"https://hf-mirror.com","token":""}]}`)
	if got := hubEntryToken(s.snapshotConfig().HubSources, "hf"); got != "" {
		t.Errorf("hf token after an explicit clear = %q, want empty", got)
	}

	file, err := LoadConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range file.HubSources {
		if isRedactedToken(entry.Token) {
			t.Errorf("stored entry %q keeps a display mask as its credential: %q", entry.ID, entry.Token)
		}
	}
}

// A hub list is only useful if a bad entry cannot half-apply: the endpoint of one
// tab decides which API shape every call uses, so a rejected POST must leave the
// live pair and the saved list exactly as they were.
func TestHubSourcesRejectInvalidInput(t *testing.T) {
	isolateTokenConfig(t)
	s := New(DefaultConfig())

	for _, tc := range []struct{ name, body, want string }{
		{"unknown source", `{"hubSources":[{"id":"git","source":"gitlab"}]}`, "gitlab"},
		{"endpoint of another source", `{"hubSources":[{"id":"hf","source":"hf","endpoint":"https://modelscope.cn"}]}`, "modelscope.cn"},
		{"unknown active hub", `{"activeHubSource":"does-not-exist"}`, "does-not-exist"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := postTokenSettings(s, tc.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400: %s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tc.want) {
				t.Errorf("body %q does not name %q", w.Body.String(), tc.want)
			}
		})
	}

	live := s.snapshotConfig()
	if live.Source != "modelscope" || live.ActiveHub != "modelscope" || len(live.HubSources) != 2 {
		t.Errorf("a rejected POST changed the live state: source %q active %q list %d", live.Source, live.ActiveHub, len(live.HubSources))
	}
}

// GET /api/settings carries the whole saved list, so every entry's credential has
// to be masked the same way the top-level token is.
func TestSettingsNeverLeaksHubSourceTokens(t *testing.T) {
	isolateTokenConfig(t)
	s := New(DefaultConfig())
	requireTokenSettingsOK(t, s, `{"hubSources":[{"id":"modelscope","source":"modelscope","token":"modelscope_secret_value"},{"id":"hf","source":"hf","endpoint":"https://hf-mirror.com","token":"hf_secret_value"}]}`)

	w := cacheRequest(t, s, http.MethodGet, "/api/settings", "")
	if w.Code != http.StatusOK {
		t.Fatalf("settings status %d: %s", w.Code, w.Body.String())
	}
	for _, secret := range []string{"modelscope_secret_value", "hf_secret_value"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Errorf("GET /api/settings leaked %q", secret)
		}
	}
	entries, _ := hubSettingsGET(t, s)
	for _, entry := range entries {
		if entry.Token != "" && !isRedactedToken(entry.Token) {
			t.Errorf("entry %q token = %q, want a display mask", entry.ID, entry.Token)
		}
	}
}

// A standalone source/endpoint POST is what the Models selector and older clients
// send. It has to land on the tab that already describes that pair instead of
// leaving the tabs describing a hub the downloads are not using.
func TestStandaloneSourceSwitchSelectsExistingHubTab(t *testing.T) {
	isolateTokenConfig(t)
	s := New(DefaultConfig())
	requireTokenSettingsOK(t, s, `{"hubSources":[{"id":"modelscope","source":"modelscope"},{"id":"hf","name":"HF mirror","source":"hf","endpoint":"https://hf-mirror.com","token":"hf_mirror_credential"}]}`)

	requireTokenSettingsOK(t, s, `{"source":"huggingface","endpoint":"https://hf-mirror.com"}`)
	live := s.snapshotConfig()
	if live.ActiveHub != "hf" {
		t.Errorf("ActiveHub = %q, want the tab that already describes the pair", live.ActiveHub)
	}
	if live.Token != "hf_mirror_credential" {
		t.Errorf("live token = %q, want the credential saved for that tab", live.Token)
	}

	// A pair no tab describes edits the tab it applies to, so the tabs stay a
	// description of what the server is really doing.
	requireTokenSettingsOK(t, s, `{"endpoint":"https://hf.internal.example"}`)
	live = s.snapshotConfig()
	entry := findHubSource(live.HubSources, live.ActiveHub)
	if entry == nil || entry.Endpoint != "https://hf.internal.example" {
		t.Errorf("active entry = %+v, want the edited endpoint", entry)
	}
}
