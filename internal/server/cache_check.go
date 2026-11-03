// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/SnailStudioX/model-fetch/pkg/hfdownloader"
)

// Whether a cached library is still intact is a question nothing here currently
// answers: a blob whose bytes were truncated by an interrupted copy, zeroed by a
// failing disk, or edited by hand keeps its name and keeps reading as
// "downloaded" until something tries to load it and fails.
//
// The cache layout makes the check possible without a manifest and without asking
// the hub anything: hfcache names every blob by the SHA-256 of its contents,
// computing the digest itself when the hub does not supply one, so a stored file
// carries the value it must hash to. A quick pass reads directory entries and
// sizes only; the byte pass re-reads every blob and compares its digest with the
// name it is stored under.

const (
	cacheCheckStatePlanned   = "planned"
	cacheCheckStateChecking  = "checking"
	cacheCheckStateHashing   = "hashing"
	cacheCheckStateCancelled = "cancelled"
)

const (
	// A stored blob that is the wrong shape.
	cacheProblemNoBlobs    = "no-blobs"
	cacheProblemEmptyBlob  = "empty-blob"
	cacheProblemHash       = "hash-mismatch"
	cacheProblemUnreadable = "unreadable"
	cacheProblemUnexpected = "unexpected-entry"
	// A snapshot entry pointing at a blob that is gone: the repository reads as
	// present but one of its files is not.
	cacheProblemDangling = "dangling-snapshot"
)

// emptyBlobDigest is the digest of zero bytes. A blob with this name is
// legitimately empty, so the size check leaves it alone.
var emptyBlobDigest = hexDigestString(sha256.Sum256(nil))

var blobNamePattern = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

func hexDigestString(sum [sha256.Size]byte) string { return hex.EncodeToString(sum[:]) }

// hexDigest reports whether name is exactly a digest, lowercased for comparison.
func hexDigest(name string) (string, bool) {
	if !blobNamePattern.MatchString(name) {
		return "", false
	}
	return strings.ToLower(name), true
}

const (
	entryBlob       = "blob"
	entryStaging    = "staging"
	entryUnexpected = "unexpected"
)

// stagingBlobPrefix is the namespace the download path writes while a blob is in
// flight: blobs/tmp-<sha>, or tmp-<path-with-underscores> when the hub reported
// no digest. Its single-part and multipart artifacts (.part, .part-NN, .parts,
// .parts.json) are appended to that name, and a concurrent second claimer of the
// same blob gets tmp-<sha>-2.
const stagingBlobPrefix = "tmp-"

// classifyBlobEntry says what an entry inside blobs/ is: a stored blob (its name
// is its digest), staging written by a running or interrupted download, or
// something this cache layout did not write. Staging is reported but never called
// corruption: it is a download that did not finish, which is what the listing and
// the Jobs page are for.
//
// Two namespaces belong to the producer, and each keeps the identity of the blob
// it stages: a name beginning with a digest and continuing with a separator
// (.incomplete and its .meta, the .part/.parts artifacts, a numbered claim
// sibling, and the .tmp-<random> publication target copyFileAtomicCtx stages
// beside a blob), or the transfer destination tmp-<sha-or-path>. The second form
// carries no leading digest, so it needs its own rule; without it a check started
// while a job runs - which is normal, the pass reserves nothing - would call that
// job's own working files corruption. pkg/hfdownloader refuses to delete against
// the same two forms, anchored to one repository's paths there: see
// RepoDir.isIncompleteBlobPath.
func classifyBlobEntry(name string) (kind, digest string) {
	if digest, ok := hexDigest(name); ok {
		return entryBlob, digest
	}
	if len(name) >= sha256.Size*2 {
		if digest, ok := hexDigest(name[:sha256.Size*2]); ok {
			// A digest followed by a separator is one of this layout's own staging
			// names. A digest with junk run onto it is not anything the cache
			// writes, so it stays unexpected rather than being excused.
			switch name[sha256.Size*2] {
			case '.', '-':
				return entryStaging, digest
			}
		}
	}
	if strings.HasPrefix(name, stagingBlobPrefix) && len(name) > len(stagingBlobPrefix) {
		return entryStaging, ""
	}
	return entryUnexpected, ""
}

// CacheCheckProblem is one thing found about one file. Path is relative to the
// repository directory, never absolute: the report is shown next to a repository
// the user already named, and an absolute path would only repeat the cache root.
type CacheCheckProblem struct {
	Kind   string `json:"kind"`
	Path   string `json:"path,omitempty"`
	Detail string `json:"detail,omitempty"`
	Size   int64  `json:"size,omitempty"`
}

