// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The cache check is the only thing in this app that answers "is what I already
// downloaded still what it was". The rules it depends on are the cache's own:
// hfcache names every blob by the SHA-256 of its contents, so a stored file
// carries the digest it must hash to. These tests pin which findings exist, that
// staging files are never called corruption, that a repository which could not be
// read is never counted as clean, and that the pass can be run as a task.

func digestOf(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// newCachedRepo creates one repository directory the way the cache writes it, and
// returns its path.
func newCachedRepo(t *testing.T, hubDir, repo string) string {
	t.Helper()

	repoDir := filepath.Join(hubDir, "models--"+strings.ReplaceAll(repo, "/", "--"))
	if err := os.MkdirAll(filepath.Join(repoDir, "blobs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoDir, "refs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "refs", "main"), []byte("deadbeef\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return repoDir
}

func writeBlob(t *testing.T, repoDir, name, content string) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(repoDir, "blobs", name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeContentNamedAs stores content under a chosen name, which is how a blob ends
// up disagreeing with its own name.
func writeContentNamedAs(t *testing.T, repoDir, content, as string) {
	t.Helper()

	writeBlob(t, repoDir, as, content)
}

func checkProblems(result *CacheCheckResult, repo string) []CacheCheckProblem {
	for _, r := range result.Repos {
		if r.Repo == repo {
			return r.Problems
		}
	}
	return nil
}

func problemKinds(problems []CacheCheckProblem) []string {
	var kinds []string
	for _, p := range problems {
		kinds = append(kinds, p.Kind)
	}
	return kinds
}

func hasKind(problems []CacheCheckProblem, kind string) bool {
	for _, p := range problems {
		if p.Kind == kind {
			return true
		}
	}
	return false
}

func TestCacheCheckNamesAndSizes(t *testing.T) {
	hub := t.TempDir()

	ok := newCachedRepo(t, hub, "owner/ok")
	writeContentNamedAs(t, ok, "hello world", digestOf("hello world"))
	// A blob named for the empty string is legitimately empty, not a defect.
	writeBlob(t, ok, digestOf(""), "")

	bad := newCachedRepo(t, hub, "owner/bad")
	writeBlob(t, bad, digestOf("payload"), "")                   // empty under a real digest
	writeBlob(t, bad, "not-a-digest.txt", "stray")               // not written by this layout
	writeBlob(t, bad, digestOf("partial")+".incomplete", "half") // a download that did not finish

	result, err := checkCacheHub(hub, cacheCheckOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Success {
		t.Fatalf("a corrupted library reported success: %+v", result)
	}
	if result.ReposChecked != 2 || result.ReposClean != 1 || result.ReposProblem != 1 {
		t.Errorf("counts = %d checked / %d clean / %d problem, want 2/1/1 (%+v)", result.ReposChecked, result.ReposClean, result.ReposProblem, result)
	}
	if result.VerifyBytes {
		t.Error("a quick pass must say it did not re-read bytes")
	}
	if result.ScannedBytes != 0 {
		t.Errorf("a quick pass hashed %d bytes", result.ScannedBytes)
	}

	problems := checkProblems(result, "owner/bad")
	if !hasKind(problems, cacheProblemEmptyBlob) {
		t.Errorf("no empty-blob finding: %v", problemKinds(problems))
	}
	if !hasKind(problems, cacheProblemUnexpected) {
		t.Errorf("no unexpected-entry finding: %v", problemKinds(problems))
	}
	for _, p := range problems {
		if p.Kind == cacheProblemUnexpected && !strings.Contains(p.Path, "not-a-digest.txt") {
			t.Errorf("the unexpected finding does not name the file: %+v", p)
		}
	}
	if checkProblems(result, "owner/ok") != nil {
		t.Errorf("an intact repository was reported with problems: %+v", checkProblems(result, "owner/ok"))
	}

	// The interrupted download is counted and named, but it is not corruption and
	// it does not make the library read as broken on its own.
	var repo CacheCheckRepo
	for _, r := range result.Repos {
		if r.Repo == "owner/bad" {
			repo = r
		}
	}
	if repo.StagingFiles != 1 || repo.StagingBytes != int64(len("half")) {
		t.Errorf("the staging file was not accounted for: %+v", repo)
	}
	if repo.Blobs != 1 {
		t.Errorf("blob count = %d, want 1 (the .incomplete and the stray name are not blobs)", repo.Blobs)
	}
	// Only the problem repository is listed; the intact one is a count.
	if len(result.Repos) != 1 {
		t.Errorf("listed %d repositories, want only the one with problems", len(result.Repos))
	}
}

// The byte pass is the one that can see content changed under a correct name -
// a truncated file, a zeroed block, a hand edit.
func TestCacheCheckBytePassFindsContentThatDoesNotMatchItsName(t *testing.T) {
	hub := t.TempDir()
	repo := newCachedRepo(t, hub, "owner/quiet")
	// Named for the digest of the original text, holding an edited one.
	writeContentNamedAs(t, repo, "hello werld", digestOf("hello world"))

	quick, err := checkCacheHub(hub, cacheCheckOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !quick.Success || checkProblems(quick, "owner/quiet") != nil {
		t.Fatalf("a name-only pass should not find this, and should not hide it either: %+v", quick)
	}
	if quick.Bytes != int64(len("hello werld")) {
		t.Errorf("library size = %d, want %d", quick.Bytes, len("hello werld"))
	}

	full, err := checkCacheHub(hub, cacheCheckOptions{VerifyBytes: true})
	if err != nil {
		t.Fatal(err)
	}
	problems := checkProblems(full, "owner/quiet")
	if !hasKind(problems, cacheProblemHash) {
		t.Fatalf("the byte pass missed a changed file: %+v", full)
	}
	for _, p := range problems {
		if p.Kind != cacheProblemHash {
			continue
		}
		if !strings.Contains(p.Detail, digestOf("hello world")) || !strings.Contains(p.Detail, digestOf("hello werld")) {
			t.Errorf("the finding does not name both digests: %+v", p)
		}
	}
	if full.ScannedBytes != int64(len("hello werld")) {
		t.Errorf("scanned %d bytes, want %d", full.ScannedBytes, len("hello werld"))
	}
	if !full.VerifyBytes {
		t.Error("the report does not say which pass ran")
	}
}

// A repository whose files cannot be read is not a clean repository. Silence here
// would turn "unknown" into "verified".
func TestCacheCheckNeverCountsAnUnreadableRepositoryAsClean(t *testing.T) {
	hub := t.TempDir()
	repo := filepath.Join(hub, "models--owner--blocked")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	// A blobs entry that is a file instead of a directory. On Unix the listing
	// fails outright; Windows reports the directory as not there. Both must end up
	// surfaced and neither may be counted clean - only the reason differs, so the
	// platform-specific half is asserted where it is observable.
	if err := os.WriteFile(filepath.Join(repo, "blobs"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	// And a repository with no blobs directory at all.
	empty := filepath.Join(hub, "models--owner--nobody")
	if err := os.MkdirAll(filepath.Join(empty, "refs"), 0o755); err != nil {
		t.Fatal(err)
	}

	result, err := checkCacheHub(hub, cacheCheckOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Success {
		t.Error("an unreadable library reported success")
	}
	if result.ReposClean != 0 {
		t.Errorf("counted %d repositories clean, want none", result.ReposClean)
	}
	var blocked *CacheCheckRepo
	for i := range result.Repos {
		if result.Repos[i].Repo == "owner/blocked" {
			blocked = &result.Repos[i]
		}
	}
	if blocked == nil {
		t.Fatalf("the unreadable repository was swallowed instead of reported: %+v", result)
	}
	if runtime.GOOS != "windows" {
		if blocked.ReadError == "" {
			t.Errorf("a blobs path that is not a directory should fail the listing, got %+v", blocked)
		}
		if !strings.Contains(result.Message, "could not be read") {
			t.Errorf("the message hides the unreadable repository: %q", result.Message)
		}
	}
	if !hasKind(checkProblems(result, "owner/nobody"), cacheProblemNoBlobs) {
		t.Errorf("a repository with no blobs was not named: %+v", result.Repos)
	}
}

// A hub that does not exist yet is a fresh cache: an empty answer, not a failure.
func TestCacheCheckEmptyLibraryIsNotAnError(t *testing.T) {
	result, err := checkCacheHub(filepath.Join(t.TempDir(), "hub"), cacheCheckOptions{})
	if err != nil {
		t.Fatalf("an uncreated cache dir: %v", err)
	}
	if !result.Success || result.ReposChecked != 0 {
		t.Errorf("empty library reported %+v", result)
	}
	if !strings.Contains(result.Message, "does not exist") {
		t.Errorf("the message does not say the cache is not there: %q", result.Message)
	}
}

func TestCacheCheckStopsBetweenRepositories(t *testing.T) {
	hub := t.TempDir()
	for _, repo := range []string{"owner/one", "owner/two", "owner/three"} {
		writeContentNamedAs(t, newCachedRepo(t, hub, repo), repo, digestOf(repo))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var seen []cacheCheckProgress
	result, err := checkCacheHub(hub, cacheCheckOptions{
		Ctx: ctx,
		Progress: func(p cacheCheckProgress) {
			seen = append(seen, p)
			if p.Done >= 1 {
				cancel()
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Success {
		t.Error("a cancelled pass reported success")
	}
	if result.ReposChecked != 1 || result.ReposClean != 1 {
		t.Errorf("a cancelled pass must report only what it looked at: %+v", result)
	}
	if !strings.Contains(result.Message, "cancelled after 1 of 3") {
		t.Errorf("the message does not say how far it got: %q", result.Message)
	}
	if len(seen) == 0 || seen[0].Total != 3 {
		t.Errorf("the first progress report should name all three repositories: %+v", seen)
	}
	var cancelled bool
	for _, p := range seen {
		if p.State == cacheCheckStateCancelled {
			cancelled = true
		}
	}
	if !cancelled {
		t.Error("the progress stream never says it was cancelled")
	}
}

// The pass can also be stopped *inside* a repository - the realistic case is a
// byte pass cancelled while it hashes a large blob. A repository only partly
// read is not known to be intact, so it belongs in no count and is not listed:
// the report says how far the pass got and stops there.
func TestCacheCheckStoppedInsideARepository(t *testing.T) {
	hub := t.TempDir()
	for _, repo := range []string{"owner/a", "owner/b", "owner/c"} {
		writeContentNamedAs(t, newCachedRepo(t, hub, repo), repo, digestOf(repo))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result, err := checkCacheHub(hub, cacheCheckOptions{
		Ctx: ctx,
		Progress: func(p cacheCheckProgress) {
			// The report naming a repository is the one just before it is read, so
			// the pass enters owner/b and stops at its first entry.
			if p.Repo == "owner/b" {
				cancel()
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ReposChecked != 1 || result.ReposClean != 1 {
		t.Errorf("a repository read in part was counted: %d checked / %d clean: %+v", result.ReposChecked, result.ReposClean, result)
	}
	if result.ReposProblem != 0 || result.ReposUnreadable != 0 {
		t.Errorf("an unfinished repository was judged: %+v", result)
	}
	if !strings.Contains(result.Message, "cancelled after 1 of 3") {
		t.Errorf("the message does not say how far it got: %q", result.Message)
	}
	for _, repo := range result.Repos {
		if repo.Repo == "owner/b" {
			t.Errorf("the unfinished repository is listed as if it had been read: %+v", repo)
		}
	}
}

// The pass over a real cache is one repository at a time; scoping it to a
// repository the user is suspicious of must not walk the rest.
func TestCacheCheckScopeFilter(t *testing.T) {
	hub := t.TempDir()
	writeContentNamedAs(t, newCachedRepo(t, hub, "owner/wanted"), "wanted", digestOf("wanted"))
	writeContentNamedAs(t, newCachedRepo(t, hub, "other/skip"), "skipped", digestOf("skipped"))

	result, err := checkCacheHub(hub, cacheCheckOptions{RepoFilter: "wanted"})
	if err != nil {
		t.Fatal(err)
	}
	if result.ReposChecked != 1 {
		t.Errorf("checked %d repositories, want the one named by the filter: %+v", result.ReposChecked, result)
	}
}

// A snapshot entry whose blob is gone is a hole in a repository that otherwise
// reads as present. Windows never creates these links, so the same code finds
// nothing there; that is why the blob checks are what carry that platform.
func TestCacheCheckFindsDanglingSnapshotLinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix symlinks")
	}
	hub := t.TempDir()
	repo := newCachedRepo(t, hub, "owner/hole")
	writeContentNamedAs(t, repo, "kept", digestOf("kept"))

	snapshot := filepath.Join(repo, "snapshots", "commit-a")
	if err := os.MkdirAll(snapshot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "..", "blobs", digestOf("gone")), filepath.Join(snapshot, "model.gguf")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "..", "blobs", digestOf("kept")), filepath.Join(snapshot, "intact.txt")); err != nil {
		t.Fatal(err)
	}

	result, err := checkCacheHub(hub, cacheCheckOptions{})
	if err != nil {
		t.Fatal(err)
	}
	problems := checkProblems(result, "owner/hole")
	if !hasKind(problems, cacheProblemDangling) {
		t.Fatalf("the missing blob behind a snapshot link was not found: %+v", result)
	}
	for _, p := range problems {
		if p.Kind == cacheProblemDangling {
			if !strings.Contains(p.Path, "model.gguf") {
				t.Errorf("the finding does not name the file: %+v", p)
			}
			if strings.Contains(p.Path, "intact.txt") {
				t.Errorf("an intact link was reported: %+v", p)
			}
		}
	}
}

func TestCacheCheckClassificationRules(t *testing.T) {
	digest := digestOf("x")
	cases := []struct{ name, want string }{
		{digest, entryBlob},
		{strings.ToUpper(digest), entryBlob},
		{digest + ".incomplete", entryStaging},
		{digest + ".incomplete.meta", entryStaging},
		{digest + ".2.incomplete", entryStaging},
		{digest + ".json.tmp", entryStaging},
		{digest + "-tmp-8f3a1c", entryStaging},
		// The names the download path itself writes into blobs/ while a blob is in
		// flight, and the numbered sibling a second concurrent claimer gets. These
		// carry no leading digest, so they need the tmp- rule to not be read as
		// something the layout did not write.
		{"tmp-" + digest, entryStaging},
		{"tmp-" + digest + "-2", entryStaging},
		{"tmp-" + digest + ".part-00", entryStaging},
		{"tmp-" + digest + ".parts", entryStaging},
		{"tmp-" + digest + ".parts.json", entryStaging},
		{"tmp-model_config.json", entryStaging},
		{"tmp-" + strings.Repeat("a", 64) + ".tmp-9c1f", entryStaging},
		{digest + "tooshort", entryUnexpected},
		{"notes.txt", entryUnexpected},
		{".DS_Store", entryUnexpected},
		{"tmp-", entryUnexpected},
		{strings.Repeat("a", 63), entryUnexpected},
		{strings.Repeat("g", 64), entryUnexpected},
	}
	for _, tc := range cases {
		if kind, _ := classifyBlobEntry(tc.name); kind != tc.want {
			t.Errorf("classifyBlobEntry(%q) = %q, want %q", tc.name, kind, tc.want)
		}
	}
}

// A check reserves nothing, so running it while a download is in progress is the
// normal case, not an edge: the job's own working files sit in blobs/ for the
// duration. They are counted as staging and never reported as corruption, and the
// library still reads as intact.
func TestCacheCheckRunningDownloadIsNotCorruption(t *testing.T) {
	hub := t.TempDir()
	repoDir := newCachedRepo(t, hub, "owner/running")

	writeContentNamedAs(t, repoDir, "complete payload", digestOf("complete payload"))

	// The names the download path writes while a blob is in flight: the transfer
	// destination, a second concurrent claim of it, a multipart range and its
	// layout metadata, the directory holding the ranges, and the no-digest
	// fallback used for a hub that reports no SHA.
	staged := "tmp-" + digestOf("still downloading")
	files := []string{
		staged,
		staged + "-2",
		staged + ".part-00",
		staged + ".parts.json",
		"tmp-model-Q4_K_M.gguf",
	}
	for _, name := range files {
		writeBlob(t, repoDir, name, "partial")
	}
	if err := os.MkdirAll(filepath.Join(repoDir, "blobs", staged+".parts"), 0o755); err != nil {
		t.Fatal(err)
	}

	result, err := checkCacheHub(hub, cacheCheckOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success {
		t.Errorf("a library with a job still running reported problems: %+v", result)
	}
	if result.ReposChecked != 1 || result.ReposClean != 1 {
		t.Errorf("counts = %d checked / %d clean: %+v", result.ReposChecked, result.ReposClean, result)
	}
	if result.Blobs != 1 {
		t.Errorf("blob count = %d, want only the published blob: %+v", result.Blobs, result)
	}
	wantBytes := int64(len("partial")) * int64(len(files))
	if result.StagingFiles != len(files)+1 || result.StagingBytes != wantBytes {
		t.Errorf("staging = %d files / %d bytes, want %d / %d: %+v", result.StagingFiles, result.StagingBytes, len(files)+1, wantBytes, result)
	}
	if len(result.Repos) != 0 {
		t.Errorf("a running download was listed as a problem: %+v", result.Repos)
	}
}

// A failing disk makes every blob it holds untrustworthy, and this report is
// rendered in a browser and pushed over the socket. The listing is therefore
// bounded while the counts stay complete, and the message says the listing is
// bounded rather than letting a short list read as a small problem.
func TestCacheCheckBoundsAReportItCannotFit(t *testing.T) {
	hub := t.TempDir()

	// "aaa" sorts before the bad-* repositories, so this one is listed and its
	// own truncation can be read back.
	noisy := newCachedRepo(t, hub, "owner/aaa-noisy")
	const noisyFindings = maxReportedProblems + 5
	for i := 0; i < noisyFindings; i++ {
		writeBlob(t, noisy, digestOf("payload-"+strconv.Itoa(i)), "")
	}

	const otherBroken = maxReportedRepos + 2
	for i := 0; i < otherBroken; i++ {
		broken := newCachedRepo(t, hub, fmt.Sprintf("owner/bad-%02d", i))
		writeBlob(t, broken, digestOf("empty-under-a-real-digest"), "")
	}

	result, err := checkCacheHub(hub, cacheCheckOptions{})
	if err != nil {
		t.Fatal(err)
	}

	wantProblemRepos := 1 + otherBroken
	if result.ReposProblem != wantProblemRepos {
		t.Errorf("reposProblem = %d, want every problem repository counted (%d)", result.ReposProblem, wantProblemRepos)
	}
	wantFindings := noisyFindings + otherBroken
	if result.Problems != wantFindings {
		t.Errorf("problems total = %d, want every finding counted (%d)", result.Problems, wantFindings)
	}

	if len(result.Repos) != maxReportedRepos {
		t.Errorf("listed %d repositories, want the bound (%d)", len(result.Repos), maxReportedRepos)
	}
	if result.ReposOmitted != wantProblemRepos-maxReportedRepos {
		t.Errorf("reposOmitted = %d, want %d", result.ReposOmitted, wantProblemRepos-maxReportedRepos)
	}

	var listed *CacheCheckRepo
	for i := range result.Repos {
		if result.Repos[i].Repo == "owner/aaa-noisy" {
			listed = &result.Repos[i]
		}
	}
	if listed == nil {
		t.Fatal("the noisiest repository is not in the report to check its own bound")
	}
	if len(listed.Problems) != maxReportedProblems {
		t.Errorf("listed %d findings for one repository, want the bound (%d)", len(listed.Problems), maxReportedProblems)
	}
	if listed.ProblemsOmitted != noisyFindings-maxReportedProblems {
		t.Errorf("problemsOmitted = %d, want %d", listed.ProblemsOmitted, noisyFindings-maxReportedProblems)
	}

	if !strings.Contains(result.Message, "not listed") {
		t.Errorf("the message hides that the report is bounded: %q", result.Message)
	}
}

// startCacheCheckAndWait posts a check and waits for its task to leave the running
// state, returning the finished task.
func startCacheCheckAndWait(t *testing.T, s *Server, body string) *CacheCheckTask {
	t.Helper()

	w := cacheRequest(t, s, http.MethodPost, "/api/cache-check", body)
	if w.Code != http.StatusAccepted {
		t.Fatalf("starting the check: status=%d body=%s", w.Code, w.Body.String())
	}
	var started struct {
		Task CacheCheckTask `json:"task"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	if started.Task.State != checkStateRunning || started.Task.ID == "" {
		t.Fatalf("a started task reports %+v", started.Task)
	}

	deadline := time.Now().Add(30 * time.Second)
	for {
		w := cacheRequest(t, s, http.MethodGet, "/api/cache-check/"+started.Task.ID, "")
		if w.Code == http.StatusOK {
			var task CacheCheckTask
			if err := json.Unmarshal(w.Body.Bytes(), &task); err != nil {
				t.Fatal(err)
			}
			if task.State != checkStateRunning {
				return &task
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %s never finished", started.Task.ID)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The endpoints: a check runs as a task, its report is readable afterwards, and a
// second one while it runs is refused.
func TestCacheCheckRunsAsTask(t *testing.T) {
	cacheDir := t.TempDir()
	hub := filepath.Join(cacheDir, "hub")
	writeContentNamedAs(t, newCachedRepo(t, hub, "owner/ok"), "fine", digestOf("fine"))
	writeBlob(t, newCachedRepo(t, hub, "owner/broken"), digestOf("original"), "")

	s := New(Config{CacheDir: cacheDir})
	task := startCacheCheckAndWait(t, s, `{"verifyBytes":true}`)

	if task.State != checkStateCompleted {
		t.Fatalf("task state = %q (%q)", task.State, task.Error)
	}
	if task.Result == nil || task.Result.Success {
		t.Fatalf("the report is missing or says the library is fine: %+v", task.Result)
	}
	if !task.Result.VerifyBytes {
		t.Error("the report does not say the byte pass ran")
	}
	if !task.VerifyBytes {
		t.Error("the task does not report what it was asked to do")
	}
	if task.Result.ReposChecked != 2 || task.Result.ReposProblem != 1 {
		t.Errorf("report counts = %+v", task.Result)
	}
	if task.TotalRepos != 2 || task.DoneRepos != 2 {
		t.Errorf("progress = %d/%d, want 2/2", task.DoneRepos, task.TotalRepos)
	}
	if task.FinishedAt == nil {
		t.Error("a finished task does not say when it finished")
	}

	// A page reloaded after the check must still be able to show the report.
	w := cacheRequest(t, s, http.MethodGet, "/api/cache-check", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/cache-check: %d %s", w.Code, w.Body.String())
	}
	var current struct {
		Task CacheCheckTask `json:"task"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &current); err != nil {
		t.Fatal(err)
	}
	if current.Task.ID != task.ID || current.Task.State != checkStateCompleted {
		t.Errorf("the last check is not reportable: %+v", current.Task)
	}
	if w := cacheRequest(t, s, http.MethodGet, "/api/cache-check/cachecheck-never-ran", ""); w.Code != http.StatusNotFound {
		t.Errorf("unknown check id status = %d, want 404", w.Code)
	}
	if w := cacheRequest(t, s, http.MethodPost, "/api/cache-check/"+task.ID+"/cancel", ""); w.Code != http.StatusConflict {
		t.Errorf("cancelling a finished check status = %d, want 409", w.Code)
	}
}

func TestCacheCheckRefusesASecondRunningPass(t *testing.T) {
	s := New(Config{CacheDir: t.TempDir()})

	s.cacheCheckTaskMu.Lock()
	s.cacheCheckTask = &CacheCheckTask{ID: "cachecheck-busy", State: checkStateRunning}
	s.cacheCheckTaskMu.Unlock()

	w := cacheRequest(t, s, http.MethodPost, "/api/cache-check", `{}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("second pass while one runs: status=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "cachecheck-busy") {
		t.Errorf("the conflict does not name the running pass: %s", w.Body.String())
	}
}
