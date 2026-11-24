// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/SnailStudioX/model-fetch/pkg/hfdownloader"
)

// errInvalidRouteKey is returned when a model request supplies a
// routeKey outside the closed, server-defined key set. Handlers map it to
// HTTP 400 rather than treating the value as a destination path.
var errInvalidRouteKey = errors.New("invalid routeKey")
var errInvalidDestination = errors.New("invalid destination")

// routeKeys is the closed, server-owned set of download-route keys. Keys are
// an internal/advanced-API detail; the dashboard renders human labels for
// them. The set is not user-extensible: any key outside it is rejected on the
// settings and download APIs.
var routeKeys = map[string]bool{
	"llm":             true,
	"llm/gguf":        true,
	"llm/safetensors": true,
	"diffusion":       true,
	"audio":           true,
	"embedding":       true,
}

// isRouteKey reports whether key belongs to the closed route-key set.
func isRouteKey(key string) bool {
	return routeKeys[key]
}

// validatedRouteKey is the shared selector boundary for downloads and previews.
// Datasets ignore selectors; models accept only the closed key set (or no key),
// regardless of whether a destination is currently configured.
func validatedRouteKey(req DownloadRequest) (string, error) {
	if req.Dataset {
		return "", nil
	}
	if req.RouteKey != "" && !isRouteKey(req.RouteKey) {
		return "", errInvalidRouteKey
	}
	return req.RouteKey, nil
}

// looksLikeLMStudioDir reports whether p lives under an LM Studio data
// directory: the modern …\.lmstudio\… layout or the legacy …\.cache\lmstudio\…
// one. The HF cache shape (hub/models--owner--repo/{blobs,refs,snapshots})
// is invisible to LM Studio, which only reads real .gguf files sitting at
// <models>/owner/repo/file.gguf — and on Windows the snapshot entries are
// plain stub files, so a cache-mode download into an LM Studio tree strands
// the model where the app cannot load it. Downloads landing there are
// switched to flat (local) mode instead.
func looksLikeLMStudioDir(p string) bool {
	if p == "" {
		return false
	}
	segs := strings.Split(strings.ToLower(filepath.ToSlash(p)), "/")
	for i, s := range segs {
		if s == ".lmstudio" {
			return true
		}
		if s == ".cache" && i+1 < len(segs) && segs[i+1] == "lmstudio" {
			return true
		}
	}
	return false
}

// lmStudioFlatNote explains the automatic cache→flat switch when a download
// destination sits inside an LM Studio tree.
const lmStudioFlatNote = "LM Studio directory detected: files written flat as <root>/owner/repo/file.gguf instead of the HF cache layout, so LM Studio can load them."

// resolveDownloadDestination is the common destination owner for job creation
// and disk previews. It preserves explicit localDir > route (including parent
// fallback) > global LocalDir > HF cache, with dataset selector bypass. The
// per-request Layout ("cache" | "local", empty = follow global settings) lets
// one download opt out of the server default; "local" without any directory
// is rejected so the job cannot quietly fall back to the cache. Finally the
// LM Studio guard runs last and wins: a destination that resolves into an
// LM Studio tree always goes flat, regardless of how it got selected.
func resolveDownloadDestination(cfg Config, req DownloadRequest) (routeKey, localDir, outputDir, note string, err error) {
	routeKey, err = validatedRouteKey(req)
	if err != nil {
		return "", "", "", "", err
	}
	switch req.Layout {
	case "", "cache", "local":
	default:
		return "", "", "", "", fmt.Errorf("%w: unknown layout %q", errInvalidDestination, req.Layout)
	}

	localDir = cfg.LocalDir
	if req.LocalDir != "" {
		localDir = req.LocalDir
	} else if resolved := resolveRoute(cfg.DownloadRoutes, routeKey); resolved != "" {
		localDir = resolved
	}
	if req.Layout == "cache" {
		// Explicit cache mode for this job: the global local dir and routes
		// are server-wide state, not a per-job choice, so they do not apply.
		// An explicit per-request localDir still counts as opting into flat
		// for this request (it only makes sense as one).
		if req.LocalDir == "" {
			localDir = ""
		}
	}
	if req.Layout == "local" && localDir == "" {
		return "", "", "", "", fmt.Errorf("%w: layout \"local\" requires a local directory", errInvalidDestination)
	}

	outputDir = localDir
	if outputDir == "" {
		outputDir = cfg.cacheRoot()
	}

	// The LM Studio guard: whichever way the destination was chosen, an HF
	// cache landing inside an LM Studio tree is rewritten to flat mode.
	if localDir == "" && looksLikeLMStudioDir(outputDir) {
		localDir = outputDir
		note = lmStudioFlatNote
	}
	return routeKey, localDir, outputDir, note, nil
}

