// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/SnailStudioX/model-fetch/internal/hubsource"
	"github.com/SnailStudioX/model-fetch/internal/hubtree"
	"github.com/SnailStudioX/model-fetch/pkg/hfdownloader"
	"github.com/SnailStudioX/model-fetch/pkg/smartdl"
)

// SearchResult is one model returned by /api/search.
type SearchResult struct {
	ID     string `json:"id"`
	Author string `json:"author,omitempty"`
	// Avatar is the author's logo URL, resolved once per author and cached. It is
	// empty when the source has no logo for that author, and the UI then keeps its
	// initials placeholder.
	Avatar        string    `json:"avatar,omitempty"`
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
	Cached        bool      `json:"cached,omitempty"`
	CacheSource   string    `json:"cacheSource,omitempty"`
	CacheStatus   string    `json:"cacheStatus,omitempty"`
	// Fit is present only for sort=recommend. Search results carry no file sizes,
	// so it is estimated from the parameter count in the repo name at Q4_K_M plus
	// the flat runtime allowance — an estimate, not a measured file.
	Fit *smartdl.HardwareFit `json:"fit,omitempty"`
}

// SearchResponse wraps a list of results and metadata.
type SearchResponse struct {
	Results []SearchResult `json:"results"`
	Total   int            `json:"total"`
	Query   string         `json:"query,omitempty"`
	// Page is the page this response carries (1-based). NextURL is the absolute
	// upstream URL for the following page, empty when the source reports no more
	// results. Callers should follow NextURL instead of assuming page numbers are
	// meaningful for every source — the Hub paginates by cursor.
	Page    int    `json:"page,omitempty"`
	NextURL string `json:"next_url,omitempty"`
	// Warnings names what the source did differently from what was asked: an
	// order it does not offer, a page it capped. Same convention as the other
	// read endpoints — a 200 that is not what the caller asked for is still not
	// a silent answer, and the status code never changes to carry it.
	Warnings []string `json:"warnings,omitempty"`
}

// maxSearchPage bounds how far a client can page through one query. Sources
// expose no total count, so pagination is driven by whether a page came back
// full; the cap keeps a client from walking an unbounded cursor.
const maxSearchPage = 20