// CacheCheckRepo is one repository: what it holds and what is wrong with it.
type CacheCheckRepo struct {
	Repo  string `json:"repo"`
	Type  string `json:"type"`
	Blobs int    `json:"blobs"`
	Bytes int64  `json:"bytes"`
	// ScannedBytes is the bytes actually re-read and hashed, which is zero unless
	// the byte pass was asked for. The difference between the two numbers is what
	// a quick pass did not look at.
	ScannedBytes int64               `json:"scannedBytes,omitempty"`
	StagingFiles int                 `json:"stagingFiles,omitempty"`
	StagingBytes int64               `json:"stagingBytes,omitempty"`
	Problems     []CacheCheckProblem `json:"problems,omitempty"`
	// ProblemsOmitted counts the findings this repository produced beyond the ones
	// listed, so a truncated list still reports how wrong things are. The counts
	// in the summary include them.
	ProblemsOmitted int    `json:"problemsOmitted,omitempty"`
	ReadError       string `json:"readError,omitempty"`
}

// A library can be wrong in more ways than one response is worth carrying: one
// failing disk makes every blob it holds untrustworthy, and a byte pass over a
// terabyte of them would report a finding per file. The totals stay complete and
// only the listing is bounded, because this report is rendered in a browser and
// travels over the socket, while the numbers are what decides what to do next.
const (
	maxReportedProblems = 20
	maxReportedRepos    = 50
)

func (r *CacheCheckRepo) addProblem(p CacheCheckProblem) {
	if len(r.Problems) >= maxReportedProblems {
		r.ProblemsOmitted++
		return
	}
	r.Problems = append(r.Problems, p)
}

// problemsFound counts every finding, listed or not.
func (r *CacheCheckRepo) problemsFound() int { return len(r.Problems) + r.ProblemsOmitted }

// CacheCheckResult is the whole answer. Repos lists only the repositories with a
// problem or a read error - a library of hundreds of intact repositories does not
// need to be listed to say they are intact - while the counts above describe every
// repository that was checked. A pass that was stopped describes only the
// repositories it read to the end: the one it stopped inside is in no count, since
// half a read says nothing about it. Both listings are bounded (maxReportedRepos,
// maxReportedProblems); ReposOmitted and each repository's ProblemsOmitted say
// what was left out, and the counts above already include it.
type CacheCheckResult struct {
	Success           bool             `json:"success"`
	VerifyBytes       bool             `json:"verifyBytes"`
	ReposChecked      int              `json:"reposChecked"`
	ReposClean        int              `json:"reposClean"`
	ReposProblem      int              `json:"reposProblem"`
	ReposUnreadable   int              `json:"reposUnreadable"`
	Blobs             int              `json:"blobs"`
	Bytes             int64            `json:"bytes"`
	BytesHuman        string           `json:"bytesHuman"`
	ScannedBytes      int64            `json:"scannedBytes"`
	ScannedBytesHuman string           `json:"scannedBytesHuman"`
	StagingFiles      int              `json:"stagingFiles"`
	StagingBytes      int64            `json:"stagingBytes"`
	ElapsedMS         int64            `json:"elapsedMs"`
	Problems          int              `json:"problems"`
	Repos             []CacheCheckRepo `json:"repos,omitempty"`
	// ReposOmitted counts the repositories with a finding that are not listed
	// because the report is bounded. Their findings are in Problems, and they are
	// in ReposProblem.
	ReposOmitted int      `json:"reposOmitted,omitempty"`
	Errors       []string `json:"errors,omitempty"`
	Message      string   `json:"message"`
}

// cacheCheckOptions bounds one pass. Ctx and Progress are nil for a caller that
// only wants the answer; the task runner sets both so the pass can be watched and
// stopped, like every other long operation in this server.
type cacheCheckOptions struct {
	// RepoFilter narrows the pass to repositories whose id contains it, the same
	// rule the mirror sync uses, so a user can check one thing after noticing a
	// problem with it.
	RepoFilter  string
	VerifyBytes bool
	Ctx         context.Context
	Progress    func(cacheCheckProgress)
}

// cacheCheckProgress is one step of a running pass: how many repositories are
// known, how many have been looked at, which one is being read, and how many
// bytes have been hashed so far.
type cacheCheckProgress struct {
	State        string `json:"state"`
	Repo         string `json:"repo,omitempty"`
	Done         int    `json:"doneRepos"`
	Total        int    `json:"totalRepos"`
	ScannedBytes int64  `json:"scannedBytes"`
	Problems     int    `json:"problems"`
}

