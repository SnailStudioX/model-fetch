// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bashrusakh/modelfetch/internal/hubsource"
)

// progressReader wraps an io.Reader and emits progress events during reads.
type progressReader struct {
	reader     io.Reader
	total      int64
	downloaded int64
	path       string
	emit       func(ProgressEvent)
	lastEmit   time.Time
	interval   time.Duration
}

func newProgressReader(r io.Reader, total int64, path string, emit func(ProgressEvent)) *progressReader {
	return &progressReader{
		reader:   r,
		total:    total,
		path:     path,
		emit:     emit,
		lastEmit: time.Now(),
		interval: 200 * time.Millisecond, // Emit at most 5 times per second
	}
}

func (pr *progressReader) Read(p []byte) (n int, err error) {
	n, err = pr.reader.Read(p)
	if n > 0 {
		pr.downloaded += int64(n)
		// Throttle emissions to avoid flooding
		if time.Since(pr.lastEmit) >= pr.interval || err == io.EOF {
			pr.emit(ProgressEvent{
				Event:      "file_progress",
				Path:       pr.path,
				Downloaded: pr.downloaded,
				Total:      pr.total,
			})
			pr.lastEmit = time.Now()
		}
	}
	return n, err
}

type multipartResumeLayout struct {
	Size        int64                `json:"size"`
	Concurrency int                  `json:"concurrency"`
	Mode        string               `json:"mode,omitempty"`
	Parts       []multipartPartRange `json:"parts"`
}

// Written belongs to the direct layout only: it records how many bytes of that
// part's range are already present in the shared temp file. Legacy layouts leave
// it zero and take the part size from the part file itself.
type multipartPartRange struct {
	Index   int   `json:"index"`
	Start   int64 `json:"start"`
	End     int64 `json:"end"`
	Written int64 `json:"written,omitempty"`
}

func multipartLayoutMetaPath(dst string) string {
	return dst + ".parts.json"
}

func buildMultipartResumeLayout(size int64, n int, chunk int64) multipartResumeLayout {
	layout := multipartResumeLayout{
		Size:        size,
		Concurrency: n,
		Parts:       make([]multipartPartRange, n),
	}
	for i := 0; i < n; i++ {
		start := int64(i) * chunk
		end := start + chunk - 1
		if i == n-1 {
			end = size - 1
		}
		layout.Parts[i] = multipartPartRange{Index: i, Start: start, End: end}
	}
	return layout
}

// stagingClaims holds the blob staging paths this process is writing right now.
//
// A staging file is named after the blob's SHA-256, so two overlapping writers
// of the same blob — a retry whose previous run has not finished unwinding, or a
// second job for the same repository — open the very same file and each appends
// at the offset its own cursor reports. Neither sees the other's bytes, and the
// result is a file whose payload is shifted, not merely short. Only the second
// claimer moves to a numbered sibling, so a paused run's partial file keeps
// being resumed by its natural name.
var stagingClaims = struct {
	mu    sync.Mutex
	taken map[string]bool
}{}

// claimStagingPath returns a staging path no other goroutine in this process is
// writing, plus the call that releases it when this file's work is done.
func claimStagingPath(path string) (string, func()) {
	stagingClaims.mu.Lock()
	if stagingClaims.taken == nil {
		stagingClaims.taken = make(map[string]bool)
	}
	claimed := path
	for i := 2; stagingClaims.taken[claimed]; i++ {
		claimed = fmt.Sprintf("%s-%d", path, i)
	}
	stagingClaims.taken[claimed] = true
	stagingClaims.mu.Unlock()

	return claimed, func() {
		stagingClaims.mu.Lock()
		delete(stagingClaims.taken, claimed)
		stagingClaims.mu.Unlock()
	}
}

func multipartPartFiles(dst string) ([]string, error) {
	dir := filepath.Dir(dst)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	prefix := filepath.Base(dst) + ".part-"
	files := make([]string, 0)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		files = append(files, filepath.Join(dir, entry.Name()))
	}
	return files, nil
}