// destinationPreview renders the exact repository folder a request's files
// would land in, for the pre-download confirmation in the UI: flat mode
// appends the repo folder (owner/name, or localRepo when set) under the
// chosen root; cache mode names the physical models--owner--repo directory
// under hub/. An error leaves the preview empty — the same resolve error
// surfaces on job creation.
func destinationPreview(cfg Config, req DownloadRequest) (string, string) {
	_, localDir, _, note, err := resolveDownloadDestination(cfg, req)
	if err != nil {
		return "", note
	}
	folder := req.Repo
	if req.LocalRepo != "" {
		folder = req.LocalRepo
	}
	if localDir != "" {
		if base, err := hfdownloader.DestinationBase(localDir, folder); err == nil {
			return base, note
		}
		return localDir, note
	}
	cache := cfg.cache()
	repoType := hfdownloader.RepoTypeModel
	if req.Dataset {
		repoType = hfdownloader.RepoTypeDataset
	}
	if rd, err := cache.Repo(folder, repoType); err == nil {
		return rd.Path(), note
	}
	return cache.HubDir(), note
}

// parentRouteKey returns the coarse parent of a namespaced route key
// ("llm/gguf" -> "llm"), or "" when the key has no parent. Used for
// most-specific-first resolution.
func parentRouteKey(key string) string {
	i := strings.LastIndex(key, "/")
	if i <= 0 {
		return ""
	}
	return key[:i]
}

// resolveRoute returns the configured destination for a route key using
// most-specific-first fallback: "llm/gguf" tries "llm/gguf" then "llm";
// a top-level key tries only itself. It returns "" when the key is unknown or
// no route (including the parent) is configured. Empty or whitespace-only
// values are ignored; non-empty values are trimmed and cleaned.
func resolveRoute(routes map[string]string, key string) string {
	if len(routes) == 0 || !isRouteKey(key) {
		return ""
	}
	for k := key; k != ""; k = parentRouteKey(k) {
		v, ok := routes[k]
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		return filepath.Clean(v)
	}
	return ""
}

// normalizeDownloadRoutes trims each value, filepath.Cleans it, drops empty
// entries, and returns nil for an empty result. It mirrors cleanPathList
// semantics for a keyed map. Keys are assumed already validated.
func normalizeDownloadRoutes(routes map[string]string) map[string]string {
	if len(routes) == 0 {
		return nil
	}
	out := make(map[string]string, len(routes))
	for key, value := range routes {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		out[key] = filepath.Clean(value)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// sanitizeDownloadRoutes drops every key outside the closed route-key set and
// then applies normalizeDownloadRoutes value semantics (trim, filepath.Clean,
// drop empties). It is the shared boundary filter for route maps so the server
// never holds or advertises a key outside the closed set: a hand-edited or
// legacy config key is silently dropped here rather than surviving in
// cfg.DownloadRoutes and failing later validation. Returns nil when no valid
// non-empty entry remains. Values dropped for an unknown key are never
// interpreted as paths.
func sanitizeDownloadRoutes(routes map[string]string) map[string]string {
	if len(routes) == 0 {
		return nil
	}
	known := make(map[string]string, len(routes))
	for key, value := range routes {
		if !isRouteKey(key) {
			continue
		}
		known[key] = value
	}
	return normalizeDownloadRoutes(known)
}

// routeDirs returns the distinct, non-empty destination paths from a
// download-routes map, cleaned and sorted for a deterministic scan order.
// Only keys in the closed route-key set are considered, so a hand-edited
// config file cannot widen the scan/disk-free allowlist with unknown keys.
// Used to make routed destinations visible to the Cache browser and the
// disk-free allowlist.
func routeDirs(routes map[string]string) []string {
	if len(routes) == 0 {
		return nil
	}
	var dirs []string
	for key, value := range routes {
		if !isRouteKey(key) {
			continue
		}
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		dirs = append(dirs, filepath.Clean(value))
	}
	// Sort before deduplication so case-equivalent Windows spellings have a
	// deterministic representative independent of map iteration order.
	sort.Strings(dirs)
	return cleanPathList(dirs)
}