// checkCacheHub walks one hub directory and reports what its repositories hold.
// The only error it returns is about the hub itself: a directory that exists but
// cannot be read is a failure, while a hub that is not there yet is an empty
// library and a complete answer.
func checkCacheHub(hubDir string, opts cacheCheckOptions) (*CacheCheckResult, error) {
	started := time.Now()
	result := &CacheCheckResult{Success: true, VerifyBytes: opts.VerifyBytes}
	report := func(p cacheCheckProgress) {
		if opts.Progress != nil {
			opts.Progress(p)
		}
	}
	stop := func() bool { return opts.Ctx != nil && opts.Ctx.Err() != nil }

	entries, err := hubRepoEntries(hubDir)
	if err != nil {
		return nil, err
	}
	if opts.RepoFilter != "" {
		var kept []hubRepoEntry
		for _, e := range entries {
			if strings.Contains(strings.ToLower(e.Repo), strings.ToLower(opts.RepoFilter)) {
				kept = append(kept, e)
			}
		}
		entries = kept
	}

	result.ReposChecked = 0
	if len(entries) == 0 {
		if _, statErr := os.Stat(hubDir); os.IsNotExist(statErr) {
			result.Message = "Cache directory does not exist yet - nothing to check"
		} else {
			result.Message = "No cached repositories to check"
		}
		result.BytesHuman = humanSizeBytes(0)
		return result, nil
	}

	total := len(entries)
	done := 0
	report(cacheCheckProgress{State: cacheCheckStatePlanned, Done: done, Total: total})

	// Findings are counted everywhere and listed up to a bound; the bound is
	// reported rather than hidden, so a caller can tell a short list from a small
	// problem. See maxReportedRepos.
	listReportedRepo := func(repo CacheCheckRepo) {
		if len(result.Repos) >= maxReportedRepos {
			result.ReposOmitted++
			return
		}
		result.Repos = append(result.Repos, repo)
	}

	for _, entry := range entries {
		if stop() {
			break
		}
		state := cacheCheckStateChecking
		if opts.VerifyBytes {
			state = cacheCheckStateHashing
		}
		report(cacheCheckProgress{State: state, Repo: entry.Repo, Done: done, Total: total, ScannedBytes: result.ScannedBytes, Problems: result.Problems})

		repo, readFully := checkCacheRepo(contextOr(opts.Ctx), entry, opts.VerifyBytes)
		if !readFully {
			// The pass stopped inside this repository. It read part of it, which is
			// not enough to call it intact, so the repository is in no count and is
			// not listed: `cancelled after N of M` below is what the caller gets,
			// and it is the only honest statement about a library this pass only
			// half looked at.
			break
		}

		result.ReposChecked++
		done++
		result.Blobs += repo.Blobs
		result.Bytes += repo.Bytes
		result.ScannedBytes += repo.ScannedBytes
		result.StagingFiles += repo.StagingFiles
		result.StagingBytes += repo.StagingBytes
		switch {
		case repo.ReadError != "":
			result.ReposUnreadable++
			result.Success = false
			listReportedRepo(repo)
		case repo.problemsFound() > 0:
			result.ReposProblem++
			result.Problems += repo.problemsFound()
			result.Success = false
			listReportedRepo(repo)
		default:
			result.ReposClean++
		}

		report(cacheCheckProgress{State: state, Done: done, Total: total, ScannedBytes: result.ScannedBytes, Problems: result.Problems})
	}

	result.BytesHuman = humanSizeBytes(result.Bytes)
	result.ScannedBytesHuman = humanSizeBytes(result.ScannedBytes)
	result.ElapsedMS = time.Since(started).Milliseconds()

	// A bounded list has to say it is bounded: without this the message reads as
	// though the findings stopped at what is on screen.
	truncated := func() string {
		var notes []string
		if result.ReposOmitted > 0 {
			notes = append(notes, fmt.Sprintf("%d more repositories with findings are not listed", result.ReposOmitted))
		}
		for i := range result.Repos {
			if result.Repos[i].ProblemsOmitted > 0 {
				notes = append(notes, fmt.Sprintf("at most %d findings are listed for one repository", maxReportedProblems))
				break
			}
		}
		if len(notes) == 0 {
			return ""
		}
		return "; " + strings.Join(notes, "; ")
	}

	if done < total {
		result.Success = false
		result.Message = fmt.Sprintf("cancelled after %d of %d repositories", done, total) + truncated()
		report(cacheCheckProgress{State: cacheCheckStateCancelled, Done: done, Total: total, ScannedBytes: result.ScannedBytes, Problems: result.Problems})
		return result, nil
	}

	if result.ReposProblem > 0 || result.ReposUnreadable > 0 {
		result.Message = fmt.Sprintf("%d repositories checked: %d with problems, %d could not be read", total, result.ReposProblem, result.ReposUnreadable) + truncated()
		return result, nil
	}

	if opts.VerifyBytes {
		result.Message = fmt.Sprintf("%d repositories checked, all intact (%s re-read)", total, result.ScannedBytesHuman)
	} else {
		result.Message = fmt.Sprintf("%d repositories checked, names and sizes intact (%s not re-read)", total, result.BytesHuman)
	}
	return result, nil
}

