// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"fmt"
	"strings"

	"github.com/bashrusakh/modelfetch/internal/hubsource"
)

// HubSource is one saved hub access source: a named (source, endpoint, token)
// triple the user can pick from the Settings tabs and the Models page selector.
//
// The live Config.Source / Config.Endpoint / Config.Token always describe the
// active entry, so every request path keeps reading the three fields it always
// did and no download code has to know the list exists. Switching hubs moves the
// credential with the hub, which is what stops a Hugging Face token from being
// sent to ModelScope (and the other way round).
type HubSource struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Source   string `json:"source"`
	Endpoint string `json:"endpoint"`
	Token    string `json:"token,omitempty"`
}

// DefaultHFHubEndpoint is the Hugging Face endpoint seeded for a fresh install.
// It is the source default rather than a second constant so the seeded tab, the
// endpoint the UI prefills, and the endpoint an empty setting resolves to cannot
// drift apart.
const DefaultHFHubEndpoint = hubsource.DefaultEndpoint

// maxHubSources bounds the saved list so a hand-edited config or a runaway
// client cannot grow the config file without limit.
const maxHubSources = 12

// DefaultHubSources is the list a fresh install starts with: ModelScope first,
// because it is the default source, then the Hugging Face mirror.
//
// The ModelScope entry leaves Endpoint empty, meaning "the source's own default",
// so a fresh install keeps the empty endpoint it has always started with instead
// of pinning a host nobody typed.
func DefaultHubSources() []HubSource {
	return []HubSource{
		{ID: "modelscope", Name: hubsource.DisplayName("modelscope"), Source: "modelscope"},
		{ID: "hf", Name: hubsource.DisplayName("hf"), Source: "hf", Endpoint: DefaultHFHubEndpoint},
	}
}

// entryEndpoint is the base URL an entry actually uses. An empty endpoint means
// the source's own default, which is also what an empty live endpoint means, so
// comparing effective values is what keeps a saved (modelscope,
// https://modelscope.cn) config matched to its seeded tab instead of appending a
// duplicate entry on every load.
func entryEndpoint(entry HubSource) string {
	return hubsource.Resolve(entry.Source, entry.Endpoint).Base()
}

// normalizeHubSource fills the parts a client may leave out and rejects a pair
// that cannot both be true. An endpoint is a base URL that fits exactly one
// source's API shape, so an entry pairing hf with modelscope.cn would answer
// every later call with an HTML error page.
func normalizeHubSource(raw HubSource) (HubSource, error) {
	out := HubSource{
		ID:       strings.TrimSpace(raw.ID),
		Name:     strings.TrimSpace(raw.Name),
		Source:   strings.ToLower(strings.TrimSpace(raw.Source)),
		Endpoint: strings.TrimSuffix(strings.TrimSpace(raw.Endpoint), "/"),
		Token:    raw.Token,
	}
	if !hubsource.Known(out.Source) {
		return HubSource{}, fmt.Errorf("source %q is not a registered hub source (known: %s)", strings.TrimSpace(raw.Source), strings.Join(hubsource.Registered(), ", "))
	}
	// Store the canonical id, the same rule the standalone source field follows:
	// the value is fed back into the pickers and an alias matches no option.
	out.Source = hubsource.Resolve(out.Source, out.Endpoint).ID()
	if out.Name == "" {
		out.Name = hubsource.DisplayName(out.Source)
	}
	if out.ID == "" {
		out.ID = out.Source
	}
	if msg := sourceEndpointMismatch(Config{Source: out.Source, Endpoint: out.Endpoint}); msg != "" {
		return HubSource{}, fmt.Errorf("%s", msg)
	}
	return out, nil
}

// uniqueHubID appends a numeric suffix until the id is free, so two entries that
// both default to "hf" stay individually addressable.
func uniqueHubID(list []HubSource, base string) string {
	candidate := base
	for id := 2; findHubSource(list, candidate) != nil; id++ {
		candidate = fmt.Sprintf("%s-%d", base, id)
	}
	return candidate
}

