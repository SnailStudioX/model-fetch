// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hubsource

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/bashrusakh/modelfetch/internal/hubtree"
)

// These tests pin the Hugging Face URL shapes to the strings the call sites
// constructed inline before the Source abstraction existed. If a builder changes,
// observable behavior changes, and that must be a deliberate decision.
func TestHFURLsMatchLegacyShapes(t *testing.T) {
	h := NewHF("https://huggingface.co")
	cases := []struct {
		name, got, want string
	}{
		{"tree model", h.TreeURL("owner/repo", false, "main", ""), "https://huggingface.co/api/models/owner/repo/tree/main"},
		{"tree model prefix", h.TreeURL("owner/repo", false, "main", "sub dir"), "https://huggingface.co/api/models/owner/repo/tree/main/sub%20dir"},
		{"tree dataset", h.TreeURL("facebook/flores", true, "main", ""), "https://huggingface.co/api/datasets/facebook/flores/tree/main"},
		{"raw model", h.FileURL("owner/repo", false, "main", "config.json"), "https://huggingface.co/owner/repo/raw/main/config.json"},
		{"raw dataset", h.FileURL("facebook/flores", true, "main", "a b.json"), "https://huggingface.co/datasets/facebook/flores/raw/main/a%20b.json"},
		{"resolve model", h.DownloadURL("owner/repo", false, "main", "model.gguf"), "https://huggingface.co/owner/repo/resolve/main/model.gguf"},
		{"resolve dataset", h.DownloadURL("facebook/flores", true, "main", "x.bin"), "https://huggingface.co/datasets/facebook/flores/resolve/main/x.bin"},
		{"repo page", h.RepoPageURL("owner/repo", false), "https://huggingface.co/owner/repo"},
		{"repo page dataset", h.RepoPageURL("facebook/flores", true), "https://huggingface.co/datasets/facebook/flores"},
		{"repo info", h.RepoInfoURL("owner/repo", false, "main"), "https://huggingface.co/api/models/owner/repo/revision/main"},
		{"refs", h.RefsURL("owner/repo", false), "https://huggingface.co/api/models/owner/repo/refs"},
		{"connectivity", h.ConnectivityURL(), "https://huggingface.co/api/whoami-v2"},
		{"search", h.SearchURL(SearchQuery{Q: "gguf", Kind: "models", Sort: "downloads", Limit: 20, Filter: "gguf"}),
			"https://huggingface.co/api/models?direction=-1&filter=gguf&limit=20&search=gguf&sort=downloads"},
		{"search dataset", h.SearchURL(SearchQuery{Kind: "datasets", Sort: "likes", Limit: 5}),
			"https://huggingface.co/api/datasets?direction=-1&limit=5&sort=likes"},
		// Hugging Face takes the comma-separated filter list verbatim; only
		// ModelScope needs the tokens split into buckets.
		{"search comma list stays verbatim", h.SearchURL(SearchQuery{Kind: "models", Sort: "downloads", Limit: 40, Filter: "text-to-speech,gguf"}),
			"https://huggingface.co/api/models?direction=-1&filter=text-to-speech%2Cgguf&limit=40&sort=downloads"},
		// The Hub paginates search with an opaque cursor, not a page number: a
		// "page" param is accepted but does not change the result set.
		{"search cursor replay", h.SearchURL(SearchQuery{Kind: "models", Sort: "downloads", Limit: 40, Cursor: "abc123"}),
			"https://huggingface.co/api/models?cursor=abc123&direction=-1&limit=40&sort=downloads"},
		{"search page number is not emitted for cursor sources", h.SearchURL(SearchQuery{Kind: "models", Sort: "downloads", Limit: 40, Page: 2}),
			"https://huggingface.co/api/models?direction=-1&limit=40&sort=downloads"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, c.got, c.want)
		}
	}
	if h.SearchPagination() != hubtree.PaginationLinkHeader || h.SearchPageParam() != "cursor" {
		t.Errorf("hf search pagination: got %v/%s", h.SearchPagination(), h.SearchPageParam())
	}
	if h.ID() != "hf" || h.DefaultRevision() != "main" {
		t.Errorf("hf identity: got %s/%s", h.ID(), h.DefaultRevision())
	}
}