// contextOr keeps a nil ctx from panicking the helpers that require one. The
// checker is called with a context by the task runner and without one by tests.
func contextOr(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// checkCacheRepo reports one repository directory. The second result says whether
// it read the whole repository: a pass stopped inside one describes part of it,
// which is not an answer about its files.
func checkCacheRepo(ctx context.Context, entry hubRepoEntry, verifyBytes bool) (CacheCheckRepo, bool) {
	repo := CacheCheckRepo{Repo: entry.Repo, Type: entry.Type}

	items, err := os.ReadDir(filepath.Join(entry.Path, "blobs"))
	switch {
	case errors.Is(err, os.ErrNotExist):
		repo.addProblem(CacheCheckProblem{Kind: cacheProblemNoBlobs, Detail: "blobs/"})
	case err != nil:
		repo.ReadError = fmt.Sprintf("read blobs directory: %v", err)
		return repo, true
	}

	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return repo, false
		}
		name := item.Name()

		kind, digest := classifyBlobEntry(name)
		info, infoErr := item.Info()
		if infoErr != nil {
			// The entry was listed but cannot be described: say so instead of
			// counting it as nothing.
			repo.addProblem(CacheCheckProblem{Kind: cacheProblemUnreadable, Path: "blobs/" + name, Detail: infoErr.Error()})
			continue
		}
		switch {
		case kind == entryStaging:
			repo.StagingFiles++
			// A .parts/ directory holds range chunks whose bytes belong to the
			// staging file beside it, so only files add to the staged total.
			if !item.IsDir() {
				repo.StagingBytes += info.Size()
			}
			continue
		case kind == entryUnexpected || item.IsDir():
			// A directory named for a digest is not a stored blob either.
			detail := "name is not a sha256"
			if item.IsDir() {
				detail = "directory"
			}
			repo.addProblem(CacheCheckProblem{Kind: cacheProblemUnexpected, Path: "blobs/" + name, Size: info.Size(), Detail: detail})
			continue
		}

		repo.Blobs++
		repo.Bytes += info.Size()
		if info.Size() == 0 && digest != emptyBlobDigest {
			repo.addProblem(CacheCheckProblem{Kind: cacheProblemEmptyBlob, Path: "blobs/" + name, Size: 0})
			continue
		}
		if !verifyBytes {
			continue
		}

		sum, err := hfdownloader.HashFileCtx(ctx, filepath.Join(entry.Path, "blobs", name))
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return repo, false
			}
			repo.addProblem(CacheCheckProblem{Kind: cacheProblemUnreadable, Path: "blobs/" + name, Size: info.Size(), Detail: err.Error()})
			continue
		}
		repo.ScannedBytes += info.Size()
		if sum != digest {
			repo.addProblem(CacheCheckProblem{Kind: cacheProblemHash, Path: "blobs/" + name, Size: info.Size(), Detail: "stored as " + digest + ", content hashes to " + sum})
		}
	}

	links, linksReadFully := checkSnapshotLinks(ctx, entry.Path)
	for _, link := range links {
		repo.addProblem(link)
	}
	return repo, linksReadFully
}

// checkSnapshotLinks finds snapshot entries pointing at a blob that is not there,
// and reports whether it read every link. On Windows the snapshot links are never
// created (they need elevated privileges), so this finds nothing there and says
// nothing - the blob checks above are what carry that platform.
func checkSnapshotLinks(ctx context.Context, repoPath string) ([]CacheCheckProblem, bool) {
	var problems []CacheCheckProblem
	snapshots := filepath.Join(repoPath, "snapshots")
	if _, err := os.Lstat(snapshots); err != nil {
		return nil, true
	}
	// WalkDir hands back whatever the callback returned, so a cancelled context is
	// how an unfinished walk is recognised.
	err := filepath.WalkDir(snapshots, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type()&os.ModeSymlink == 0 {
			return nil
		}
		if _, statErr := os.Stat(path); statErr == nil {
			return nil
		}
		target, _ := os.Readlink(path)
		rel, relErr := filepath.Rel(repoPath, path)
		if relErr != nil {
			rel = path
		}
		problems = append(problems, CacheCheckProblem{Kind: cacheProblemDangling, Path: filepath.ToSlash(rel), Detail: "points at " + filepath.ToSlash(target)})
		return ctx.Err()
	})
	return problems, err == nil
}