// handleSearch proxies model search to the configured hub source.
//
// Query parameters:
//
//	q        – free-text search
//	sort     – downloads | likes | lastModified | trendingScore | recommend (default: downloads)
//	limit    – max results (default 20, max 100)
//	filter   – comma-separated tag filters forwarded to HF (e.g. "gguf,text-generation")
//	datasets – "true" to search datasets instead of models
//	page     – 1-based result page (default 1, max 20)
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	query := strings.TrimSpace(q.Get("q"))
	sort := q.Get("sort")
	if sort == "" {
		sort = "downloads"
	}
	limitStr := q.Get("limit")
	limit := 20
	if n, err := strconv.Atoi(limitStr); err == nil && n > 0 {
		if n > 100 {
			n = 100
		}
		limit = n
	}
	filterParam := q.Get("filter")
	isDataset := strings.EqualFold(q.Get("datasets"), "true")
	// "recommend" is not an index any source offers, so the upstream query runs in
	// its default order and this handler ranks the page against the machine
	// profile. Ranking therefore covers only the page it sees, and next_url keeps
	// sort=recommend so the following page is ranked the same way.
	rankByHardware := sort == "recommend" && !isDataset
	upstreamSort := sort
	if rankByHardware {
		upstreamSort = "downloads"
	}
	page := 1
	if n, err := strconv.Atoi(q.Get("page")); err == nil && n > 1 {
		if n > maxSearchPage {
			n = maxSearchPage
		}
		page = n
	}
	// cursor is accepted for cursor-paginating sources; counter sources ignore it.
	cursor := strings.TrimSpace(q.Get("cursor"))

	// Build the search URL through the configured source. For the Hugging Face
	// source this is the same path and parameter set as before.
	cfg := s.snapshotConfig()
	src := hubsource.Resolve(cfg.Source, cfg.Endpoint)
	if msg := sourceEndpointMismatch(cfg); msg != "" {
		writeError(w, http.StatusBadRequest, "Endpoint does not match source", msg)
		return
	}

	kind := "models"
	if isDataset {
		kind = "datasets"
	}
	sq := hubsource.SearchQuery{Q: query, Kind: kind, Sort: upstreamSort, Limit: limit, Filter: filterParam, Page: page, Cursor: cursor}
	searchURL := src.SearchURL(sq)

	// What the source will really do with this request. A hub that cannot honour
	// an order or a page size still answers 200, so the difference has to be said:
	// otherwise a caller cannot tell the order it asked for from the one it got.
	var warnings []string
	pageSize := limit
	if lim, ok := src.(hubsource.SearchLimits); ok {
		if used := lim.SearchSortFor(sq.Sort); used != sq.Sort {
			warnings = append(warnings, fmt.Sprintf("%s has no %q search order, so results come back ordered by %s", src.ID(), sq.Sort, used))
		}
		if size := lim.SearchPageSize(limit); size < pageSize {
			pageSize = size
			warnings = append(warnings, fmt.Sprintf("%s caps a search page at %d results; %d were requested", src.ID(), size, limit))
		}
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, searchURL, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to build upstream request", err.Error())
		return
	}
	if cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
	}
	req.Header.Set("User-Agent", "modelfetch/1")

	httpClient, err := s.hubClient(cfg)
	if err != nil {
		log.Printf("search: invalid proxy config: %v", err)
		writeError(w, http.StatusInternalServerError, "Invalid proxy configuration", err.Error())
		return
	}
	httpClient.Timeout = 20 * time.Second
	resp, err := httpClient.Do(req)
	if err != nil {
		log.Printf("search: upstream error: %v", err)
		writeError(w, http.StatusBadGateway, "Upstream request failed", err.Error())
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20)) // 4 MiB cap
	if err != nil {
		writeError(w, http.StatusBadGateway, "Failed to read upstream response", err.Error())
		return
	}

	if resp.StatusCode != http.StatusOK {
		writeUpstreamError(w, src, resp.StatusCode, body)
		return
	}

	hits, err := src.ParseSearch(bytes.NewReader(body))
	if err != nil {
		if looksLikeHTML(body) {
			writeError(w, http.StatusBadGateway, "Unexpected HTML response", fmt.Sprintf("search returned HTML from %s, not JSON. An endpoint fits one source's API shape; source %q expects %s.", src.SearchURL(sq), src.ID(), hubsource.DefaultEndpointFor(cfg.Source)))
			return
		}
		writeError(w, http.StatusBadGateway, "Failed to parse upstream response", err.Error())
		return
	}

	// Next page. The URL handed back is API-relative on purpose: an upstream cursor
	// is opaque and cross-origin, so the browser must come back through this
	// handler rather than fetching the advertised URL directly.
	nextURL := searchNextURL(r.URL, src, hits, resp, page, pageSize)

	// Logos are resolved per author, not per row, because neither source's search
	// API reports one. A lookup failure never fails the search: those rows keep the
	// UI's initials placeholder.
	avatars := s.resolveAvatars(r.Context(), cfg, src, hits)

	cacheDir := cfg.cacheRoot()

	results := make([]SearchResult, 0, len(hits))
	for _, m := range hits {
		result := SearchResult{
			ID:            m.ID,
			Author:        m.Author,
			Avatar:        avatars[rowAuthor(m)],
			Downloads:     m.Downloads,
			Likes:         m.Likes,
			TrendingScore: m.TrendingScore,
			Private:       m.Private,
			Gated:         m.Gated,
			PipelineTag:   m.PipelineTag,
			LibraryName:   m.LibraryName,
			Tags:          m.Tags,
			LastModified:  m.LastModified,
			CreatedAt:     m.CreatedAt,
		}
		if localRepo, localErr := findLocalCachedRepo(cacheDir, cfg.LocalDir, cfg.LocalScanDirs, cfg.DownloadRoutes, m.ID, isDataset); localErr == nil {
			result.Cached = true
			result.CacheSource = localRepo.Source
			result.CacheStatus = localRepo.DownloadStatus
		} else if hfdownloader.IsValidModelName(m.ID) {
			repoType := hfdownloader.RepoTypeModel
			if isDataset {
				repoType = hfdownloader.RepoTypeDataset
			}
			rd, err := cfg.cache().Repo(m.ID, repoType)
			if err == nil && hubRepoHasContent(rd) {
				result.Cached, result.CacheSource, result.CacheStatus = true, "HF cache", "unknown"
			}
		}
		results = append(results, result)
	}

	if rankByHardware {
		hw, _, _, _ := s.effectiveHardware(r.Context())
		rankSearchResultsByHardware(results, hw)
	}

	writeJSON(w, http.StatusOK, SearchResponse{
		Results:  results,
		Total:    len(results),
		Query:    query,
		Page:     page,
		NextURL:  nextURL,
		Warnings: warnings,
	})
}

