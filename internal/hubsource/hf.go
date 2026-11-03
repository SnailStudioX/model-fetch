// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hubsource

import (
	"encoding/json"
	"io"
	"net/url"
	"strconv"
	"time"

	"github.com/SnailStudioX/model-fetch/internal/hubtree"
)

// HF is the Hugging Face Hub source. It is the historical shape of this project:
// every builder below reproduces, byte for byte, the URL that pkg/hfdownloader,
// pkg/smartdl and internal/server used to construct inline. Migrating the call
// sites to this source must not change any observable behavior.
type HF struct {
	endpoint string
	host     string
}

// NewHF returns the Hugging Face source for an endpoint. An empty endpoint uses
// DefaultEndpoint, matching the historical getEndpoint behavior.
func NewHF(endpoint string) *HF {
	ep := NormalizeEndpoint(endpoint)
	h := &HF{endpoint: ep, host: "huggingface.co"}
	if u, err := url.Parse(ep); err == nil && u.Host != "" {
		h.host = u.Host
	}
	return h
}

func (h *HF) ID() string   { return "hf" }
func (h *HF) Base() string { return h.endpoint }

// DefaultRevision is "main". Callers keep their own fallback rules; this is the
// value used when no revision is supplied.
func (h *HF) DefaultRevision() string { return "main" }

// TreeURL builds the tree listing for a prefix. Repository ids keep literal
// slashes because the Hub requires them; only path segments are escaped.
func (h *HF) TreeURL(repo string, isDataset bool, revision, prefix string) string {
	kind := "models"
	if isDataset {
		kind = "datasets"
	}
	base := h.endpoint + "/api/" + kind + "/" + repo + "/tree/" + url.PathEscape(revision)
	if prefix == "" {
		return base
	}
	return base + "/" + PathEscapeAll(prefix)
}

// FileURL is the raw endpoint used for metadata and small files.
func (h *HF) FileURL(repo string, isDataset bool, revision, path string) string {
	if isDataset {
		return h.endpoint + "/datasets/" + repo + "/raw/" + url.PathEscape(revision) + "/" + PathEscapeAll(path)
	}
	return h.endpoint + "/" + repo + "/raw/" + url.PathEscape(revision) + "/" + PathEscapeAll(path)
}

// DownloadURL is the resolve endpoint used for LFS and binary content.
func (h *HF) DownloadURL(repo string, isDataset bool, revision, path string) string {
	if isDataset {
		return h.endpoint + "/datasets/" + repo + "/resolve/" + url.PathEscape(revision) + "/" + PathEscapeAll(path)
	}
	return h.endpoint + "/" + repo + "/resolve/" + url.PathEscape(revision) + "/" + PathEscapeAll(path)
}

// RepoPageURL is the repository page, quoted in the terms-acceptance error.
func (h *HF) RepoPageURL(repo string, isDataset bool) string {
	if isDataset {
		return h.endpoint + "/datasets/" + repo
	}
	return h.endpoint + "/" + repo
}

// ResolveBaseURL is the prefix README asset links are rewritten to.
func (h *HF) ResolveBaseURL(repo string, isDataset bool, revision string) string {
	if isDataset {
		return h.endpoint + "/datasets/" + repo + "/resolve/" + url.PathEscape(revision) + "/"
	}
	return h.endpoint + "/" + repo + "/resolve/" + url.PathEscape(revision) + "/"
}

// BlobBaseURL is the prefix README source links are rewritten to.
func (h *HF) BlobBaseURL(repo string, isDataset bool, revision string) string {
	if isDataset {
		return h.endpoint + "/datasets/" + repo + "/blob/" + url.PathEscape(revision) + "/"
	}
	return h.endpoint + "/" + repo + "/blob/" + url.PathEscape(revision) + "/"
}

// RepoInfoURL is the commit metadata endpoint.
func (h *HF) RepoInfoURL(repo string, isDataset bool, revision string) string {
	kind := "models"
	if isDataset {
		kind = "datasets"
	}
	return h.endpoint + "/api/" + kind + "/" + repo + "/revision/" + url.PathEscape(revision)
}

// RefsURL is the branches and tags endpoint.
func (h *HF) RefsURL(repo string, isDataset bool) string {
	kind := "models"
	if isDataset {
		kind = "datasets"
	}
	return h.endpoint + "/api/" + kind + "/" + repo + "/refs"
}

// SearchURL builds the search query. sort, direction and limit are always
// present, as in the historical search handler.
func (h *HF) SearchURL(q SearchQuery) string {
	kind := "models"
	if q.Kind == "datasets" {
		kind = "datasets"
	}
	params := url.Values{}
	if q.Q != "" {
		params.Set("search", q.Q)
	}
	params.Set("sort", q.Sort)
	params.Set("direction", "-1")
	params.Set("limit", strconv.Itoa(q.Limit))
	if q.Filter != "" {
		// The Hub supports repeated filter params; a single comma-joined value
		// is what the historical handler sent.
		params.Set("filter", q.Filter)
	}
	if q.Cursor != "" {
		// The cursor is opaque: it is the value the Hub advertised in the previous
		// Link header, not a page number. A "page" param is accepted but does not
		// change the result set.
		params.Set(h.SearchPageParam(), q.Cursor)
	}
	return h.endpoint + "/api/" + kind + "?" + params.Encode()
}

