// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
)

// finalizeReq records how the downloader asked for one file. The shape of the
// requests is what makes "did this file go through the multipart stitch path"
// observable from outside the package.
type finalizeReq struct {
	method      string
	hasRange    bool
	rangeHeader string
}

// finalizeRecorderServer serves a fixed payload for HEAD/GET, honouring Range
// requests, and records every request made against it.
func finalizeRecorderServer(t *testing.T, full []byte) (*httptest.Server, func() []finalizeReq) {
	t.Helper()
	var (
		mu   sync.Mutex
		reqs []finalizeReq
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		rq := r.Header.Get("Range")
		reqs = append(reqs, finalizeReq{method: r.Method, hasRange: rq != "", rangeHeader: rq})
		mu.Unlock()

		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", strconv.Itoa(len(full)))
			w.Header().Set("Accept-Ranges", "bytes")
			w.WriteHeader(http.StatusOK)
			return
		}
		rng := r.Header.Get("Range")
		if rng == "" {
			w.Header().Set("Accept-Ranges", "bytes")
			w.Header().Set("Content-Length", strconv.Itoa(len(full)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(full)
			return
		}
		var start, end int64
		if _, err := fmt.Sscanf(rng, "bytes=%d-%d", &start, &end); err != nil || start > end || end >= int64(len(full)) {
			http.Error(w, "bad range", http.StatusBadRequest)
			return
		}
		part := full[start : end+1]
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(full)))
		w.Header().Set("Content-Length", strconv.Itoa(len(part)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(part)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []finalizeReq {
		mu.Lock()
		defer mu.Unlock()
		return append([]finalizeReq(nil), reqs...)
	}
}

// TestDownloadMultipartHashed_PublishesAssemblyDigest covers the finalize
// window of the LEGACY multipart layout, whose part files hold real bytes and
// must still be stitched into the final file: stitching (a) produces the digest
// the caller needs for verification, so a multi-GB payload is not read a second
// time just to hash it, and (b) is announced as a byte-bounded "assembling"
// stage so the job does not look finished while the disk is still rewriting it.
// A fresh run no longer takes this path (see
// TestDownloadMultipartHashed_DirectWritePublishesWithoutAssembly), so the
// completed part files and their layout are seeded here.
func TestDownloadMultipartHashed_PublishesAssemblyDigest(t *testing.T) {
	full := make([]byte, 20000)
	for i := range full {
		full[i] = byte(i*7 + i/251)
	}
	wantSum := sha256.Sum256(full)

	srv, _ := finalizeRecorderServer(t, full)
	dst := filepath.Join(t.TempDir(), "tmp-assembly")

	// Seed four finished part files plus the layout that describes them, so the
	// run resumes into the legacy assembly path without issuing any request.
	for i := 0; i < 4; i++ {
		p := fmt.Sprintf("%s.part-%02d", dst, i)
		if err := os.WriteFile(p, full[int64(i)*5000:int64(i)*5000+5000], 0o644); err != nil {
			t.Fatalf("seed %s: %v", p, err)
		}
	}
	if err := writeMultipartResumeLayout(dst, buildMultipartResumeLayout(int64(len(full)), 4, 5000)); err != nil {
		t.Fatalf("seed layout: %v", err)
	}

	var (
		mu     sync.Mutex
		stages []ProgressEvent
	)
	emit := func(evt ProgressEvent) {
		if evt.Event == "file_finalizing" {
			mu.Lock()
			stages = append(stages, evt)
			mu.Unlock()
		}
	}

	var assembled string
	cfg := Settings{Concurrency: 4, Retries: 1, BackoffInitial: "1ms", BackoffMax: "2ms", StallTimeout: "0"}
	it := PlanItem{RelativePath: "big.bin", URL: srv.URL + "/big.bin", Size: int64(len(full)), AcceptRanges: true}

	if err := downloadMultipartHashed(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, emit, &assembled); err != nil {
		t.Fatalf("downloadMultipartHashed: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if !bytes.Equal(got, full) {
		t.Fatalf("assembled content mismatch: got len=%d want len=%d", len(got), len(full))
	}
	if assembled != hex.EncodeToString(wantSum[:]) {
		t.Errorf("assembled digest = %q, want %q", assembled, hex.EncodeToString(wantSum[:]))
	}

	mu.Lock()
	announced := false
	for _, evt := range stages {
		if evt.Stage == "assembling" && evt.StageTotal == int64(len(full)) {
			announced = true
			break
		}
	}
	mu.Unlock()
	if !announced {
		t.Errorf("no byte-bounded assembling stage event; got %+v", stages)
	}

	if left, _ := filepath.Glob(dst + ".part-*"); len(left) != 0 {
		t.Errorf("part files left behind after assembly: %v", left)
	}
}

// TestVerifyDigest_ReusesPrecomputedDigest pins the cost model of verification:
// a digest already computed over the same bytes is trusted without re-reading
// the file, a mismatch is still a hard error, and without a digest the file is
// hashed on disk exactly as before.
func TestVerifyDigest_ReusesPrecomputedDigest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "payload.bin")
	content := []byte("on-disk bytes")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	onDisk := sha256.Sum256(content)
	other := sha256.Sum256([]byte("different payload"))

	// The path deliberately does not exist: accepting a precomputed digest must
	// not touch the filesystem, which the full re-hash would.
	missing := filepath.Join(dir, "absent.bin")
	if err := verifyDigest(context.Background(), missing, hex.EncodeToString(other[:]), hex.EncodeToString(other[:])); err != nil {
		t.Errorf("matching precomputed digest rejected: %v", err)
	}
	if err := verifyDigest(context.Background(), missing, hex.EncodeToString(other[:]), hex.EncodeToString(onDisk[:])); err == nil {
		t.Error("mismatching precomputed digest accepted")
	}

	// No precomputed digest: fall back to hashing what is on disk.
	if err := verifyDigest(context.Background(), path, hex.EncodeToString(onDisk[:]), ""); err != nil {
		t.Errorf("fallback hash of matching file failed: %v", err)
	}
	if err := verifyDigest(context.Background(), path, hex.EncodeToString(other[:]), ""); err == nil {
		t.Error("fallback hash of mismatching file accepted")
	}
}

// TestDownload_ConnectionsOneNeverAssembles is the regression test for "the
// download looks finished but is still running". With one connection per file
// there is nothing to parallelize, so the multipart path would only add a full
// rewrite of the payload plus a full re-hash. Such files must stay on the single
// stream path: one plain GET, no part files, no assembling stage.
func TestDownload_ConnectionsOneNeverAssembles(t *testing.T) {
	full := make([]byte, 4096)
	for i := range full {
		full[i] = byte(i * 3)
	}
	sum := sha256.Sum256(full)
	digest := hex.EncodeToString(sum[:])

	run := func(t *testing.T, concurrency int) ([]finalizeReq, []ProgressEvent) {
		t.Helper()
		fileSrv, getReqs := finalizeRecorderServer(t, full)

		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.URL.Path == "/api/models/o/r/revision/main":
				_ = json.NewEncoder(w).Encode(RepoInfo{SHA: "abc123"})
			case r.URL.Path == "/api/models/o/r/tree/main":
				_ = json.NewEncoder(w).Encode([]hfNode{{
					Type: "file",
					Path: "big.bin",
					Size: 128,
					LFS:  &hfLfsInfo{Oid: digest, Size: int64(len(full))},
				}})
			default:
				fileSrv.Config.Handler.ServeHTTP(w, r)
			}
		}))
		defer api.Close()

		cfg := Settings{
			CacheDir:           t.TempDir(),
			Endpoint:           api.URL,
			MultipartThreshold: "1KiB",
			Concurrency:        concurrency,
			Retries:            1,
			BackoffInitial:     "1ms",
			BackoffMax:         "2ms",
			StallTimeout:       "0",
		}

		var (
			mu     sync.Mutex
			events []ProgressEvent
		)
		if err := Download(context.Background(), Job{Repo: "o/r", Revision: "main"}, cfg, func(evt ProgressEvent) {
			if evt.Event == "file_finalizing" {
				mu.Lock()
				events = append(events, evt)
				mu.Unlock()
			}
		}); err != nil {
			t.Fatalf("Download: %v", err)
		}

		mu.Lock()
		defer mu.Unlock()
		return getReqs(), append([]ProgressEvent(nil), events...)
	}

	t.Run("one connection downloads in a single stream", func(t *testing.T) {
		reqs, events := run(t, 1)
		var gets, ranged int
		for _, rq := range reqs {
			if rq.method == http.MethodGet {
				gets++
				if rq.hasRange {
					ranged++
				}
			}
		}
		if gets != 1 || ranged != 0 {
			t.Fatalf("file requests = %d GET (%d ranged), want 1 GET with no Range header: %+v", gets, ranged, reqs)
		}
		for _, evt := range events {
			if evt.Stage == "assembling" {
				t.Errorf("single-stream download reported an assembling stage: %+v", evt)
			}
		}
	})

	t.Run("several connections still use ranges", func(t *testing.T) {
		reqs, events := run(t, 4)
		ranged := 0
		for _, rq := range reqs {
			if rq.method == http.MethodGet && rq.hasRange {
				ranged++
			}
		}
		if ranged < 2 {
			t.Fatalf("expected ranged part requests with 4 connections, got %+v", reqs)
		}
		// Fresh runs write each range straight into the final temp file, so there
		// is no stitching pass left to announce for a 4 KiB payload.
		for _, evt := range events {
			if evt.Stage == "assembling" {
				t.Errorf("multipart download announced an assembling stage: %+v", evt)
			}
		}
	})
}

