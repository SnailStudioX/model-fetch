// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SnailStudioX/model-fetch/pkg/hfdownloader"
)

// useStreamBudget restores the process-wide stream gate after a test changes it.
// The gate is one global budget shared by every job in the process, so a leaked
// value would silently change how many streams unrelated tests in this package
// open against their local httptest hub.
func useStreamBudget(t *testing.T) {
	t.Helper()
	previous := hfdownloader.MaxStreamsPerHost()
	t.Cleanup(func() { hfdownloader.SetMaxStreamsPerHost(previous) })
}

// TestStreamBudgetForResolvesExplicitBudget pins the resolution order for the
// per-host stream budget: the explicit cap wins, otherwise the budget follows the
// connection count, and only a config with neither falls back to the package
// default. Keeping Concurrency as the fallback means an existing install that
// never set the new key keeps its current behaviour.
func TestStreamBudgetForResolvesExplicitBudget(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
		want int
	}{
		{"explicit budget wins over connections", Config{Concurrency: 8, MaxStreamsPerHost: 3}, 3},
		{"explicit budget may exceed connections", Config{Concurrency: 4, MaxStreamsPerHost: 12}, 12},
		{"unset budget follows connections", Config{Concurrency: 6}, 6},
		{"negative budget is ignored", Config{Concurrency: 6, MaxStreamsPerHost: -4}, 6},
		{"nothing set uses the package default", Config{}, hfdownloader.DefaultMaxStreamsPerHost},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := streamBudgetFor(tc.cfg); got != tc.want {
				t.Fatalf("streamBudgetFor(%+v) = %d, want %d", tc.cfg, got, tc.want)
			}
		})
	}
}

// TestSettingsStreamBudgetRoundTrip proves the new setting behaves like the other
// download knobs: it lands in the live server config, the job manager config, the
// process-wide gate (no restart needed), and on disk, and it survives a reload.
// It also pins the API contract: 0 means "follow the connection count" and is
// reported separately from the effective value, negative is rejected without
// touching the live gate.
func TestSettingsStreamBudgetRoundTrip(t *testing.T) {
	useStreamBudget(t)
	path := isolateTokenConfig(t)

	cfg := DefaultConfig()
	cfg.CacheDir = t.TempDir()
	cfg.Concurrency = 8
	srv := newTestServerWithConfig(t, cfg)

	requireTokenSettingsOK(t, srv, `{"connections":16,"maxStreamsPerHost":3}`)
	if got := srv.snapshotConfig().MaxStreamsPerHost; got != 3 {
		t.Fatalf("server config budget = %d, want 3", got)
	}
	if got := srv.jobs.snapshotConfig().MaxStreamsPerHost; got != 3 {
		t.Fatalf("job manager budget = %d, want 3", got)
	}
	if got := hfdownloader.MaxStreamsPerHost(); got != 3 {
		t.Fatalf("live gate budget = %d, want 3: a settings change must apply to running jobs without a restart", got)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "max-streams-per-host") {
		t.Fatalf("stored config does not carry the stream budget: %s", raw)
	}
	fileCfg, err := LoadConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	if fileCfg.MaxStreamsPerHost != 3 {
		t.Fatalf("stored budget = %d, want 3", fileCfg.MaxStreamsPerHost)
	}

	settings := settingsFor(t, srv)
	if settings.MaxStreamsPerHost != 3 || settings.EffectiveMaxStreamsPerHost != 3 {
		t.Fatalf("GET settings budget = %d (effective %d), want 3/3",
			settings.MaxStreamsPerHost, settings.EffectiveMaxStreamsPerHost)
	}

	// A restart reads the same file back through the CLI-style path.
	reloaded := DefaultConfig()
	if err := ApplyConfigToServer(&reloaded); err != nil {
		t.Fatal(err)
	}
	if reloaded.MaxStreamsPerHost != 3 {
		t.Fatalf("budget after reload = %d, want 3", reloaded.MaxStreamsPerHost)
	}

	// Resetting to 0 hands the budget back to the connection count, and the API
	// must keep the stored zero distinguishable from the effective number.
	requireTokenSettingsOK(t, srv, `{"connections":6,"maxStreamsPerHost":0}`)
	if got := hfdownloader.MaxStreamsPerHost(); got != 6 {
		t.Fatalf("gate budget after reset = %d, want 6: 0 must follow the connection count", got)
	}
	settings = settingsFor(t, srv)
	if settings.MaxStreamsPerHost != 0 || settings.EffectiveMaxStreamsPerHost != 6 {
		t.Fatalf("GET settings after reset = %d (effective %d), want 0/6",
			settings.MaxStreamsPerHost, settings.EffectiveMaxStreamsPerHost)
	}

	// A rejected value must not move anything, least of all the live gate.
	w := postTokenSettings(srv, `{"maxStreamsPerHost":-1}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("negative budget status = %d, want 400: %s", w.Code, w.Body.String())
	}
	if got := srv.snapshotConfig().MaxStreamsPerHost; got != 0 {
		t.Fatalf("rejected update stored budget = %d, want 0", got)
	}
	if got := hfdownloader.MaxStreamsPerHost(); got != 6 {
		t.Fatalf("rejected update moved the gate to %d, want 6", got)
	}
}

// TestJobRunDoesNotOverrideStreamBudget is the regression guard for the removed
// per-job override. Publishing the connection count as the gate budget at job
// start made the cap unobservable from Settings and let a newly started job widen
// the budget underneath jobs already running, which is exactly how one file ends
// up with more live streams than another.
func TestJobRunDoesNotOverrideStreamBudget(t *testing.T) {
	useStreamBudget(t)
	base := isolateCacheState(t)
	ep, _, _, _ := cacheDownloadFixture(t, "")

	mgr := NewJobManager(Config{
		CacheDir:          filepath.Join(base, "friendly"),
		MaxActive:         1,
		Concurrency:       8,
		MaxStreamsPerHost: 3,
		Verify:            "sha256",
		Endpoint:          ep.URL,
	}, nil)
	t.Cleanup(func() {
		for _, pending := range mgr.ListJobs() {
			mgr.CancelJob(pending.ID)
		}
		mgr.WaitAll(5 * time.Second)
	})

	if got := hfdownloader.MaxStreamsPerHost(); got != 3 {
		t.Fatalf("gate budget at manager start = %d, want 3", got)
	}

	job, _, err := mgr.CreateJob(DownloadRequest{Repo: "owner/model"})
	if err != nil {
		t.Fatal(err)
	}
	waitJobStatus(t, mgr, job.ID, JobStatusCompleted, 15*time.Second)

	if got := hfdownloader.MaxStreamsPerHost(); got != 3 {
		t.Fatalf("gate budget after the job ran = %d, want 3: Concurrency=8 must not widen the gate for jobs that were already running", got)
	}
	if got := streamBudgetFor(mgr.snapshotConfig()); got != 3 {
		t.Fatalf("resolved budget = %d, want 3", got)
	}
}
