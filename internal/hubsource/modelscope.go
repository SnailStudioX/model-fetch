// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hubsource

import (
	"encoding/json"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bashrusakh/modelfetch/internal/hubtree"
)

// DefaultModelScopeEndpoint is the base URL used when a ModelScope source is
// selected without an explicit endpoint.
const DefaultModelScopeEndpoint = "https://modelscope.cn"

// ModelScope is the ModelScope Hub source.
//
// It is not a Hugging Face-compatible mirror: every path differs, responses are
// wrapped in an envelope, the default revision is "master", and the resolve
// endpoint does not expose the expected digest through HEAD.
//
// Verified 2026-10-11 against https://modelscope.cn (anonymous, Qwen/Qwen2.5-7B-Instruct):
//   - /api/v1/models/{repo}/repo/files?Revision=master&Recursive=true -> 200, Data.Files
//   - /api/v1/models/{repo}/commits?Revision=master                   -> 200, Data.Commit (newest first)
//   - /api/v1/models/{repo}/revisions                                 -> 200, Data.RevisionMap
//   - /openapi/v1/models?search=...                                   -> 200, data.models
//   - /{repo}/resolve/master/{path}                                   -> 200 (LFS files 302 to a signed CDN URL)
//   - HEAD on resolve returns X-Linked-Etag only: no Content-Length, no Accept-Ranges
type ModelScope struct {
	endpoint string
	host     string
}

// NewModelScope returns the ModelScope source for an endpoint. An empty endpoint
// uses DefaultModelScopeEndpoint, not the Hugging Face default.
func NewModelScope(endpoint string) *ModelScope {
	ep := strings.TrimSpace(endpoint)
	if ep == "" {
		ep = DefaultModelScopeEndpoint
	}
	ep = NormalizeEndpoint(ep)
	m := &ModelScope{endpoint: ep, host: "modelscope.cn"}
	if u, err := url.Parse(ep); err == nil && u.Host != "" {
		m.host = u.Host
	}
	return m
}

func (m *ModelScope) ID() string   { return "modelscope" }
func (m *ModelScope) Base() string { return m.endpoint }

// DefaultRevision is "master" on ModelScope.
func (m *ModelScope) DefaultRevision() string { return "master" }

// TreeURL builds the file listing for one directory. ModelScope selects the
// directory with the Root query parameter, so prefix must be carried in the URL:
// without it every directory walk re-reads the repository root and the same
// files are emitted once per directory. ModelScope lists one level per response
// (its recursive flag is spelled Recursive, so the walker's lowercase
// recursive=true is accepted but has no effect), which is why the walk recurses
// per directory here. Models use repo/files, datasets use repo/tree — both
// return Data.Files with Path/Size/Type/Sha256/IsLFS. The OSS tree endpoint
// answers 200 but returns an empty Data array for anonymous callers, so it is
// not usable here.
func (m *ModelScope) TreeURL(repo string, isDataset bool, revision, prefix string) string {
	q := url.Values{}
	q.Set("Revision", revision)
	query := q.Encode()
	if prefix != "" {
		// Root is matched by the hub against the bytes that follow the "=", without
		// percent-decoding them: probed on modelscope.cn on 2026-10-31, Root=figures
		// lists that directory, Root=fig%75ures lists nothing, and the same spelling
		// in Revision *is* decoded. So the prefix goes back exactly as the listing
		// reported it, which is the only form that reaches a directory named outside
		// [A-Za-z0-9] - Chinese names included. Only characters that would end the
		// parameter or the request line are escaped; a name containing those cannot
		// be listed through this parameter either way.
		query += "&Root=" + strings.NewReplacer(" ", "%20", "#", "%23", "&", "%26").Replace(prefix)
	}
	if isDataset {
		return m.endpoint + "/api/v1/datasets/" + repo + "/repo/tree?" + query
	}
	return m.endpoint + "/api/v1/models/" + repo + "/repo/files?" + query
}

// FileURL and DownloadURL are the same endpoint on ModelScope: the raw path is a
// single-page application catch-all that returns HTML, while resolve serves the
// file content.
func (m *ModelScope) FileURL(repo string, isDataset bool, revision, path string) string {
	return m.DownloadURL(repo, isDataset, revision, path)
}

func (m *ModelScope) DownloadURL(repo string, isDataset bool, revision, path string) string {
	if isDataset {
		return m.endpoint + "/datasets/" + repo + "/resolve/" + url.PathEscape(revision) + "/" + PathEscapeAll(path)
	}
	return m.endpoint + "/" + repo + "/resolve/" + url.PathEscape(revision) + "/" + PathEscapeAll(path)
}

func (m *ModelScope) RepoPageURL(repo string, isDataset bool) string {
	if isDataset {
		return m.endpoint + "/datasets/" + repo
	}
	return m.endpoint + "/" + repo
}