func removeMultipartResumeFiles(dst string) error {
	files, err := multipartPartFiles(dst)
	if err != nil {
		return err
	}
	for _, p := range files {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := os.Remove(multipartLayoutMetaPath(dst)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func removeUnexpectedMultipartPartFiles(dst string, layout multipartResumeLayout) error {
	files, err := multipartPartFiles(dst)
	if err != nil {
		return err
	}
	expected := make(map[string]struct{}, len(layout.Parts))
	for _, part := range layout.Parts {
		expected[fmt.Sprintf("%s.part-%02d", dst, part.Index)] = struct{}{}
	}
	for _, p := range files {
		if _, ok := expected[p]; ok {
			continue
		}
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func writeMultipartResumeLayout(dst string, layout multipartResumeLayout) error {
	data, err := json.Marshal(layout)
	if err != nil {
		return err
	}
	metaPath := multipartLayoutMetaPath(dst)
	tmpPath := metaPath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpPath, metaPath)
}

func prepareMultipartResume(dst string, layout multipartResumeLayout) error {
	files, err := multipartPartFiles(dst)
	if err != nil {
		return err
	}
	if len(files) > 0 {
		data, err := os.ReadFile(multipartLayoutMetaPath(dst))
		if err != nil {
			if !os.IsNotExist(err) {
				return err
			}
			if err := removeMultipartResumeFiles(dst); err != nil {
				return err
			}
			return writeMultipartResumeLayout(dst, layout)
		}

		var existing multipartResumeLayout
		if err := json.Unmarshal(data, &existing); err != nil || !multipartLayoutsEqual(existing, layout) {
			if err := removeMultipartResumeFiles(dst); err != nil {
				return err
			}
			return writeMultipartResumeLayout(dst, layout)
		}
		if err := removeUnexpectedMultipartPartFiles(dst, layout); err != nil {
			return err
		}
	}

	return writeMultipartResumeLayout(dst, layout)
}

// multipartLayoutsEqual compares the part plan only. Written is live progress,
// not part of the plan, so it never decides whether a layout still matches.
func multipartLayoutsEqual(a, b multipartResumeLayout) bool {
	if a.Size != b.Size || a.Concurrency != b.Concurrency || a.Mode != b.Mode || len(a.Parts) != len(b.Parts) {
		return false
	}
	for i := range a.Parts {
		if a.Parts[i].Index != b.Parts[i].Index ||
			a.Parts[i].Start != b.Parts[i].Start ||
			a.Parts[i].End != b.Parts[i].End {
			return false
		}
	}
	return true
}

// Download scans and downloads files from a HuggingFace repo.
//
// v3.0+: Files are stored in HuggingFace Hub cache structure by default:
//   - Blobs: hub/models--{owner}--{repo}/blobs/{sha256}
//   - Snapshots: hub/models--{owner}--{repo}/snapshots/{commit}/{path} (symlinks)
//   - Friendly: models/{owner}/{repo}/{path} (symlinks)
//
// Legacy mode (OutputDir set): Falls back to flat directory structure.
//
// Cancellation: all loops/sleeps/requests are tied to ctx for fast abort.
func Download(ctx context.Context, job Job, cfg Settings, progress ProgressFunc) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := validate(job, cfg); err != nil {
		return err
	}

	src := hubsource.Resolve(cfg.Source, cfg.Endpoint)

	// Apply defaults
	if job.Revision == "" {
		job.Revision = src.DefaultRevision()
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 8
	}
	if cfg.MaxActiveDownloads <= 0 {
		cfg.MaxActiveDownloads = runtime.GOMAXPROCS(0)
	}

	// Resolve the shared speed limiter. A caller-supplied SpeedLimiter (the
	// server's process-wide limiter) wins; otherwise build a per-download one
	// from MaxSpeed. When neither is set, cfg.SpeedLimiter stays nil and the
	// copy sites read at full speed.
	if cfg.SpeedLimiter == nil {
		if bps := ParseSize(cfg.MaxSpeed); bps > 0 {
			cfg.SpeedLimiter = NewRateLimiter(bps)
		}
	}

	// Determine storage mode: HF cache (new) vs flat directory (legacy)
	// Use HF cache mode when:
	// 1. --cache-dir is explicitly set, OR
	// 2. --output is NOT set (default to HF cache)
	useHFCache := cfg.CacheDir != "" || cfg.OutputDir == ""
	var hfCache *HFCache
	var repoDir *RepoDir

	if useHFCache {
		var err error
		hfCache, err = cfg.BuildHFCache()
		if err != nil {
			return fmt.Errorf("build hf cache: %w", err)
		}
		repoType := RepoTypeModel
		if job.IsDataset {
			repoType = RepoTypeDataset
		}
		// LocalRepo overrides the cache folder name: files fetched from an
		// upstream repo are stored under the target model's cache directory
		// instead of the upstream repo's own directory.
		cacheRepoID := job.Repo
		if job.LocalRepo != "" {
			cacheRepoID = job.LocalRepo
		}
		repoDir, err = hfCache.Repo(cacheRepoID, repoType)
		if err != nil {
			return fmt.Errorf("create repo dir: %w", err)
		}
		if err := repoDir.EnsureDirs(); err != nil {
			return fmt.Errorf("ensure repo dirs: %w", err)
		}
	} else {
		// Legacy mode: use OutputDir
		if cfg.OutputDir == "" {
			cfg.OutputDir = "Storage"
		}
	}

	thresholdBytes, err := parseSizeString(cfg.MultipartThreshold, 256<<20)
	if err != nil {
		return fmt.Errorf("invalid multipart-threshold: %w", err)
	}

	// Large files share a per-destination arbiter so that one file's whole-file
	// local pass (SHA-256 re-read, cache publish) never overlaps the network
	// transfers of sibling files on the same disk.
	arbiterRoot := cfg.OutputDir
	if useHFCache {
		arbiterRoot = hfCache.Root
	}
	arbiter := diskArbiterFor(arbiterRoot)

	httpc := buildHTTPClientWithProxy(cfg.Proxy)

	emit := func(ev ProgressEvent) {
		if progress != nil {
			if ev.Time.IsZero() {
				ev.Time = time.Now()
			}
			if ev.Repo == "" {
				ev.Repo = job.Repo
			}
			if ev.Revision == "" {
				ev.Revision = job.Revision
			}
			progress(ev)
		}
	}

	emit(ProgressEvent{Event: "scan_start", Message: "scanning repo"})

	plan, err := scanRepoResolved(ctx, httpc, cfg.Token, &job, cfg)
	if err != nil {
		return err
	}
	if plan.CommitIsAlias {
		emit(ProgressEvent{Level: "warn", Event: "warning", Message: fmt.Sprintf("this hub reported no commit for revision %q, so the cache entry is filed under the revision name instead of a commit hash", job.Revision)})
	}
	if len(plan.Items) == 0 {
		if plan.ScannedFiles > 0 {
			// The hub reported files and this job's own selection kept none of them,
			// so nothing will move. Ending green would say the download happened.
			return emptySelectionError(job, plan)
		}
		// An empty repository is nothing to do, not a failure - the plan is still
		// reported to the caller below so the scan is known to have finished. It is
		// not something the caller should have to infer from a green job either.
		emit(ProgressEvent{Level: "warn", Event: "warning", Message: emptyRepositoryMessage(job, plan)})
	}
	if cfg.OnPlanComplete != nil {
		completedPlan := *plan
		completedPlan.Items = append([]PlanItem(nil), plan.Items...)
		cfg.OnPlanComplete(completedPlan)
	}

	// Emit ALL plan_item events upfront so TUI knows total size immediately
	for _, item := range plan.Items {
		displayRel := item.RelativePath
		if job.AppendFilterSubdir && item.Subdir != "" {
			displayRel = filepath.ToSlash(filepath.Join(item.Subdir, item.RelativePath))
		}
		emit(ProgressEvent{Event: "plan_item", Path: displayRel, Total: item.Size})
	}

	// Ensure destination root exists (only for legacy mode)
	// HF cache mode already created directories via repoDir.EnsureDirs()
	if !useHFCache {
		base, err := destinationBase(job, cfg)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(base, 0o755); err != nil {
			return err
		}
	}

	// Overall concurrency limiter (ctx-aware acquisition)
	type token struct{}
	lim := make(chan token, cfg.MaxActiveDownloads)

	var wg sync.WaitGroup
	errCh := make(chan error, len(plan.Items))

	// Job-scoped cancellable context. Every per-file context derives from it so
	// that a permanent (fail-fast) file/part error — or a job-fatal security
	// rejection — can terminate the whole job promptly instead of letting
	// sibling files/parts keep downloading, retrying, or waiting until
	// wg.Wait() returns. The cancel carries the original error as its cause, so
	// a sibling that exits via its ctx path can tell an internal abort
	// (preserve resumable bytes) apart from a user cancel/pause.
	jobCtx, jobCancel := context.WithCancelCause(ctx)
	defer jobCancel(nil)

	// fatalErr records the first job-fatal error. It is surfaced to the caller
	// in preference to the errCh drain so a sibling goroutine's "context
	// canceled" (produced while unwinding from the abort) can never mask or
	// precede the original permanent error.
	var (
		fatalMu  sync.Mutex
		fatalErr error
	)
	setFatalErr := func(err error) {
		fatalMu.Lock()
		if fatalErr == nil {
			fatalErr = err
		}
		fatalMu.Unlock()
		jobCancel(abortError(err))
	}

	// To print "skip" only once per final path per run
	var skipOnce sync.Map

	var skippedCount int64
	var downloadedCount int64

	// Build manifest during download (thread-safe)
	// Manifest is always written unless explicitly disabled with NoManifest
	var manifestBuilder *ManifestBuilder
	var manifestMu sync.Mutex
	if useHFCache && !cfg.NoManifest {
		manifestBuilder = NewManifestBuilder(job, cfg.Command)
		manifestBuilder.SetCommit(plan.Commit)
		// RepoPath describes the selected physical repository, relative to the
		// friendly root when possible (absolute on different Windows volumes).
		rel, err := filepath.Rel(hfCache.Root, repoDir.Path())
		if err != nil {
			rel = repoDir.Path()
		}
		manifestBuilder.manifest.RepoPath = filepath.ToSlash(rel)
	}

LOOP:
	for _, item := range plan.Items {
		// Stop scheduling more work once canceled
		select {
		case <-jobCtx.Done():
			break LOOP
		default:
		}

		it := item // capture for goroutine

		// Acquire a slot or abort if canceled
		select {
		case lim <- token{}:
		case <-jobCtx.Done():
			break LOOP
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-lim }()

			// Per-file context; ensures all inner loops stop on cancellation.
			// It derives from jobCtx so a fatal error in a sibling file also
			// stops this one promptly. The stream-owner tag puts every range
			// and part request for this file into one fairness bucket on the
			// per-host stream gate, so files share the host budget fairly
			// instead of racing goroutine-by-goroutine (see streamgate.go).
			fileCtx, fileCancel := context.WithCancel(withStreamOwner(jobCtx, streamOwnerFor(job, it)))
			defer fileCancel()

			finalRel := it.RelativePath
			filterSubdir := ""
			if job.AppendFilterSubdir && it.Subdir != "" {
				filterSubdir = it.Subdir
				finalRel = filepath.ToSlash(filepath.Join(it.Subdir, it.RelativePath))
			}

			var dst string
			var skipCheck func() (bool, string, error)

			if useHFCache {
				// HF Cache mode: check blob existence
				skipCheck = func() (bool, string, error) {
					if it.SHA256 != "" {
						status, _, err := repoDir.CheckBlob(it.SHA256)
						if err != nil {
							return false, "", err
						}
						if status == BlobComplete {
							// Blob exists, but ensure symlinks are in place
							if err := repoDir.createSnapshotSymlink(plan.Commit, it.RelativePath, it.SHA256); err == nil {
								if !cfg.NoFriendlyView {
									repoDir.CreateFriendlySymlink(plan.Commit, it.RelativePath, filterSubdir)
								}
							}
							return true, "blob exists", nil
						}
						if status == BlobDownloading {
							return true, "downloading by another process", nil
						}
					}
					return false, "", nil
				}
				// Download to temp location, will be moved to blob later
				// Use SHA256 as temp name to avoid collisions (e.g., multiple config.json files)
				tmpName := "tmp-" + it.SHA256
				if it.SHA256 == "" {
					// Fallback: sanitize path to avoid collisions
					tmpName = "tmp-" + strings.ReplaceAll(it.RelativePath, "/", "_")
				}
				// Guard: SafeJoin so the remote-controlled temp name (SHA256
				// or path-derived) cannot escape the blobs directory — the
				// same containment the legacy branch applies to finalRel.
				// SafeJoin's filepath.IsLocal check also makes this barrier
				// visible to CodeQL's go/path-injection analysis.
				safeDst, err := SafeJoin(repoDir.BlobsDir(), tmpName)
				if err != nil {
					fatal := fmt.Errorf("path traversal: %q would escape blobs directory", tmpName)
					setFatalErr(fatal)
					select {
					case errCh <- fatal:
					default:
					}
					return
				}
				dst = safeDst
				// Claim it for the duration of this file's work: two concurrent
				// writers of the same blob would otherwise append into one staging
				// file at offsets each derived from its own cursor.
				var releaseStaging func()
				dst, releaseStaging = claimStagingPath(dst)
				defer releaseStaging()
			} else {
				// Legacy mode: flat directory structure
				base, err := destinationBase(job, cfg)
				if err != nil {
					fatal := fmt.Errorf("path traversal: %w", err)
					setFatalErr(fatal)
					select {
					case errCh <- fatal:
					default:
					}
					return
				}
				// Guard: SafeJoin so finalRel (from HF Hub) cannot escape the
				// output directory via path traversal.
				safeDst, err := SafeJoin(base, finalRel)
				if err != nil {
					fatal := fmt.Errorf("path traversal: %q would escape output directory", finalRel)
					setFatalErr(fatal)
					select {
					case errCh <- fatal:
					default:
					}
					return
				}
				dst = safeDst
				skipCheck = func() (bool, string, error) {
					return shouldSkipLocalCtx(fileCtx, it, dst)
				}
			}

			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				select {
				case errCh <- err:
				default:
				}
				return
			}

			// Check if we can skip
			alreadyOK, reason, err := skipCheck()
			if err != nil {
				select {
				case errCh <- err:
				default:
				}
				return
			}
			if alreadyOK {
				if _, loaded := skipOnce.LoadOrStore(finalRel, struct{}{}); !loaded {
					emit(ProgressEvent{Event: "file_done", Path: finalRel, Message: "skip (" + reason + ")"})
					atomic.AddInt64(&skippedCount, 1)
					// Add to manifest (skipped files are still part of the download job)
					if manifestBuilder != nil {
						manifestMu.Lock()
						manifestBuilder.AddFile(it.RelativePath, it.SHA256, it.Size, it.LFS)
						manifestMu.Unlock()
					}
				}
				return
			}

			// Register the per-file destination with the caller's in-flight
			// tracker before any bytes hit disk. If the goroutine exits
			// (cancel, pause, error) before the matching finalize call below,
			// the dst stays in the caller's set and is the exact set of
			// partial files the caller may safely remove.
			if cfg.OnPartialFile != nil {
				cfg.OnPartialFile(dst, false)
			}

			emit(ProgressEvent{Event: "file_start", Path: finalRel, Total: it.Size})

			// Create a copy with updated RelativePath for progress display
			itForIO := it
			itForIO.RelativePath = finalRel

			// Choose single/multipart path. With a single connection there is
			// nothing to parallelize, and the multipart path would still write
			// the payload to a part file and then rewrite it once more during
			// assembly, so those files stay on the single-stream path.
			// Arbitrated files announce the transfer phase here and the local
			// phase below. Small files never enter the arbiter: their local
			// passes are too short to matter, and they must never be able to
			// delay a sibling's start.
			arbitrate := it.Size >= diskArbiterMinSize
			if arbitrate {
				if err := arbiter.beginTransfer(fileCtx); err != nil {
					select {
					case errCh <- fmt.Errorf("download %s: %w", finalRel, err):
					default:
					}
					return
				}
			}

			var dlErr error
			var assembledSHA string
			if it.Size >= thresholdBytes && it.AcceptRanges && cfg.Concurrency > 1 {
				dlErr = downloadMultipartHashed(fileCtx, httpc, cfg.Token, job, cfg, itForIO, dst, emit, &assembledSHA)
			} else {
				dlErr = downloadSingle(fileCtx, httpc, cfg.Token, job, cfg, itForIO, dst, emit)
			}
			if arbitrate {
				arbiter.endTransfer()
			}
			if dlErr != nil {
				fatal := fmt.Errorf("download %s: %w", finalRel, dlErr)
				if isFailFastError(dlErr) {
					setFatalErr(fatal)
				}
				select {
				case errCh <- fatal:
				default:
				}
				return
			}

			// The bytes are on disk, but SHA-256 verification may still reread
			// the whole file and storing it into the cache may copy it again —
			// minutes of local I/O for a large model, during which no
			// file_progress arrives. The stage emitter below marks the file as
			// finalizing and reports that re-read as byte progress; when the
			// multipart assembly already hashed the payload there is nothing
			// left to read, so the marker emitted below stays the only event.
			// The marker is unconditional on purpose: it is how the caller learns
			// the transfer is over and that no further file_progress will arrive,
			// including when verification is off and only the cache store copy is
			// left, which reports no byte progress of its own.
			// Everything below re-reads or rewrites whole files, so it takes the
			// disk for itself: the deferred release runs when this file's
			// goroutine finishes storing it.
			if arbitrate {
				if err := arbiter.beginLocalPass(fileCtx); err != nil {
					select {
					case errCh <- fmt.Errorf("verify %s: %w", finalRel, err):
					default:
					}
					return
				}
				defer arbiter.endLocalPass()
			}

			emit(ProgressEvent{Event: "file_finalizing", Path: finalRel, Stage: "verifying", Message: "verifying"})
			verifyStage := &stageProgress{
				path:    finalRel,
				stage:   "verifying",
				message: "verifying",
				total:   it.Size,
				emit:    emit,
			}

			// Verify after download
			if it.LFS && it.SHA256 != "" {
				if err := verifyDigestProgress(fileCtx, dst, it.SHA256, assembledSHA, verifyStage); err != nil {
					select {
					case errCh <- fmt.Errorf("sha256 verify failed: %s: %w", finalRel, err):
					default:
					}
					return
				}
			} else if cfg.Verify == "size" && it.Size > 0 {
				fi, err := os.Stat(dst)
				if err != nil || fi.Size() != it.Size {
					select {
					case errCh <- fmt.Errorf("size mismatch for %s", finalRel):
					default:
					}
					return
				}
			} else if cfg.Verify == "sha256" && src.HeadForSHA() {
				_, remoteSha, herr := headForETag(fileCtx, httpc, cfg.Token, itForIO)
				if herr != nil {
					// headForETag maps every non-2xx status (401/403
					// included) to an optional-metadata miss with no error,
					// so the ordinary miss path falls through and the actual
					// file request stays authoritative. Only a genuine
					// caller cancellation surfaces here, and it must fail
					// this file promptly instead of pretending verification
					// succeeded; a fail-fast status error, should one ever
					// surface, is escalated job-fatal below.
					if isFailFastError(herr) {
						setFatalErr(fmt.Errorf("verify head %s: %w", finalRel, herr))
					}
					select {
					case errCh <- fmt.Errorf("verify head %s: %w", finalRel, herr):
					default:
					}
					return
				}
				if remoteSha != "" {
					if err := verifyDigestProgress(fileCtx, dst, remoteSha, assembledSHA, verifyStage); err != nil {
						select {
						case errCh <- fmt.Errorf("sha256 verify failed: %s: %w", finalRel, err):
						default:
						}
						return
					}
				}
			} else if cfg.Verify == "sha256" && it.Size > 0 {
				// A hub whose resolve endpoint exposes neither ETag nor
				// x-amz-meta-sha256 cannot supply the expected digest through
				// HEAD. Size is then the only trusted signal for a file that
				// carries no hash in the tree listing.
				fi, err := os.Stat(dst)
				if err != nil || fi.Size() != it.Size {
					select {
					case errCh <- fmt.Errorf("size mismatch for %s", finalRel):
					default:
					}
					return
				}
			}

			// For HF Cache mode: move to blob and create symlinks. The store
			// re-hashes/copies a large file, so pass the per-file context: a
			// sibling's fail-fast abort must not wait for it to finish.
			var finalSHA256 string
			if useHFCache {
				sha := it.SHA256
				result, err := repoDir.storeDownloadedFileCtx(fileCtx, dst, it.RelativePath, plan.Commit, sha, filterSubdir, cfg.NoFriendlyView)
				if err != nil {
					select {
					case errCh <- fmt.Errorf("store file %s: %w", finalRel, err):
					default:
					}
					return
				}
				finalSHA256 = result.SHA256 // Use computed SHA256 from store result
			} else {
				finalSHA256 = it.SHA256
			}

			// File is now at its final location; the partial (dst in HF
			// cache, dst+".part*" in legacy) is gone. Deregister so the
			// caller's in-flight set reflects "this dst is no longer
			// partial". Errors below this point do not un-finalize; the
			// file is on disk and the only thing at risk is metadata.
			if cfg.OnPartialFile != nil {
				cfg.OnPartialFile(dst, true)
			}

			// Add to manifest with actual LFS info from API and final SHA256
			if manifestBuilder != nil {
				manifestMu.Lock()
				manifestBuilder.AddFile(it.RelativePath, finalSHA256, it.Size, it.LFS)
				manifestMu.Unlock()
			}

			emit(ProgressEvent{Event: "file_done", Path: finalRel})
			atomic.AddInt64(&downloadedCount, 1)
		}()
	}

	wg.Wait()
	close(errCh)

	// Drain errors, but remember every error so the job-fatal error (if any)
	// can take precedence over a sibling goroutine's "context canceled" that
	// happened to land in errCh first.
	drainedErr := error(nil)
	for e := range errCh {
		if e != nil && drainedErr == nil {
			drainedErr = e
		}
	}

	fatalMu.Lock()
	jobFatal := fatalErr
	fatalMu.Unlock()

	// The original permanent (fail-fast) error is surfaced in preference to
	// anything a sibling produced while unwinding from the abort.
	if jobFatal != nil {
		emit(ProgressEvent{Level: "error", Event: "error", Message: jobFatal.Error()})
		return jobFatal
	}
	if drainedErr != nil {
		emit(ProgressEvent{Level: "error", Event: "error", Message: drainedErr.Error()})
		return drainedErr
	}

	if ctx.Err() != nil {
		return ctx.Err()
	}

	// Post-download finalization (refs, building the friendly view, rebuild
	// script, manifest) can take a while — copying the friendly view on
	// filesystems without symlink support is the slow part — so signal the
	// phase to the UI instead of leaving the job stuck at 100%.
	if useHFCache && repoDir != nil && !cfg.NoFriendlyView {
		emit(ProgressEvent{Event: "finalizing", Message: "finalizing"})
	}

	// For HF Cache mode: write ref file and ensure friendly directory exists
	if useHFCache && repoDir != nil {
		// Write refs/main (or the revision used)
		ref := job.Revision
		if ref == "" {
			ref = src.DefaultRevision()
		}
		if err := repoDir.WriteRef(ref, plan.Commit); err != nil {
			emit(ProgressEvent{Level: "warn", Event: "warning", Message: fmt.Sprintf("failed to write ref: %v", err)})
		}
		// Ensure friendly directory structure exists (unless disabled)
		if !cfg.NoFriendlyView {
			if err := repoDir.EnsureFriendlyDir(); err != nil {
				emit(ProgressEvent{Level: "warn", Event: "warning", Message: fmt.Sprintf("failed to create friendly dir: %v", err)})
			}
			// Windows without Developer Mode (or elevation) cannot create the
			// snapshot and friendly-view symlinks, so hfcache skips them
			// unconditionally on this platform and reports success. The job would
			// otherwise end with "download complete" while
			// models/<owner>/<name>/ holds only hfd.yaml; warnWindowsSymlink
			// writes to stderr, which the dashboard never sees.
			if isWindows() {
				emit(ProgressEvent{Level: "warn", Event: "warning", Message: fmt.Sprintf("this build does not create friendly-view links on Windows: %s will hold only hfd.yaml, the files are in %s", repoDir.FriendlyPath(), repoDir.BlobsDir())})
			}
		}
	}

	// Write/update the rebuild shell script if using HF cache (unless friendly view disabled)
	if hfCache != nil && !cfg.NoFriendlyView {
		if _, err := hfCache.WriteRebuildScript(); err != nil {
			emit(ProgressEvent{Level: "warn", Event: "warning", Message: fmt.Sprintf("failed to write rebuild script: %v", err)})
		}
	}

	// Write manifest file (hfd.yaml) if using HF cache (unless friendly view disabled)
	if manifestBuilder != nil && repoDir != nil && !cfg.NoFriendlyView {
		manifest := manifestBuilder.Build()
		if _, err := manifest.Write(repoDir.FriendlyPath()); err != nil {
			emit(ProgressEvent{Level: "warn", Event: "warning", Message: fmt.Sprintf("failed to write manifest: %v", err)})
		}
	}

	emit(ProgressEvent{
		Event:   "done",
		Message: fmt.Sprintf("download complete (downloaded %d, skipped %d)", downloadedCount, skippedCount),
	})
	return nil
}

// cleanupPartialsOnCancel removes partial download artifacts (.part,
// .part-NN, .parts.json) for dst when the caller's cancel-cleanup
// callback reports that the context was cancelled for a non-resumable
// reason (explicit cancel, not pause).
func cleanupPartialsOnCancel(cfg Settings, dst string) {
	if cfg.CleanupPartialsOnCancel == nil || !cfg.CleanupPartialsOnCancel() {
		return
	}
	// dst is derived upstream in Download() through SafeJoin in both modes:
	// legacy mode joins the remote file path to the output directory, and HF
	// cache mode joins the tmp-<sha> name to the blobs directory. SafeJoin
	// rejects non-local values via filepath.IsLocal and proves containment
	// with PathInside, so dst cannot name a file outside those roots.
	// lgtm[go/path-injection]
	if err := os.Remove(dst + ".part"); err != nil && !os.IsNotExist(err) {
		log.Printf("warning: cleanup partial file %s: %v", dst+".part", err)
	}
	if err := removeMultipartResumeFiles(dst); err != nil {
		log.Printf("warning: cleanup multipart resume files for %s: %v", dst, err)
	}
}

// CleanupJobPartFiles removes the partial download artifacts
// associated with the given list of per-file destinations. The
// caller is responsible for tracking which dsts belong to a given
// run via Settings.OnPartialFile; this helper then removes exactly
// those dsts' partial files without scanning the repo, so a
// concurrent job downloading into the same blobs directory or
// output subtree is not disturbed.
//
// For each dst, the helper removes the partial files the downloader
// would have produced for that dst: dst+".part", dst+".part-NN"
// (any digit run), and dst+".parts.json". In HF cache mode, dst is
// the temporary tmp-<sha> path under blobs/, so the helper also removes
// dst itself for the window after the single/multipart downloader has
// renamed the completed bytes to tmp-<sha> but before StoreDownloadedFile
// has moved them into the final blob.
//
// settings determines the mode (HF cache vs legacy) the same way
// Download() does: HF cache is the default when neither CacheDir nor
// OutputDir is set.
//
// All operations are best-effort: missing files are not an error, and
// individual file removal failures are logged and skipped.
func CleanupJobPartFiles(settings Settings, dsts []string) error {
	useHFCache := settings.CacheDir != "" || settings.OutputDir == ""
	root := cleanupRoot(settings, useHFCache)
	for _, dst := range dsts {
		if !pathWithinRoot(root, dst) {
			log.Printf("warning: skip cleanup for out-of-root partial dst %s", dst)
			continue
		}
		if err := removePartialArtifacts(dst); err != nil {
			log.Printf("warning: cleanup partials for %s: %v", dst, err)
		}
		if useHFCache {
			removeHFCacheTemp(dst)
		}
	}
	return nil
}

func cleanupRoot(settings Settings, useHFCache bool) string {
	if useHFCache {
		cache, err := settings.BuildHFCache()
		if err != nil {
			return "" // Invalid settings must never widen cleanup's root.
		}
		return cache.HubDir()
	}
	return settings.OutputDir
}

func pathWithinRoot(root, path string) bool {
	if root == "" || path == "" {
		return false
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	if resolvedRoot, err := filepath.EvalSymlinks(absRoot); err == nil {
		absRoot = resolvedRoot
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	if resolvedParent, err := filepath.EvalSymlinks(filepath.Dir(absPath)); err == nil {
		absPath = filepath.Join(resolvedParent, filepath.Base(absPath))
	}
	rel, err := filepath.Rel(absRoot, absPath)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// removeHFCacheTemp removes the completed-but-not-yet-stored tmp-<sha>
// file at dst. The downloader normally writes partial bytes to
// dst+".part" / dst+".part-NN" first, then renames assembled bytes to
// dst before StoreDownloadedFile moves them into the final blob. dst is
// allocated in Download() via SafeJoin(blobsDir, tmpName), which rejects
// non-local values (filepath.IsLocal) and proves containment with
// PathInside, and scanRepo rejects malformed remote SHAs before they can
// become a tmpName at all.
func removeHFCacheTemp(dst string) {
	// lgtm[go/path-injection]
	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		log.Printf("warning: cleanup partial file %s: %v", dst, err)
	}
}

// removePartialArtifacts removes all partial-file artifacts associated
// with a single downloader dst: the single-part partial (dst+".part"),
// any multipart parts (dst+".part-NN" for N being any non-empty run of
// digits, to match the downloader's %02d / %d format), and the
// multipart layout metadata (dst+".parts.json").
//
// dst is already validated as residing inside the output subtree at
// allocation time in Download() (via SafeJoin), so removing the
// derived partial paths cannot escape the output directory. The
// glob-style match for .part-NN is intentionally narrow: only the
// recognized suffix family and a digit run, so a coincidentally
// named "weights.part" or similar user file with a different base
// (which would not appear in dsts) is not touched.
func removePartialArtifacts(dst string) error {
	// lgtm[go/path-injection]
	if err := os.Remove(dst + ".part"); err != nil && !os.IsNotExist(err) {
		log.Printf("warning: cleanup partial file %s: %v", dst+".part", err)
	}
	// Multipart parts: glob the dst's directory for files matching
	// dst+".part-NNN" pattern. This matches what downloadMultipart
	// produced (the parts have indices 0..n-1).
	dir := filepath.Dir(dst)
	base := filepath.Base(dst)
	prefix := base + ".part-"
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		suffix := name[len(prefix):]
		if !isAllDigits(suffix) {
			continue
		}
		full := filepath.Join(dir, name)
		if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
			log.Printf("warning: cleanup partial file %s: %v", full, err)
		}
	}
	// Multipart layout metadata.
	if err := os.Remove(dst + ".parts.json"); err != nil && !os.IsNotExist(err) {
		log.Printf("warning: cleanup partial file %s: %v", dst+".parts.json", err)
	}
	return nil
}

// isAllDigits reports whether s is a non-empty run of ASCII digits.
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// RepoTypeFromJob returns the RepoType matching job.IsDataset.
func RepoTypeFromJob(job Job) RepoType {
	if job.IsDataset {
		return RepoTypeDataset
	}
	return RepoTypeModel
}

// isFailFastError reports whether err is a permanent file/part error (an
// *APIError with a fail-fast status: 401/403/404). It is used to distinguish
// "abort the whole job" from ordinary retryable/network failures when
// registering the fatal error for a file.
func isFailFastError(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.IsFailFast()
}

// abortCause wraps an internally-generated fail-fast error used as a context
// cancellation cause. It marks the cancel as an internal abort (preserve
// resumable bytes) independent of the wrapped error's concrete type, which may
// be an *APIError (401/403/404) or a plain error such as the job-level
// path-traversal rejection. Testing a dedicated sentinel rather than the
// error's HTTP status also keeps it distinct from a user cancel/pause.
type abortCause struct{ err error }

func (a *abortCause) Error() string { return a.err.Error() }
func (a *abortCause) Unwrap() error { return a.err }

// abortError wraps err so it can be used as an internal-abort cancel cause.
func abortError(err error) error { return &abortCause{err: err} }

// abortPartAttempt reports whether ctx was canceled by an internal fail-fast
// abort (see abortCause) rather than a user cancel/pause/shutdown. Returning
// without calling cleanupPartialsOnCancel preserves the resumable
// .part/.part-NN/.parts.json bytes for a later retry.
func abortPartAttempt(ctx context.Context) bool {
	var a *abortCause
	return errors.As(context.Cause(ctx), &a)
}

// maxAttemptsPerRetry scales the configured retry budget into the absolute
// per-file/per-part attempt ceiling below. It is deliberately generous: a
// legitimately flaky long download that keeps advancing past many stalls must
// still complete, so the ceiling tolerates many progress refunds. It is a
// package variable (not a public API) so tests can lower it deterministically.
var maxAttemptsPerRetry = 100

// absoluteAttemptCeiling bounds retry attempts over the lifetime of one file or
// part, including retries enabled by progress refunds. The limit is checked
// after each request attempt returns; it does not bound a request that keeps
// delivering data without returning. The high-water-mark rule refunds only
// after non-truncating progress strictly beyond the previous mark. An ignored-
// Range restart can record a higher mark but never refunds the budget, and a
// later bodyless failure earns no refund from that mark. retries<0 is treated
// as 0.
func absoluteAttemptCeiling(retries int) int {
	if retries < 0 {
		retries = 0
	}
	if maxAttemptsPerRetry <= 0 {
		return 0
	}
	maxInt := int(^uint(0) >> 1)
	if retries >= maxInt/maxAttemptsPerRetry {
		return maxInt
	}
	return (retries + 1) * maxAttemptsPerRetry
}

// downloadSingle downloads a file in a single request.
//
// Resume behavior: if a .part file already exists from a previous interrupted
// run, its bytes are preserved and the HTTP request uses a Range header to
// fetch only the remaining bytes. If the server ignores the Range header and
// responds with 200 (full body), the .part file is truncated and the download
// restarts from zero.
// A stalled attempt refunds the retry budget only when it actually moved data.
// The floor is a rate, not a fixed byte count, so it scales with the stall
// window that produced the failure: a short watchdog window that still delivered
// data is a flaky-but-live peer, while a long window that delivered almost
// nothing is a dead stream. Without this, a throttled mirror that accepts a
// connection and then dribbles a few kilobytes per stall window earns an
// unlimited refund and keeps a file alive for hours at effectively zero
// throughput; failing fast reports a stall error the user can act on instead.
const (
	// minStallRefundRate is the throughput, in bytes per second, a stalled
	// attempt must have sustained for its progress to refund the budget.
	minStallRefundRate = 1024
	// minStallRefundFloorBytes keeps a floor for very short stall windows, so a
	// watchdog tuned to fire quickly still requires a real byte transfer.
	minStallRefundFloorBytes = 512
)

// deadStallAttempt reports whether an attempt ended in a stall that delivered
// less progress than the stall window justifies. err is the classified failure
// of the attempt that just ended and gained is the new data it wrote.
func deadStallAttempt(err error, gained int64, stall time.Duration) bool {
	var se *StallError
	if !errors.As(err, &se) {
		return false
	}
	secs := int64(stall / time.Second)
	floor := int64(minStallRefundRate) * secs
	if floor < minStallRefundFloorBytes {
		floor = minStallRefundFloorBytes
	}
	return gained < floor
}

// streamOwnerFor names the per-host stream-gate fairness bucket every request
// for this file competes in. Two jobs pulling the same repo path share a
// bucket on purpose: they are the same logical file, and one file should not
// double its fair share just because it appears twice.
func streamOwnerFor(job Job, it PlanItem) string {
	return fmt.Sprintf("%d|%s|%s|%s", btoi(job.IsDataset), job.Repo, job.Revision, it.RelativePath)
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

func downloadSingle(ctx context.Context, httpc *http.Client, token string, job Job, cfg Settings, it PlanItem, dst string, emit func(ProgressEvent)) error {

	tmp := dst + ".part"

	// A direct-mode multipart run leaves a preallocated temp file behind whose
	// unwritten region reads back as zeros, so its size proves nothing about the
	// bytes it holds. The "already the right size, finalize without network"
	// path below would publish such a file as complete: drop it together with
	// the layout that describes it.
	if data, err := os.ReadFile(multipartLayoutMetaPath(dst)); err == nil {
		var existing multipartResumeLayout
		if json.Unmarshal(data, &existing) == nil && existing.Mode == multipartModeDirect {
			_ = os.Remove(tmp)
			_ = os.Remove(multipartLayoutMetaPath(dst))
		}
	}
	out, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()

	fi, err := out.Stat()
	if err != nil {
		return err
	}
	pos := fi.Size()

	// If the partial is already exactly the right size, finalize without network.
	if it.Size > 0 && pos == it.Size {
		out.Close()
		return os.Rename(tmp, dst)
	}
	// If the partial is larger than expected (stale/corrupt), start over.
	if it.Size > 0 && pos > it.Size {
		if err := out.Truncate(0); err != nil {
			return err
		}
		pos = 0
	}
	if _, err := out.Seek(pos, io.SeekStart); err != nil {
		return err
	}
	if pos > 0 {
		emit(ProgressEvent{Event: "file_progress", Path: it.RelativePath, Downloaded: pos, Total: it.Size})
	}

	retry := newRetry(cfg)
	stall := stallTimeout(cfg)
	attempts := cfg.Retries
	if attempts < 0 {
		attempts = 0
	}
	remaining := attempts
	attempt := 0
	ceiling := absoluteAttemptCeiling(attempts)
	var lastErr error
	// The high-water mark prevents non-progress and ignored-Range restarts from
	// refunding the budget. The separate lifetime retry ceiling limits retries
	// once attempts return; it does not time-bound a request that keeps delivering
	// data. An ignored-Range restart may record a higher mark but cannot earn a
	// refund, so a following bodyless failure also earns none.
	// hwm is the highest offset ever reached for this file, seeded with the
	// initial on-disk offset.
	hwm := pos

	for {
		select {
		case <-ctx.Done():
			out.Close()
			// An internal fail-fast abort must not delete resumable bytes.
			if !abortPartAttempt(ctx) {
				cleanupPartialsOnCancel(cfg, dst)
			}
			return ctx.Err()
		default:
		}

		// Each attempt gets its own cancellable context so the stall
		// watchdog can abort just this attempt (closing the connection or
		// HTTP/2 stream) without cancelling the whole job.
		attemptCtx, attemptCancel := context.WithCancel(ctx)

		req, _ := http.NewRequestWithContext(attemptCtx, "GET", it.URL, nil)
		addAuth(req, token)
		if pos > 0 {
			if it.Size > 0 {
				req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", pos, it.Size-1))
			} else {
				req.Header.Set("Range", fmt.Sprintf("bytes=%d-", pos))
			}
		}

		// waitResp carries a throttling response (429/503) whose Retry-After /
		// RateLimit header must shape the next wait.
		var waitResp *http.Response
		// truncatedRestart is set when the server ignored our Range and
		// answered 200 while resumable bytes were present, forcing a restart
		// from zero. That is destructive rather than progress, so it must not
		// refund the budget (and it bounds the ignored-Range growing-prefix
		// server, which would otherwise advance the high-water mark on every
		// attempt).
		truncatedRestart := false
		// preHwm is the high-water mark as of the start of this attempt; the
		// refund decision compares the final pos against it (see below).
		preHwm := hwm

		resp, err := httpc.Do(req)
		if err != nil {
			lastErr = err
		} else {
			// If we asked for a range but the server returned the whole body,
			// throw away any existing partial bytes and start fresh.
			if pos > 0 && resp.StatusCode == http.StatusOK {
				truncatedRestart = true
				if err := out.Truncate(0); err != nil {
					resp.Body.Close()
					attemptCancel()
					return err
				}
				if _, err := out.Seek(0, io.SeekStart); err != nil {
					resp.Body.Close()
					attemptCancel()
					return err
				}
				pos = 0
			}
			if apiErr := classifyFileResponse(resp, it.URL); apiErr != nil {
				lastErr = apiErr
				resp.Body.Close()
				if apiErr.IsFailFast() {
					// Permanent for the whole job: no retry, no request
					// storm. Remove any empty partial this attempt created
					// before returning the actionable error.
					attemptCancel()
					out.Close()
					removeEmptyPartialFile(tmp)
					return lastErr
				}
				if !apiErr.IsRetryable() {
					attemptCancel()
					out.Close()
					return lastErr
				}
				waitResp = resp
			} else {
				// The stall watchdog wraps the raw network body BEFORE the
				// speed limiter's pacing, so deliberate throttling is not
				// mistaken for a stalled peer.
				body := newStallReader(resp.Body, stall, attemptCancel)
				pr := newProgressReader(cfg.SpeedLimiter.Reader(attemptCtx, body), it.Size, it.RelativePath, emit)
				pr.downloaded = pos // emitted progress reflects cumulative bytes
				_, cerr := io.Copy(out, pr)
				resp.Body.Close()
				attemptCancel()
				if cerr == nil {
					out.Close()
					return os.Rename(tmp, dst)
				}
				// If this attempt's stall watchdog fired, the failure is a
				// stall, not a caller cancellation — classifyStall keeps a
				// done parent context (genuine cancel) unchanged.
				lastErr = classifyStall(ctx, body, cerr)
				// Update pos to current file position so the next retry issues
				// a Range request for the remaining bytes instead of duplicating.
				if cur, serr := out.Seek(0, io.SeekCurrent); serr == nil {
					pos = cur
				}
			}
		}
		attemptCancel()

		// Refund the retry budget only when this attempt moved STRICTLY above
		// the high-water mark captured BEFORE the attempt AND did not truncate
		// previously-resumable bytes (an ignored-Range 200 restart is
		// destructive, not progress). preHwm is read before the attempt because
		// hwm is otherwise updated unconditionally below: a restart that writes
		// past the old mark must be recorded (so it cannot earn a later free
		// refund from a bodyless failure) without itself granting a refund.
		// The ceiling above still bounds how far refunds may extend the budget.
		// A stall that delivered almost nothing is a dead stream rather than
		// progress, so it does not refund either (see deadStallAttempt).
		advanced := pos > preHwm && !truncatedRestart && !deadStallAttempt(lastErr, pos-preHwm, stall)
		// Always record the highest offset reached, regardless of refund
		// eligibility, so a truncating restart's higher offset is never lost.
		if pos > hwm {
			hwm = pos
		}
		if advanced {
			retry.reset()
			remaining = attempts
		}

		if remaining == 0 || attempt >= ceiling {
			if ctx.Err() != nil {
				out.Close()
				if !abortPartAttempt(ctx) {
					cleanupPartialsOnCancel(cfg, dst)
				}
				return ctx.Err()
			}
			break
		}
		remaining--
		attempt++
		emit(ProgressEvent{Event: "retry", Path: it.RelativePath, Attempt: attempt, Message: lastErr.Error()})
		if d := NextRetryWait(waitResp, retry.Next(), DefaultMaxRetryAfter, time.Now()); !sleepCtx(ctx, d) {
			out.Close()
			if !abortPartAttempt(ctx) {
				cleanupPartialsOnCancel(cfg, dst)
			}
			return ctx.Err()
		}
	}
	return lastErr
}

// removeEmptyPartialFile removes path when it exists and is empty. It is used
// to clean up a zero-byte .part file that a fail-fast attempt created, without
// touching a partial that predates the attempt and may hold resumable bytes.
func removeEmptyPartialFile(path string) {
	fi, err := os.Stat(path)
	if err != nil {
		return
	}
	if fi.Size() != 0 {
		return
	}
	// lgtm[go/path-injection]
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		log.Printf("warning: cleanup empty partial file %s: %v", path, err)
	}
}

// assemblyBufferSize is the buffer used to stitch multipart part files back
// into one payload. The copy is purely sequential and multi-GB, so a buffer
// well above io.Copy's default keeps the disk saturated.
const assemblyBufferSize = 1 << 20

// stageReader makes the local finalize window visible. It keeps contextReader's
// prompt stop on cancellation, feeds the running SHA-256 of the payload, and
// emits one "file_finalizing" stage event per second so a multi-GB part stitch
// shows as progress instead of a job sitting silently at 100%.
type stageReader struct {
	ctx   context.Context
	path  string
	total int64
	emit  func(ProgressEvent)
	hash  hash.Hash

	r        io.Reader
	copied   int64
	lastEmit time.Time
}

func (s *stageReader) Read(p []byte) (int, error) {
	if err := s.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := s.r.Read(p)
	if n > 0 {
		s.hash.Write(p[:n])
		s.copied += int64(n)
		if now := time.Now(); now.Sub(s.lastEmit) >= time.Second {
			s.lastEmit = now
			s.emit(ProgressEvent{
				Event:      "file_finalizing",
				Path:       s.path,
				Stage:      "assembling",
				StageBytes: s.copied,
				StageTotal: s.total,
				Message:    "assembling parts",
			})
		}
	}
	return n, err
}

// downloadMultipart downloads a file using multiple parallel range requests.
func downloadMultipart(ctx context.Context, httpc *http.Client, token string, job Job, cfg Settings, it PlanItem, dst string, emit func(ProgressEvent)) error {
	return downloadMultipartHashed(ctx, httpc, token, job, cfg, it, dst, emit, nil)
}

// downloadMultipartHashed is downloadMultipart with the finalize window both
// visible and cheap: it hashes the payload while stitching the part files,
// publishes that digest through assembledSHA so the caller can verify without
// reading the whole file a second time, and reports the stitch as byte
// progress. assembledSHA may be nil.
func downloadMultipartHashed(ctx context.Context, httpc *http.Client, token string, job Job, cfg Settings, it PlanItem, dst string, emit func(ProgressEvent), assembledSHA *string) error {
	// HEAD to resolve size, reissued only when the server throttles (429/503)
	// and bounded by the same attempt/backoff conventions as the transfer
	// loops: wait the larger of the server-requested Retry-After and the
	// local backoff (capped at DefaultMaxRetryAfter) with a context-aware
	// sleep so pause/cancel/shutdown stays prompt, then reissue the HEAD
	// within the configured retry budget.
	retry := newRetry(cfg)
	remaining := cfg.Retries
	if remaining < 0 {
		remaining = 0
	}
	attempt := 0
	for {
		req, _ := http.NewRequestWithContext(ctx, "HEAD", it.URL, nil)
		addAuth(req, token)
		resp, err := httpc.Do(req)
		if err != nil {
			return err
		}
		// A HEAD response carries headers only, so its body is closed
		// immediately instead of being held open (via defer) for the whole
		// parallel transfer. Status and headers stay readable after Close
		// for the classification and size resolution below.
		resp.Body.Close()

		apiErr := classifyFileResponse(resp, it.URL)

		// A permanent gated/private/missing response on HEAD fails the whole
		// job immediately with the actionable message, before any part
		// requests are issued.
		if apiErr != nil && apiErr.IsFailFast() {
			removeEmptyPartialFile(dst + ".part")
			return apiErr
		}

		// A throttling 429/503 must not be accepted as the resolved HEAD.
		// Every other status keeps the previous behavior (proceed and let
		// the part loop classify it).
		if resp.StatusCode != http.StatusTooManyRequests &&
			resp.StatusCode != http.StatusServiceUnavailable {
			if it.Size == 0 {
				if clen := resp.Header.Get("Content-Length"); clen != "" {
					var n int64
					fmt.Sscan(clen, &n)
					it.Size = n
				}
			}
			break
		}

		// Throttled: wait out the server-requested Retry-After (or the local
		// backoff, whichever is larger) and reissue the HEAD. When the retry
		// budget is exhausted, surface the transient error like an exhausted
		// transfer retry instead of silently proceeding with a throttled
		// response.
		if remaining == 0 {
			return apiErr
		}
		remaining--
		attempt++
		emit(ProgressEvent{Event: "retry", Path: it.RelativePath, Attempt: attempt, Message: apiErr.Error()})
		if d := NextRetryWait(resp, retry.Next(), DefaultMaxRetryAfter, time.Now()); !sleepCtx(ctx, d) {
			return ctx.Err()
		}
	}
	if it.Size == 0 {
		return downloadSingle(ctx, httpc, token, job, cfg, it, dst, emit)
	}

	// Plan parts
	n := cfg.Concurrency
	chunk := it.Size / int64(n)
	if chunk <= 0 {
		chunk = it.Size
		n = 1
	}

	layout := buildMultipartResumeLayout(it.Size, n, chunk)
	layout, direct, err := prepareMultipartMode(dst, layout)
	if err != nil {
		return err
	}

	tmpParts := make([]string, n)
	for i := 0; i < n; i++ {
		tmpParts[i] = fmt.Sprintf("%s.part-%02d", dst, i)
	}

	// Direct mode writes every part into one preallocated temp file at its final
	// offset, so the payload is never rewritten by an assembly pass. The
	// trade-off is that the digest can no longer be collected while stitching:
	// verification re-reads the file once instead, with byte progress.
	//
	// counters[i] is how many bytes of part i's range are present in the target.
	// It seeds the resume offset and feeds the progress ticker, which also drops
	// the per-tick stat() of every part file.
	var target *os.File
	counters := make([]atomic.Int64, n)
	if direct {
		target, err = os.OpenFile(multipartTargetPath(dst), os.O_RDWR|os.O_CREATE, 0o644)
		if err != nil {
			return err
		}
		// The deferred close is a safety net: the paths below close the file
		// explicitly so later cleanup can remove it on Windows.
		defer func() {
			if target != nil {
				_ = target.Close()
			}
		}()
		for i := range layout.Parts {
			counters[i].Store(layout.Parts[i].Written)
		}
	}

	// persistDirectLayout snapshots the live part offsets so a pause, abort,
	// crash or restart keeps the bytes already on disk. It only reads
	// layout.Parts, which the part goroutines read concurrently, and writes a
	// fresh copy.
	persistDirectLayout := func() {
		if !direct {
			return
		}
		snapshot := multipartResumeLayout{
			Size:        layout.Size,
			Concurrency: layout.Concurrency,
			Mode:        layout.Mode,
			Parts:       make([]multipartPartRange, len(layout.Parts)),
		}
		copy(snapshot.Parts, layout.Parts)
		for i := range snapshot.Parts {
			snapshot.Parts[i].Written = counters[i].Load()
		}
		if perr := writeMultipartResumeLayout(dst, snapshot); perr != nil {
			log.Printf("warning: persist multipart layout %s: %v", multipartLayoutMetaPath(dst), perr)
		}
	}

	// Download parts in parallel
	var wg sync.WaitGroup
	errCh := make(chan error, n)

	// Part-scoped cancellable context: a permanent (fail-fast) error on any one
	// part cancels the whole file so sibling parts stop promptly instead of
	// downloading/retrying/waiting until wg.Wait() returns. The cancel cause is
	// an abortCause wrapping the original error (see abortError), which lets a
	// sibling part that exits via its ctx path distinguish an internal abort
	// from a user cancel/pause.
	partCtx, partCancel := context.WithCancelCause(ctx)
	defer partCancel(nil)

	// partErr records the first permanent part error so the caller surfaces the
	// original error rather than a sibling's "context canceled" from errCh.
	var (
		partErrMu sync.Mutex
		partErr   error
	)

	for i := 0; i < n; i++ {
		i := i
		start := layout.Parts[i].Start
		end := layout.Parts[i].End

		wg.Add(1)
		go func() {
			defer wg.Done()

			tmp := tmpParts[i]
			expected := end - start + 1

			// Direct mode writes into the shared target opened by the caller, so the
			// resume offset comes from the persisted layout; legacy mode owns a part
			// file whose size is the resume offset.
			var out *os.File
			var pos int64
			if direct {
				pos = counters[i].Load()
				if pos > expected {
					pos = expected
				}
			} else {
				// Open or create the part file without truncating; we may be
				// resuming from a previous interrupted run.
				partFile, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE, 0o644)
				if err != nil {
					select {
					case errCh <- err:
					default:
					}
					return
				}
				defer partFile.Close()
				out = partFile

				fi, err := out.Stat()
				if err != nil {
					select {
					case errCh <- err:
					default:
					}
					return
				}
				pos = fi.Size()
				// Oversize (stale/corrupt) — reset.
				if pos > expected {
					if err := out.Truncate(0); err != nil {
						select {
						case errCh <- err:
						default:
						}
						return
					}
					pos = 0
				}
				if _, err := out.Seek(pos, io.SeekStart); err != nil {
					select {
					case errCh <- err:
					default:
					}
					return
				}
			}
			// Already fully downloaded.
			if pos == expected {
				return
			}

			retry := newRetry(cfg)
			stall := stallTimeout(cfg)
			attempts := cfg.Retries
			if attempts < 0 {
				attempts = 0
			}
			remaining := attempts
			attempt := 0
			ceiling := absoluteAttemptCeiling(attempts)
			var lastErr error
			// hwm is the highest offset ever reached for this part, seeded with
			// the initial on-disk offset. The retry budget is refunded only when
			// an attempt moves STRICTLY above it, so an ignored-Range restart or
			// oscillation never refills the budget; the absolute ceiling then
			// bounds a server that streams a strictly growing prefix (see
			// downloadSingle for the two-term rationale).
			hwm := pos

			for {
				select {
				case <-partCtx.Done():
					return
				default:
				}

				// Per-attempt context: the stall watchdog cancels only this
				// part's attempt, leaving sibling parts and the job untouched.
				attemptCtx, attemptCancel := context.WithCancel(partCtx)

				rq, _ := http.NewRequestWithContext(attemptCtx, "GET", it.URL, nil)
				addAuth(rq, token)
				rq.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start+pos, end))

				preHwm := hwm
				var waitResp *http.Response

				rs, err := httpc.Do(rq)
				if err != nil {
					lastErr = err
				} else if rs.StatusCode == http.StatusPartialContent {
					// The stall watchdog wraps the raw network body before the
					// speed limiter's pacing.

					body := newStallReader(rs.Body, stall, attemptCancel)
					var cerr error
					if direct {
						// Place the body at this part's offset inside the final file. The
						// wrapper keeps io.CopyBuffer from routing the copy through
						// os.File.ReadFrom, which would ignore the shared buffer.
						rw := &rangeWriter{
							f:       target,
							off:     start + pos,
							limit:   end + 1,
							ctx:     partCtx,
							counter: &counters[i],
						}
						_, cerr = io.CopyBuffer(
							struct{ io.Writer }{rw},
							cfg.SpeedLimiter.Reader(attemptCtx, body),
							make([]byte, assemblyBufferSize),
						)
						// pos follows the bytes actually placed in the range, so a retry
						// resumes from the real offset instead of re-asking for them.
						pos = rw.off - start
					} else {
						_, cerr = io.Copy(out, cfg.SpeedLimiter.Reader(attemptCtx, body))
						// Advance pos by what we actually wrote so the next retry
						// Range request picks up from the correct offset.
						if cur, serr := out.Seek(0, io.SeekCurrent); serr == nil {
							pos = cur
						}
					}
					rs.Body.Close()
					attemptCancel()
					if cerr == nil {
						// A 206 body that ends before the end of the requested range is
						// a truncated stream in both modes: direct would leave a
						// zero-filled hole in the shared file, and legacy would publish a
						// short part file whose successor's bytes then land right after
						// the cut. Resume from the real offset instead of finishing.
						if pos >= expected {
							return
						}
						cerr = io.ErrUnexpectedEOF
					}
					// A fired stall watchdog marks this attempt's failure as
					// a stall; a done partCtx keeps the genuine-cancel error
					// (see classifyStall precedence).
					lastErr = classifyStall(partCtx, body, cerr)
				} else {
					// Any non-206 is a failure. A permanent 401/403/404 fails
					// the whole job immediately; a throttling 429/503 shapes
					// the next wait; a 200 means the server ignored Range (a
					// multipart protocol violation) and is retried as before.
					rs.Body.Close()
					if apiErr := classifyFileResponse(rs, it.URL); apiErr != nil {
						lastErr = apiErr
						if apiErr.IsFailFast() {
							// Permanent for the whole file/job: cancel sibling
							// parts with the original error as the cause, record
							// it for the caller, and return. Close out BEFORE
							// removing the (possibly empty) partial so Windows
							// can unlink the still-open file — mirroring the
							// single-file ordering.
							attemptCancel()
							partErrMu.Lock()
							if partErr == nil {
								partErr = lastErr
							}
							partErrMu.Unlock()
							partCancel(abortError(lastErr))

							if !direct {
								out.Close()
								removeEmptyPartialFile(tmp)
							}
							select {
							case errCh <- lastErr:
							default:
							}
							return
						}
						if !apiErr.IsRetryable() {
							attemptCancel()
							partErrMu.Lock()
							if partErr == nil {
								partErr = lastErr
							}
							partErrMu.Unlock()
							partCancel(abortError(lastErr))

							if !direct {
								out.Close()
							}
							select {
							case errCh <- lastErr:
							default:
							}
							return
						}
						waitResp = rs
					} else {
						lastErr = fmt.Errorf("range not supported (status %s)", rs.Status)
					}
				}
				attemptCancel()

				// Refund the retry budget only when this attempt moved STRICTLY
				// above the high-water mark captured BEFORE the attempt.
				// preHwm is read first because hwm is updated unconditionally
				// below, so a restart's higher offset is recorded and can never
				// later earn a free refund from a bodyless failure. Multipart
				// has no truncation path. A stall that delivered almost nothing is a
				// dead stream rather than progress (see deadStallAttempt).
				advanced := pos > preHwm && !deadStallAttempt(lastErr, pos-preHwm, stall)
				if pos > hwm {
					hwm = pos
				}
				if advanced {
					retry.reset()
					remaining = attempts
				}

				if remaining == 0 || attempt >= ceiling {
					break
				}
				remaining--
				attempt++
				emit(ProgressEvent{Event: "retry", Path: it.RelativePath, Attempt: attempt, Message: lastErr.Error()})
				if d := NextRetryWait(waitResp, retry.Next(), DefaultMaxRetryAfter, time.Now()); !sleepCtx(partCtx, d) {
					return
				}
			}

			select {
			case errCh <- lastErr:
			default:
			}
		}()
	}

	// Emit periodic progress while parts download. The ticker is stopped
	// cleanly after wg.Wait() so it cannot observe mid-assembly state (parts
	// being deleted) and emit a bogus 0-byte progress event — the bug behind
	// the "progress jumps 2.4% ↔ 2.5% for hours" symptom in github #75.
	tickerDone := make(chan struct{})
	var tickerWG sync.WaitGroup
	tickerWG.Add(1)
	go func() {
		defer tickerWG.Done()
		t := time.NewTicker(200 * time.Millisecond) // More frequent updates for responsive UI
		defer t.Stop()

		ticks := 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-tickerDone:
				return
			case <-t.C:
				var downloaded int64
				if direct {
					for i := range counters {
						downloaded += counters[i].Load()
					}
				} else {
					for _, p := range tmpParts {
						if fi, err := os.Stat(p); err == nil {
							downloaded += fi.Size()
						}
					}
				}
				emit(ProgressEvent{Event: "file_progress", Path: it.RelativePath, Downloaded: downloaded, Total: it.Size})
				// Snapshot the resume layout roughly every two seconds of progress.
				if direct {
					ticks++
					if ticks >= 10 {
						ticks = 0
						persistDirectLayout()
					}
				}
			}
		}
	}()

	wg.Wait()

	// No part goroutine writes to the shared target after this point, so close
	// it now: every later path (cancel cleanup, retry, job cleanup, the publish
	// rename) can then touch it even on Windows, where an open file cannot be
	// unlinked.
	var targetCloseErr error
	if direct {
		targetCloseErr = target.Close()
		target = nil
	}

	// Stop the progress ticker and wait for it to exit before touching the
	// part files. Any in-flight tick will finish and emit one final event
	// while parts are still on disk at their real sizes.
	close(tickerDone)
	tickerWG.Wait()

	// Record the final part offsets before any error path below returns, so a
	// crash or restart after this point still resumes from real progress.
	persistDirectLayout()

	// A permanent (fail-fast) part error cancels partCtx and is the error the
	// caller must see; return it in preference to the sibling "context
	// canceled" that the ctx check below would surface. An internal abort
	// must NOT run cancel-cleanup (T3): only genuinely empty partials are
	// removed, by the failing part itself, and resumable bytes are preserved.
	partErrMu.Lock()
	permanentErr := partErr
	partErrMu.Unlock()
	if permanentErr != nil {
		return permanentErr
	}

	// If the context was cancelled while parts were running (pause / abort /
	// timeout), return the cancellation error immediately. Part goroutines
	// that exit via their ctx-aware retry/sleep path do NOT push to errCh,
	// so we cannot rely on the errCh drain to catch this — we must check
	// ctx.Err() explicitly. Returning here is critical: it prevents the
	// bogus "downloaded == total" progress emit below AND stops the
	// assembly loop from stitching an incomplete part set into a corrupt
	// final file and deleting the partial bytes the next resume needs.
	//
	// The guard keys off partCtx — the context a part fail-fast actually
	// cancels (via abortCause) — not the file ctx, so it stays correct even
	// if the cancel wiring changes. (A file fail-fast is already returned
	// above via partErr, so this branch handles user cancel/pause and any
	// internal abort of partCtx.)
	if ctx.Err() != nil || partCtx.Err() != nil {
		if !abortPartAttempt(partCtx) {
			cleanupPartialsOnCancel(cfg, dst)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return partCtx.Err()
	}

	select {
	case e := <-errCh:
		return e
	default:
	}

	// Direct mode has no assembly pass to notice a short part, so require every
	// range to be fully present before publishing the preallocated file.
	if direct {
		for i := range layout.Parts {
			want := layout.Parts[i].End - layout.Parts[i].Start + 1
			if got := counters[i].Load(); got != want {
				return fmt.Errorf("part %d of %s incomplete: %d/%d bytes", i, it.RelativePath, got, want)
			}
		}
	}

	// Emit one explicit full-progress reading so the caller's last observed
	// file_progress value is the full byte count, regardless of when the
	// ticker happened to last fire.
	emit(ProgressEvent{Event: "file_progress", Path: it.RelativePath, Downloaded: it.Size, Total: it.Size})

	if direct {
		// The payload already sits in its final temp file, so publishing it is
		// a rename rather than a rewrite of the whole file, and there is no
		// assembling stage to announce. A cancel that landed in the meantime
		// keeps the resumable target and its layout for an internal abort and
		// runs the user-cancel cleanup otherwise.
		if cerr := partCtx.Err(); cerr != nil {
			if !abortPartAttempt(partCtx) {
				cleanupPartialsOnCancel(cfg, dst)
			}
			return cerr
		}
		if targetCloseErr != nil {
			return targetCloseErr
		}
		if err := os.Rename(multipartTargetPath(dst), dst); err != nil {
			return err
		}
		_ = os.Remove(multipartLayoutMetaPath(dst))
		return nil
	}

	// Everything past this point is local disk I/O: stitching the part files
	// back into one rewrites the entire payload, which takes real time for
	// multi-GB files. Signal it so the UI doesn't sit silently at 100%.
	emit(ProgressEvent{Event: "file_finalizing", Path: it.RelativePath, Stage: "assembling", StageTotal: it.Size, Message: "assembling parts"})

	// Abort before touching the part files if the job/part context was
	// cancelled between the guard above and here. Preserve the resumable
	// .part-NN part files (they are the resumable data); an internal fail-fast
	// abort must NOT run the user-cancel cleanup. Remove any incomplete
	// intermediate dst.part this call may have created — none yet at this
	// point, so just return the cancellation cause.
	if cerr := partCtx.Err(); cerr != nil {
		if !abortPartAttempt(partCtx) {
			cleanupPartialsOnCancel(cfg, dst)
		}
		return cerr
	}

	// Assemble parts. The context-checking reader and the per-part check make
	// the local copy stop promptly once a sibling's permanent error (or a user
	// cancel) has cancelled partCtx, instead of hashing/stitching a multi-GB
	// sibling to completion. An internal abort preserves the .part-NN files
	// (resumable data) and removes the incomplete intermediate dst.part.
	out, err := os.Create(dst + ".part")
	if err != nil {
		return err
	}

	// Hash while stitching: the copy already touches every byte of the
	// payload, so the digest the caller needs for verification costs no extra
	// read, and the separate full re-hash of a multi-GB shard goes away.
	hasher := sha256.New()
	buf := make([]byte, assemblyBufferSize)
	stitch := &stageReader{ctx: partCtx, path: it.RelativePath, total: it.Size, emit: emit, hash: hasher}

	for i := 0; i < n; i++ {
		if cerr := partCtx.Err(); cerr != nil {
			out.Close()
			removeIncompleteAssembly(cfg, dst, partCtx)
			return cerr
		}
		p := tmpParts[i]
		in, err := os.Open(p)
		if err != nil {
			out.Close()
			removeIncompleteAssembly(cfg, dst, partCtx)
			return err
		}
		stitch.r = in
		// The io.Writer wrapper stops io.CopyBuffer from routing the copy
		// through os.File.ReadFrom, which would ignore the shared buffer.
		if _, err := io.CopyBuffer(struct{ io.Writer }{out}, stitch, buf); err != nil {
			in.Close()
			out.Close()
			if cerr := partCtx.Err(); cerr != nil {
				removeIncompleteAssembly(cfg, dst, partCtx)
				return cerr
			}
			return err
		}
		in.Close()
	}
	out.Close()

	if cerr := partCtx.Err(); cerr != nil {
		removeIncompleteAssembly(cfg, dst, partCtx)
		return cerr
	}

	if assembledSHA != nil {
		*assembledSHA = hex.EncodeToString(hasher.Sum(nil))
	}

	if err := os.Rename(dst+".part", dst); err != nil {
		return err
	}

	for _, p := range tmpParts {
		_ = os.Remove(p)
	}
	_ = os.Remove(multipartLayoutMetaPath(dst))

	return nil
}

// removeIncompleteAssembly handles a cancellation that landed during part
// assembly. On an internal fail-fast abort it preserves the resumable .part-NN
// part files and removes only the incomplete intermediate dst.part (which is
// not resumable and would otherwise be a corrupt leftover). On a user cancel
// it keeps the existing cleanupPartialsOnCancel semantics unchanged.
func removeIncompleteAssembly(cfg Settings, dst string, ctx context.Context) {
	if abortPartAttempt(ctx) {
		// lgtm[go/path-injection]
		if err := os.Remove(dst + ".part"); err != nil && !os.IsNotExist(err) {
			log.Printf("warning: cleanup incomplete assembly %s: %v", dst+".part", err)
		}
		return
	}
	cleanupPartialsOnCancel(cfg, dst)
}
