// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

// Package hubsource abstracts the API shape of a model hub.
//
// pkg/hfdownloader and pkg/smartdl were written against the Hugging Face Hub
// shape: URL layout, JSON field names, pagination, and which capabilities the
// server actually supports. A Source is exactly the set of things that differ
// between hubs.
//
// The boundary is deliberate: a Source answers "what URL do I fetch" and "how
// do I read this body". It does not perform HTTP. Status handling, retries,
// response passthrough, cancellation, and authentication headers stay with the
// callers, so migrating to a Source does not change any observable behavior for
// the Hugging Face source.
//
// Two things are intentionally NOT part of a Source:
//   - error text. pkg/hfdownloader and pkg/smartdl have historically produced
//     different 401/403/404 messages, and callers keep their own StatusErr and
//     DecodeErr closures. A Source only supplies the URL those messages quote.
//   - authentication. Callers attach the Bearer header. Callers must resolve the
//     token from the Source so a token issued for one hub is never sent to
//     another host.
package hubsource

import (
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/SnailStudioX/model-fetch/internal/hubtree"
)

// DefaultEndpoint is the Hugging Face endpoint used when settings leave the
// endpoint empty. It is hf-mirror.com rather than huggingface.co because the
// mirror answers the same Hugging Face API shape and is reachable from the
// networks this project is usually used on. huggingface.co stays a host the hf
// source claims, so a config that names it explicitly keeps working.
// pkg/hfdownloader.DefaultEndpoint and pkg/smartdl alias this value.
const DefaultEndpoint = "https://hf-mirror.com"

// NormalizeEndpoint trims a trailing slash and falls back to the default.
func NormalizeEndpoint(endpoint string) string {
	if endpoint == "" {
		return DefaultEndpoint
	}
	return strings.TrimSuffix(endpoint, "/")
}

// PathEscapeAll escapes each path segment while keeping literal separators.
// Repository ids are not escaped because the Hub requires literal slashes there;
// this helper is for file and directory paths only.
func PathEscapeAll(p string) string {
	segs := strings.Split(p, "/")
	for i := range segs {
		segs[i] = url.PathEscape(segs[i])
	}
	return strings.Join(segs, "/")
}

// RepoInfo is repository metadata for a revision. pkg/hfdownloader.RepoInfo
// aliases this type, so the public JSON contract is unchanged.
type RepoInfo struct {
	SHA          string `json:"sha"`          // Commit hash
	LastModified string `json:"lastModified"` // ISO timestamp
}

// Ref is a branch or tag. pkg/smartdl.RepoRef keeps its own shape; callers map.
type Ref struct {
	Name         string `json:"name"`
	Ref          string `json:"ref"`
	Type         string `json:"type"` // "branch" or "tag"
	TargetCommit string `json:"targetCommit"`
}

// Hit is one normalized search result.
type Hit struct {
	ID            string    `json:"id"`
	Author        string    `json:"author,omitempty"`
	Downloads     int64     `json:"downloads"`
	Likes         int       `json:"likes"`
	TrendingScore float64   `json:"trendingScore,omitempty"`
	Private       bool      `json:"private,omitempty"`
	Gated         bool      `json:"gated,omitempty"`
	PipelineTag   string    `json:"pipelineTag,omitempty"`
	LibraryName   string    `json:"libraryName,omitempty"`
	Tags          []string  `json:"tags,omitempty"`
	LastModified  time.Time `json:"lastModified,omitempty"`
	CreatedAt     time.Time `json:"createdAt,omitempty"`
}

// SearchQuery is the normalized search request. Each source maps it to its own
// parameter names; Sort is already resolved by the caller.
//
// Page is 1-based. Cursor is an opaque token used only by sources that paginate
// through a cursor (Hugging Face); counter sources ignore it, and cursor sources
// ignore Page. Each source names its own parameter, so SearchURL must not emit a
// page parameter for cursor sources.
type SearchQuery struct {
	Q      string
	Kind   string // "models" or "datasets"
	Sort   string
	Limit  int
	Filter string
	Page   int
	Cursor string
}

// SearchLimits is an optional interface for a source whose search API does not
// answer a request exactly as asked. SearchURL applies the substitution itself;
// a caller asks the same two questions so it can say in the response what the
// source actually did, rather than letting a replaced order or a shortened page
// look like the user's own choice. A source that omits this interface honours
// both, which is the case for Hugging Face.
type SearchLimits interface {
	// SearchSortFor returns the order the source will really apply to a request
	// for the given sort. It returns the argument unchanged when the source can
	// honour it, including when it only spells the same order differently.
	SearchSortFor(requested string) string

	// SearchPageSize returns the page size the source will really request for a
	// requested limit. A page shorter than this is not the end of the results.
	SearchPageSize(requested int) int
}