// ResolveBaseURL is the prefix README asset links are rewritten to.
func (m *ModelScope) ResolveBaseURL(repo string, isDataset bool, revision string) string {
	if isDataset {
		return m.endpoint + "/datasets/" + repo + "/resolve/" + url.PathEscape(revision) + "/"
	}
	return m.endpoint + "/" + repo + "/resolve/" + url.PathEscape(revision) + "/"
}

// BlobBaseURL: ModelScope has no HTML blob page, so source links fall back to
// the resolve base rather than a path that would return the SPA shell.
func (m *ModelScope) BlobBaseURL(repo string, isDataset bool, revision string) string {
	return m.ResolveBaseURL(repo, isDataset, revision)
}

// RepoInfoURL is the commit listing; its first entry is the branch tip. ModelScope
// has no per-revision metadata endpoint.
func (m *ModelScope) RepoInfoURL(repo string, isDataset bool, revision string) string {
	if isDataset {
		return m.endpoint + "/api/v1/datasets/" + repo
	}
	return m.endpoint + "/api/v1/models/" + repo + "/commits?Revision=" + url.QueryEscape(revision)
}

// RefsURL is the branch/tag listing.
func (m *ModelScope) RefsURL(repo string, isDataset bool) string {
	if isDataset {
		return m.endpoint + "/api/v1/datasets/" + repo + "/revisions"
	}
	return m.endpoint + "/api/v1/models/" + repo + "/revisions"
}

// modelScopeTaskAlias translates Hugging Face pipeline-tag values into the task
// names ModelScope actually indexes. ModelScope applies filter.task strictly:
// an unrecognised value returns zero results instead of being ignored, so these
// cannot be passed through unchanged. Verified 2026-10-11 against
// https://modelscope.cn/openapi/v1/models.
var modelScopeTaskAlias = map[string]string{
	"automatic-speech-recognition": "auto-speech-recognition",
	"text-to-image":                "text-to-image-synthesis",
	"image-to-text":                "image-captioning",
}

// modelScopeLibraryTokens are filter values ModelScope stores as library tags
// rather than tasks (the UI's GGUF checkbox is one). Library filters are only
// honoured for models; the datasets endpoint ignores them.
var modelScopeLibraryTokens = map[string]bool{
	"gguf":                  true,
	"pytorch":               true,
	"safetensors":           true,
	"transformers":          true,
	"diffusers":             true,
	"sentence-transformers": true,
	"onnx":                  true,
	"parquet":               true,
}

// modelScopeFilterParam routes one Hugging Face filter token to the ModelScope
// query param that actually applies to it.
func modelScopeFilterParam(token string) (string, string) {
	if modelScopeLibraryTokens[token] {
		return "filter.library", token
	}
	if alias, ok := modelScopeTaskAlias[token]; ok {
		return "filter.task", alias
	}
	return "filter.task", token
}

// modelScopeSort maps the sort names the UI sends (Hugging Face spellings) to
// the values ModelScope accepts: default, downloads, likes, last_modified. It
// reports whether the order was only renamed or actually replaced: an
// unrecognised sort returns zero results, which is worse than a fallback, so the
// code falls back to downloads and the caller says so.
func modelScopeSort(sort string) (string, bool) {
	switch sort {
	case "downloads", "likes", "last_modified":
		return sort, false
	case "lastModified", "updated":
		return "last_modified", false
	default:
		return "downloads", true
	}
}

// modelScopePageSize clamps a requested page to the OpenAPI cap. A limit under
// the cap is used as asked; zero means the caller had no preference.
func modelScopePageSize(limit int) int {
	if limit <= 0 {
		return modelScopeDefaultPageSize
	}
	if limit > modelScopeMaxPageSize {
		return modelScopeMaxPageSize
	}
	return limit
}

const (
	modelScopeDefaultPageSize = 20
	modelScopeMaxPageSize     = 50 // OpenAPI page_size cap
)

// ModelScope substitutes both, so it answers for them.
var _ SearchLimits = (*ModelScope)(nil)

// SearchSortFor implements SearchLimits, so /api/search can name the order it
// really got instead of leaving the caller to compare result rows with the sort
// they picked. Renaming an order the source does support is not reported.
func (m *ModelScope) SearchSortFor(requested string) string {
	used, substituted := modelScopeSort(requested)
	if !substituted {
		return requested
	}
	return used
}

// SearchPageSize implements SearchLimits: a page larger than the OpenAPI cap
// comes back short, and the caller must not read that as the end of the results.
func (m *ModelScope) SearchPageSize(requested int) int { return modelScopePageSize(requested) }