func TestNormalizeEndpoint(t *testing.T) {
	if got := NormalizeEndpoint("https://hf-mirror.com/"); got != "https://hf-mirror.com" {
		t.Errorf("trailing slash: got %s", got)
	}
	if got := NormalizeEndpoint(""); got != DefaultEndpoint {
		t.Errorf("empty: got %s, want %s", got, DefaultEndpoint)
	}
	if got := NewHF("http://127.0.0.1:8090/").ImageHosts(); len(got) != 1 || got[0] != "127.0.0.1:8090" {
		t.Errorf("explicit port host: got %v", got)
	}
}

func TestResolveFallsBackToHF(t *testing.T) {
	for _, id := range []string{"", "hf", "huggingface", "unknown-mirror"} {
		if got := Resolve(id, "https://hf-mirror.com").ID(); got != "hf" {
			t.Errorf("resolve(%q) = %s, want hf", id, got)
		}
	}
	for _, id := range []string{"modelscope", "ms", "ModelScope"} {
		s := Resolve(id, "")
		if s.ID() != "modelscope" {
			t.Errorf("resolve(%q) = %s", id, s.ID())
		}
		if s.Base() != DefaultModelScopeEndpoint {
			t.Errorf("resolve(%q) base = %s, want %s", id, s.Base(), DefaultModelScopeEndpoint)
		}
		if s.DefaultRevision() != "master" {
			t.Errorf("resolve(%q) revision = %s, want master", id, s.DefaultRevision())
		}
	}
	if !Known("") || Known("nope") || len(Registered()) != 2 {
		t.Errorf("known/registered: %v %v %v", Known(""), Known("nope"), Registered())
	}
}

// SearchURL substitutes an unsupported order and a page over the cap by itself,
// and the answer is still a 200 with results. A caller can only say so if the
// source states what it actually applied.
func TestModelScopeSearchLimitsAreReportable(t *testing.T) {
	var src Source = NewModelScope("")
	lim, ok := src.(SearchLimits)
	if !ok {
		t.Fatal("ModelScope must report the order and page size it really uses")
	}
	if got := lim.SearchSortFor("trendingScore"); got != "downloads" {
		t.Errorf("trendingScore: got %q, want the substituted order", got)
	}
	// Renaming an order the hub does support is not a fallback, so it must not be
	// reported as one.
	if got := lim.SearchSortFor("lastModified"); got != "lastModified" {
		t.Errorf("lastModified: got %q, want the request echoed back", got)
	}
	for _, supported := range []string{"downloads", "likes", "last_modified"} {
		if got := lim.SearchSortFor(supported); got != supported {
			t.Errorf("%s: got %q", supported, got)
		}
	}
	if got := lim.SearchPageSize(100); got != 50 {
		t.Errorf("page 100: got %d, want the cap", got)
	}
	if got := lim.SearchPageSize(40); got != 40 {
		t.Errorf("page 40: got %d, want it kept", got)
	}
	if hf, ok := Source(NewHF("")).(SearchLimits); ok {
		t.Errorf("Hugging Face honours both and must not claim substitutions: %v", hf.SearchPageSize(100))
	}
}

// A prefix that reaches the hub changed is a directory that silently disappears
// from a download plan, so the bytes the listing reported have to survive the
// request builder and arrive in the request line unchanged.
func TestModelScopeTreePrefixSurvivesTheRequest(t *testing.T) {
	treeURL := NewModelScope("").TreeURL("iic/nlp_gte_sentence-embedding_chinese-base", false, "master", "resources/图片")
	req, err := http.NewRequest(http.MethodGet, treeURL, nil)
	if err != nil {
		t.Fatalf("the built URL is not a request: %v (%s)", err, treeURL)
	}
	uri := req.URL.RequestURI()
	if !strings.Contains(uri, "Root=resources/图片") {
		t.Errorf("request line = %s, want the prefix sent as the hub reported it", uri)
	}
	if _, err := url.Parse(uri); err != nil {
		t.Errorf("request line does not parse: %v (%s)", err, uri)
	}
	if strings.Contains(uri, "%E5") || strings.Contains(uri, "%25") {
		t.Errorf("the prefix was percent-encoded on the way out: %s", uri)
	}
}