// ModelDetail is the repository card metadata the analyzer needs. Hugging Face
// serves it from /api/models/{repo}; ModelScope wraps it in an envelope and does
// not expose cardData, so callers must treat missing fields as normal.
type ModelDetail struct {
	ID       string                 `json:"id"`
	Tags     []string               `json:"tags"`
	CardData map[string]interface{} `json:"cardData"`
}

// Source is the per-hub API shape.
type Source interface {
	// ID is the stable source key used by settings, manifests and API responses.
	ID() string

	// Base is the normalized endpoint (no trailing slash).
	Base() string

	// DefaultRevision is the revision used when a job does not name one.
	// Hugging Face uses "main"; ModelScope uses "master".
	DefaultRevision() string

	// URL builders. They take a repository id in "owner/name" form and never
	// escape the separator.
	TreeURL(repo string, isDataset bool, revision, prefix string) string
	FileURL(repo string, isDataset bool, revision, path string) string
	DownloadURL(repo string, isDataset bool, revision, path string) string
	RepoPageURL(repo string, isDataset bool) string
	RepoInfoURL(repo string, isDataset bool, revision string) string
	RefsURL(repo string, isDataset bool) string
	SearchURL(q SearchQuery) string
	// SearchPagination reports how the source advances search pages: a page
	// counter (PaginationQueryPage) or an opaque cursor advertised in the Link
	// header (PaginationLinkHeader).
	SearchPagination() hubtree.PaginationMode
	// SearchPageParam is the upstream query parameter that carries the page number
	// or the cursor. Empty means the source cannot paginate search.
	SearchPageParam() string
	ConnectivityURL() string
	ModelDetailURL(repo string) string

	// ResolveBaseURL and BlobBaseURL are the prefixes used to rewrite relative
	// README links. A source with no HTML blob page returns its resolve base.
	ResolveBaseURL(repo string, isDataset bool, revision string) string
	BlobBaseURL(repo string, isDataset bool, revision string) string

	// Parsers. They read from the response body so callers keep streaming and
	// size limits; the error text callers produce for a decode failure stays in
	// the caller.
	DecodePage(body io.Reader) ([]hubtree.Node, error)
	ParseRepoInfo(body io.Reader) (*RepoInfo, error)
	ParseRefs(body io.Reader) ([]Ref, error)
	ParseSearch(body io.Reader) ([]Hit, error)
	ParseModelDetail(body io.Reader) (*ModelDetail, error)

	// AvatarURLs lists the candidate endpoints that can resolve an author's
	// logo, in attempt order. sampleRepo is one repository owned by that author,
	// for sources whose logo lives in the repository detail rather than a
	// per-author route. Callers fetch these URLs and pass the body to ParseAvatar.
	AvatarURLs(author, sampleRepo string) []string

	// ParseAvatar extracts the displayable logo URL from an AvatarURLs response.
	// A source rewrites the host only for path trees its endpoint is known to
	// proxy, so a mirror endpoint serves mirror-reachable image URLs.
	ParseAvatar(body io.Reader) (string, error)

	// Pagination. Pagination() selects the strategy; NextPage is only consulted
	// for PaginationQueryPage.
	Pagination() hubtree.PaginationMode
	NextPage(cur *url.URL, prefix string, lastPageLen int) (string, bool)

	// Capability flags. These replace implicit assumptions in the downloader.
	//
	// HeadForSHA reports whether a HEAD request can yield the expected digest.
	// Hugging Face exposes ETag and x-amz-meta-sha256; ModelScope exposes neither
	// on its resolve endpoint, so the digest must come from the tree listing.
	HeadForSHA() bool

	// AssumeRangesForLFS reports whether LFS files can be assumed to support
	// range requests during planning, without a HEAD probe.
	AssumeRangesForLFS() bool

	// ImageHosts is the README asset allow-list. Entries are compared against
	// the request URL's host, so they include an explicit port when the endpoint
	// uses one. Callers must still derive scheme and port from the endpoint, not
	// from the user-supplied URL.
	ImageHosts() []string
}

// Known reports whether a source id is one Resolve understands. Empty is known
// because it resolves to the default source.
func Known(id string) bool {
	switch strings.ToLower(strings.TrimSpace(id)) {
	case "", "hf", "huggingface", "modelscope", "ms":
		return true
	}
	return false
}

// Registered lists the source ids accepted by settings, in display order.
func Registered() []string { return []string{"hf", "modelscope"} }