// SearchURL uses the OpenAPI surface, which is the only anonymous search path.
// A plain "filter" param is ignored by this API, so each token is routed to its
// own bucket (filter.task / filter.library) instead of being sent as the
// comma-separated list Hugging Face expects.
func (m *ModelScope) SearchURL(q SearchQuery) string {
	kind := "models"
	if q.Kind == "datasets" {
		kind = "datasets"
	}
	params := url.Values{}
	if q.Q != "" {
		params.Set("search", q.Q)
	}
	sort, _ := modelScopeSort(q.Sort)
	params.Set("sort", sort)
	params.Set("page_size", strconv.Itoa(modelScopePageSize(q.Limit)))
	if q.Page > 1 {
		// Verified live: the OpenAPI page counter is "page_number" (lowercase with
		// underscore). "page" and "PageNumber" are accepted but ignored, so page 2
		// must use this exact spelling.
		params.Set("page_number", strconv.Itoa(q.Page))
	}
	for _, raw := range strings.Split(q.Filter, ",") {
		token := strings.TrimSpace(raw)
		if token == "" {
			continue
		}
		key, value := modelScopeFilterParam(token)
		if params.Get(key) != "" {
			continue // ModelScope does not accept comma lists inside one bucket
		}
		params.Set(key, value)
	}
	return m.endpoint + "/openapi/v1/" + kind + "?" + params.Encode()
}

// SearchPagination is counter-based: the OpenAPI accepts a page number but does
// not advertise a Link header.
func (m *ModelScope) SearchPagination() hubtree.PaginationMode { return hubtree.PaginationQueryPage }

// SearchPageParam is the OpenAPI page counter. Verified live: "page_number"
// advances results; "page" and "PageNumber" are ignored.
func (m *ModelScope) SearchPageParam() string { return "page_number" }

// ConnectivityURL is a cheap anonymous listing request. ModelScope has no
// whoami endpoint, so connectivity is proven by a search that returns 200.
func (m *ModelScope) ConnectivityURL() string {
	return m.endpoint + "/openapi/v1/models?page_size=1"
}

// ModelDetailURL is the model detail endpoint. ModelScope does not expose
// cardData, so base_model lineage is unavailable on this source.
func (m *ModelScope) ModelDetailURL(repo string) string {
	return m.endpoint + "/api/v1/models/" + repo
}

func (m *ModelScope) ParseModelDetail(body io.Reader) (*ModelDetail, error) {
	var env struct {
		Data struct {
			ID   string   `json:"Id"`
			Path string   `json:"Path"`
			Name string   `json:"Name"`
			Tags []string `json:"Tags"`
		} `json:"Data"`
	}
	if err := json.NewDecoder(body).Decode(&env); err != nil {
		return nil, err
	}
	id := env.Data.ID
	if id == "" && env.Data.Path != "" && env.Data.Name != "" {
		id = env.Data.Path + "/" + env.Data.Name
	}
	return &ModelDetail{ID: id, Tags: env.Data.Tags}, nil
}

// DecodePage reads the envelope and maps ModelScope's PascalCase file entries to
// hubtree nodes. Entries carry no nested lfs object, so the LFS marker is
// reconstructed from IsLFS plus the top-level Size and Sha256.
func (m *ModelScope) DecodePage(body io.Reader) ([]hubtree.Node, error) {
	var env struct {
		Data struct {
			Files []struct {
				Path   string `json:"Path"`
				Size   int64  `json:"Size"`
				Type   string `json:"Type"`
				Sha256 string `json:"Sha256"`
				IsLFS  bool   `json:"IsLFS"`
			} `json:"Files"`
		} `json:"Data"`
	}
	if err := json.NewDecoder(body).Decode(&env); err != nil {
		return nil, err
	}
	nodes := make([]hubtree.Node, 0, len(env.Data.Files))
	for _, f := range env.Data.Files {
		n := hubtree.Node{Type: f.Type, Path: f.Path, Size: f.Size, Sha256: f.Sha256}
		if f.IsLFS {
			n.LFS = &hubtree.LFS{Oid: f.Sha256, Size: f.Size, Sha256: f.Sha256}
		}
		nodes = append(nodes, n)
	}
	return nodes, nil
}

