// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/SnailStudioX/model-fetch/internal/hubsource"
	"github.com/SnailStudioX/model-fetch/internal/hubtree"
)

// DefaultEndpoint is the HuggingFace Hub URL used when Settings.Endpoint is empty:
// the hf-mirror.com mirror, aliased from internal/hubsource.DefaultEndpoint.
// Can be overridden via Settings.Endpoint for mirrors or enterprise deployments.
// Credits: Custom endpoint feature suggested by windtail (#38)
const DefaultEndpoint = hubsource.DefaultEndpoint

// hfNode represents a file or directory in the HuggingFace repo tree.
type hfNode struct {
	Type   string     `json:"type"` // "file"|"directory" (sometimes "blob"|"tree")
	Path   string     `json:"path"`
	Size   int64      `json:"size,omitempty"`
	LFS    *hfLfsInfo `json:"lfs,omitempty"`
	Sha256 string     `json:"sha256,omitempty"`
}

// hfLfsInfo contains LFS metadata for large files.
type hfLfsInfo struct {
	Oid    string `json:"oid,omitempty"`
	Size   int64  `json:"size,omitempty"`
	Sha256 string `json:"sha256,omitempty"`
}

// buildHTTPClientWithProxy creates an HTTP client with proxy support.
func buildHTTPClientWithProxy(proxy *ProxyConfig) *http.Client {
	client, err := BuildHTTPClient(proxy)
	if err != nil {
		// Fallback to no proxy on error
		client, _ = BuildHTTPClient(&ProxyConfig{NoEnvProxy: true})
	}
	// Transfer client only: cap how many response bodies this process reads
	// from one hub at a time, across every job (see streamgate.go).
	client.Transport = &streamGatedTransport{next: client.Transport}
	return client
}

// addAuth adds authentication and user-agent headers to a request.
func addAuth(req *http.Request, token string) {
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("User-Agent", "modelfetch/1")
}

// headForETagTimeout bounds how long the optional ETag/SHA HEAD may take before
// it is treated as a benign optional-metadata miss. It is a package variable
// (not a public API) so tests can lower it deterministically, mirroring
// maxAttemptsPerRetry and socksHandshakeNanos.
var headForETagTimeout = 10 * time.Second