// ModelScope is not a Hugging Face-compatible mirror; these are the paths verified
// against modelscope.cn on 2026-10-11.
func TestModelScopeURLs(t *testing.T) {
	m := NewModelScope("")
	cases := []struct {
		name, got, want string
	}{
		{"tree model", m.TreeURL("Qwen/Qwen2.5-7B-Instruct", false, "master", ""),
			"https://modelscope.cn/api/v1/models/Qwen/Qwen2.5-7B-Instruct/repo/files?Revision=master"},
		// The listing is per-directory, so the prefix must reach the URL as Root.
		{"tree model prefix", m.TreeURL("TabbyML/Mistral-7B", false, "master", "ggml"),
			"https://modelscope.cn/api/v1/models/TabbyML/Mistral-7B/repo/files?Revision=master&Root=ggml"},
		{"tree dataset", m.TreeURL("AI-ModelScope/alpaca", true, "master", "data"),
			"https://modelscope.cn/api/v1/datasets/AI-ModelScope/alpaca/repo/tree?Revision=master&Root=data"},
		// The hub matches Root against the literal bytes after the "=" and does not
		// percent-decode them, so a name that needs escaping has to travel as the
		// bytes the listing reported. Verified on modelscope.cn 2026-10-31:
		// Root=figures lists a real directory, Root=fig%75ures lists nothing, and
		// Root=图片 is accepted (empty, because no such directory is there) rather
		// than rejected.
		{"tree model prefix non-ascii", m.TreeURL("Qwen/Qwen2.5-7B-Instruct", false, "master", "figures/图"),
			"https://modelscope.cn/api/v1/models/Qwen/Qwen2.5-7B-Instruct/repo/files?Revision=master&Root=figures/图"},
		// /raw/ is an SPA catch-all, so metadata reads go through resolve.
		{"raw == resolve", m.FileURL("Qwen/Qwen2.5-7B-Instruct", false, "master", "config.json"),
			"https://modelscope.cn/Qwen/Qwen2.5-7B-Instruct/resolve/master/config.json"},
		{"resolve dataset", m.DownloadURL("AI-ModelScope/alpaca", true, "master", "a.json"),
			"https://modelscope.cn/datasets/AI-ModelScope/alpaca/resolve/master/a.json"},
		{"repo info", m.RepoInfoURL("Qwen/Qwen2.5-7B-Instruct", false, "master"),
			"https://modelscope.cn/api/v1/models/Qwen/Qwen2.5-7B-Instruct/commits?Revision=master"},
		{"refs", m.RefsURL("Qwen/Qwen2.5-7B-Instruct", false),
			"https://modelscope.cn/api/v1/models/Qwen/Qwen2.5-7B-Instruct/revisions"},
		{"connectivity", m.ConnectivityURL(), "https://modelscope.cn/openapi/v1/models?page_size=1"},
		{"search sort fallback", m.SearchURL(SearchQuery{Q: "Qwen", Kind: "models", Sort: "trendingScore", Limit: 100}),
			"https://modelscope.cn/openapi/v1/models?page_size=50&search=Qwen&sort=downloads"},
		// A plain "filter" param is ignored by this API, so tokens are routed to
		// the param that actually applies.
		{"search task bucket", m.SearchURL(SearchQuery{Kind: "models", Sort: "trendingScore", Limit: 40, Filter: "text-to-speech"}),
			"https://modelscope.cn/openapi/v1/models?filter.task=text-to-speech&page_size=40&sort=downloads"},
		{"search gguf is a library", m.SearchURL(SearchQuery{Kind: "models", Sort: "downloads", Limit: 40, Filter: "gguf"}),
			"https://modelscope.cn/openapi/v1/models?filter.library=gguf&page_size=40&sort=downloads"},
		{"search task+library split", m.SearchURL(SearchQuery{Kind: "models", Sort: "lastModified", Limit: 40, Filter: "text-to-speech,gguf"}),
			"https://modelscope.cn/openapi/v1/models?filter.library=gguf&filter.task=text-to-speech&page_size=40&sort=last_modified"},
		{"search task alias", m.SearchURL(SearchQuery{Kind: "models", Sort: "downloads", Limit: 40, Filter: "automatic-speech-recognition"}),
			"https://modelscope.cn/openapi/v1/models?filter.task=auto-speech-recognition&page_size=40&sort=downloads"},
		{"search dataset filter", m.SearchURL(SearchQuery{Kind: "datasets", Sort: "likes", Limit: 5, Filter: "text-generation"}),
			"https://modelscope.cn/openapi/v1/datasets?filter.task=text-generation&page_size=5&sort=likes"},
		// ModelScope does not accept comma lists inside one bucket, so the first
		// task token wins.
		{"search first task wins", m.SearchURL(SearchQuery{Kind: "models", Sort: "downloads", Limit: 40, Filter: "text-to-speech,text-generation"}),
			"https://modelscope.cn/openapi/v1/models?filter.task=text-to-speech&page_size=40&sort=downloads"},
		// Verified live: the OpenAPI page counter is "page_number". "page" and
		// "PageNumber" are accepted but ignored, so this spelling is load-bearing.
		{"search page 2", m.SearchURL(SearchQuery{Q: "Qwen", Kind: "models", Sort: "downloads", Limit: 40, Page: 2}),
			"https://modelscope.cn/openapi/v1/models?page_number=2&page_size=40&search=Qwen&sort=downloads"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, c.got, c.want)
		}
	}
	if m.Pagination() != hubtree.PaginationQueryPage {
		t.Errorf("pagination mode: got %v", m.Pagination())
	}
	if m.SearchPagination() != hubtree.PaginationQueryPage || m.SearchPageParam() != "page_number" {
		t.Errorf("search pagination: got %v/%s", m.SearchPagination(), m.SearchPageParam())
	}
	if hf := NewHF(""); hf.Pagination() != hubtree.PaginationLinkHeader {
		t.Errorf("hf pagination mode must stay the zero value, got %v", hf.Pagination())
	}
	if m.HeadForSHA() {
		t.Error("ModelScope HEAD exposes no digest; HeadForSHA must be false")
	}
	if !m.AssumeRangesForLFS() {
		t.Error("ModelScope LFS redirects to a CDN that supports ranges")
	}
	if _, ok := m.NextPage(nil, "", 0); ok {
		t.Error("ModelScope ignores page parameters; NextPage must report no next page")
	}
	hosts := m.ImageHosts()
	if len(hosts) != 3 || hosts[0] != "modelscope.cn" || hosts[1] != "resouces.modelscope.cn" || hosts[2] != "cdn-lfs-cn-1.modelscope.cn" {
		t.Errorf("image hosts: got %v", hosts)
	}
}