// ParseRepoInfo reads the commit listing. The first entry is the branch tip, so
// it is the commit the caller wants. A dataset detail response has no commit, so
// only LastModified is filled there.
func (m *ModelScope) ParseRepoInfo(body io.Reader) (*RepoInfo, error) {
	var env struct {
		Data struct {
			Commit []struct {
				Id            string `json:"Id"`
				CommittedDate int64  `json:"CommittedDate"`
			} `json:"Commit"`
			LastUpdatedTime int64 `json:"LastUpdatedTime"`
		} `json:"Data"`
	}
	if err := json.NewDecoder(body).Decode(&env); err != nil {
		return nil, err
	}
	info := &RepoInfo{}
	if len(env.Data.Commit) > 0 {
		info.SHA = env.Data.Commit[0].Id
		if env.Data.Commit[0].CommittedDate > 0 {
			info.LastModified = time.Unix(env.Data.Commit[0].CommittedDate, 0).UTC().Format(time.RFC3339)
		}
		return info, nil
	}
	// Datasets have no commit listing endpoint (all variants return 404/405), so
	// no branch-tip sha is available. Callers then key the cache ref by the
	// branch name instead of a sha; blobs stay content-addressed by sha256.
	if env.Data.LastUpdatedTime > 0 {
		info.LastModified = time.Unix(env.Data.LastUpdatedTime, 0).UTC().Format(time.RFC3339)
	}
	return info, nil
}

// ParseRefs reads Data.RevisionMap. ModelScope reports branch names only, so
// TargetCommit stays empty; callers must not assume a commit hash here.
func (m *ModelScope) ParseRefs(body io.Reader) ([]Ref, error) {
	var env struct {
		Data struct {
			RevisionMap struct {
				Branches []struct {
					Revision string `json:"Revision"`
				} `json:"Branches"`
				Tags []struct {
					Revision string `json:"Revision"`
				} `json:"Tags"`
			} `json:"RevisionMap"`
		} `json:"Data"`
	}
	if err := json.NewDecoder(body).Decode(&env); err != nil {
		return nil, err
	}
	refs := make([]Ref, 0, len(env.Data.RevisionMap.Branches)+len(env.Data.RevisionMap.Tags))
	for _, b := range env.Data.RevisionMap.Branches {
		refs = append(refs, Ref{Name: b.Revision, Ref: b.Revision, Type: "branch"})
	}
	for _, t := range env.Data.RevisionMap.Tags {
		refs = append(refs, Ref{Name: t.Revision, Ref: t.Revision, Type: "tag"})
	}
	return refs, nil
}

// ParseSearch reads the OpenAPI envelope. Models and datasets use different
// array names, so both are accepted.
func (m *ModelScope) ParseSearch(body io.Reader) ([]Hit, error) {
	var env struct {
		Data struct {
			Models   []rawSearchItem `json:"models"`
			Datasets []rawSearchItem `json:"datasets"`
		} `json:"data"`
	}
	if err := json.NewDecoder(body).Decode(&env); err != nil {
		return nil, err
	}
	items := env.Data.Models
	if len(items) == 0 {
		items = env.Data.Datasets
	}
	hits := make([]Hit, 0, len(items))
	for _, it := range items {
		author := it.Owner
		if author == "" && strings.Contains(it.ID, "/") {
			author = strings.SplitN(it.ID, "/", 2)[0]
		}
		hits = append(hits, Hit{
			ID:           it.ID,
			Author:       author,
			Downloads:    it.Downloads,
			Likes:        it.Likes,
			Private:      it.Private,
			Gated:        it.Gated,
			LibraryName:  it.LibraryName,
			Tags:         it.Tags,
			LastModified: it.LastModified,
			CreatedAt:    it.CreatedAt,
		})
	}
	return hits, nil
}

type rawSearchItem struct {
	ID           string    `json:"id"`
	Owner        string    `json:"owner"`
	Downloads    int64     `json:"downloads"`
	Likes        int       `json:"likes"`
	Private      bool      `json:"private"`
	Gated        bool      `json:"gated"`
	LibraryName  string    `json:"library_name"`
	Tags         []string  `json:"tags"`
	LastModified time.Time `json:"last_modified"`
	CreatedAt    time.Time `json:"created_at"`
}

// Pagination: the file listing returns everything in one response and ignores
// PageNumber/PageSize, so there is no next page.
func (m *ModelScope) Pagination() hubtree.PaginationMode { return hubtree.PaginationQueryPage }

func (m *ModelScope) NextPage(cur *url.URL, prefix string, lastPageLen int) (string, bool) {
	return "", false
}

// HeadForSHA is false: HEAD on the resolve endpoint returns X-Linked-Etag only,
// with no Content-Length and no x-amz-meta-sha256. The digest must come from the
// file listing.
func (m *ModelScope) HeadForSHA() bool { return false }

// AssumeRangesForLFS is true: LFS files redirect to a signed CDN URL that answers
// range requests with 206. The signed URL can expire mid-transfer, so a 403 on a
// resumed part must re-resolve instead of failing hard.
func (m *ModelScope) AssumeRangesForLFS() bool { return true }

// ImageHosts covers the endpoint host plus the asset and LFS CDN hosts that
// README images point to. Scheme and port still come from the endpoint.
func (m *ModelScope) ImageHosts() []string {
	return []string{m.host, "resouces.modelscope.cn", "cdn-lfs-cn-1.modelscope.cn"}
}
