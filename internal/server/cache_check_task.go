// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log"
	"net/http"
	"time"
)

// A cache check is a pass over every byte of the library when it is asked to
// re-hash, which on a multi-terabyte NAS is minutes of disk work. It therefore
// runs the way a mirror sync does: the handler starts it, answers at once, and the
// caller follows the task. Only one runs at a time - two passes would read the
// same files twice for no reason and the second report would be indistinguishable
// from the first.
//
// Unlike a mirror sync, a check reserves nothing: it only reads, and holding the
// cache-write reservation would freeze deletions and syncs for as long as a byte
// pass takes. A repository that is deleted mid-pass is reported as unreadable
// instead.

// Task states. The phase values a running pass reports are the checker's own
// (cacheCheckState* in cache_check.go); these say whether the task is over.
const (
	checkStateRunning   = "running"
	checkStateCompleted = "completed"
	checkStateFailed    = "failed"
	checkStateCancelled = "cancelled"
)

// CacheCheckTask is one pass and what it has found so far.
type CacheCheckTask struct {
	ID           string     `json:"id"`
	State        string     `json:"state"`
	StartedAt    time.Time  `json:"startedAt"`
	FinishedAt   *time.Time `json:"finishedAt,omitempty"`
	Phase        string     `json:"phase,omitempty"`
	CurrentRepo  string     `json:"currentRepo,omitempty"`
	TotalRepos   int        `json:"totalRepos"`
	DoneRepos    int        `json:"doneRepos"`
	ScannedBytes int64      `json:"scannedBytes"`
	Problems     int        `json:"problems"`
	// VerifyBytes says whether blobs are being re-read and hashed, or only
	// described from their directory entries. A report says which it is, because
	// "checked" would otherwise mean two different things to two different users.
	VerifyBytes bool   `json:"verifyBytes"`
	RepoFilter  string `json:"repoFilter,omitempty"`
	Error       string `json:"error,omitempty"`
	// Result is the finished report: per-repository problems and the counts.
	Result *CacheCheckResult `json:"result,omitempty"`

	cancel context.CancelFunc
}

func (t *CacheCheckTask) snapshot() *CacheCheckTask {
	if t == nil {
		return nil
	}
	out := *t
	out.cancel = nil
	return &out
}

func newCacheCheckTaskID() string {
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "cachecheck-" + hex.EncodeToString([]byte(time.Now().Format(time.RFC3339Nano)))
	}
	return "cachecheck-" + hex.EncodeToString(random[:])
}

func (s *Server) broadcastCacheCheck() {
	if s.wsHub == nil {
		return
	}
	// The copy has to happen inside the lock: the running pass writes these same
	// fields while holding it, so a snapshot taken after unlocking races with it.
	s.cacheCheckTaskMu.Lock()
	snapshot := s.cacheCheckTask.snapshot()
	s.cacheCheckTaskMu.Unlock()
	if snapshot == nil {
		return
	}
	s.wsHub.Broadcast("cache_check", snapshot)
}

// updateCacheCheck applies one progress report from the running pass. The checker
// reports per repository, which is the rate the page shows; the byte count moves
// only between repositories, so a long file shows no progress until it ends.
func (s *Server) updateCacheCheck(progress cacheCheckProgress) {
	s.cacheCheckTaskMu.Lock()
	task := s.cacheCheckTask
	if task != nil && task.State == checkStateRunning {
		task.Phase = progress.State
		task.TotalRepos = progress.Total
		task.DoneRepos = progress.Done
		task.ScannedBytes = progress.ScannedBytes
		task.Problems = progress.Problems
		if progress.Repo != "" {
			task.CurrentRepo = progress.Repo
		}
	}
	s.cacheCheckTaskMu.Unlock()
	s.broadcastCacheCheck()
}

func (s *Server) handleCacheCheckCurrent(w http.ResponseWriter, r *http.Request) {
	s.cacheCheckTaskMu.Lock()
	task := s.cacheCheckTask.snapshot()
	s.cacheCheckTaskMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"task": task})
}

func (s *Server) handleCacheCheckByID(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.cacheCheckTaskMu.Lock()
	task := s.cacheCheckTask.snapshot()
	s.cacheCheckTaskMu.Unlock()
	if task == nil || task.ID != id {
		writeError(w, http.StatusNotFound, "Cache check not found", "")
		return
	}
	writeJSON(w, http.StatusOK, task)
}