// headForETag fetches ETag and SHA256 headers for a file.
//
// Any non-2xx status and any transient transport error are treated as an
// optional-metadata miss — the file may simply have no remote SHA to verify
// against — and yield no error. A genuine CALLER cancellation is
// surfaced promptly so a cancelled job stops; the internal request timeout is
// itself a benign miss, so a slow-but-successful HEAD on a cold mirror does not
// fail the job.
func headForETag(parentCtx context.Context, httpc *http.Client, token string, it PlanItem) (etag string, remoteSha string, err error) {
	reqCtx, cancel := context.WithTimeout(parentCtx, headForETagTimeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(reqCtx, "HEAD", it.URL, nil)
	addAuth(req, token)
	resp, err := httpc.Do(req)
	if err != nil {
		// Check the CALLER's context, not the internally-derived reqCtx: an
		// internal timeout (context.DeadlineExceeded from reqCtx) is NOT
		// fail-fast and must be treated as a benign optional-metadata miss. Only
		// a genuine parent cancellation is surfaced.
		if cerr := parentCtx.Err(); cerr != nil {
			return "", "", cerr
		}
		return "", "", nil
	}
	defer resp.Body.Close()
	if apiErr := classifyFileResponse(resp, it.URL); apiErr != nil {
		return "", "", nil
	}
	return resp.Header.Get("ETag"), resp.Header.Get("x-amz-meta-sha256"), nil
}

// walkTree walks the HuggingFace repo tree through the shared hubtree walker
// (issue #96): it requests recursive=true, follows Link rel="next"
// pagination, falls back to per-directory listing for mirrors that ignore
// recursive, bounds malformed listings, and retries 429/5xx/network errors
// with Retry-After/RateLimit-aware, context-cancellable waits. 401/403 (and
// every other terminal status) fail immediately with the historical error
// messages below.
func walkTree(ctx context.Context, httpc *http.Client, token string, src hubsource.Source, job Job, prefix string, fn func(hfNode) error) error {
	w := &hubtree.Walker{
		TreeURL: func(p string) string {
			return treeURL(src, job, p)
		},
		Token:     token,
		UserAgent: "modelfetch/1",
		Client:    httpc,
		StatusErr: func(resp *http.Response) error {
			switch resp.StatusCode {
			case http.StatusUnauthorized:
				return fmt.Errorf("401 unauthorized: repo requires token or you do not have access (visit %s)", agreementURL(src, job))
			case http.StatusForbidden:
				return fmt.Errorf("403 forbidden: please accept the repository terms: %s", agreementURL(src, job))
			default:
				return &treeStatusError{status: resp.StatusCode, statusText: resp.Status}
			}
		},
		DecodePage: src.DecodePage,
		Pagination: src.Pagination(),
		NextPage:   src.NextPage,
	}
	return w.Walk(ctx, prefix, func(n hubtree.Node) error {
		return fn(toHfNode(n))
	})
}

// treeStatusError is the error for a terminal (non-retried) tree response that
// is not 401/403. It keeps the historical "tree API failed: <status>" message
// but carries the status code, so the scan can tell a missing revision (404)
// from a transient failure with errors.Is instead of matching message text.
type treeStatusError struct {
	status     int
	statusText string
}

func (e *treeStatusError) Error() string {
	return fmt.Sprintf("tree API failed: %s", e.statusText)
}

// Is makes a 404 tree response match ErrNotFound, the package's existing
// "repository or revision not found" sentinel, rather than inventing a second
// not-found error for the same condition.
func (e *treeStatusError) Is(target error) bool {
	return e.status == http.StatusNotFound && target == ErrNotFound
}

// toHfNode converts a shared hubtree node to the downloader's node type
// without losing any field.
func toHfNode(n hubtree.Node) hfNode {
	out := hfNode{Type: n.Type, Path: n.Path, Size: n.Size, Sha256: n.Sha256}
	if n.LFS != nil {
		out.LFS = &hfLfsInfo{Oid: n.LFS.Oid, Size: n.LFS.Size, Sha256: n.LFS.Sha256}
	}
	return out
}

// URL builders - all resolve through the configured Source so a hub with a
// different URL shape can be swapped in without touching the download path.
// Repository ids keep literal slashes; only path segments are escaped.

func rawURL(src hubsource.Source, job Job, path string) string {
	return src.FileURL(job.Repo, job.IsDataset, job.Revision, path)
}

func lfsURL(src hubsource.Source, job Job, path string) string {
	return src.DownloadURL(job.Repo, job.IsDataset, job.Revision, path)
}

func treeURL(src hubsource.Source, job Job, prefix string) string {
	return src.TreeURL(job.Repo, job.IsDataset, job.Revision, prefix)
}

func agreementURL(src hubsource.Source, job Job) string {
	return src.RepoPageURL(job.Repo, job.IsDataset)
}

// pathEscapeAll escapes each path segment.
func pathEscapeAll(p string) string { return hubsource.PathEscapeAll(p) }

// RepoInfo contains metadata about a HuggingFace repository.
type RepoInfo = hubsource.RepoInfo

// fetchRepoInfo fetches repository metadata including the commit SHA for a given revision.
func fetchRepoInfo(ctx context.Context, httpc *http.Client, token string, src hubsource.Source, job Job) (*RepoInfo, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", src.RepoInfoURL(job.Repo, job.IsDataset, job.Revision), nil)
	addAuth(req, token)
	resp, err := httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("repo info API failed: %s", resp.Status)
	}

	return src.ParseRepoInfo(resp.Body)
}