// sanitizeHubSources normalizes a saved or submitted list. strict rejects the
// first invalid entry so a settings POST cannot half-apply; the lenient path is
// for values already on disk, where one bad entry must not cost the user the
// rest of the list. An empty list becomes the seeded defaults.
func sanitizeHubSources(in []HubSource, strict bool) ([]HubSource, error) {
	if len(in) == 0 {
		return DefaultHubSources(), nil
	}
	if len(in) > maxHubSources {
		if strict {
			return nil, fmt.Errorf("too many hub sources: %d, at most %d", len(in), maxHubSources)
		}
		in = in[:maxHubSources]
	}
	out := make([]HubSource, 0, len(in))
	for _, raw := range in {
		entry, err := normalizeHubSource(raw)
		if err != nil {
			if strict {
				label := firstNonEmptyString(raw.Name, raw.ID, raw.Source)
				return nil, fmt.Errorf("hub source %q: %w", label, err)
			}
			continue
		}
		entry.ID = uniqueHubID(out, entry.ID)
		out = append(out, entry)
	}
	if len(out) == 0 {
		if strict {
			return nil, fmt.Errorf("no usable hub source in the submitted list")
		}
		return DefaultHubSources(), nil
	}
	return out, nil
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// findHubSource returns a copy of the entry with an exact id, so a caller cannot
// write through the pointer into a slice the job manager is also reading.
func findHubSource(list []HubSource, id string) *HubSource {
	if id == "" {
		return nil
	}
	for i := range list {
		if list[i].ID == id {
			entry := list[i]
			return &entry
		}
	}
	return nil
}

// hubSourceIDs lists a saved list's ids in order, for the message that rejects an
// unknown activeHubSource.
func hubSourceIDs(list []HubSource) []string {
	out := make([]string, 0, len(list))
	for _, entry := range list {
		out = append(out, entry.ID)
	}
	return out
}

// findHubSourceByPair returns the entry that already describes a live
// (source, endpoint) pair. A standalone source/endpoint update that matches one
// is a switch to an existing tab, not a rewrite of the current one.
func findHubSourceByPair(list []HubSource, source, endpoint string) *HubSource {
	src := hubsource.Resolve(source, endpoint).ID()
	base := hubsource.Resolve(source, endpoint).Base()
	for i := range list {
		if list[i].Source == src && entryEndpoint(list[i]) == base {
			entry := list[i]
			return &entry
		}
	}
	return nil
}

// resolveActiveHubSource decides which entry the live pair belongs to. An unknown
// stored id falls back to an exact pair match, then - only when the live endpoint
// is empty - to an entry that merely shares the source, and finally to the first
// entry.
//
// It returns "" only when a non-empty live endpoint matches no entry: that pair
// was written by hand or by an older version and must stay reachable, so
// ensureHubSources appends an entry describing it instead of quietly pointing the
// tabs at a different host than the one downloads are actually using.
func resolveActiveHubSource(list []HubSource, activeID, source, endpoint string) string {
	if len(list) == 0 {
		return ""
	}
	if entry := findHubSource(list, activeID); entry != nil {
		return entry.ID
	}
	if entry := findHubSourceByPair(list, source, endpoint); entry != nil {
		return entry.ID
	}
	if strings.TrimSpace(endpoint) != "" {
		return ""
	}
	src := hubsource.Resolve(source, endpoint).ID()
	for _, entry := range list {
		if entry.Source == src {
			return entry.ID
		}
	}
	return list[0].ID
}

// ensureHubSources makes the invariants true for any Config that reached the
// server: a non-empty saved list, an active id that exists, and an active entry
// that describes the live pair. It never rewrites a source or endpoint the user
// chose, except to fill an empty endpoint from an entry that names one.
//
// New calls it so a server built in a test always has a list, and
// handleUpdateSettings calls it after every commit.
func (c *Config) ensureHubSources() {
	list, err := sanitizeHubSources(c.HubSources, false)
	if err != nil {
		list = DefaultHubSources()
	}
	id := resolveActiveHubSource(list, c.ActiveHub, c.Source, c.Endpoint)
	if id == "" {
		// The live pair is not in the list. Describe it as its own entry so the
		// hub downloads actually go through stays visible and editable.
		if entry, err := normalizeHubSource(HubSource{Source: c.Source, Endpoint: c.Endpoint, Token: c.Token}); err == nil {
			entry.ID = uniqueHubID(list, entry.ID)
			list = append(list, entry)
			id = entry.ID
		}
	}
	if id == "" {
		// The live pair is not even a valid entry (a hand-edited contradiction).
		// Every request already reports that; keep the tab that shares its source
		// so the UI has something to show and fix.
		src := hubsource.Resolve(c.Source, c.Endpoint).ID()
		for _, entry := range list {
			if entry.Source == src {
				id = entry.ID
				break
			}
		}
		if id == "" {
			id = list[0].ID
		}
	}
	c.HubSources = list
	c.ActiveHub = id
	if entry := findHubSource(list, id); entry != nil && c.Endpoint == "" && entry.Endpoint != "" {
		c.Endpoint = entry.Endpoint
	}
}

// setHubSourceToken returns a copy of the list with one entry's credential
// replaced. The slice is replaced rather than written in place because its
// backing memory is shared with the job manager's config copy and any in-flight
// job reading it.
func setHubSourceToken(list []HubSource, id, token string) []HubSource {
	out := append([]HubSource(nil), list...)
	for i := range out {
		if out[i].ID == id {
			out[i].Token = token
			break
		}
	}
	return out
}

// setHubSourcePair returns a copy of the list with one entry's source and
// endpoint replaced. That is how a standalone source/endpoint POST edits the tab
// it applies to, which keeps the live pair and the saved tab the same thing.
func setHubSourcePair(list []HubSource, id, source, endpoint string) []HubSource {
	out := append([]HubSource(nil), list...)
	for i := range out {
		if out[i].ID != id {
			continue
		}
		out[i].Source = source
		out[i].Endpoint = endpoint
		if out[i].Name == "" {
			out[i].Name = hubsource.DisplayName(source)
		}
		break
	}
	return out
}

// hubCredentialKnown reports whether a credential is actually in play around a hub
// switch: one stored on an entry before or after the request, or one that the
// active entry had or has. A server whose only credential came from the
// environment or a launch flag has none of these, and there the live value must
// stay put - moving it would drop a credential the config file never held, which
// is a silent loss rather than a hub switch.
func hubCredentialKnown(before, after []HubSource, prevToken, activeToken string) bool {
	return prevToken != "" || activeToken != "" || hubSourcesCarryTokens(before) || hubSourcesCarryTokens(after)
}

// hubSourcesCarryTokens reports whether any entry stores its own credential.
// Once they do, the entries are the source of truth and the legacy top-level
// token must not be re-attached to whichever entry happens to be active.
func hubSourcesCarryTokens(list []HubSource) bool {
	for _, entry := range list {
		if entry.Token != "" {
			return true
		}
	}
	return false
}

// redactedHubSources returns a copy safe for an API response: every credential is
// replaced by the same display mask the top-level token uses.
func redactedHubSources(list []HubSource) []HubSource {
	out := make([]HubSource, 0, len(list))
	for _, entry := range list {
		entry.Token = redactToken(entry.Token)
		out = append(out, entry)
	}
	return out
}

// redactToken is the display form of a credential: never the value, and only
// long enough to tell two tokens apart. The prefix is reserved, so a value that
// starts with it can never be accepted as a credential.
func redactToken(token string) string {
	if token == "" {
		return ""
	}
	status := tokenRedactionPrefix
	if len(token) > 4 {
		status += token[len(token)-4:]
	}
	return status
}

// hubSourceRequestEntry is one submitted hub source. Token is a pointer so
// "leave it alone" (omitted, null, or a stale display mask) is distinguishable
// from "set this" and from "clear it" ("").
type hubSourceRequestEntry struct {
	ID       string  `json:"id,omitempty"`
	Name     string  `json:"name,omitempty"`
	Source   string  `json:"source,omitempty"`
	Endpoint string  `json:"endpoint,omitempty"`
	Token    *string `json:"token,omitempty"`
}

// resolveRequestHubSources normalizes a submitted list and carries each entry's
// stored credential forward unless the client authored a new one. An id the
// server has never seen (a tab the user just added) starts with no credential.
func resolveRequestHubSources(reqEntries []hubSourceRequestEntry, previous []HubSource) ([]HubSource, error) {
	in := make([]HubSource, 0, len(reqEntries))
	for _, raw := range reqEntries {
		entry := HubSource{ID: raw.ID, Name: raw.Name, Source: raw.Source, Endpoint: raw.Endpoint}
		if raw.Token != nil && !isRedactedToken(*raw.Token) {
			entry.Token = *raw.Token
		} else if stored := findHubSource(previous, strings.TrimSpace(raw.ID)); stored != nil {
			entry.Token = stored.Token
		}
		in = append(in, entry)
	}
	return sanitizeHubSources(in, true)
}
