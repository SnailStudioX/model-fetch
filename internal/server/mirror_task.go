// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"time"

	"github.com/SnailStudioX/model-fetch/pkg/hfdownloader"
)

// A mirror push or pull used to run inside the request that asked for it. A
// multi-gigabyte library held that request open for minutes with nothing to show,
// could not be cancelled, and if the browser gave up the copy carried on anyway
// with nowhere to report to. The task below moves that work off the request: the
// handler starts it and answers at once, and the UI follows it the way it follows
// a download job.

const (
	mirrorStateRunning   = "running"
	mirrorStateCompleted = "completed"
	mirrorStateFailed    = "failed"
	mirrorStateCancelled = "cancelled"
)

// MirrorTask is one mirror sync and what it has done so far. There is one at a
// time per server: two of them writing the same destination would race over the
// same repository directories, so a second one is refused until the first ends.
type MirrorTask struct {
	ID          string     `json:"id"`
	Mode        string     `json:"mode"` // push or pull
	Target      string     `json:"target"`
	TargetPath  string     `json:"targetPath"`
	State       string     `json:"state"`
	StartedAt   time.Time  `json:"startedAt"`
	FinishedAt  *time.Time `json:"finishedAt,omitempty"`
	Phase       string     `json:"phase,omitempty"`
	CurrentRepo string     `json:"currentRepo,omitempty"`
	TotalRepos  int        `json:"totalRepos"`
	DoneRepos   int        `json:"doneRepos"`
	CopiedBytes int64      `json:"copiedBytes"`
	Error       string     `json:"error,omitempty"`
	// Result is the completed sync: the counts, the per-repository errors and
	// the message. It is what the page shows when the task ends.
	Result *MirrorSyncResult `json:"result,omitempty"`

	cancel  context.CancelFunc
	release func()
	// done is closed when the run has finished unwinding - after the copy cleanup
	// and after the cache reservation is released - so a shutdown can wait for the
	// destination to be left in a stated condition rather than mid-copy.
	done chan struct{}
}

// snapshot copies the fields a client may see; the cancel, reservation and
// completion handles stay on the server.
func (t *MirrorTask) snapshot() *MirrorTask {
	if t == nil {
		return nil
	}
	out := *t
	out.cancel = nil
	out.release = nil
	out.done = nil
	return &out
}

func newMirrorTaskID() string {
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		// A failing random source is not worth failing the operation over: the id
		// only has to be unique inside this process.
		return "mirror-" + hex.EncodeToString([]byte(time.Now().Format(time.RFC3339Nano)))
	}
	return "mirror-" + hex.EncodeToString(random[:])
}

// broadcastMirrorTask publishes the current task to every connected page, so a
// second tab sees a sync started elsewhere and a reloaded page can reattach to
// the one already running.
func (s *Server) broadcastMirrorTask() {
	if s.wsHub == nil {
		return
	}
	// The copy has to happen inside the lock: the running sync writes these same
	// fields while holding it, so a snapshot taken after unlocking races with it.
	s.mirrorTaskMu.Lock()
	snapshot := s.mirrorTask.snapshot()
	s.mirrorTaskMu.Unlock()
	if snapshot == nil {
		return
	}
	s.wsHub.Broadcast("mirror_task", snapshot)
}

// updateMirrorTask applies one progress report from the running sync and
// publishes it. The sync reports once per repository, which is the rate the UI
// shows.
func (s *Server) updateMirrorTask(progress mirrorProgress) {
	s.mirrorTaskMu.Lock()
	task := s.mirrorTask
	if task != nil && task.State == mirrorStateRunning {
		task.Phase = progress.State
		task.TotalRepos = progress.Total
		task.DoneRepos = progress.Done
		task.CopiedBytes = progress.CopiedBytes
		if progress.Repo != "" {
			task.CurrentRepo = progress.Repo
		}
	}
	s.mirrorTaskMu.Unlock()
	s.broadcastMirrorTask()
}

// handleMirrorTaskCurrent reports the running or most recent mirror task, so a
// page opened after the sync started can still find it.
func (s *Server) handleMirrorTaskCurrent(w http.ResponseWriter, r *http.Request) {
	s.mirrorTaskMu.Lock()
	task := s.mirrorTask.snapshot()
	s.mirrorTaskMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"task": task})
}

// handleMirrorTaskByID reports one task by id. A task stops being addressable
// once a newer one replaces it, which is what the 404 says.
func (s *Server) handleMirrorTaskByID(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mirrorTaskMu.Lock()
	task := s.mirrorTask.snapshot()
	s.mirrorTaskMu.Unlock()
	if task == nil || task.ID != id {
		writeError(w, http.StatusNotFound, "Mirror task not found", "")
		return
	}
	writeJSON(w, http.StatusOK, task)
}

// handleMirrorTaskCancel asks a running task to stop. The sync finishes the
// repository it is inside of - or removes the partial copy - and then ends
// cancelled, so this answers 202 and the caller watches the task for the rest.
func (s *Server) handleMirrorTaskCancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mirrorTaskMu.Lock()
	task := s.mirrorTask
	if task != nil && task.ID == id && task.State == mirrorStateRunning && task.cancel != nil {
		task.cancel()
		task.Phase = "cancelling"
	}
	var snapshot *MirrorTask
	if task != nil {
		snapshot = task.snapshot()
	}
	s.mirrorTaskMu.Unlock()

	if snapshot == nil || snapshot.ID != id {
		writeError(w, http.StatusNotFound, "Mirror task not found", "")
		return
	}
	if snapshot.State != mirrorStateRunning {
		writeError(w, http.StatusConflict, "Mirror task is not running", "state="+snapshot.State)
		return
	}
	s.broadcastMirrorTask()
	writeJSON(w, http.StatusAccepted, map[string]any{
		"success": true,
		"message": "Cancellation requested",
		"task":    snapshot,
	})
}