// rankSearchResultsByHardware scores each result against the machine profile and
// sorts the page best-first. A result whose name carries no size token gets no
// fit and keeps its relative order after the ranked ones. When nothing is known
// about the hardware the upstream order is left untouched rather than guessing.
func rankSearchResultsByHardware(results []SearchResult, hw smartdl.Hardware) {
	if hw.VRAMGB == 0 && hw.SystemRAMGB == 0 {
		return
	}
	for i := range results {
		if fit, ok := smartdl.AssessEstimatedRepo(results[i].ID, hw); ok {
			results[i].Fit = &fit
		}
	}
	sort.SliceStable(results, func(a, b int) bool {
		fa, fb := results[a].Fit, results[b].Fit
		if fa == nil {
			return false
		}
		if fb == nil {
			return true
		}
		return fa.Score > fb.Score
	})
}

// searchNextURL builds the API-relative URL for the following page. Counter
// sources advance a page number; cursor sources advertise an opaque token in the
// Link header. Neither API exposes a total count, so a full page is the only
// signal that more results exist for counter sources.
//
// pageSize is the page size the source was really asked for, which is not always
// the requested limit: a source that caps a page answers a short page without
// running out of results, and reading that as the end would hide the rest.
//
// The page number is carried on cursor URLs too, purely so the response reports
// the page the client is on: the cursor is what actually advances results, and a
// client cannot derive page 3 from page 2 without it.
func searchNextURL(apiURL *url.URL, src hubsource.Source, hits []hubsource.Hit, resp *http.Response, page, pageSize int) string {
	param := src.SearchPageParam()
	if param == "" {
		return ""
	}
	if page >= maxSearchPage {
		return ""
	}
	nextPage := strconv.Itoa(page + 1)
	if src.SearchPagination() == hubtree.PaginationQueryPage {
		if len(hits) < pageSize {
			return ""
		}
		return relativeSearchURL(apiURL, map[string]string{"page": nextPage})
	}
	next, err := hubtree.NextLink(resp.Header.Values("Link"))
	if err != nil {
		log.Printf("search: unusable Link header: %v", err)
		return ""
	}
	if next == "" {
		return ""
	}
	u, err := url.Parse(next)
	if err != nil {
		log.Printf("search: unusable Link target: %v", err)
		return ""
	}
	value := u.Query().Get(param)
	if value == "" {
		// A next link that does not carry its cursor cannot be replayed through
		// this handler; reporting no more results is honest, not a silent failure.
		log.Printf("search: Link next carries no %s value: %s", param, next)
		return ""
	}
	return relativeSearchURL(apiURL, map[string]string{searchAPIParam(hubtree.PaginationLinkHeader): value, "page": nextPage})
}

// searchAPIParam maps a source's upstream pagination parameter to the name this
// API exposes, so a client never has to know which hub it is talking to.
func searchAPIParam(mode hubtree.PaginationMode) string {
	if mode == hubtree.PaginationLinkHeader {
		return "cursor"
	}
	return "page"
}

// relativeSearchURL rebuilds the current request with the pagination values
// replaced, keeping q/sort/limit/filter/datasets exactly as the client sent them.
func relativeSearchURL(apiURL *url.URL, sets map[string]string) string {
	q := url.Values{}
	for key, values := range apiURL.Query() {
		if _, ok := sets[key]; ok {
			continue
		}
		q[key] = values
	}
	for key, value := range sets {
		q.Set(key, value)
	}
	return "/api/search?" + q.Encode()
}