// TestDownloadMultipartHashed_DirectWritePublishesWithoutAssembly pins the
// no-stitch path: a fresh multipart run places every range at its final offset
// inside one temp file and publishes that file with a rename. The payload must
// never be rewritten, so there is no assembling stage to announce and no digest
// to hand back (verification re-reads the file once, with byte progress).
func TestDownloadMultipartHashed_DirectWritePublishesWithoutAssembly(t *testing.T) {
	full := make([]byte, 20000)
	for i := range full {
		full[i] = byte(i*11 + i/199)
	}

	srv, getReqs := finalizeRecorderServer(t, full)
	dst := filepath.Join(t.TempDir(), "tmp-direct")

	var (
		mu     sync.Mutex
		events []ProgressEvent
	)
	emit := func(evt ProgressEvent) {
		mu.Lock()
		events = append(events, evt)
		mu.Unlock()
	}

	var assembled string
	cfg := Settings{Concurrency: 4, Retries: 1, BackoffInitial: "1ms", BackoffMax: "2ms", StallTimeout: "0"}
	it := PlanItem{RelativePath: "big.bin", URL: srv.URL + "/big.bin", Size: int64(len(full)), AcceptRanges: true}
	if err := downloadMultipartHashed(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, emit, &assembled); err != nil {
		t.Fatalf("downloadMultipartHashed: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if !bytes.Equal(got, full) {
		t.Fatalf("direct write content mismatch: got len=%d want len=%d", len(got), len(full))
	}
	if assembled != "" {
		t.Errorf("direct mode published an assembly digest %q; verification must re-read instead", assembled)
	}
	if left, _ := filepath.Glob(dst + ".part-*"); len(left) != 0 {
		t.Errorf("direct mode created per-part files: %v", left)
	}
	if _, err := os.Stat(multipartLayoutMetaPath(dst)); !os.IsNotExist(err) {
		t.Errorf("resume layout left behind after publish: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, evt := range events {
		if evt.Stage == "assembling" {
			t.Errorf("direct mode announced an assembling stage: %+v", evt)
		}
	}
	if ranged := len(getReqs()); ranged == 0 {
		t.Fatal("no requests recorded")
	}
}

// TestDownloadMultipartHashed_DirectResumeUsesPersistedOffsets pins the resume
// half of the direct layout: the per-part offsets recorded by an interrupted
// run decide both where the write resumes inside the shared temp file and which
// Range the request asks for, so already-placed bytes are never re-fetched.
func TestDownloadMultipartHashed_DirectResumeUsesPersistedOffsets(t *testing.T) {
	full := make([]byte, 40000)
	for i := range full {
		full[i] = byte(i*3 + i/97)
	}

	srv, getReqs := finalizeRecorderServer(t, full)
	dst := filepath.Join(t.TempDir(), "tmp-direct-resume")

	// Recreate the state an interrupted run leaves behind: a preallocated target
	// whose first half of every 10000-byte range holds real bytes, plus the
	// layout that says so.
	const chunk = 10000
	const placed = 5000
	target, err := os.OpenFile(multipartTargetPath(dst), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatalf("open target: %v", err)
	}
	if err := target.Truncate(int64(len(full))); err != nil {
		t.Fatalf("preallocate target: %v", err)
	}
	for i := 0; i < 4; i++ {
		start := int64(i) * chunk
		if _, err := target.WriteAt(full[start:start+placed], start); err != nil {
			t.Fatalf("seed target range %d: %v", i, err)
		}
	}
	if err := target.Close(); err != nil {
		t.Fatalf("close target: %v", err)
	}

	layout := buildMultipartResumeLayout(int64(len(full)), 4, chunk)
	layout.Mode = multipartModeDirect
	for i := range layout.Parts {
		layout.Parts[i].Written = placed
	}
	if err := writeMultipartResumeLayout(dst, layout); err != nil {
		t.Fatalf("seed layout: %v", err)
	}

	var assembled string
	cfg := Settings{Concurrency: 4, Retries: 0, BackoffInitial: "1ms", BackoffMax: "2ms", StallTimeout: "0"}
	it := PlanItem{RelativePath: "big.bin", URL: srv.URL + "/big.bin", Size: int64(len(full)), AcceptRanges: true}
	if err := downloadMultipartHashed(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, func(ProgressEvent) {}, &assembled); err != nil {
		t.Fatalf("downloadMultipartHashed: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if !bytes.Equal(got, full) {
		t.Fatal("resumed direct write produced wrong bytes")
	}

	// Every request must ask for the tail of its range, never the whole range:
	// a request starting at a part boundary would mean the recorded offsets were
	// ignored and the already-placed bytes were fetched a second time.
	seen := 0
	for _, rq := range getReqs() {
		if rq.method != http.MethodGet {
			continue
		}
		if !rq.hasRange {
			t.Errorf("resume issued a full GET instead of a range: %+v", rq)
			continue
		}
		var start, end int64
		if _, err := fmt.Sscanf(rq.rangeHeader, "bytes=%d-%d", &start, &end); err != nil {
			t.Errorf("unparsable Range header %q: %v", rq.rangeHeader, err)
			continue
		}
		if start%chunk == 0 {
			t.Errorf("resume restarted range at its boundary: %q", rq.rangeHeader)
		}
		if end != start+(chunk-placed)-1 {
			t.Errorf("resume asked for the wrong tail: %q", rq.rangeHeader)
		}
		seen++
	}
	if seen != 4 {
		t.Fatalf("recorded %d ranged part requests, want 4: %+v", seen, getReqs())
	}
	if left, _ := filepath.Glob(dst + ".part-*"); len(left) != 0 {
		t.Errorf("direct resume created per-part files: %v", left)
	}
}