// startMirrorSync is the shared body of POST /api/mirror/push and
// POST /api/mirror/pull. A dry run stays synchronous: it reads two directory
// trees and writes nothing, so there is nothing to supervise, and the diff panel
// needs its answer in the response.
func (s *Server) startMirrorSync(w http.ResponseWriter, r *http.Request, mode string) {
	var req MirrorSyncRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body", err.Error())
		return
	}
	if req.Target == "" {
		writeError(w, http.StatusBadRequest, "Missing required field: target", "")
		return
	}

	targetsCfg, err := hfdownloader.LoadTargets("")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to load targets", err.Error())
		return
	}
	targetPath := targetsCfg.ResolvePath(req.Target)

	cfg := s.snapshotConfig()
	// Pull is push with the two roots exchanged: both directions compare and
	// write the explicit target's hub child, never a path derived from local ENV.
	srcHub, dstHub := cfg.cache().HubDir(), filepath.Join(targetPath, "hub")
	if mode == "pull" {
		srcHub, dstHub = filepath.Join(targetPath, "hub"), cfg.cache().HubDir()
	}

	if req.DryRun {
		result, err := mirrorSyncHubs(srcHub, dstHub, mirrorSyncOptions{
			RepoFilter:  req.RepoFilter,
			DryRun:      true,
			Verify:      req.Verify,
			DeleteExtra: req.DeleteExtra,
			Force:       req.Force,
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "Mirror sync failed", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, result)
		return
	}

	// The reservation that keeps a selected-GGUF deletion and a mirror write from
	// touching the same files is held for the task's lifetime, not the request
	// that started it, so the 409 still answers before anything is queued.
	release, ok := s.jobs.beginCacheMutation(dstHub)
	if !ok {
		writeError(w, http.StatusConflict, "Selected GGUF is busy", "A selected local GGUF group is being deleted")
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	task := &MirrorTask{
		ID:         newMirrorTaskID(),
		Mode:       mode,
		Target:     req.Target,
		TargetPath: targetPath,
		State:      mirrorStateRunning,
		StartedAt:  time.Now(),
		Phase:      "scanning",
		cancel:     cancel,
		release:    release,
		done:       make(chan struct{}),
	}

	s.mirrorTaskMu.Lock()
	if s.mirrorTask != nil && s.mirrorTask.State == mirrorStateRunning {
		running := s.mirrorTask.snapshot()
		s.mirrorTaskMu.Unlock()
		cancel()
		release()
		writeError(w, http.StatusConflict, "A mirror operation is already running", "id="+running.ID+" mode="+running.Mode)
		return
	}
	s.mirrorTask = task
	// Snapshot before unlocking: the sync started below writes these fields under
	// the same lock, and the response must be the state as started anyway.
	started := task.snapshot()
	s.mirrorTaskMu.Unlock()

	go s.runMirrorTask(task, mirrorSyncOptions{
		RepoFilter:  req.RepoFilter,
		Verify:      req.Verify,
		DeleteExtra: req.DeleteExtra,
		Force:       req.Force,
		Ctx:         ctx,
		Progress:    s.updateMirrorTask,
	}, srcHub, dstHub)

	s.broadcastMirrorTask()
	writeJSON(w, http.StatusAccepted, map[string]any{"task": started})
}

// runMirrorTask performs the sync and records how it ended. The cache
// reservation is released here - at the end of the task - so a selected
// deletion cannot start against a destination that is still being written.
func (s *Server) runMirrorTask(task *MirrorTask, opts mirrorSyncOptions, srcHub, dstHub string) {
	// Registered first so it runs last: a shutdown woken by task.done must see the
	// reservation released as well, or it could go on to exit while a deletion
	// still has the destination half written.
	defer close(task.done)
	defer func() {
		if task.release != nil {
			task.release()
		}
	}()

	result, err := mirrorSyncHubs(srcHub, dstHub, opts)

	finished := time.Now()
	s.mirrorTaskMu.Lock()
	task.FinishedAt = &finished
	task.Result = result
	task.CurrentRepo = ""
	task.Phase = ""
	switch {
	case err != nil:
		task.State = mirrorStateFailed
		task.Error = err.Error()
	case opts.Ctx != nil && opts.Ctx.Err() != nil:
		task.State = mirrorStateCancelled
	case result != nil && !result.Success:
		task.State = mirrorStateFailed
		task.Error = result.Message
	default:
		task.State = mirrorStateCompleted
	}
	s.mirrorTaskMu.Unlock()

	if err != nil {
		log.Printf("mirror %s (%s) failed: %v", task.Mode, task.ID, err)
	}
	s.broadcastMirrorTask()
}

// stopMirrorTaskForShutdown cancels a running sync and waits up to wait for it to
// finish. A sync's context belongs to the task, not to the request that started
// it, so nothing else would stop it: the process would exit while the copy was
// inside a repository, leaving a half-written destination behind with nothing to
// remove it - the one outcome mirrorSyncHubs takes care to avoid. The copy loop
// checks its context per file, so the wait covers the files not yet started and
// the removal of the one just interrupted.
func (s *Server) stopMirrorTaskForShutdown(wait time.Duration) error {
	s.mirrorTaskMu.Lock()
	task := s.mirrorTask
	running := task != nil && task.State == mirrorStateRunning && task.cancel != nil
	if running {
		task.cancel()
	}
	s.mirrorTaskMu.Unlock()
	if !running {
		return nil
	}

	select {
	case <-task.done:
		return nil
	case <-time.After(wait):
		return fmt.Errorf("mirror %s %s did not stop within %s", task.Mode, task.ID, wait)
	}
}