// handleDiskFree returns free/total disk space for a given path.
// The path is always derived from server configuration — the "path" query
// param is accepted for UI compatibility but is validated against configured
// directories to prevent arbitrary filesystem stat via the API.
func (s *Server) handleDiskFree(w http.ResponseWriter, r *http.Request) {
	cfg := s.snapshotConfig().captureCacheEnvironment()

	// Build the ordered list of configured candidate paths.
	routed := routeDirs(cfg.DownloadRoutes)
	configured := []string{cfg.LocalDir, cfg.cache().HubDir(), cfg.cacheRoot(), cfg.cacheEnv.defaultRoot, RunDir()}
	configured = append(configured, routed...)

	// If the caller provides an explicit path, only honour it when it matches
	// one of the configured directories (prevents arbitrary fs-stat via the API).
	var path string
	if requested := r.URL.Query().Get("path"); requested != "" {
		for _, c := range configured {
			if c != "" && pathIdentityKey(requested) == pathIdentityKey(c) {
				path = c
				// Legacy clients pass cacheDir (R); cache capacity is measured
				// at H, not the possibly unrelated friendly-view filesystem.
				localPath := cfg.LocalDir != "" && pathIdentityKey(requested) == pathIdentityKey(cfg.LocalDir)
				for _, dir := range routed {
					if pathIdentityKey(requested) == pathIdentityKey(dir) {
						localPath = true
						break
					}
				}
				if pathIdentityKey(c) == pathIdentityKey(cfg.cacheRoot()) && !localPath {
					path = cfg.cache().HubDir()
				}
				break
			}
		}
		if path == "" {
			writeError(w, http.StatusBadRequest, "Invalid path", "path must be one of the configured directories")
			return
		}
	} else {
		// No explicit path: fall back to the first non-empty configured directory.
		for _, c := range configured {
			if c != "" {
				path = c
				break
			}
		}
	}
	writeDiskFree(w, path, cfg)
}

// handleDownloadDiskFree previews the same destination as CreateJob without
// creating directories, contacting the Hub, or starting a job. Explicit
// localDir has the same semantics as on POST /api/download; GET retains its
// configured-path allowlist for filesystem browsing.
func (s *Server) handleDownloadDiskFree(w http.ResponseWriter, r *http.Request) {
	var req DownloadRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body", err.Error())
		return
	}
	cfg := s.snapshotConfig()
	_, localDir, path, err := resolveDownloadDestination(cfg, req)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid routeKey", "routeKey must be one of the configured route keys")
		return
	}
	if localDir == "" {
		path = cfg.cache().HubDir()
	} else {
		path, err = filepath.Abs(path)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "Failed to resolve destination", err.Error())
			return
		}
	}
	writeDiskFree(w, path, cfg)
}

func writeDiskFree(w http.ResponseWriter, path string, cfg Config) {
	free, total, err := diskFreeBytes(path)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Could not stat disk", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path":            path,
		"free":            free,
		"total":           total,
		"cacheDir":        cfg.cacheRoot(),
		"effectiveHubDir": cfg.cache().HubDir(),
		"hubDirSource":    cfg.hubDirSource(),
	})
}

// handleHistory returns entries from download_history.json.
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	entries, err := LoadHistory()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to load history", err.Error())
		return
	}
	if entries == nil {
		entries = []HistoryEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entries": entries,
		"total":   len(entries),
	})
}

// looksLikeHTML reports whether a body starts with markup. Hubs that serve a
// single-page app answer unknown API paths with HTML, which is what a source and
// endpoint that do not match look like from the client's side.
func looksLikeHTML(body []byte) bool {
	b := bytes.TrimSpace(body)
	return len(b) > 0 && b[0] == '<'
}

// writeUpstreamError answers a search the hub itself rejected.
//
// A non-200 body is only passed through when it already has this API's error
// shape: Hugging Face answers {"error": "..."}, and its own wording is the best
// explanation of its 401 or 429, so that reaches the caller unchanged. ModelScope
// answers {"Code":..,"Message":..,"Success":false}, which carries no "error" key,
// so passing it through left the caller with a status code and no reason at all.
// Anything else is re-raised here, naming the source and its endpoint and quoting
// the start of the body, so the reason survives even for a response shape this
// server has no parser for. The status is kept: an upstream 401 stays a 401.
func writeUpstreamError(w http.ResponseWriter, src hubsource.Source, status int, body []byte) {
	var probe struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(body, &probe); err == nil && len(probe.Error) > 0 && string(probe.Error) != "null" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write(body)
		return
	}
	log.Printf("search: upstream %s returned %d", src.ID(), status)
	writeError(w, status, "Upstream search failed", fmt.Sprintf("%s at %s answered %d: %s", src.ID(), src.Base(), status, quoteBodyExcerpt(body)))
}

// quoteBodyExcerpt collapses an upstream body into a single line and keeps its
// first 300 characters. Enough to explain a gateway error or an unknown field,
// short enough that a full HTML error page does not end up in the UI.
func quoteBodyExcerpt(body []byte) string {
	text := strings.Join(strings.Fields(string(body)), " ")
	if runes := []rune(text); len(runes) > 300 {
		return string(runes[:300]) + "…"
	}
	return text
}