// SearchPagination is cursor-based: the Hub advertises the next page in the Link
// header instead of accepting a page counter.
func (h *HF) SearchPagination() hubtree.PaginationMode { return hubtree.PaginationLinkHeader }

// SearchPageParam is the query parameter the Hub uses for its cursor.
func (h *HF) SearchPageParam() string { return "cursor" }

// ModelDetailURL is the repository card endpoint used by the analyzer.
func (h *HF) ModelDetailURL(repo string) string {
	return h.endpoint + "/api/models/" + repo
}

func (h *HF) ParseModelDetail(body io.Reader) (*ModelDetail, error) {
	var d ModelDetail
	if err := json.NewDecoder(body).Decode(&d); err != nil {
		return nil, err
	}
	return &d, nil
}

// ConnectivityURL is the endpoint used by the proxy connectivity test.
func (h *HF) ConnectivityURL() string {
	return h.endpoint + "/api/whoami-v2"
}

// DecodePage reads a tree page, which is a JSON array of nodes.
func (h *HF) DecodePage(body io.Reader) ([]hubtree.Node, error) {
	var page []hubtree.Node
	if err := json.NewDecoder(body).Decode(&page); err != nil {
		return nil, err
	}
	return page, nil
}

func (h *HF) ParseRepoInfo(body io.Reader) (*RepoInfo, error) {
	var info RepoInfo
	if err := json.NewDecoder(body).Decode(&info); err != nil {
		return nil, err
	}
	return &info, nil
}

// ParseRefs reads the branches/tags envelope and flattens it, keeping branch
// order before tag order as the analyzer did.
func (h *HF) ParseRefs(body io.Reader) ([]Ref, error) {
	var resp struct {
		Branches []Ref `json:"branches"`
		Tags     []Ref `json:"tags"`
	}
	if err := json.NewDecoder(body).Decode(&resp); err != nil {
		return nil, err
	}
	refs := make([]Ref, 0, len(resp.Branches)+len(resp.Tags))
	for _, b := range resp.Branches {
		b.Type = "branch"
		refs = append(refs, b)
	}
	for _, t := range resp.Tags {
		t.Type = "tag"
		refs = append(refs, t)
	}
	return refs, nil
}

// rawSearchModel is the Hub search shape. gated is a bool or a string
// ("auto", "manual"), so it is normalized during parsing.
type rawSearchModel struct {
	ID            string    `json:"id"`
	Author        string    `json:"author"`
	Downloads     int64     `json:"downloads"`
	Likes         int       `json:"likes"`
	TrendingScore float64   `json:"trendingScore"`
	Private       bool      `json:"private"`
	Gated         any       `json:"gated"`
	PipelineTag   string    `json:"pipeline_tag"`
	LibraryName   string    `json:"library_name"`
	Tags          []string  `json:"tags"`
	LastModified  time.Time `json:"lastModified"`
	CreatedAt     time.Time `json:"createdAt"`
}

func (h *HF) ParseSearch(body io.Reader) ([]Hit, error) {
	var raw []rawSearchModel
	if err := json.NewDecoder(body).Decode(&raw); err != nil {
		return nil, err
	}
	hits := make([]Hit, 0, len(raw))
	for _, m := range raw {
		hits = append(hits, Hit{
			ID:            m.ID,
			Author:        m.Author,
			Downloads:     m.Downloads,
			Likes:         m.Likes,
			TrendingScore: m.TrendingScore,
			Private:       m.Private,
			Gated:         normalizeGated(m.Gated),
			PipelineTag:   m.PipelineTag,
			LibraryName:   m.LibraryName,
			Tags:          m.Tags,
			LastModified:  m.LastModified,
			CreatedAt:     m.CreatedAt,
		})
	}
	return hits, nil
}

// normalizeGated accepts both shapes the Hub returns for gated.
func normalizeGated(v any) bool {
	switch g := v.(type) {
	case bool:
		return g
	case string:
		return g != "" && g != "false"
	}
	return false
}

// Pagination follows the Link rel="next" header, which is the historical
// behavior and the zero value of the mode.
func (h *HF) Pagination() hubtree.PaginationMode { return hubtree.PaginationLinkHeader }

// NextPage is not used for the Link-header mode.
func (h *HF) NextPage(cur *url.URL, prefix string, lastPageLen int) (string, bool) {
	return "", false
}

func (h *HF) HeadForSHA() bool         { return true }
func (h *HF) AssumeRangesForLFS() bool { return true }

// ImageHosts is the endpoint host, including an explicit port. Callers still
// derive scheme and port from the endpoint itself.
func (h *HF) ImageHosts() []string { return []string{h.host} }