// handleCacheCheckCancel asks a running pass to stop between repositories. A pass
// that stopped has only looked at part of the library, so its report says so
// rather than reporting the part it saw as if it were the whole answer.
func (s *Server) handleCacheCheckCancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.cacheCheckTaskMu.Lock()
	task := s.cacheCheckTask
	if task != nil && task.ID == id && task.State == checkStateRunning && task.cancel != nil {
		task.cancel()
		task.Phase = "cancelling"
	}
	var snapshot *CacheCheckTask
	if task != nil {
		snapshot = task.snapshot()
	}
	s.cacheCheckTaskMu.Unlock()

	if snapshot == nil || snapshot.ID != id {
		writeError(w, http.StatusNotFound, "Cache check not found", "")
		return
	}
	if snapshot.State != checkStateRunning {
		writeError(w, http.StatusConflict, "Cache check is not running", "state="+snapshot.State)
		return
	}
	s.broadcastCacheCheck()
	writeJSON(w, http.StatusAccepted, map[string]any{
		"success": true,
		"message": "Cancellation requested",
		"task":    snapshot,
	})
}

// startCacheCheck is POST /api/cache-check. `verifyBytes` asks for every blob to
// be re-read and hashed against the name it is stored under; without it the pass
// reads directory entries and sizes only, which is what makes a whole-library
// check cheap enough to run often.
func (s *Server) startCacheCheck(w http.ResponseWriter, r *http.Request) {
	var req struct {
		VerifyBytes bool   `json:"verifyBytes"`
		RepoFilter  string `json:"repoFilter"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body", err.Error())
		return
	}

	cfg := s.snapshotConfig()
	hubDir := cfg.cache().HubDir()

	ctx, cancel := context.WithCancel(context.Background())
	task := &CacheCheckTask{
		ID:          newCacheCheckTaskID(),
		State:       checkStateRunning,
		StartedAt:   time.Now(),
		Phase:       cacheCheckStatePlanned,
		VerifyBytes: req.VerifyBytes,
		RepoFilter:  req.RepoFilter,
		cancel:      cancel,
	}

	s.cacheCheckTaskMu.Lock()
	if s.cacheCheckTask != nil && s.cacheCheckTask.State == checkStateRunning {
		running := s.cacheCheckTask.snapshot()
		s.cacheCheckTaskMu.Unlock()
		cancel()
		writeError(w, http.StatusConflict, "A cache check is already running", "id="+running.ID)
		return
	}
	s.cacheCheckTask = task
	// Snapshot before unlocking: the pass started below writes these fields under
	// the same lock, and the response must be the state as started anyway.
	started := task.snapshot()
	s.cacheCheckTaskMu.Unlock()

	go s.runCacheCheck(task, cacheCheckOptions{
		RepoFilter:  req.RepoFilter,
		VerifyBytes: req.VerifyBytes,
		Ctx:         ctx,
		Progress:    s.updateCacheCheck,
	}, hubDir)

	s.broadcastCacheCheck()
	writeJSON(w, http.StatusAccepted, map[string]any{"task": started})
}

// runCacheCheck performs the pass and records how it ended.
func (s *Server) runCacheCheck(task *CacheCheckTask, opts cacheCheckOptions, hubDir string) {
	result, err := checkCacheHub(hubDir, opts)

	finished := time.Now()
	s.cacheCheckTaskMu.Lock()
	task.FinishedAt = &finished
	task.Result = result
	task.CurrentRepo = ""
	task.Phase = ""
	switch {
	case err != nil:
		task.State = checkStateFailed
		task.Error = err.Error()
	case opts.Ctx != nil && opts.Ctx.Err() != nil:
		task.State = checkStateCancelled
	default:
		// A pass that found something wrong with the library is a completed check,
		// not a failed one: the report is the answer, and `result.success` says
		// what was found.
		task.State = checkStateCompleted
	}
	s.cacheCheckTaskMu.Unlock()

	if err != nil {
		log.Printf("cache check %s failed: %v", task.ID, err)
	}
	s.broadcastCacheCheck()
}

// stopCacheCheckForShutdown stops a running pass when the server is going away.
// Unlike a mirror sync there is nothing to wait for: a check only reads, so an
// interrupted pass leaves no state behind that has to be cleaned up. Cancelling
// is what stops it reading a slow disk after the server has gone.
func (s *Server) stopCacheCheckForShutdown() {
	s.cacheCheckTaskMu.Lock()
	task := s.cacheCheckTask
	if task != nil && task.State == checkStateRunning && task.cancel != nil {
		task.cancel()
	}
	s.cacheCheckTaskMu.Unlock()
}