// DefaultEndpointFor returns the base URL a source uses when settings leave the
// endpoint empty. Callers use it to prefill the endpoint when the saved value
// belongs to a different source, because an endpoint is a base URL that only one
// source's API shape fits.
func DefaultEndpointFor(id string) string {
	switch strings.ToLower(strings.TrimSpace(id)) {
	case "modelscope", "ms":
		return DefaultModelScopeEndpoint
	default:
		return DefaultEndpoint
	}
}

// Defaults lists the default endpoint of every registered source, keyed by source
// id. The UI uses it to prefill the endpoint when the source changes.
func Defaults() map[string]string {
	return map[string]string{
		"hf":         DefaultEndpoint,
		"modelscope": DefaultModelScopeEndpoint,
	}
}

// HubDefaultRevisions lists the default branch name of every registered source.
// A download job stores the revision name of the hub it was created on, so a
// job made on ModelScope carries "master" and one made on Hugging Face carries
// "main". Callers use this list to tell a default the app filled in by itself
// from a revision the user chose.
func HubDefaultRevisions() []string {
	out := make([]string, 0, len(Registered()))
	for _, id := range Registered() {
		rev := Resolve(id, "").DefaultRevision()
		if rev == "" {
			continue
		}
		duplicate := false
		for _, seen := range out {
			if strings.EqualFold(seen, rev) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			out = append(out, rev)
		}
	}
	return out
}

// IsHubDefaultRevision reports whether revision is the default branch name of a
// registered source. Such a name is what the app fills in on its own, so it can
// be re-mapped onto another hub's default; a branch, tag or commit the user
// picked cannot.
func IsHubDefaultRevision(revision string) bool {
	rev := strings.TrimSpace(revision)
	if rev == "" {
		return false
	}
	for _, def := range HubDefaultRevisions() {
		if strings.EqualFold(def, rev) {
			return true
		}
	}
	return false
}

// RevisionFallback returns the revision to retry a request with when the hub
// answered that the stored revision does not exist (HTTP 404) and the stored
// value is another source's default branch name: the settings changed hub
// underneath the request, so the request must follow the new hub's default
// branch. ok is false for a revision this source already uses and for any name
// the user chose, where substituting a guess would download different content.
func RevisionFallback(src Source, revision string) (string, bool) {
	rev := strings.TrimSpace(revision)
	if src == nil || !IsHubDefaultRevision(rev) {
		return "", false
	}
	def := strings.TrimSpace(src.DefaultRevision())
	if def == "" || strings.EqualFold(def, rev) {
		return "", false
	}
	return def, true
}

// Hosts lists the hosts each registered source is known to serve, keyed by source
// id. Callers use it to tell a stale endpoint left behind by a source switch from a
// mirror the user chose: hf-mirror.com answers the Hugging Face paths, so a config
// that pairs it with ModelScope was switched, not configured.
func Hosts() map[string][]string {
	hosts := make(map[string][]string, len(sourceHosts))
	for id, list := range sourceHosts {
		hosts[id] = append([]string(nil), list...)
	}
	return hosts
}

// sourceHosts records the hosts known to serve each source's API. A settings
// endpoint that points at another source's host is almost always a stale value
// left behind when the source was switched, so callers can report that instead of
// sending the request and passing an HTML error page through to the UI. Well-known
// mirrors are listed under the source whose API shape they serve: hf-mirror.com
// answers the Hugging Face paths, not ModelScope's.
var sourceHosts = map[string][]string{
	"hf":         {"huggingface.co", "hf.co", "hf-mirror.com"},
	"modelscope": {"modelscope.cn"},
}

// SourceForHost reports which registered source owns a host. The port is ignored
// so a self-hosted instance on a nonstandard port still matches. Hosts no source
// claims (a mirror, a private deployment) return false, which is intentional:
// those are legitimate endpoints and must not be rejected.
func SourceForHost(raw string) (string, bool) {
	host := strings.TrimSpace(raw)
	if u, err := url.Parse(host); err == nil && u.Host != "" {
		host = u.Host
	}
	host = strings.ToLower(strings.SplitN(host, ":", 2)[0])
	if host == "" {
		return "", false
	}
	for id, hosts := range sourceHosts {
		for _, h := range hosts {
			if host == h {
				return id, true
			}
		}
	}
	return "", false
}

// Resolve returns the source for a source id and endpoint. An empty or unknown
// id resolves to the Hugging Face source, so existing configurations that only
// set Endpoint keep their current behavior.
func Resolve(source, endpoint string) Source {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case "modelscope", "ms":
		return NewModelScope(endpoint)
	default:
		return NewHF(endpoint)
	}
}
