// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"testing"
	"time"

	"github.com/SnailStudioX/model-fetch/pkg/hfdownloader"
)

// TestApplyJobProgress_ResumeFloorReleasesOnAuthoritativeProgress is the
// regression for "the card says 100% but the job is still downloading". A
// resumed job arms a floor holding the pre-pause total so the bar cannot
// collapse while the plan is being replayed. That floor used to be released
// only when the running sum caught up to it, so a job whose partial bytes were
// discarded on resume, or whose pre-pause total was inflated by the full-size
// progress emit that runs before part assembly, stayed pinned at the old value
// forever. Once every planned file has reported its real on-disk position the
// sum is authoritative and the floor must be released even when the truth is
// LOWER.
func TestApplyJobProgress_ResumeFloorReleasesOnAuthoritativeProgress(t *testing.T) {
	now := time.Now()

	job := &Job{Progress: JobProgress{DownloadedBytes: 9000}}
	job.resumeFloor = 9000
	apply := func(evt hfdownloader.ProgressEvent) { applyJobProgress(job, evt, now) }

	apply(hfdownloader.ProgressEvent{Event: "plan_item", Path: "a.bin", Total: 1000})
	apply(hfdownloader.ProgressEvent{Event: "plan_item", Path: "b.bin", Total: 5000})

	// b.bin has not reported yet, so the sum is still incomplete and the floor
	// keeps the bar from collapsing.
	apply(hfdownloader.ProgressEvent{Event: "file_done", Path: "a.bin"})
	if job.resumeFloor != 9000 || job.Progress.DownloadedBytes != 9000 {
		t.Fatalf("floor released too early: floor=%d downloaded=%d, want 9000/9000",
			job.resumeFloor, job.Progress.DownloadedBytes)
	}

	// Every file has now reported. The real total (6000) is below the preserved
	// estimate, and that is the truth: the floor must go.
	apply(hfdownloader.ProgressEvent{Event: "file_done", Path: "b.bin"})
	if job.resumeFloor != 0 {
		t.Fatalf("floor not released once progress is authoritative: got %d, want 0", job.resumeFloor)
	}
	if got := job.Progress.DownloadedBytes; got != 6000 {
		t.Fatalf("DownloadedBytes stuck at the stale floor: got %d, want 6000", got)
	}
}

// TestApplyJobProgress_DiscardedResumeBytesAreNotHidden is the same guarantee
// seen from the other side: a single planned file whose partial bytes were
// thrown away reports Downloaded=0, and the displayed total must follow the
// disk rather than the pre-pause estimate.
func TestApplyJobProgress_DiscardedResumeBytesAreNotHidden(t *testing.T) {
	now := time.Now()

	job := &Job{Progress: JobProgress{DownloadedBytes: 8000}}
	job.resumeFloor = 8000
	apply := func(evt hfdownloader.ProgressEvent) { applyJobProgress(job, evt, now) }

	apply(hfdownloader.ProgressEvent{Event: "plan_item", Path: "shard-00001.safetensors", Total: 8000})
	apply(hfdownloader.ProgressEvent{Event: "file_progress", Path: "shard-00001.safetensors", Downloaded: 0})

	if job.resumeFloor != 0 {
		t.Fatalf("floor not released after the file reported its real position: got %d", job.resumeFloor)
	}
	if got := job.Progress.DownloadedBytes; got != 0 {
		t.Fatalf("DownloadedBytes = %d, want 0 (the disk is empty again)", got)
	}
}

// TestApplyJobProgress_FinalizingStageIsVisible keeps the local finalize window
// on screen. A multi-GB shard is finished transferring long before the part
// files are stitched back together and hashed, so the per-file stage and its
// byte bounds are the only thing that tells the user the job is still working.
func TestApplyJobProgress_FinalizingStageIsVisible(t *testing.T) {
	now := time.Now()

	job := &Job{}
	apply := func(evt hfdownloader.ProgressEvent) { applyJobProgress(job, evt, now) }

	apply(hfdownloader.ProgressEvent{Event: "plan_item", Path: "model-00001-of-00002.safetensors", Total: 10000})

	apply(hfdownloader.ProgressEvent{
		Event: "file_finalizing", Path: "model-00001-of-00002.safetensors",
		Stage: "assembling", StageBytes: 4000, StageTotal: 10000,
	})
	f := job.Files[0]
	if f.Status != "finalizing" {
		t.Fatalf("file status = %q, want finalizing", f.Status)
	}
	if f.Stage != "assembling" || f.StageBytes != 4000 || f.StageTotal != 10000 {
		t.Fatalf("assembly stage not recorded: stage=%q bytes=%d total=%d", f.Stage, f.StageBytes, f.StageTotal)
	}

	// The verify stage carries no byte bounds, so the assembly counter must not
	// keep being displayed as if it still applied.
	apply(hfdownloader.ProgressEvent{
		Event: "file_finalizing", Path: "model-00001-of-00002.safetensors",
		Stage: "verifying",
	})
	f = job.Files[0]
	if f.Stage != "verifying" || f.StageBytes != 0 || f.StageTotal != 0 {
		t.Fatalf("verify stage not recorded: stage=%q bytes=%d total=%d", f.Stage, f.StageBytes, f.StageTotal)
	}
}
