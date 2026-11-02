// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hubsource

import (
	"encoding/json"
	"io"
	"net/url"
	"strings"
)

// The search APIs of both hubs report no per-repository logo, so a row's logo is
// resolved per author instead of per hit. These methods describe where each source
// answers that lookup; the caller caches one result per author.
//
// Verified 2026-10-11:
//   - https://hf-mirror.com/api/users/{name}/avatar        -> 200 {"avatarUrl": ...}
//   - https://hf-mirror.com/api/organizations/{name}/avatar -> 200, same shape
//     (a repository author is an organization, so the user route 404s for it)
//   - hf-mirror proxies the image path /avatars/{hash}.svg, and — verified
//     2026-10-23 — serves the cdn-avatars uploads tree on its own
//     cdn-avatars.hf-mirror.com subdomain, so both trees can be pinned to the
//     mirror. Pinning only /avatars/ left most org logos (which use uploaded
//     cdn-avatars URLs) unreachable on networks that cannot reach Hugging Face.
//   - ModelScope has no anonymous per-author avatar route: its internal search
//     POST requires login and /api/v1/organizations/{name} 404s. The anonymous
//     repository detail GET /api/v1/models/{repo} carries Data.Avatar and
//     Data.Organization.Avatar.

// AvatarURLs tries the user route first, then the organization route. Both answer
// with the same shape, so an author that is an organization falls through to the
// second URL rather than failing.
func (h *HF) AvatarURLs(author, sampleRepo string) []string {
	a := url.PathEscape(author)
	return []string{
		h.endpoint + "/api/users/" + a + "/avatar",
		h.endpoint + "/api/organizations/" + a + "/avatar",
	}
}

// ParseAvatar reads {"avatarUrl": ...}. When the endpoint is a mirror, the
// returned URL is on the canonical host, so the /avatars/ tree is pinned back to
// the configured endpoint and the cdn-avatars upload tree to the mirror's own
// cdn-avatars subdomain.
func (h *HF) ParseAvatar(body io.Reader) (string, error) {
	var raw struct {
		AvatarURL string `json:"avatarUrl"`
	}
	if err := json.NewDecoder(body).Decode(&raw); err != nil {
		return "", err
	}
	return h.rewriteAvatarHost(raw.AvatarURL), nil
}

// rewriteAvatarHost pins an image URL to the configured endpoint when the
// endpoint is known to serve that tree: the /avatars/ path lives on the mirror
// host itself, and the cdn-avatars upload tree on the mirror's cdn-avatars
// subdomain (verified for hf-mirror). Every other host stays as the source
// returned it, so an endpoint that cannot serve a tree does not silently break
// an otherwise loadable URL.
func (h *HF) rewriteAvatarHost(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	ep, err := url.Parse(h.endpoint)
	if err != nil || ep.Host == "" || u.Host == ep.Host {
		return raw
	}
	switch {
	case strings.HasPrefix(u.Path, "/avatars/"):
		u.Scheme, u.Host = ep.Scheme, ep.Host
	case u.Host == "cdn-avatars.huggingface.co":
		// On the canonical endpoint this maps back to the same host, so the
		// canonical case needs no special case.
		u.Scheme, u.Host = ep.Scheme, "cdn-avatars."+ep.Host
	}
	return u.String()
}

// AvatarURLs uses the repository detail, the only anonymous route that carries a
// ModelScope logo. sampleRepo must belong to author; without it there is nothing
// to fetch.
func (m *ModelScope) AvatarURLs(author, sampleRepo string) []string {
	if sampleRepo == "" {
		return nil
	}
	return []string{m.endpoint + "/api/v1/models/" + PathEscapeAll(sampleRepo)}
}

// ParseAvatar reads the ModelScope envelope. Organization.Avatar is the logo the
// site shows for a repository author; the model-level Avatar is the fallback.
func (m *ModelScope) ParseAvatar(body io.Reader) (string, error) {
	var env struct {
		Data struct {
			Avatar       string `json:"Avatar"`
			Organization struct {
				Avatar string `json:"Avatar"`
			} `json:"Organization"`
		} `json:"Data"`
	}
	if err := json.NewDecoder(body).Decode(&env); err != nil {
		return "", err
	}
	if env.Data.Organization.Avatar != "" {
		return env.Data.Organization.Avatar, nil
	}
	return env.Data.Avatar, nil
}