func TestModelScopeDecodePage(t *testing.T) {
	body := strings.NewReader(`{"Code":200,"Success":true,"Data":{"Files":[
		{"Path":"config.json","Size":663,"Type":"blob","Sha256":"aa","IsLFS":false},
		{"Path":"model-00001-of-00002.safetensors","Size":4000,"Type":"blob","Sha256":"bb","IsLFS":true},
		{"Path":"sub","Size":0,"Type":"tree","Sha256":"","IsLFS":false}]}}`)
	nodes, err := NewModelScope("").DecodePage(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 3 {
		t.Fatalf("nodes: %d", len(nodes))
	}
	if nodes[0].Type != "blob" || nodes[0].Path != "config.json" || nodes[0].Size != 663 || nodes[0].LFS != nil {
		t.Errorf("plain file: %+v", nodes[0])
	}
	// plan.go decides LFS from n.LFS != nil, so IsLFS must materialize it.
	if nodes[1].LFS == nil || nodes[1].LFS.Oid != "bb" || nodes[1].LFS.Sha256 != "bb" || nodes[1].LFS.Size != 4000 {
		t.Errorf("LFS marker: %+v", nodes[1])
	}
	if nodes[2].Type != "tree" {
		t.Errorf("directory entry: %+v", nodes[2])
	}
}

func TestModelScopeParsers(t *testing.T) {
	info, err := NewModelScope("").ParseRepoInfo(strings.NewReader(`{"Data":{"Commit":[{"Id":"16c1749","CommittedDate":1741335540}],"LastUpdatedTime":1741335540}}`))
	if err != nil {
		t.Fatal(err)
	}
	if info.SHA != "16c1749" || info.LastModified == "" {
		t.Errorf("repo info: %+v", info)
	}

	refs, err := NewModelScope("").ParseRefs(strings.NewReader(`{"Data":{"RevisionMap":{"Branches":[{"Revision":"master"}],"Tags":[{"Revision":"v1.0"}]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 || refs[0].Name != "master" || refs[0].Type != "branch" || refs[1].Type != "tag" {
		t.Errorf("refs: %+v", refs)
	}

	hits, err := NewModelScope("").ParseSearch(strings.NewReader(`{"data":{"models":[{"id":"Qwen/Qwen2.5-7B-Instruct","downloads":123,"likes":4}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != "Qwen/Qwen2.5-7B-Instruct" || hits[0].Downloads != 123 {
		t.Errorf("search hits: %+v", hits)
	}
}

func TestHFParsersKeepLegacyShapes(t *testing.T) {
	nodes, err := NewHF("").DecodePage(strings.NewReader(`[{"type":"file","path":"config.json","size":2}]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || nodes[0].Path != "config.json" || nodes[0].Size != 2 {
		t.Errorf("hf page: %+v", nodes)
	}
	info, err := NewHF("").ParseRepoInfo(strings.NewReader(`{"sha":"abc","lastModified":"2025-01-02T03:04:05Z"}`))
	if err != nil || info.SHA != "abc" || info.LastModified != "2025-01-02T03:04:05Z" {
		t.Errorf("hf repo info: %+v err=%v", info, err)
	}
	// gated is a bool on some endpoints and a string on others.
	hits, err := NewHF("").ParseSearch(strings.NewReader(`[{"id":"owner/model","gated":"auto"},{"id":"b","gated":true}]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 || !hits[0].Gated || !hits[1].Gated {
		t.Errorf("hf gated: %+v", hits)
	}
}

// An endpoint is a base URL that fits one source's API shape. These pin the
// per-source defaults and the host ownership callers use to catch a stale endpoint
// left behind when the source is switched.
func TestEndpointDefaultsAndHostOwnership(t *testing.T) {
	for id, want := range map[string]string{
		"modelscope": DefaultModelScopeEndpoint,
		"ms":         DefaultModelScopeEndpoint,
		"":           DefaultEndpoint,
		"hf":         DefaultEndpoint,
		"bogus":      DefaultEndpoint,
	} {
		if got := DefaultEndpointFor(id); got != want {
			t.Errorf("DefaultEndpointFor(%q) = %s want %s", id, got, want)
		}
	}

	cases := []struct {
		raw  string
		want string
		ok   bool
	}{
		{"https://huggingface.co", "hf", true},
		{"https://hf.co", "hf", true},
		{"https://modelscope.cn", "modelscope", true},
		{"modelscope.cn:8443", "modelscope", true},
		// hf-mirror.com answers the Hugging Face paths, so it belongs to the hf
		// source; a private deployment is claimed by no source and must not be
		// rejected.
		{"https://hf-mirror.com", "hf", true},
		{"http://127.0.0.1:9801", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := SourceForHost(c.raw)
		if got != c.want || ok != c.ok {
			t.Errorf("SourceForHost(%q) = %q,%v want %q,%v", c.raw, got, ok, c.want, c.ok)
		}
	}

	// Resolve keeps an explicit endpoint authoritative, so a mismatch is only
	// detectable through host ownership.
	if s := Resolve("modelscope", ""); s.Base() != DefaultModelScopeEndpoint {
		t.Errorf("Resolve(modelscope, \"\") base = %s", s.Base())
	}
	if s := Resolve("modelscope", "https://hf-mirror.com"); s.Base() != "https://hf-mirror.com" {
		t.Errorf("Resolve must not rewrite an explicit endpoint: %s", s.Base())
	}
	if d := Defaults(); d["hf"] != DefaultEndpoint || d["modelscope"] != DefaultModelScopeEndpoint {
		t.Errorf("Defaults() = %v", d)
	}

	// The Hugging Face default is the mirror, not the canonical host: an empty
	// endpoint has to resolve to a host this project's users can reach.
	if DefaultEndpoint != "https://hf-mirror.com" {
		t.Errorf("DefaultEndpoint = %s, want the hf-mirror.com mirror", DefaultEndpoint)
	}
	if owner, ok := SourceForHost("https://huggingface.co"); !ok || owner != "hf" {
		t.Errorf("huggingface.co should still belong to hf: %q %v", owner, ok)
	}
}
