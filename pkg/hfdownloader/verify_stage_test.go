// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// TestVerifyDigestProgress_ReportsHashBytes is the regression test for the
// "verifying" half of the finalize window: without a precomputed digest the
// whole payload has to be read again, which on a multi-GB shard is minutes of
// disk I/O that looked like a job frozen at 100%. The re-read must surface as
// byte progress on a file_finalizing event, and the free path (digest already
// known from part assembly) must stay free: no open, no events.
func TestVerifyDigestProgress_ReportsHashBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "payload.bin")
	content := make([]byte, 5000)
	for i := range content {
		content[i] = byte(i*7 + 1)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])

	var events []ProgressEvent
	sp := &stageProgress{
		path:    "payload.bin",
		stage:   "verifying",
		message: "verifying",
		total:   int64(len(content)),
		emit:    func(ev ProgressEvent) { events = append(events, ev) },
	}
	if err := verifyDigestProgress(context.Background(), path, digest, "", sp); err != nil {
		t.Fatalf("fallback hash failed: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("fallback hash emitted no stage progress")
	}
	for _, ev := range events {
		if ev.Event != "file_finalizing" || ev.Stage != "verifying" {
			t.Fatalf("unexpected stage event: %+v", ev)
		}
		if ev.StageTotal != int64(len(content)) {
			t.Fatalf("stage total = %d, want %d", ev.StageTotal, len(content))
		}
	}
	if last := events[len(events)-1]; last.StageBytes != int64(len(content)) {
		t.Fatalf("last stage bytes = %d, want %d", last.StageBytes, len(content))
	}

	// Planned size unknown: the bytes already on disk bound the hash, otherwise
	// the card would show a stage with no percentage at all.
	var statEvents []ProgressEvent
	sp2 := &stageProgress{
		path:    "payload.bin",
		stage:   "verifying",
		message: "verifying",
		emit:    func(ev ProgressEvent) { statEvents = append(statEvents, ev) },
	}
	if err := verifyDigestProgress(context.Background(), path, digest, "", sp2); err != nil {
		t.Fatalf("fallback hash with unknown total failed: %v", err)
	}
	if len(statEvents) == 0 || statEvents[len(statEvents)-1].StageTotal != int64(len(content)) {
		t.Fatalf("stage total not taken from the file on disk: %+v", statEvents)
	}

	// Precomputed digest: the path deliberately does not exist, so any attempt
	// to open it (or any emitted event) would mean the second full read is back.
	events = nil
	missing := filepath.Join(dir, "absent.bin")
	if err := verifyDigestProgress(context.Background(), missing, digest, digest, sp); err != nil {
		t.Fatalf("matching precomputed digest rejected: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("precomputed digest emitted %d events, want 0", len(events))
	}
	if err := verifyDigestProgress(context.Background(), missing, digest, hex.EncodeToString([]byte("not-a-digest")), sp); err == nil {
		t.Error("mismatching precomputed digest accepted")
	}
}
