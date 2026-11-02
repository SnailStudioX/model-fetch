// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/bashrusakh/modelfetch/internal/hubsource"
)

// unsafeRepoPath reports whether a relative path returned by the repo tree API
// would escape the repository root if joined onto a local directory. The path
// list is remote-controlled (and the endpoint is operator-configurable via
// --endpoint, so a malicious or MITM'd mirror can return anything), and the
// path flows unchecked into file writes (filepath.Join(base, rel)) and symlink
// creation. Anything absolute, containing a "\\" (Windows separator / drive
// escape), or normalising to "" / "." / ".." / a "../" prefix is rejected to
// prevent arbitrary-file-write.
func unsafeRepoPath(rel string) bool {
	if rel == "" {
		return true
	}
	if strings.ContainsRune(rel, '\\') || strings.HasPrefix(rel, "/") || filepath.IsAbs(rel) {
		return true
	}
	cleaned := path.Clean(rel)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return true
	}
	return false
}

// unsafeBlobName reports whether a SHA256 value taken from the remote tree
// API is not acceptable as a canonical SHA-256 hash for the plan. The SHA is
// remote-controlled (see the unsafeRepoPath rationale above) and is used
// verbatim both as a filesystem name (blobs/<sha> storage and the tmp-<sha>
// staging file) and as the expected digest in verification, so it must be
// either empty (a file without a hash) or exactly 64 hex characters -- the
// canonical SHA-256 form. The separator / absolute / traversal checks below
// keep the path-safety property stated on its own guard; the length+hex check
// subsumes them but is deliberately not the only check. Hex is accepted
// case-insensitively; scanRepo lowercases accepted values before they enter
// the plan. Everything else -- including 40-hex git-OID-shaped values and
// "sha256:"-prefixed LFS pointer oids -- is rejected: neither can ever match
// a computed 64-hex digest at verify time, so accepting them would only
// defer a guaranteed verification failure while writing non-canonical cache
// entries. Callers must still check for the empty shape before using the
// value as a path.
func unsafeBlobName(sha string) bool {
	if sha == "" {
		return false
	}
	if strings.ContainsAny(sha, `/\`) || filepath.IsAbs(sha) || filepath.VolumeName(sha) != "" {
		return true
	}
	cleaned := path.Clean(sha)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return true
	}
	return !canonicalSHA256(sha)
}

// canonicalSHA256 reports whether sha is exactly 64 hexadecimal characters,
// the canonical SHA-256 digest representation, accepting upper or lower case.
func canonicalSHA256(sha string) bool {
	if len(sha) != 64 {
		return false
	}
	for i := 0; i < len(sha); i++ {
		c := sha[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// PathInside reports whether target resolves to base itself or to a location
// nested within base. Both paths are cleaned first, so "../" segments are
// resolved before the comparison. This is the single home for the containment
// guard that the cache, mirror, and downloader call sites previously
// open-coded as inline strings.HasPrefix checks.
//
// The separator-strict prefix below keeps a sibling that shares the base's
// name (e.g. /tmp/models2 versus base /tmp/models) from matching: appending
// a separator to both sides forces the comparison onto a component boundary.
// A filesystem/volume root — "/" on Unix, a drive root such as "C:\" or a
// UNC share root on Windows (filepath.VolumeName names its volume; Clean
// keeps the root's trailing separator) — already ends in a separator after
// filepath.Clean, so it must not receive a second one: base+sep would be
// "//" (or "\\"), a prefix no path can ever match, which rejected every
// destination legitimately inside the root as an escape. For a root the
// cleaned root itself is the strict prefix: a root has no shallower sibling,
// so any cleaned target carrying that prefix is inside it (relative targets
// still fail the prefix, as before). filepath.Clean never leaves a trailing
// separator on a non-root path, so non-root bases take the unchanged
// separator-strict branch and keep byte-identical acceptance.
func PathInside(base, target string) bool {
	base = filepath.Clean(base)
	target = filepath.Clean(target)
	if target == base {
		return true
	}
	sep := string(filepath.Separator)
	if strings.HasSuffix(base, sep) {
		return strings.HasPrefix(target, base)
	}
	return strings.HasPrefix(target+sep, base+sep)
}

// SafeJoin joins rel onto base and verifies the result stays within base,
// returning an error if rel would escape (via "../", an absolute component,
// etc.). Use this instead of a bare filepath.Join wherever rel is remote- or
// user-influenced.
func SafeJoin(base, rel string) (string, error) {
	// Reject absolute or volume-qualified rel: filepath.Join would otherwise
	// silently rewrite them under base (or, on Windows, splice in a drive
	// letter), hiding what is really an absolute-path input.
	if filepath.IsAbs(rel) || filepath.VolumeName(rel) != "" {
		return "", fmt.Errorf("path %q must be relative", rel)
	}
	// Reject non-local values ("", "..", escaping segments, and the platform's
	// reserved names) before joining. filepath.IsLocal is the standard-library
	// primitive CodeQL models as a go/path-injection sanitizer barrier, so this
	// check makes the containment below legible to code scanning; the
	// PathInside check remains the containment proof.
	if !filepath.IsLocal(rel) {
		return "", fmt.Errorf("path %q must be local", rel)
	}
	dst := filepath.Clean(filepath.Join(base, rel))
	if !PathInside(base, dst) {
		return "", fmt.Errorf("path %q escapes %q", rel, base)
	}
	// Restate the containment PathInside just proved against the cleaned base
	// with strings.HasPrefix, the primitive code scanning models as a
	// sanitizer guard: this check dominates the return below, so taint
	// carried in `base` itself cannot flow out of SafeJoin unnoticed.
	// Acceptance is identical to PathInside's (its separator-strict prefix
	// implies this plain prefix), so no input that passed before is rejected.
	if !strings.HasPrefix(dst, filepath.Clean(base)) {
		return "", fmt.Errorf("path %q escapes %q", rel, base)
	}
	return dst, nil
}

// DestinationBase joins a user-selected repository folder to its configured
// root and proves the resulting path remains under that root. Unlike SafeJoin,
// repository folder names may contain normalizing segments such as a/../b;
// only absolute paths and paths that actually escape the root are rejected.
func DestinationBase(root, folder string) (string, error) {
	// Keep the platform-aware component rules used by the download path: these
	// reject reserved names/characters in the repo component on platforms where
	// they are not valid local paths, while still allowing in-root a/../b.
	if !filepath.IsLocal(folder) {
		return "", fmt.Errorf("destination folder %q must be local", folder)
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve output root %q: %w", root, err)
	}
	base := filepath.Clean(filepath.Join(rootAbs, folder))
	if !PathInside(rootAbs, base) {
		return "", fmt.Errorf("destination %q escapes output root %q", base, rootAbs)
	}
	// Preserve the modeled containment barrier used by the previous download
	// destination check, after the component-boundary proof above.
	if !strings.HasPrefix(base, rootAbs) {
		return "", fmt.Errorf("destination %q escapes output root %q", base, rootAbs)
	}
	return base, nil
}

// PlanItem represents a single file in the download plan.
type PlanItem struct {
	RelativePath string `json:"path"`
	URL          string `json:"url"`
	LFS          bool   `json:"lfs"`
	SHA256       string `json:"sha256,omitempty"`
	Size         int64  `json:"size"`
	AcceptRanges bool   `json:"acceptRanges"`
	// Subdir holds the matched filter (if any) used when --append-filter-subdir is set.
	Subdir string `json:"subdir,omitempty"`
}

// Plan contains the list of files to download.
type Plan struct {
	Items  []PlanItem `json:"items"`
	Commit string     `json:"commit,omitempty"` // Commit hash for this plan (for HF cache snapshots)
	// Revision is the revision the plan was actually read from. It differs from
	// the requested one only when the scan had to fall back to this hub's
	// default branch (see scanRepoResolved), so callers can record the revision
	// that matches the files instead of keeping a stale cross-hub name.
	Revision string `json:"revision,omitempty"`
	// CommitIsAlias reports that the hub's commit endpoint was unavailable, so
	// the snapshot identity below fell back to the revision name ("main")
	// instead of a SHA. Callers must surface it: once the endpoint answers, the
	// same repo gains a second snapshots/<id>/ identity and the first is never
	// cleaned up (ListSnapshots returns both).
	CommitIsAlias bool `json:"commitIsAlias,omitempty"`
	// ScannedFiles is how many files this hub reported for the revision, before
	// any filter or exclude was applied. It is what makes an empty `Items`
	// readable: zero means the repository really has no files, and any other
	// number means the selection matched none of the files that are there. Without
	// it a dry run and a finished job both look like "nothing", which is the
	// difference between an empty repository and a wrong filter.
	ScannedFiles int `json:"scannedFiles,omitempty"`
}

// planRevision is the revision the files were read from. It differs from the
// requested one only when the scan had to fall back to this hub's default branch,
// and an error message that quotes the wrong revision sends the reader to a
// revision that was never queried.
func planRevision(job Job, plan *Plan) string {
	if plan.Revision != "" {
		return plan.Revision
	}
	if job.Revision != "" {
		return job.Revision
	}
	return "the default branch"
}

// emptySelectionError explains a plan that is empty because of the caller's own
// selection: the hub reported files, and the filters or excludes kept none of them.
// Nothing will be downloaded, so this is a failure to explain rather than a
// completed run - and the count is what makes it fixable, because it says the
// files are there and the pattern is wrong.
func emptySelectionError(job Job, plan *Plan) error {
	var selection []string
	if len(job.Filters) > 0 {
		selection = append(selection, "filters "+quotePatternList(job.Filters))
	}
	if len(job.Excludes) > 0 {
		selection = append(selection, "excludes "+quotePatternList(job.Excludes))
	}
	if len(selection) == 0 {
		return fmt.Errorf("nothing to download: none of the %s in %s at revision %s could be selected",
			fileCount(plan.ScannedFiles), job.Repo, planRevision(job, plan))
	}
	return fmt.Errorf("nothing to download: %s match none of the %s in %s at revision %s",
		strings.Join(selection, " and "), fileCount(plan.ScannedFiles), job.Repo, planRevision(job, plan))
}

func fileCount(n int) string {
	if n == 1 {
		return "1 file"
	}
	return fmt.Sprintf("%d files", n)
}

func quotePatternList(items []string) string {
	quoted := make([]string, len(items))
	for i, item := range items {
		quoted[i] = strconv.Quote(item)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// emptyRepositoryMessage says what an empty plan with no selection means: this
// repository has nothing at this revision. There is nothing to download and no
// mistake to fix, so it is a no-op rather than a failure - but a completed job
// that moved no bytes must not be the only clue, so the caller reports it.
func emptyRepositoryMessage(job Job, plan *Plan) string {
	return fmt.Sprintf("this repository has no files at revision %s, so there was nothing to download", planRevision(job, plan))
}

// PlanRepo builds the file list without downloading.
func PlanRepo(ctx context.Context, job Job, cfg Settings) (*Plan, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := validate(job, cfg); err != nil {
		return nil, err
	}
	if job.Revision == "" {
		// The default branch belongs to the hub, not to the downloader:
		// Hugging Face uses "main", ModelScope uses "master".
		job.Revision = hubsource.Resolve(cfg.Source, cfg.Endpoint).DefaultRevision()
	}
	httpc, err := BuildHTTPClient(cfg.Proxy)
	if err != nil {
		return nil, fmt.Errorf("build planning HTTP client: %w", err)
	}
	return scanRepoResolved(ctx, httpc, cfg.Token, &job, cfg)
}

// scanRepoResolved scans the repository and adapts the revision when the hub
// reports that the requested one does not exist.
//
// A job stores the revision name of the hub it was created on: "master" for
// ModelScope, "main" for Hugging Face. Switching the hub source in settings
// leaves that name pointing at a revision the new hub never had, so every tree
// and file request answers 404 and the user is shown a bare "tree API failed:
// 404 Not Found" every time they press retry. When that happens and the stored
// name is a hub default rather than a revision the user chose, the scan is
// re-run against this source's default branch and *job keeps it, so plan URLs,
// cache refs and progress events all follow the revision that was actually
// read. The extra request is only paid on the 404 path, and a revision the user
// picked themselves is never replaced with a guess.
func scanRepoResolved(ctx context.Context, httpc *http.Client, token string, job *Job, cfg Settings) (*Plan, error) {
	requested := job.Revision
	plan, err := scanRepo(ctx, httpc, token, *job, cfg)
	if err == nil {
		plan.Revision = job.Revision
		return plan, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	src := hubsource.Resolve(cfg.Source, cfg.Endpoint)
	alt, ok := hubsource.RevisionFallback(src, requested)
	if !ok {
		return nil, revisionScanError(err, src, *job, requested, "")
	}
	job.Revision = alt
	plan, err = scanRepo(ctx, httpc, token, *job, cfg)
	if err == nil {
		plan.Revision = alt
		return plan, nil
	}
	if errors.Is(err, ErrNotFound) {
		return nil, revisionScanError(err, src, *job, requested, alt)
	}
	return nil, err
}

// revisionScanError appends the hub, the repository and the revisions that were
// tried to a 404. The status line alone cannot distinguish "this repository is
// not on this hub" from "that revision belongs to a different hub source",
// which is exactly what the user needs in order to fix the settings.
func revisionScanError(err error, src hubsource.Source, job Job, requested, tried string) error {
	kind := "repo"
	if job.IsDataset {
		kind = "dataset"
	}
	hint := fmt.Sprintf(" (%s %q at revision %q is not available on %s", kind, job.Repo, requested, src.Base())
	if tried != "" {
		hint += fmt.Sprintf("; also tried %q", tried)
	}
	return &scanHintError{err: err, hint: hint + ")"}
}

// scanHintError adds context to a scan failure without rewriting the original
// message, so errors.Is and errors.As still reach the underlying API error.
type scanHintError struct {
	err  error
	hint string
}

func (e *scanHintError) Error() string { return e.err.Error() + e.hint }
func (e *scanHintError) Unwrap() error { return e.err }

// scanRepo walks the repo tree and builds a download plan.
func scanRepo(ctx context.Context, httpc *http.Client, token string, job Job, cfg Settings) (*Plan, error) {
	var items []PlanItem
	seen := make(map[string]struct{})      // ensure each relative path appears once in the plan
	seenLower := make(map[string]struct{}) // ensure each path appears at most once modulo case

	src := hubsource.Resolve(cfg.Source, cfg.Endpoint)

	// Fetch actual commit SHA for the revision
	repoInfo, err := fetchRepoInfo(ctx, httpc, token, src, job)
	commitIsAlias := err != nil
	if err != nil {
		// Fall back to revision name if API call fails (e.g., some mirrors)
		repoInfo = &RepoInfo{SHA: job.Revision}
	}
	commitSHA := repoInfo.SHA
	if commitSHA == "" {
		commitSHA = job.Revision // fallback
		commitIsAlias = true
	}

	// Collect every file node first. We need the full list before building the
	// plan so we can detect a GGUF-only download and skip companion files that
	// a self-contained GGUF doesn't need.
	var fileNodes []hfNode
	err = walkTree(ctx, httpc, token, src, job, "", func(n hfNode) error {
		if n.Type == "file" || n.Type == "blob" {
			// Reject path-traversal entries before they reach any filesystem
			// operation downstream. Fail the whole plan rather than silently
			// skipping so a tampered tree is loud, not partial.
			if unsafeRepoPath(n.Path) {
				return fmt.Errorf("refusing unsafe path from repo tree: %q", n.Path)
			}
			fileNodes = append(fileNodes, n)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	baseNames := make([]string, 0, len(fileNodes))
	for _, node := range fileNodes {
		baseNames = append(baseNames, strings.ToLower(filepath.Base(node.Path)))
	}
	ggufMode := isGGUFFilterDownload(baseNames, job.Filters, job.ExactMatch)

	for _, n := range fileNodes {
		rel := n.Path

		// Deduplicate by relative path
		if _, ok := seen[rel]; ok {
			continue
		}
		seen[rel] = struct{}{}

		name := filepath.Base(rel)
		nameLower := strings.ToLower(name)
		relLower := strings.ToLower(rel)
		isLFS := n.LFS != nil

		// Check excludes first - if file matches any exclude pattern, skip it
		// Credits: Exclude feature suggested by jeroenkroese (#41)
		excluded := false
		for _, ex := range job.Excludes {
			exLower := strings.ToLower(ex)
			if strings.Contains(nameLower, exLower) || strings.Contains(relLower, exLower) {
				excluded = true
				break
			}
		}
		if excluded {
			continue
		}

		// Determine which filter (if any) matches this file name, prefer the longest match
		// Filter matching is case-insensitive (e.g., q4_0 matches Q4_0)
		matchedFilter := ""
		// This shared rule narrows only GGUF entries. In GGUF mode, retain the
		// original filter behavior for non-GGUF files, including explicitly
		// requested companions.
		if strings.HasSuffix(nameLower, ".gguf") && len(job.Filters) > 0 &&
			!GGUFPathSelected(rel, job.Filters, job.Excludes, job.ExactMatch) {
			continue
		}
		if ggufMode {
			// Keep only files that match a filter: the selected quant's shards
			// and any mmproj companion. Everything else is skipped.
			for _, f := range job.Filters {
				if filterMatches(nameLower, strings.ToLower(f), job.ExactMatch) {
					if len(f) > len(matchedFilter) {
						matchedFilter = f
					}
				}
			}
			if matchedFilter == "" {
				continue
			}
		} else if isLFS && len(job.Filters) > 0 {
			for _, f := range job.Filters {
				fLower := strings.ToLower(f)
				if filterMatches(nameLower, fLower, job.ExactMatch) {
					if len(f) > len(matchedFilter) {
						matchedFilter = f
					}
				}
			}
			// If filters provided and none matched, skip typical large LFS blobs
			if matchedFilter == "" {
				ln := strings.ToLower(name)
				ext := strings.ToLower(filepath.Ext(name))
				if ext == ".bin" || ext == ".act" || ext == ".safetensors" || ext == ".zip" || strings.HasSuffix(ln, ".gguf") || strings.HasSuffix(ln, ".ggml") {
					continue
				}
			}
		}

		// Build URL and file size
		var urlStr string
		if isLFS {
			urlStr = lfsURL(src, job, rel)
		} else {
			urlStr = rawURL(src, job, rel)
		}
		// For LFS files, ALWAYS use LFS.Size (n.Size is the pointer file size, not actual)
		var size int64
		if n.LFS != nil && n.LFS.Size > 0 {
			size = n.LFS.Size
		} else {
			size = n.Size
		}

		// Assume LFS files support range requests when the hub is known to
		// always do so (HuggingFace does). A hub whose resolve endpoint does not
		// advertise Accept-Ranges must not be assumed to. Don't block with HEAD
		// requests during planning - too slow for large repos.
		acceptRanges := isLFS && src.AssumeRangesForLFS()

		sha := n.Sha256
		shaField := "sha256"
		if sha == "" && n.LFS != nil {
			// LFS files have SHA256 in either Sha256 field or Oid field (LFS spec uses oid)
			sha = n.LFS.Sha256
			shaField = "lfs.sha256"
			if sha == "" {
				sha = n.LFS.Oid
				shaField = "lfs.oid"
			}
		}

		// The SHA is remote-controlled too, and unlike n.Path it flows into
		// filesystem paths verbatim (tmp-<sha> staging, blobs/<sha> storage)
		// and into verification as the expected digest. Only the canonical
		// SHA-256 form may reach the plan: empty (a file without a hash) or
		// 64 hex characters. Reject anything else before it reaches the
		// plan, failing the whole plan rather than silently skipping so a
		// tampered tree is loud, not partial. The error names the offending
		// field so a malformed mirror response is diagnosable.
		if unsafeBlobName(sha) {
			return nil, fmt.Errorf("refusing non-canonical sha256 from repo tree for %q (field %s): %q: want empty or 64 hex characters", rel, shaField, sha)
		}
		// Store the canonical lowercase spelling so every consumer of
		// PlanItem.SHA256 (tmp-<sha> staging, blobs/<sha>, CheckBlob,
		// verify, manifest) sees "" or lowercase hex no matter how the
		// remote spelled the digest. ToLower is a no-op for lowercase input,
		// so canonical values pass through byte-identical.
		sha = strings.ToLower(sha)

		// Case-insensitive dedup: filterMatches is intentionally case-insensitive
		// (q4_k_m matches Q4_K_M), so two files that differ only in case both
		// pass the same filter and would otherwise both end up in the plan.
		// Treat them as the same logical file and keep the first occurrence,
		// matching what the HF API returns first. The exact-path dedup above
		// doesn't catch this because the two paths differ only in case.
		if _, dup := seenLower[relLower]; dup {
			continue
		}
		seenLower[relLower] = struct{}{}

		items = append(items, PlanItem{
			RelativePath: rel,
			URL:          urlStr,
			LFS:          isLFS,
			SHA256:       sha,
			Size:         size,
			AcceptRanges: acceptRanges,
			Subdir:       matchedFilter, // empty when no filter matched
		})
	}
	return &Plan{Items: items, Commit: commitSHA, CommitIsAlias: commitIsAlias, ScannedFiles: len(fileNodes)}, nil
}

// GGUFPathSelected reports whether a known relative GGUF filename survives the
// same excludes and filter predicate used by PlanRepo. It is also used by the
// job manager to prove that a queued filtered job cannot write a selected
// local GGUF path before that job has been dispatched and planned.
func GGUFPathSelected(relativePath string, filters, excludes []string, exact bool) bool {
	name := strings.ToLower(filepath.Base(relativePath))
	if !strings.HasSuffix(name, ".gguf") {
		return false
	}
	rel := strings.ToLower(relativePath)
	for _, ex := range excludes {
		exLower := strings.ToLower(ex)
		if strings.Contains(name, exLower) || strings.Contains(rel, exLower) {
			return false
		}
	}
	if len(filters) == 0 {
		return true
	}
	for _, filter := range filters {
		if filterMatches(name, strings.ToLower(filter), exact) {
			return true
		}
	}
	return false
}

// filterMatches reports whether filter fLower matches the file name nameLower
// (both already lowercased). In substring mode (the default) it uses a plain
// substring check. In exact mode it matches only when fLower equals a whole
// delimiter-bounded segment of the name, so "q6_k" matches "...-Q6_K.gguf" but
// not "...-Q6_K_XL.gguf". See Settings.ExactMatch (github issue #78).
func filterMatches(nameLower, fLower string, exact bool) bool {
	if !exact {
		return strings.Contains(nameLower, fLower)
	}
	for _, seg := range strings.FieldsFunc(nameLower, isFilterDelimiter) {
		if seg == fLower {
			return true
		}
	}
	if strings.Contains(fLower, "-") || strings.Contains(fLower, ".") || strings.Contains(fLower, " ") {
		start := 0
		for {
			idx := strings.Index(nameLower[start:], fLower)
			if idx < 0 {
				break
			}
			idx += start
			beforeOK := idx == 0 || isFilterDelimiter(rune(nameLower[idx-1]))
			afterIdx := idx + len(fLower)
			afterOK := afterIdx == len(nameLower) || isFilterDelimiter(rune(nameLower[afterIdx]))
			if beforeOK && afterOK {
				return true
			}
			start = idx + 1
		}
	}
	return false
}

// isFilterDelimiter reports whether r separates segments for exact-match
// filtering. Underscores are intentionally NOT delimiters because quantization
// names contain them (e.g. Q6_K, Q4_K_M).
func isFilterDelimiter(r rune) bool {
	return r == '-' || r == '.' || r == ' '
}

// isGGUFFilterDownload reports whether the given filters target a .gguf file in
// the supplied set of file base names (all expected lowercased). When true the
// download is treated as GGUF-only: because a GGUF file is self-contained, the
// plan keeps just the filter-matched files (the chosen quant's shards plus any
// mmproj companion) and drops config/tokenizer JSON, README, .gitattributes and
// fp16/ transformers metadata. For non-GGUF filters (e.g. "safetensors") this
// returns false so those companion files are still downloaded.
func isGGUFFilterDownload(baseNames, filters []string, exact bool) bool {
	if len(filters) == 0 {
		return false
	}
	for _, base := range baseNames {
		if !strings.HasSuffix(base, ".gguf") {
			continue
		}
		for _, f := range filters {
			if filterMatches(base, strings.ToLower(f), exact) {
				return true
			}
		}
	}
	return false
}

// destinationBase returns the base output directory for a job.
func destinationBase(job Job, cfg Settings) (string, error) {
	// LocalRepo overrides the folder name: use it when the files are fetched
	// from an upstream repo but should be stored alongside another model's files
	// (e.g. mmproj from a base model saved next to the current model's quants).
	repoForPath := job.Repo
	if job.LocalRepo != "" {
		repoForPath = job.LocalRepo
	}
	return DestinationBase(cfg.OutputDir, repoForPath)
}

// ScanPlan scans a repository and emits plan_item events via the progress callback.
// This is useful for dry-run/preview functionality.
func ScanPlan(ctx context.Context, job Job, cfg Settings, progress ProgressFunc) error {
	plan, err := PlanRepo(ctx, job, cfg)
	if err != nil {
		return err
	}

	// The scan may have read a different revision than the caller asked for;
	// report the one the files came from.
	revision := plan.Revision
	if revision == "" {
		revision = job.Revision
	}

	if progress != nil {
		for _, item := range plan.Items {
			progress(ProgressEvent{
				Time:     time.Now().UTC(),
				Event:    "plan_item",
				Repo:     job.Repo,
				Revision: revision,
				Path:     item.RelativePath,
				Total:    item.Size,
				IsLFS:    item.LFS,
			})
		}
	}

	return nil
}

// Run is an alias for Download for API compatibility.
func Run(ctx context.Context, job Job, cfg Settings, progress ProgressFunc) error {
	return Download(ctx, job, cfg, progress)
}
