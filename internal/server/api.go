// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/bashrusakh/modelfetch/internal/hubsource"
	"github.com/bashrusakh/modelfetch/pkg/hfdownloader"
	"github.com/bashrusakh/modelfetch/pkg/smartdl"
)

// --- Handlers ---

// Version is the application version reported by the API and shown in the web
// UI. main() sets it from the build-time version, so bumping VERSION and
// rebuilding updates the UI automatically — no hardcoded numbers to edit.
var Version = "dev"

// handleHealth returns server health status.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"version": Version,
		"time":    time.Now().UTC().Format(time.RFC3339),
	})
}

// handleStartDownload starts a new download job.
func (s *Server) handleStartDownload(w http.ResponseWriter, r *http.Request) {
	var req DownloadRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body", err.Error())
		return
	}

	// Validate
	if req.Repo == "" {
		writeError(w, http.StatusBadRequest, "Missing required field: repo", "")
		return
	}

	// Parse filters from repo:filter syntax
	if strings.Contains(req.Repo, ":") && len(req.Filters) == 0 {
		parts := strings.SplitN(req.Repo, ":", 2)
		req.Repo = parts[0]
		if parts[1] != "" {
			for _, f := range strings.Split(parts[1], ",") {
				f = strings.TrimSpace(f)
				if f != "" {
					req.Filters = append(req.Filters, f)
				}
			}
		}
	}

	if !hfdownloader.IsValidModelName(req.Repo) {
		writeError(w, http.StatusBadRequest, "Invalid repo format", "Expected owner/name")
		return
	}

	// If dry-run, return the plan
	if req.DryRun {
		s.handlePlanInternal(w, req)
		return
	}

	// Create and start the job (or return existing if duplicate)
	job, wasExisting, err := s.jobs.CreateJob(req)
	if err != nil {
		if errors.Is(err, errInvalidDestination) {
			writeError(w, http.StatusBadRequest, "Invalid destination", err.Error())
			return
		}
		if errors.Is(err, errInvalidRouteKey) {
			writeError(w, http.StatusBadRequest, "Invalid routeKey", "routeKey must be one of the configured route keys")
			return
		}
		if errors.Is(err, errSelectionWriterBusy) {
			writeError(w, http.StatusConflict, "Selected GGUF is being deleted", "Retry the download after the selected-file operation finishes")
			return
		}
		writeError(w, http.StatusInternalServerError, "Failed to create job", err.Error())
		return
	}

	// Return appropriate status
	if wasExisting {
		// Job already exists for this repo - return it with 200
		writeJSON(w, http.StatusOK, map[string]any{
			"job":     job,
			"message": "Download already in progress",
		})
	} else {
		// New job created
		writeJSON(w, http.StatusAccepted, job)
	}
}

// handlePlan returns a download plan without starting the download.
func (s *Server) handlePlan(w http.ResponseWriter, r *http.Request) {
	var req DownloadRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body", err.Error())
		return
	}

	req.DryRun = true
	s.handlePlanInternal(w, req)
}

func (s *Server) handlePlanInternal(w http.ResponseWriter, req DownloadRequest) {
	if req.Repo == "" {
		writeError(w, http.StatusBadRequest, "Missing required field: repo", "")
		return
	}
	if _, err := validatedRouteKey(req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid routeKey", "routeKey must be one of the configured route keys")
		return
	}

	// Parse filters from repo:filter syntax
	if strings.Contains(req.Repo, ":") && len(req.Filters) == 0 {
		parts := strings.SplitN(req.Repo, ":", 2)
		req.Repo = parts[0]
		if parts[1] != "" {
			for _, f := range strings.Split(parts[1], ",") {
				f = strings.TrimSpace(f)
				if f != "" {
					req.Filters = append(req.Filters, f)
				}
			}
		}
	}

	// Take a snapshot of the server config; every read below uses it
	// instead of touching s.config directly to avoid racing a concurrent
	// settings update.
	cfg := s.snapshotConfig()

	if msg := sourceEndpointMismatch(cfg); msg != "" {
		writeError(w, http.StatusBadRequest, "Endpoint does not match source", msg)
		return
	}

	revision := req.Revision
	if revision == "" {
		revision = hubsource.Resolve(cfg.Source, cfg.Endpoint).DefaultRevision()
	}

	// Create job for scanning
	dlJob := hfdownloader.Job{
		Repo:               req.Repo,
		Revision:           revision,
		IsDataset:          req.Dataset,
		Filters:            req.Filters,
		Excludes:           req.Excludes,
		ExactMatch:         req.ExactMatch,
		AppendFilterSubdir: req.AppendFilterSubdir,
	}

	// Use server-configured output directory (not from request for security)
	outputDir := cfg.ModelsDir
	if req.Dataset {
		outputDir = cfg.DatasetsDir
	}

	settings := hfdownloader.Settings{
		OutputDir: outputDir,
		Token:     cfg.Token,
		Endpoint:  cfg.Endpoint,
		Source:    cfg.Source,
		Proxy:     cfg.Proxy,
	}

	// Collect plan items
	var files []PlanFile
	var totalSize int64
	// The scan reports the revision the files were actually read from, which is
	// not the requested one when the stored revision only exists on a different
	// hub source (see hfdownloader.scanRepoResolved).
	scannedRevision := ""

	progressFunc := func(evt hfdownloader.ProgressEvent) {
		if evt.Event == "plan_item" {
			if scannedRevision == "" {
				scannedRevision = evt.Revision
			}
			files = append(files, PlanFile{
				Path: evt.Path,
				Size: evt.Total,
				LFS:  evt.IsLFS,
			})
			totalSize += evt.Total
		}
	}

	// Run in dry-run mode (plan only)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// We need to get the plan - use a modified Run that returns early
	// For now, we'll scan the repo manually
	err := hfdownloader.ScanPlan(ctx, dlJob, settings, progressFunc)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to scan repository", err.Error())
		return
	}

	if scannedRevision != "" {
		revision = scannedRevision
	}

	resp := PlanResponse{
		Repo:       req.Repo,
		Revision:   revision,
		Files:      files,
		TotalSize:  totalSize,
		TotalFiles: len(files),
	}

	writeJSON(w, http.StatusOK, resp)
}

// handleListJobs returns all jobs.
func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	jobs := s.jobs.ListJobs()
	writeJSON(w, http.StatusOK, map[string]any{
		"jobs":  jobs,
		"count": len(jobs),
	})
}

// handleGetJob returns a specific job.
func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "Missing job ID", "")
		return
	}

	job, ok := s.jobs.GetJob(id)
	if !ok {
		writeError(w, http.StatusNotFound, "Job not found", "")
		return
	}

	writeJSON(w, http.StatusOK, job)
}

// handleCancelJob cancels a job.
func (s *Server) handleCancelJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "Missing job ID", "")
		return
	}

	if s.jobs.CancelJob(id) {
		writeJSON(w, http.StatusOK, SuccessResponse{
			Success: true,
			Message: "Job cancelled",
		})
	} else {
		writeError(w, http.StatusNotFound, "Job not found or already completed", "")
	}
}

// handlePauseJob pauses a running job.
func (s *Server) handlePauseJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "Missing job ID", "")
		return
	}

	if s.jobs.PauseJob(id) {
		writeJSON(w, http.StatusOK, SuccessResponse{
			Success: true,
			Message: "Job paused",
		})
	} else {
		writeError(w, http.StatusNotFound, "Job not found or not running", "")
	}
}

// handleResumeJob resumes a paused job.
func (s *Server) handleResumeJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "Missing job ID", "")
		return
	}

	if s.jobs.ResumeJob(id) {
		writeJSON(w, http.StatusOK, SuccessResponse{
			Success: true,
			Message: "Job resumed",
		})
	} else {
		writeError(w, http.StatusNotFound, "Job not found or not paused", "")
	}
}

// handleRetryJob restarts a failed or cancelled job using its original
// parameters, reusing the same job ID.
func (s *Server) handleRetryJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "Missing job ID", "")
		return
	}

	if s.jobs.RetryJob(id) {
		writeJSON(w, http.StatusOK, SuccessResponse{
			Success: true,
			Message: "Job restarted",
		})
	} else {
		writeError(w, http.StatusNotFound, "Job not found or not retryable", "")
	}
}

// handleDismissJob permanently removes a finished job from the list so it
// doesn't reappear on page refresh (github issue #68 secondary ask). Only
// jobs in terminal states (completed, failed, cancelled, paused) can be
// dismissed — active downloads must be cancelled first.
func (s *Server) handleDismissJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "Missing job ID", "")
		return
	}

	switch res, _ := s.jobs.DismissJobResult(id); res {
	case DismissJobOK:
		writeJSON(w, http.StatusOK, SuccessResponse{
			Success: true,
			Message: "Job dismissed",
		})
	case DismissJobStillActive:
		writeError(w, http.StatusConflict, "Cannot dismiss an active job; cancel it first", "")
	default:
		writeError(w, http.StatusNotFound, "Job not found", "")
	}
}

// handleGetSettings returns current settings.
func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	cfg := s.snapshotConfig()
	src := hubsource.Resolve(cfg.Source, cfg.Endpoint)

	// Don't expose full token, just indicate if set
	tokenStatus := redactToken(cfg.Token)

	cacheDir := cfg.cacheRoot()

	storageMode := "cache"
	if cfg.LocalDir != "" {
		storageMode = "local"
	}

	resp := SettingsResponse{
		Token:                      tokenStatus,
		CacheDir:                   cacheDir,
		ConfiguredCacheDir:         cfg.CacheDir,
		EffectiveHubDir:            cfg.cache().HubDir(),
		HubDirSource:               cfg.hubDirSource(),
		Concurrency:                cfg.Concurrency,
		MaxActive:                  cfg.MaxActive,
		MultipartThreshold:         cfg.MultipartThreshold,
		MaxSpeed:                   cfg.MaxSpeed,
		MaxStreamsPerHost:          cfg.MaxStreamsPerHost,
		EffectiveMaxStreamsPerHost: streamBudgetFor(cfg),
		Verify:                     cfg.Verify,
		Retries:                    cfg.Retries,
		StallTimeout:               cfg.StallTimeout,
		Endpoint:                   cfg.Endpoint,
		Source:                     cfg.Source,
		SourceDefaults:             hubsource.Defaults(),
		SourceHosts:                hubsource.Hosts(),
		SourcePageTemplates:        sourcePageTemplates(src),
		// The saved hub list travels with masked credentials only. The live Token
		// above is the active entry's credential, so the Settings tabs and the
		// Models selector can render without ever seeing a real token.
		HubSources:      redactedHubSources(cfg.HubSources),
		ActiveHubSource: cfg.ActiveHub,
		StorageMode:     storageMode,
		LocalDir:        cfg.LocalDir,
		LocalScanDirs:   cfg.LocalScanDirs,
		DownloadRoutes:  cfg.DownloadRoutes,
		Hardware:        cfg.Hardware,
		ConfigFile:      ConfigPath(),
		TargetsFile:     hfdownloader.DefaultTargetsPath(),
	}

	// Add proxy settings (without password for security)
	if cfg.Proxy != nil && cfg.Proxy.URL != "" {
		resp.Proxy = &ProxySettingsResponse{
			URL:                cfg.Proxy.URL,
			Username:           cfg.Proxy.Username,
			NoProxy:            cfg.Proxy.NoProxy,
			NoEnvProxy:         cfg.Proxy.NoEnvProxy,
			InsecureSkipVerify: cfg.Proxy.InsecureSkipVerify,
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

// handleUpdateSettings updates settings and persists them to config file.
// Note: Output directories cannot be changed via API for security.
func (s *Server) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token         *string  `json:"token,omitempty"`
		CacheDir      *string  `json:"cacheDir,omitempty"`
		LocalDir      *string  `json:"localDir,omitempty"`
		LocalScanDirs []string `json:"localScanDirs,omitempty"`
		// DownloadRoutes, when present, replaces the route map. Unknown keys
		// outside the closed set (routes.go) are dropped rather than rejected;
		// valid values are paths (trimmed/Cleaned) and an empty/missing map
		// clears all routes.
		DownloadRoutes *map[string]string `json:"downloadRoutes,omitempty"`
		// Hardware replaces the machine-profile override. Values outside the
		// sane range are dropped rather than rejected, matching the route
		// behavior above; an empty object clears the override.
		Hardware           *HardwareConfig `json:"hardware,omitempty"`
		Concurrency        *int            `json:"connections,omitempty"`
		MaxActive          *int            `json:"maxActive,omitempty"`
		MultipartThreshold *string         `json:"multipartThreshold,omitempty"`
		MaxSpeed           *string         `json:"maxSpeed,omitempty"`
		// MaxStreamsPerHost caps the total streams this process reads from one hub
		// host at once, across every job and file. Omit to keep the current value,
		// 0 to follow the connection count; negative is rejected.
		MaxStreamsPerHost *int    `json:"maxStreamsPerHost,omitempty"`
		Verify            *string `json:"verify,omitempty"`
		Retries           *int    `json:"retries,omitempty"`
		StallTimeout      *string `json:"stallTimeout,omitempty"`
		Endpoint          *string `json:"endpoint,omitempty"`
		Source            *string `json:"source,omitempty"`
		// HubSources, when present, replaces the saved hub access source list
		// wholesale. An entry's token is a pointer so the three cases stay
		// distinguishable: omitted/null or a display mask preserves the stored
		// credential, a value sets it, and "" clears it.
		HubSources *[]hubSourceRequestEntry `json:"hubSources,omitempty"`
		// ActiveHubSource selects which saved hub source the server uses. Omitting
		// it keeps the current one; an unknown id is rejected.
		ActiveHubSource *string `json:"activeHubSource,omitempty"`
		// Proxy settings
		Proxy *struct {
			URL                *string `json:"url,omitempty"`
			Username           *string `json:"username,omitempty"`
			Password           *string `json:"password,omitempty"`
			NoProxy            *string `json:"noProxy,omitempty"`
			NoEnvProxy         *bool   `json:"noEnvProxy,omitempty"`
			InsecureSkipVerify *bool   `json:"insecureSkipVerify,omitempty"`
		} `json:"proxy,omitempty"`
		// Note: ModelsDir and DatasetsDir are NOT updatable via API for security
	}

	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body", err.Error())
		return
	}

	// Phase 1: validate every field that can fail with 400. No s.config
	// mutations happen here, so a 400 leaves in-memory state consistent with
	// the persisted file.
	var (
		newMultipartThreshold string
		newMaxSpeed           string
		newStallTimeout       string
		newDownloadRoutes     map[string]string
	)
	if req.MultipartThreshold != nil && *req.MultipartThreshold != "" {
		trimmed := strings.TrimSpace(*req.MultipartThreshold)
		if _, err := hfdownloader.ParseSizeStrict(trimmed); err != nil {
			writeError(w, http.StatusBadRequest, "Invalid multipartThreshold", err.Error())
			return
		}
		newMultipartThreshold = trimmed
	}
	// StallTimeout accepts a duration string; an explicit "0"/"0s" disables
	// the watchdog. Empty is skipped (preserve), matching multipartThreshold.
	if req.StallTimeout != nil && strings.TrimSpace(*req.StallTimeout) != "" {
		trimmed := strings.TrimSpace(*req.StallTimeout)
		d, err := time.ParseDuration(trimmed)
		if err != nil || d < 0 {
			writeError(w, http.StatusBadRequest, "Invalid stallTimeout", "expected a non-negative duration such as \"60s\", \"2m\", or \"0\" to disable")
			return
		}
		newStallTimeout = trimmed
	}
	if req.MaxSpeed != nil {
		trimmed := strings.TrimSpace(*req.MaxSpeed)
		if _, err := hfdownloader.ParseSizeStrict(trimmed); err != nil {
			writeError(w, http.StatusBadRequest, "Invalid maxSpeed", err.Error())
			return
		}
		newMaxSpeed = trimmed
	}
	if req.MaxStreamsPerHost != nil && *req.MaxStreamsPerHost < 0 {
		writeError(w, http.StatusBadRequest, "Invalid maxStreamsPerHost", "expected 0 to follow the connection count, or a positive per-host stream budget")
		return
	}
	if req.DownloadRoutes != nil {
		// Sanitize rather than reject: the UI echoes the loaded map on save, so
		// a legacy/hand-edited key (e.g. a reserved key the server no longer
		// knows) must not fail the whole settings save. Unknown keys are
		// dropped and valid keys are kept; an unknown key can never be stored
		// and its value is never interpreted as a path. An empty result clears
		// the map (nil), matching "empty value clears a key".
		newDownloadRoutes = sanitizeDownloadRoutes(*req.DownloadRoutes)
	}

	// Source and endpoint are validated together here, before any mutation. A 400
	// written inside the withConfig closure cannot abort this handler, so a value
	// rejected there would still be stored. The endpoint is a base URL that fits
	// exactly one source's API shape, so a pair that cannot both be true is
	// rejected instead of persisted.
	cur := s.snapshotConfig()
	newEndpoint := cur.Endpoint
	if req.Endpoint != nil {
		newEndpoint = *req.Endpoint
	}
	newSource := cur.Source
	if req.Source != nil {
		id := strings.ToLower(strings.TrimSpace(*req.Source))
		if !hubsource.Known(id) {
			writeError(w, http.StatusBadRequest, "Invalid source", fmt.Sprintf("unknown source %q; known sources: %s", id, strings.Join(hubsource.Registered(), ", ")))
			return
		}
		// Store the canonical id rather than the alias the caller sent: this value
		// is fed straight back into the source pickers, and an alias ("ms",
		// "huggingface") would match no option they offer.
		newSource = hubsource.Resolve(id, newEndpoint).ID()
	}
	// The hub source list is validated by the same rule as the standalone pair: an
	// entry pairing a source with another source's endpoint would answer every
	// later call with an HTML error page, so the whole list is rejected before
	// anything is stored.
	hubListSent := req.HubSources != nil
	var newHubSources []HubSource
	if hubListSent {
		list, err := resolveRequestHubSources(*req.HubSources, cur.HubSources)
		if err != nil {
			writeError(w, http.StatusBadRequest, "Invalid hubSources", err.Error())
			return
		}
		newHubSources = list
	}
	activeList := newHubSources
	if len(activeList) == 0 {
		activeList = cur.HubSources
	}
	// newActiveHub is the entry the live triple will describe once this request
	// lands. An omitted activeHubSource keeps the current one; a submitted id must
	// exist in the list this request leaves behind.
	newActiveHub := cur.ActiveHub
	if req.ActiveHubSource != nil {
		newActiveHub = strings.TrimSpace(*req.ActiveHubSource)
		if findHubSource(activeList, newActiveHub) == nil {
			writeError(w, http.StatusBadRequest, "Invalid activeHubSource", fmt.Sprintf("unknown hub source id %q; known ids: %s", newActiveHub, strings.Join(hubSourceIDs(activeList), ", ")))
			return
		}
	}
	// prevActive* describe the entry that owns the live triple right now. Phase 2
	// compares them with the post-request entry to decide whether a hub switch has
	// to move the credential along with it.
	prevActiveID := resolveActiveHubSource(cur.HubSources, cur.ActiveHub, cur.Source, cur.Endpoint)
	prevActiveToken := ""
	if prev := findHubSource(cur.HubSources, prevActiveID); prev != nil {
		prevActiveToken = prev.Token
	}
	activeEntry := findHubSource(activeList, newActiveHub)
	if hubListSent && activeEntry == nil {
		// The list was replaced and the old active id went with it (the tab was
		// deleted, or a client omitted ids). Fall back to the entry describing the
		// live pair so saving the list cannot silently retarget downloads to a
		// different host.
		newActiveHub = resolveActiveHubSource(activeList, "", cur.Source, cur.Endpoint)
		activeEntry = findHubSource(activeList, newActiveHub)
	}
	if hubListSent && activeEntry != nil {
		// The submitted list is authoritative for the live triple: the active
		// entry's source and endpoint become what downloads use, which is what
		// keeps a saved tab and the running configuration from drifting apart.
		newSource = activeEntry.Source
		newEndpoint = activeEntry.Endpoint
	}
	if msg := sourceEndpointMismatch(Config{Source: newSource, Endpoint: newEndpoint}); msg != "" {
		writeError(w, http.StatusBadRequest, "Endpoint does not match source", msg)
		return
	}
	// Verify is compared by exact match inside the downloader, so an unknown
	// mode is rejected here rather than stored and silently switching off every
	// post-download check.
	newVerify := cur.Verify
	if req.Verify != nil {
		mode, ok := normalizeVerifyMode(*req.Verify)
		if !ok {
			writeError(w, http.StatusBadRequest, "Invalid verify", fmt.Sprintf("unknown verification mode %q; expected none, size or sha256", *req.Verify))
			return
		}
		newVerify = mode
	}

	// Phase 2: build the new config on a local copy under withConfig (which
	// takes the write lock for the whole mutation, so a concurrent read sees
	// either the pre- or post-state, never a torn mix). The lock is released
	// before we touch the job manager or the file, so the critical section
	// stays short. myGen is the generation we just produced; the side
	// effects below drop themselves if a newer commit landed in between.
	_, myGen := s.withConfig(func(c *Config) {
		if req.Token != nil && !isRedactedToken(*req.Token) {
			token := *req.Token
			c.Token = token
			c.tokenWrite = &token
			// A genuinely new explicit set/clear authors a fresh intent
			// identity so an older persister cannot clear it.
			c.tokenWriteGen++
		}
		if req.CacheDir != nil {
			c.CacheDir = strings.TrimSpace(*req.CacheDir)
		}
		if req.LocalDir != nil {
			c.LocalDir = strings.TrimSpace(*req.LocalDir)
		}
		if req.LocalScanDirs != nil {
			c.LocalScanDirs = cleanPathList(req.LocalScanDirs)
		}
		// DownloadRoutes is replaced wholesale (copy-on-write) so the current
		// map is never mutated in place while in-flight jobs read it.
		if req.DownloadRoutes != nil {
			c.DownloadRoutes = newDownloadRoutes
		}
		if req.Hardware != nil {
			c.Hardware = sanitizeHardwareConfig(req.Hardware)
		}
		if req.Concurrency != nil && *req.Concurrency > 0 {
			c.Concurrency = *req.Concurrency
		}
		if req.MaxActive != nil && *req.MaxActive > 0 {
			c.MaxActive = *req.MaxActive
		}
		if newMultipartThreshold != "" {
			c.MultipartThreshold = newMultipartThreshold
		}
		if req.MaxSpeed != nil {
			c.MaxSpeed = newMaxSpeed
		}
		// Negative values were rejected in phase 1; 0 is a real choice meaning
		// "follow the connection count", so it is applied rather than skipped.
		if req.MaxStreamsPerHost != nil && *req.MaxStreamsPerHost >= 0 {
			c.MaxStreamsPerHost = *req.MaxStreamsPerHost
		}
		if req.Verify != nil && newVerify != "" {
			c.Verify = newVerify
		}
		if req.Retries != nil && *req.Retries >= 0 {
			c.Retries = *req.Retries
		}
		if newStallTimeout != "" {
			c.StallTimeout = newStallTimeout
		}
		if req.Endpoint != nil {
			c.Endpoint = *req.Endpoint
		}
		if req.Source != nil {
			// Validated in Phase 1; a rejection there cannot abort this closure.
			c.Source = newSource
		}

		// Hub sources: the slice is replaced wholesale (copy-on-write) because its
		// backing memory is shared with the job manager's config copy and any
		// in-flight job runner reading it.
		if hubListSent {
			c.HubSources = newHubSources
		}
		if req.ActiveHubSource != nil {
			c.ActiveHub = newActiveHub
		}
		if req.Source != nil || req.Endpoint != nil {
			// A standalone source/endpoint change either switches to the tab that
			// already describes the new pair or edits the tab it applies to, so the
			// live triple and the saved tab stay the same thing.
			if entry := findHubSourceByPair(c.HubSources, newSource, newEndpoint); entry != nil {
				c.ActiveHub = entry.ID
			} else {
				c.HubSources = setHubSourcePair(c.HubSources, c.ActiveHub, newSource, newEndpoint)
			}
		}
		tokenAuthored := req.Token != nil && !isRedactedToken(*req.Token)
		if hubListSent || req.ActiveHubSource != nil {
			// The list is authoritative: the live triple always describes the active
			// entry, so every request path keeps reading the three fields it always
			// did and no download code has to know the list exists.
			if entry := findHubSource(c.HubSources, c.ActiveHub); entry != nil {
				c.Source = entry.Source
				c.Endpoint = entry.Endpoint
			}
		}
		if tokenAuthored {
			// An authored credential belongs to the active entry, not only to the
			// live field, or the next hub switch would drop it again.
			c.HubSources = setHubSourceToken(c.HubSources, c.ActiveHub, c.Token)
		}
		c.ensureHubSources()
		// Switching hubs moves the credential with the hub. Without this the token
		// saved for a Hugging Face mirror would keep being sent to ModelScope after
		// a switch, and a hub that stores its own token would be used without it.
		// Only a real change of the active entry authors an intent: an ordinary save
		// must not, or it would overwrite a credential set through another path.
		hubChanged := req.Source != nil || req.Endpoint != nil || req.ActiveHubSource != nil || hubListSent
		if hubChanged && !tokenAuthored {
			if entry := findHubSource(c.HubSources, c.ActiveHub); entry != nil &&
				(c.ActiveHub != prevActiveID || entry.Token != prevActiveToken) &&
				hubCredentialKnown(cur.HubSources, c.HubSources, prevActiveToken, entry.Token) {
				token := entry.Token
				c.Token = token
				c.tokenWrite = &token
				c.tokenWriteGen++
			}
		}

		// Update proxy settings. Build a fresh ProxyConfig (copy-on-write)
		// rather than mutating the existing struct in place: the current
		// *ProxyConfig pointer is shared with the JobManager's config and
		// any in-flight job runner reading it, so an in-place write would
		// race with them. Replacing the pointer leaves the old struct
		// immutable for existing readers.
		if req.Proxy != nil {
			var newProxy hfdownloader.ProxyConfig
			if c.Proxy != nil {
				newProxy = *c.Proxy
			}
			if req.Proxy.URL != nil {
				newProxy.URL = *req.Proxy.URL
			}
			if req.Proxy.Username != nil {
				newProxy.Username = *req.Proxy.Username
			}
			if req.Proxy.Password != nil {
				newProxy.Password = *req.Proxy.Password
			}
			if req.Proxy.NoProxy != nil {
				newProxy.NoProxy = *req.Proxy.NoProxy
			}
			if req.Proxy.NoEnvProxy != nil {
				newProxy.NoEnvProxy = *req.Proxy.NoEnvProxy
			}
			if req.Proxy.InsecureSkipVerify != nil {
				newProxy.InsecureSkipVerify = *req.Proxy.InsecureSkipVerify
			}
			// Clear proxy if URL is empty, otherwise swap in the new struct.
			if newProxy.URL == "" {
				c.Proxy = nil
			} else {
				c.Proxy = &newProxy
			}
		}
	})

	// Side effects (job-manager update + file persistence) are serialized
	// under persistMu, and the generation is re-checked *after* acquiring it.
	// That ordering guarantees the snapshot sees every earlier commit, so
	// only the writer whose generation is still current persists; an older
	// writer drops its side effects instead of rolling state back.
	s.persistMu.Lock()
	defer s.persistMu.Unlock()

	finalCfg, currentGen := s.snapshotConfigWithGen()
	if currentGen != myGen {
		writeJSON(w, http.StatusOK, SuccessResponse{
			Success: true,
			Message: "Settings updated (a newer update was applied concurrently; this request's job-manager and file persistence were skipped to preserve the latest state)",
		})
		return
	}

	// Also update job manager config. UpdateConfig takes the manager lock and
	// re-runs the scheduler, so a raised max-active starts queued jobs now.
	s.jobs.UpdateConfig(finalCfg)

	// Persist settings to config file
	retries := finalCfg.Retries
	fileCfg := &ConfigFile{
		CacheDir:           finalCfg.CacheDir,
		LocalDir:           finalCfg.LocalDir,
		LocalScanDirs:      finalCfg.LocalScanDirs,
		Connections:        finalCfg.Concurrency,
		MaxActive:          finalCfg.MaxActive,
		MultipartThreshold: finalCfg.MultipartThreshold,
		MaxSpeed:           finalCfg.MaxSpeed,
		MaxStreamsPerHost:  finalCfg.MaxStreamsPerHost,
		Verify:             finalCfg.Verify,
		Retries:            &retries,
		StallTimeout:       finalCfg.StallTimeout,
		Endpoint:           finalCfg.Endpoint,
		Source:             finalCfg.Source,
		HubSources:         finalCfg.HubSources,
		ActiveHub:          finalCfg.ActiveHub,
		DownloadRoutes:     finalCfg.DownloadRoutes,
	}
	// Add proxy to config file if set
	if finalCfg.Proxy != nil {
		fileCfg.Proxy = &ProxyConfig{
			URL:                finalCfg.Proxy.URL,
			Username:           finalCfg.Proxy.Username,
			Password:           finalCfg.Proxy.Password,
			NoProxy:            finalCfg.Proxy.NoProxy,
			NoEnvProxy:         finalCfg.Proxy.NoEnvProxy,
			InsecureSkipVerify: finalCfg.Proxy.InsecureSkipVerify,
		}
	}
	if err := saveSettingsConfig(fileCfg, finalCfg.tokenWrite); err != nil {
		// Log error but don't fail the request - settings are still applied in-memory
		writeJSON(w, http.StatusOK, SuccessResponse{
			Success: true,
			Message: "Settings updated (warning: could not persist to config file)",
		})
		return
	}

	// The pending explicit token intent is now durable for the exact intent
	// this snapshot carried, so consume it by identity. If a newer explicit
	// set/clear committed concurrently, keep it: that intent has its own
	// tokenWriteGen and is not the one just persisted.
	s.consumeTokenWrite(finalCfg.tokenWriteGen)

	writeJSON(w, http.StatusOK, SuccessResponse{
		Success: true,
		Message: "Settings saved",
	})
}

// sourceEndpointMismatch reports a config contradiction: the selected source and
// an endpoint that belongs to a different source. An endpoint is only a base URL,
// so it fits exactly one source's API shape; a stale endpoint left over from
// switching sources makes every API call return an HTML error page instead of
// data. Hosts no registered source claims (mirrors, private deployments) are not
// flagged, because those are legitimate endpoints.
func sourceEndpointMismatch(cfg Config) string {
	src := hubsource.Resolve(cfg.Source, cfg.Endpoint)
	owner, ok := hubsource.SourceForHost(src.Base())
	if !ok || owner == src.ID() {
		return ""
	}
	return fmt.Sprintf("source %q is set but endpoint %s is the %s API host; use %s for %s, or switch the source back to %s",
		src.ID(), src.Base(), owner, hubsource.DefaultEndpointFor(cfg.Source), src.ID(), owner)
}

// sourcePageTemplates returns the repository page URL for each repo kind with
// "{repo}" left as a placeholder. The browser builds its hub links from this so a
// non-Hugging-Face source links to its own site instead of huggingface.co.
func sourcePageTemplates(src hubsource.Source) map[string]string {
	return map[string]string{
		"model":   src.RepoPageURL("{repo}", false),
		"dataset": src.RepoPageURL("{repo}", true),
	}
}

// --- Smart Analyzer ---

// handleAnalyze analyzes a HuggingFace repository.
func (s *Server) handleAnalyze(w http.ResponseWriter, r *http.Request) {
	// Get repo from path (supports owner/name format)
	repo := r.PathValue("repo")
	if repo == "" {
		writeError(w, http.StatusBadRequest, "Missing repository", "Format: /api/analyze/owner/name")
		return
	}
	if !hfdownloader.IsValidModelName(repo) {
		writeError(w, http.StatusBadRequest, "Invalid repo format", "Expected owner/name")
		return
	}

	// Check if it's a dataset (explicit selection)
	isDataset := r.URL.Query().Get("dataset") == "true"

	// Get revision (defaults to the source's default branch: "main" for
	// Hugging Face, "master" for ModelScope)
	cfg := s.snapshotConfig()
	src := hubsource.Resolve(cfg.Source, cfg.Endpoint)
	if msg := sourceEndpointMismatch(cfg); msg != "" {
		writeError(w, http.StatusBadRequest, "Endpoint does not match source", msg)
		return
	}
	revision := strings.TrimSpace(r.URL.Query().Get("revision"))
	if revision == "" {
		revision = src.DefaultRevision()
	}

	// Create analyzer
	client, err := s.hubClient(cfg)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Analysis failed", err.Error())
		return
	}
	opts := smartdl.AnalyzerOptions{
		Token:      cfg.Token,
		Endpoint:   cfg.Endpoint,
		Source:     cfg.Source,
		HTTPClient: client,
	}
	analyzer := smartdl.NewAnalyzer(opts)

	// Analyze with timeout
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	info, err := analyzer.AnalyzeWithRevision(ctx, repo, isDataset, revision)
	if err != nil {
		// Check if both model and dataset exist
		if err == smartdl.ErrBothExist {
			writeJSON(w, http.StatusOK, map[string]any{
				"needsSelection": true,
				"repo":           repo,
				"message":        "This repository exists as both a model and a dataset. Please select which one you want to analyze.",
				"options":        []string{"model", "dataset"},
			})
			return
		}
		writeError(w, http.StatusInternalServerError, "Analysis failed", err.Error())
		return
	}

	// Hardware-aware recommendation. Only annotates when a machine profile is
	// known; otherwise the existing quality-based recommendation stands
	// untouched, so clients without hardware info keep the old behavior.
	if hw, _, _, _ := s.effectiveHardware(ctx); hw.VRAMGB > 0 || hw.SystemRAMGB > 0 {
		shape := smartdl.ModelShapeFromInfo(info)
		smartdl.RecommendForHardware(info.SelectableItems, hw, shape)
		info.Hardware = &hw
		// The recommended flag may have moved, so the ready-to-send download
		// body is rebuilt from the new flags.
		info.PopulateDownloadSelection()
	}

	writeJSON(w, http.StatusOK, info)
}

// handleReadme returns a minimal README payload for the analysis panel.
func (s *Server) handleReadme(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("repo")
	if repo == "" {
		writeError(w, http.StatusBadRequest, "Missing repository", "Format: /api/readme/owner/name")
		return
	}
	if !hfdownloader.IsValidModelName(repo) {
		writeError(w, http.StatusBadRequest, "Invalid repo format", "Expected owner/name")
		return
	}
	revision := r.URL.Query().Get("revision")
	isDataset := strings.EqualFold(r.URL.Query().Get("dataset"), "true")
	cfg := s.snapshotConfig()
	src := hubsource.Resolve(cfg.Source, cfg.Endpoint)
	revision = strings.TrimSpace(revision)
	if revision == "" {
		revision = src.DefaultRevision()
	}

	client, err := s.hubClient(cfg)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Invalid proxy configuration", err.Error())
		return
	}
	client.Timeout = 20 * time.Second

	candidates := []string{"README.md", "readme.md", "Readme.md"}
	for _, name := range candidates {
		rawURL := src.FileURL(repo, isDataset, revision, name)
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, rawURL, nil)
		if err != nil {
			continue
		}
		if cfg.Token != "" {
			req.Header.Set("Authorization", "Bearer "+cfg.Token)
		}
		req.Header.Set("User-Agent", "modelfetch/1")

		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 768<<10))
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK && readErr == nil && len(body) > 0 {
			baseRaw := src.ResolveBaseURL(repo, isDataset, revision)
			baseBlob := src.BlobBaseURL(repo, isDataset, revision)
			writeJSON(w, http.StatusOK, map[string]any{
				"repo":       repo,
				"revision":   revision,
				"path":       name,
				"markdown":   string(body),
				"baseRawURL": baseRaw,
				"html":       renderReadmeHTMLForSource(string(body), baseRaw, baseBlob, readmeEndpointHost(cfg.Endpoint), src),
			})
			return
		}
	}

	writeError(w, http.StatusNotFound, "README not found", "")
}

// --- Cache Browser ---

// CachedRepoInfo represents a cached repository for the API response.
type CachedRepoInfo struct {
	Repo           string           `json:"repo"`
	Owner          string           `json:"owner"`
	Name           string           `json:"name"`
	Type           string           `json:"type"` // "model" or "dataset"
	Path           string           `json:"path"`
	FriendlyPath   string           `json:"friendlyPath,omitempty"`
	Size           int64            `json:"size"`
	SizeHuman      string           `json:"sizeHuman"`
	FileCount      int              `json:"fileCount"`
	Branch         string           `json:"branch,omitempty"`
	Commit         string           `json:"commit,omitempty"`
	Downloaded     string           `json:"downloaded,omitempty"`
	DownloadStatus string           `json:"downloadStatus,omitempty"` // "complete", "filtered", "unknown"
	Snapshots      []string         `json:"snapshots,omitempty"`
	Files          []CachedFileInfo `json:"files,omitempty"`
	Manifest       *ManifestInfo    `json:"manifest,omitempty"`
	Source         string           `json:"source,omitempty"` // "HF cache", "Friendly view", "Local"
	Quantizations  []string         `json:"quantizations,omitempty"`
	HasMMProj      bool             `json:"hasMMProj,omitempty"`
	MMProjFiles    []string         `json:"mmprojFiles,omitempty"`
	Capabilities   []string         `json:"capabilities,omitempty"`
}

// CachedFileInfo represents a file in the cache.
type CachedFileInfo struct {
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	SizeHuman string `json:"sizeHuman"`
	IsLFS     bool   `json:"isLfs"`
}

// ManifestInfo contains manifest data if available.
type ManifestInfo struct {
	Branch     string `json:"branch"`
	Commit     string `json:"commit"`
	Downloaded string `json:"downloaded"`
	Command    string `json:"command,omitempty"`
	TotalSize  int64  `json:"totalSize"`
	TotalFiles int    `json:"totalFiles"`
	IsFiltered bool   `json:"isFiltered"`        // True if download used filters
	Filters    string `json:"filters,omitempty"` // The filter string if used
}

// CacheStats contains aggregate statistics about the cache.
type CacheStats struct {
	TotalModels    int    `json:"totalModels"`
	TotalDatasets  int    `json:"totalDatasets"`
	TotalSize      int64  `json:"totalSize"`
	TotalSizeHuman string `json:"totalSizeHuman"`
	TotalFiles     int    `json:"totalFiles"`
}

// localCacheRoot describes a single directory to scan for cached
// repos, with the Source label to display in the UI and a flag for
// whether to skip the HF-cache special subdirectories (hub, blobs,
// etc.) that aren't user-visible repos.
type localCacheRoot struct {
	Path        string
	Source      string
	SkipSpecial bool
}

// skipsOwner applies the root's exclusion policy only to a top-level owner.
func (root localCacheRoot) skipsOwner(owner string) bool {
	if root.SkipSpecial {
		switch strings.ToLower(owner) {
		case "hub", "models", "datasets", "blobs", "snapshots", "refs":
			return true
		}
	}
	return false
}

// pathIdentityKey preserves case-sensitive paths except on Windows, where
// configured paths have historically been compared case-insensitively.
// It is a lexical comparison key, not symlink or filesystem canonicalization.
func pathIdentityKey(path string) string {
	path = filepath.Clean(path)
	if runtime.GOOS == "windows" {
		return strings.ToLower(path)
	}
	return path
}

// cleanPathList trims whitespace, filepath.Cleans each path, drops
// empties, and de-duplicates by platform path identity. Used to normalize
// the user-supplied LocalScanDirs list before it lands in s.config.
func cleanPathList(paths []string) []string {
	var cleaned []string
	seen := make(map[string]bool)
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		path = filepath.Clean(path)
		key := pathIdentityKey(path)
		if seen[key] {
			continue
		}
		seen[key] = true
		cleaned = append(cleaned, path)
	}
	return cleaned
}

// localCacheRoots builds the list of directories to scan for cached
// repos: the Friendly-view <cache>/models tree, the user-supplied
// localDir, the user-supplied localScanDirs, every configured download-route
// destination, and the raw cache dir (with SkipSpecial because its hub/blobs
// layout is internal). Roots are deduped by lexical path identity, preserving
// the first path/source/order and merging SkipSpecial with OR.
func localCacheRoots(cacheDir, localDir string, localScanDirs []string, downloadRoutes map[string]string) []localCacheRoot {
	var roots []localCacheRoot
	seen := make(map[string]int)
	add := func(path, source string, skipSpecial bool) {
		if path == "" {
			return
		}
		cleaned := filepath.Clean(path)
		key := pathIdentityKey(cleaned)
		if index, ok := seen[key]; ok {
			roots[index].SkipSpecial = roots[index].SkipSpecial || skipSpecial
			return
		}
		seen[key] = len(roots)
		roots = append(roots, localCacheRoot{Path: cleaned, Source: source, SkipSpecial: skipSpecial})
	}

	cacheRoot := filepath.Clean(cacheDir)
	localSkipSpecial := localDir != "" && pathIdentityKey(localDir) == pathIdentityKey(cacheRoot)

	add(filepath.Join(cacheDir, "models"), "Friendly view", false)
	add(localDir, "Local", localSkipSpecial)
	for _, dir := range localScanDirs {
		add(dir, "Local", false)
	}
	for _, dir := range routeDirs(downloadRoutes) {
		add(dir, "Local", false)
	}
	add(cacheDir, "Local", true)
	return roots
}

// configuredRootIdentity resolves an existing configured directory through
// symlinks. For an ordinary missing suffix, it resolves the nearest existing
// directory and reconstructs the absent components so adding a missing scan
// root does not make unrelated selections unavailable.
func configuredRootIdentity(name string) (lexical, physical string, ok bool) {
	lexical, err := filepath.Abs(filepath.Clean(name))
	if err != nil {
		return "", "", false
	}
	missing := []string{}
	for current := lexical; ; current = filepath.Dir(current) {
		info, statErr := os.Lstat(current)
		if statErr == nil {
			if info.Mode()&os.ModeSymlink == 0 && !info.IsDir() {
				return lexical, "", false
			}
			resolved, resolveErr := filepath.EvalSymlinks(current)
			if resolveErr != nil {
				return lexical, "", false
			}
			target, targetErr := os.Stat(current)
			if targetErr != nil || !target.IsDir() {
				return lexical, "", false
			}
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return pathIdentityKey(lexical), pathIdentityKey(filepath.Clean(resolved)), true
		}
		if !os.IsNotExist(statErr) {
			return lexical, "", false
		}
		parent := filepath.Dir(current)
		if parent == current {
			return lexical, "", false
		}
		missing = append(missing, filepath.Base(current))
	}
}

// excludedSubrootsWithStatus projects every known descendant boundary into
// the walker's lexical root namespace. Walking remains lexical (Walk does not
// follow symlinked directories), while overlap ownership is established from
// resolved configured directory identities.
func (root localCacheRoot) excludedSubrootsWithStatus(roots []localCacheRoot) ([]string, bool) {
	rootLexical, rootPhysical, rootOK := configuredRootIdentity(root.Path)
	if !rootOK {
		return nil, false
	}
	var excluded []string
	seen := make(map[string]bool)
	complete := true
	add := func(candidate string) {
		key := pathIdentityKey(filepath.Clean(candidate))
		if !seen[key] {
			seen[key] = true
			excluded = append(excluded, key)
		}
	}
	for _, candidate := range roots {
		candidateLexical, candidatePhysical, candidateOK := configuredRootIdentity(candidate.Path)
		if !candidateOK {
			complete = false
			// Keep any provable lexical boundary for best-effort reads, but do not
			// claim that an unresolved configured root is physically disjoint.
			if abs, err := filepath.Abs(filepath.Clean(candidate.Path)); err == nil &&
				pathIdentityKey(abs) != rootLexical && withinLocalRoot(rootLexical, pathIdentityKey(abs)) {
				add(abs)
			}
			continue
		}
		if candidateLexical != rootLexical && withinLocalRoot(rootLexical, candidateLexical) {
			add(candidateLexical)
		}
		if candidatePhysical != rootPhysical && withinLocalRoot(rootPhysical, candidatePhysical) {
			rel, err := filepath.Rel(rootPhysical, candidatePhysical)
			if err != nil || rel == "." || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				complete = false
				continue
			}
			add(filepath.Join(rootLexical, rel))
		}
	}
	return excluded, complete
}

// excludedSubroots is the best-effort boundary view used by metadata and
// qualification scans. Selected deletion also consumes the completeness bit.
func (root localCacheRoot) excludedSubroots(roots []localCacheRoot) []string {
	excluded, _ := root.excludedSubrootsWithStatus(roots)
	return excluded
}

// withinLocalRoot compares lexical path keys with a directory boundary, so
// GGUF-other is not within GGUF. Keys retain the platform's case semantics.
func withinLocalRoot(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// walkLocalCacheRepo uses the same subtree ownership for weight qualification,
// file accounting, and metadata. Check even the initial directory: a candidate
// repo may itself be a configured subroot or lie beneath one.
func walkLocalCacheRepo(dir string, excluded []string, walkFn filepath.WalkFunc) error {
	return filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err == nil && len(excluded) > 0 {
			absolute, absErr := filepath.Abs(path)
			if absErr != nil {
				return absErr
			}
			key := pathIdentityKey(absolute)
			for _, subroot := range excluded {
				if withinLocalRoot(subroot, key) {
					if info.IsDir() {
						return filepath.SkipDir
					}
					return nil
				}
			}
		}
		return walkFn(path, info, err)
	})
}

// hasLocalWeightFile reports whether dir (or any subdirectory) contains
// at least one .gguf or .safetensors file. Used to decide whether an
// owner/name folder under a cache root is a real repo worth listing
// vs. an unrelated directory, without crossing configured subroots.
func hasLocalWeightFile(dir string, excluded []string) bool {
	found := false
	walkLocalCacheRepo(dir, excluded, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		switch strings.ToLower(filepath.Ext(info.Name())) {
		case ".gguf", ".safetensors":
			found = true
		}
		return nil
	})
	return found
}

// cacheQuantPattern matches a GGUF quantisation token inside a filename
// (e.g. "Q4_K_M", "UD_Q6_K", "BF16", "APEX-BALANCED"). The optional
// "UD[-_]" prefix is captured separately so the result can be
// returned as "UD_Q4_K" rather than "UD_ Q4_K" or similar. The
// pattern is case-insensitive.
var cacheQuantPattern = regexp.MustCompile(`(?i)(UD[-_])?(IQ[1-4]_(?:XXS|XS|S|M|NL)|Q[2-8]_(?:[01]|K(?:_(?:XXL|XL|L|M|S))?)|APEX[-_](?:I[-_])?(?:BALANCED|COMPACT|MINI|QUALITY)|MXFP4_MOE|F(?:16|32)|BF16)`)

// cacheQuantLabel extracts the quantisation token from a GGUF filename
// (e.g. "Q4_K_M" from "model-Q4_K_M.gguf") and returns it in a
// canonical, dash-free uppercase form ("Q4_K_M", or "UD_Q4_K_M" for
// un-distilled variants). Returns "" if no quant token is present.
func cacheQuantLabel(name string) string {
	if m := cacheQuantPattern.FindStringSubmatch(strings.ToUpper(name)); len(m) >= 3 {
		quant := strings.ReplaceAll(strings.ToUpper(m[2]), "-", "_")
		if m[1] != "" {
			return "UD_" + quant
		}
		return quant
	}
	return ""
}

// isCacheMMProjFile reports whether a GGUF filename looks like an
// mmproj companion (clip/projector for multimodal models). Detected
// by an "mmproj" prefix or a "-mmproj" substring in the basename.
func isCacheMMProjFile(name string) bool {
	base := strings.ToLower(filepath.Base(name))
	return strings.HasPrefix(base, "mmproj") || strings.Contains(base, "-mmproj")
}

// cacheGGUFMetadata summarises the GGUF content of a cached repo:
// the set of distinct quantisation labels, whether any mmproj
// companion file is present, the relative paths of those companions
// (when includeMMProjFiles is true), and any inferred capabilities
// (e.g. "vision" for multimodal).
type cacheGGUFMetadata struct {
	Quantizations []string
	HasMMProj     bool
	MMProjFiles   []string
	Capabilities  []string
}

// collectCacheGGUFMetadata walks dir and gathers the GGUF quantisation
// summary (distinct labels, mmproj presence, capabilities) for the
// repo rooted there. When includeMMProjFiles is true, the relative
// paths of mmproj companions are also recorded.
func collectCacheGGUFMetadata(dir string, includeMMProjFiles bool, excluded []string) cacheGGUFMetadata {
	seen := make(map[string]bool)
	meta := cacheGGUFMetadata{}
	walkLocalCacheRepo(dir, excluded, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.EqualFold(filepath.Ext(info.Name()), ".gguf") {
			return nil
		}
		relPath, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			relPath = info.Name()
		}
		if isCacheMMProjFile(info.Name()) {
			meta.HasMMProj = true
			if includeMMProjFiles {
				meta.MMProjFiles = append(meta.MMProjFiles, relPath)
			}
			return nil
		}
		if q := cacheQuantLabel(info.Name()); q != "" && !seen[q] {
			seen[q] = true
			meta.Quantizations = append(meta.Quantizations, q)
		}
		return nil
	})
	sort.Strings(meta.Quantizations)
	sort.Strings(meta.MMProjFiles)
	if meta.HasMMProj {
		meta.Capabilities = append(meta.Capabilities, "vision")
	}
	return meta
}

// collectHFFriendlyGGUFMetadata preserves the friendly projection's metadata,
// but applies the same configured-subroot ownership as local scans. Derive
// exclusions from the library root, not the repo: a configured root may equal
// or enclose the repo's friendly path and must then exclude the entire walk.
func collectHFFriendlyGGUFMetadata(cache *hfdownloader.HFCache, repo *hfdownloader.RepoDir, cfg Config, includeMMProjFiles bool) cacheGGUFMetadata {
	library := localCacheRoot{Path: cache.ModelsDir()}
	if repo.Type() == hfdownloader.RepoTypeDataset {
		library.Path = cache.DatasetsDir()
	}
	roots := localCacheRoots(cache.Root, cfg.LocalDir, cfg.LocalScanDirs, cfg.DownloadRoutes)
	return collectCacheGGUFMetadata(repo.FriendlyPath(), includeMMProjFiles, library.excludedSubroots(roots))
}

// buildLocalCacheRepo builds a CachedRepoInfo for a single owner/name
// folder under a local cache root. It walks the directory, computes
// total size / file count, gathers GGUF metadata, and (when
// includeFiles is true) enumerates the files. The source label
// (e.g. "HF cache", "Friendly view", "Local") is propagated to the
// result so the UI can group repos by origin.
func buildLocalCacheRepo(owner, name, repoDir, source string, includeFiles bool, excluded []string) (*CachedRepoInfo, error) {
	var totalSize int64
	var fileCount int
	var files []CachedFileInfo
	var newest time.Time

	err := walkLocalCacheRepo(repoDir, excluded, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		fileInfo := info
		if info.Mode()&os.ModeSymlink != 0 {
			if realPath, evalErr := filepath.EvalSymlinks(path); evalErr == nil {
				if realInfo, statErr := os.Stat(realPath); statErr == nil {
					fileInfo = realInfo
				}
			}
		}
		totalSize += fileInfo.Size()
		fileCount++
		if fileInfo.ModTime().After(newest) {
			newest = fileInfo.ModTime()
		}
		if includeFiles {
			relPath, relErr := filepath.Rel(repoDir, path)
			if relErr != nil {
				relPath = info.Name()
			}
			files = append(files, CachedFileInfo{
				Name:      relPath,
				Size:      fileInfo.Size(),
				SizeHuman: humanSizeBytes(fileInfo.Size()),
				IsLFS:     fileInfo.Size() > 10*1024*1024,
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	downloaded := ""
	if !newest.IsZero() {
		downloaded = newest.Format("2006-01-02")
	}
	ggufMeta := collectCacheGGUFMetadata(repoDir, includeFiles, excluded)

	return &CachedRepoInfo{
		Repo:           owner + "/" + name,
		Owner:          owner,
		Name:           name,
		Type:           "model",
		Path:           repoDir,
		Size:           totalSize,
		SizeHuman:      humanSizeBytes(totalSize),
		FileCount:      fileCount,
		Downloaded:     downloaded,
		DownloadStatus: "unknown",
		Files:          files,
		Source:         source,
		Quantizations:  ggufMeta.Quantizations,
		HasMMProj:      ggufMeta.HasMMProj,
		MMProjFiles:    ggufMeta.MMProjFiles,
		Capabilities:   ggufMeta.Capabilities,
	}, nil
}

// scanLocalCachedRepos is the two-value form, for callers that cannot report a
// partial failure. Prefer scanLocalCachedReposWarned: dropping the warnings is
// what makes an unreadable directory look like an empty cache.
func scanLocalCachedRepos(cacheDir, localDir string, localScanDirs []string, downloadRoutes map[string]string, includeFiles bool) ([]CachedRepoInfo, error) {
	repos, _, err := scanLocalCachedReposWarned(cacheDir, localDir, localScanDirs, downloadRoutes, includeFiles)
	return repos, err
}

// scanLocalCachedReposWarned lists every local cache root (Friendly view,
// localDir, localScanDirs, raw cache) and returns the discovered repos plus a
// warning for each root, owner directory or repository that could not be read.
//
// Partial failure stays a success — the listing describes what could be read —
// but it must not be silent: with no warnings the caller cannot tell "the cache
// is empty" from "the cache is a directory I have no permission to open", and
// the UI renders both as an empty state. A root that does not exist is not a
// warning: that is the normal state of an uncreated folder.
func scanLocalCachedReposWarned(cacheDir, localDir string, localScanDirs []string, downloadRoutes map[string]string, includeFiles bool) ([]CachedRepoInfo, []string, error) {
	var repos []CachedRepoInfo
	var warnings []string
	roots := localCacheRoots(cacheDir, localDir, localScanDirs, downloadRoutes)
	for _, root := range roots {
		excluded := root.excludedSubroots(roots)
		if _, err := os.Stat(root.Path); os.IsNotExist(err) {
			continue
		}

		owners, err := os.ReadDir(root.Path)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("cannot read cache directory %s: %v", root.Path, err))
			continue
		}
		for _, ownerEntry := range owners {
			if !ownerEntry.IsDir() {
				continue
			}
			owner := ownerEntry.Name()
			if root.skipsOwner(owner) {
				continue
			}
			ownerDir := filepath.Join(root.Path, owner)
			models, err := os.ReadDir(ownerDir)
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("cannot read %s: %v", ownerDir, err))
				continue
			}
			for _, modelEntry := range models {
				if !modelEntry.IsDir() {
					continue
				}
				name := modelEntry.Name()
				repoDir := filepath.Join(ownerDir, name)
				if !hasLocalWeightFile(repoDir, excluded) {
					continue
				}
				repo, err := buildLocalCacheRepo(owner, name, repoDir, root.Source, includeFiles, excluded)
				if err != nil {
					warnings = append(warnings, fmt.Sprintf("cannot read repository %s: %v", repoDir, err))
					continue
				}
				repos = append(repos, *repo)
			}
		}
	}
	return repos, warnings, nil
}

// findLocalCachedRepo resolves a single owner/name repo across every
// local cache root and returns its CachedRepoInfo. Returns
// os.ErrNotExist if the repo is not present in any root. The
// includeFiles flag controls whether the per-file list is filled in.
func findLocalCachedRepo(cacheDir, localDir string, localScanDirs []string, downloadRoutes map[string]string, repoID string, includeFiles bool) (*CachedRepoInfo, error) {
	parts := strings.SplitN(repoID, "/", 2)
	if len(parts) != 2 {
		return nil, os.ErrNotExist
	}
	roots := localCacheRoots(cacheDir, localDir, localScanDirs, downloadRoutes)
	for _, root := range roots {
		if root.skipsOwner(parts[0]) {
			continue
		}
		repoDir := filepath.Join(root.Path, parts[0], parts[1])
		excluded := root.excludedSubroots(roots)
		if !hasLocalWeightFile(repoDir, excluded) {
			continue
		}
		return buildLocalCacheRepo(parts[0], parts[1], repoDir, root.Source, includeFiles, excluded)
	}
	return nil, os.ErrNotExist
}

// handleCacheList lists all cached repositories with rich metadata.
func (s *Server) handleCacheList(w http.ResponseWriter, r *http.Request) {
	cfg := s.snapshotConfig()
	cacheDir := cfg.cacheRoot()

	// Get query params
	repoType := r.URL.Query().Get("type") // "model" or "dataset"
	search := strings.ToLower(r.URL.Query().Get("search"))

	cache := cfg.cache()
	repoDirs, err := cache.ListRepos()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to list cache", err.Error())
		return
	}

	var repos []CachedRepoInfo
	var stats CacheStats
	seenRepos := make(map[string]bool)

	for _, rd := range repoDirs {
		rdType := string(rd.Type())
		repoID := rd.RepoID()

		// Filter by type if specified
		if repoType != "" {
			if repoType == "dataset" && rdType != "dataset" {
				continue
			}
			if repoType == "model" && rdType != "model" {
				continue
			}
		}

		// Filter by search term
		if search != "" && !strings.Contains(strings.ToLower(repoID), search) {
			continue
		}

		// Get size by walking blobs directory
		blobsDir := rd.BlobsDir()
		var totalSize int64
		var fileCount int
		filepath.Walk(blobsDir, func(path string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() && !strings.HasSuffix(path, ".incomplete") && !strings.HasSuffix(path, ".meta") {
				totalSize += info.Size()
				fileCount++
			}
			return nil
		})

		// Update stats
		if rdType == "model" {
			stats.TotalModels++
		} else {
			stats.TotalDatasets++
		}
		stats.TotalSize += totalSize
		stats.TotalFiles += fileCount

		// Try to read commit from refs/main
		branch := "main"
		commit, _ := rd.ReadRef("main")
		if commit == "" {
			// Try other common refs
			commit, _ = rd.ReadRef("master")
			if commit != "" {
				branch = "master"
			}
		}

		// Get modification time from blobs dir
		var downloaded string
		if info, err := os.Stat(blobsDir); err == nil {
			downloaded = info.ModTime().Format("2006-01-02")
		}

		// Try to read manifest from friendly path
		var manifest *ManifestInfo
		var downloadStatus string
		friendlyPath := rd.FriendlyPath()
		manifestPath := filepath.Join(friendlyPath, hfdownloader.ManifestFilename)
		if m, err := hfdownloader.ReadManifest(manifestPath); err == nil && manifestBelongsToRepo(cache, rd, m) {
			// Parse command for filter flags
			isFiltered, filters := parseCommandFilters(m.Command)

			manifest = &ManifestInfo{
				Branch:     m.Branch,
				Commit:     m.Commit,
				Downloaded: m.CompletedAt.Format("2006-01-02 15:04"),
				Command:    m.Command,
				TotalSize:  m.TotalSize,
				TotalFiles: m.TotalFiles,
				IsFiltered: isFiltered,
				Filters:    filters,
			}
			// Override with manifest data if available
			if m.Branch != "" {
				branch = m.Branch
			}
			if m.Commit != "" {
				commit = m.Commit
			}
			downloaded = m.CompletedAt.Format("2006-01-02")

			// Set download status based on manifest
			if isFiltered {
				downloadStatus = "filtered"
			} else {
				downloadStatus = "complete"
			}
		} else {
			// No manifest - either downloaded by Python or external tool
			downloadStatus = "unknown"
		}

		// Shorten commit hash
		shortCommit := commit
		if len(shortCommit) > 7 {
			shortCommit = shortCommit[:7]
		}

		ggufMeta := collectHFFriendlyGGUFMetadata(cache, rd, cfg, false)

		repo := CachedRepoInfo{
			Repo:           repoID,
			Owner:          rd.Owner(),
			Name:           rd.Name(),
			Type:           rdType,
			Path:           rd.Path(),
			FriendlyPath:   friendlyPath,
			Size:           totalSize,
			SizeHuman:      humanSizeBytes(totalSize),
			FileCount:      fileCount,
			Branch:         branch,
			Commit:         shortCommit,
			Downloaded:     downloaded,
			DownloadStatus: downloadStatus,
			Manifest:       manifest,
			Source:         "HF cache",
			Quantizations:  ggufMeta.Quantizations,
			HasMMProj:      ggufMeta.HasMMProj,
			Capabilities:   ggufMeta.Capabilities,
		}
		repos = append(repos, repo)
		seenRepos[strings.ToLower(rdType+":"+repoID)] = true
	}

	localRepos, localWarnings, _ := scanLocalCachedReposWarned(cacheDir, cfg.LocalDir, cfg.LocalScanDirs, cfg.DownloadRoutes, false)
	for _, repo := range localRepos {
		if repoType != "" && repoType != repo.Type {
			continue
		}
		if search != "" && !strings.Contains(strings.ToLower(repo.Repo), search) {
			continue
		}
		key := strings.ToLower(repo.Type + ":" + repo.Repo)
		if seenRepos[key] {
			continue
		}
		seenRepos[key] = true
		repos = append(repos, repo)

		if repo.Type == "model" {
			stats.TotalModels++
		} else {
			stats.TotalDatasets++
		}
		stats.TotalSize += repo.Size
		stats.TotalFiles += repo.FileCount
	}

	stats.TotalSizeHuman = humanSizeBytes(stats.TotalSize)

	response := map[string]any{
		"repos":           repos,
		"stats":           stats,
		"cacheDir":        cacheDir,
		"effectiveHubDir": cache.HubDir(),
		"hubDirSource":    cfg.hubDirSource(),
	}
	// Omitted when there is nothing to say. A non-empty list means this listing is
	// only part of the truth: the response still describes what could be read, and
	// every root, owner directory or repository that could not is named here.
	if len(localWarnings) > 0 {
		response["warnings"] = localWarnings
	}
	writeJSON(w, http.StatusOK, response)
}

// handleCacheInfo returns details about a specific cached repository.
func (s *Server) handleCacheInfo(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("repo")
	if repo == "" {
		writeError(w, http.StatusBadRequest, "Missing repository", "Format: /api/cache/owner/name")
		return
	}
	if !hfdownloader.IsValidModelName(repo) {
		writeError(w, http.StatusBadRequest, "Invalid repo format", "Expected owner/name")
		return
	}

	repoTypeValue, suppliedType := r.URL.Query()["type"]
	repoType := hfdownloader.RepoTypeModel
	if suppliedType {
		if len(repoTypeValue) != 1 || (repoTypeValue[0] != string(hfdownloader.RepoTypeModel) && repoTypeValue[0] != string(hfdownloader.RepoTypeDataset)) {
			writeError(w, http.StatusBadRequest, "Invalid repository type", "Expected type=model or type=dataset")
			return
		}
		repoType = hfdownloader.RepoType(repoTypeValue[0])
	}
	cfg := s.snapshotConfig()
	cacheDir := cfg.cacheRoot()
	cache := cfg.cache()

	repoDir, err := cache.Repo(repo, repoType)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid repository format", err.Error())
		return
	}

	// Preserve the legacy untyped model-then-dataset lookup. Explicit type is authoritative.
	if _, err := os.Stat(repoDir.Path()); os.IsNotExist(err) && !suppliedType {
		// Try as dataset
		repoDir, _ = cache.Repo(repo, hfdownloader.RepoTypeDataset)
		if _, err := os.Stat(repoDir.Path()); os.IsNotExist(err) {
			localRepo, localErr := findLocalCachedRepo(cacheDir, cfg.LocalDir, cfg.LocalScanDirs, cfg.DownloadRoutes, repo, true)
			if localErr == nil {
				writeJSON(w, http.StatusOK, localRepo)
				return
			}
			writeError(w, http.StatusNotFound, "Repository not found in cache", "")
			return
		}
	} else if _, statErr := os.Stat(repoDir.Path()); os.IsNotExist(statErr) && suppliedType {
		if repoType == hfdownloader.RepoTypeModel {
			localRepo, localErr := findLocalCachedRepo(cacheDir, cfg.LocalDir, cfg.LocalScanDirs, cfg.DownloadRoutes, repo, true)
			if localErr == nil {
				writeJSON(w, http.StatusOK, localRepo)
				return
			}
		}
		writeError(w, http.StatusNotFound, "Repository not found in cache", "")
		return
	}

	// Get snapshots
	snapshots, _ := repoDir.ListSnapshots()

	// Get size and file list by walking blobs directory
	blobsDir := repoDir.BlobsDir()
	var totalSize int64
	var files []CachedFileInfo

	// If we have snapshots, walk the latest one to get file names
	if len(snapshots) > 0 {
		// Use the first snapshot (usually the most recent)
		snapshotDir, sderr := repoDir.SnapshotDir(snapshots[0])
		if sderr != nil {
			writeError(w, http.StatusBadRequest, "Invalid snapshot", sderr.Error())
			return
		}
		filepath.Walk(snapshotDir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			// Get actual size by following symlink to blob
			realPath, err := filepath.EvalSymlinks(path)
			if err != nil {
				return nil
			}
			realInfo, err := os.Stat(realPath)
			if err != nil {
				return nil
			}
			relPath, _ := filepath.Rel(snapshotDir, path)
			files = append(files, CachedFileInfo{
				Name:      relPath,
				Size:      realInfo.Size(),
				SizeHuman: humanSizeBytes(realInfo.Size()),
				IsLFS:     realInfo.Size() > 10*1024*1024, // Assume >10MB is LFS
			})
			totalSize += realInfo.Size()
			return nil
		})
	} else {
		// No snapshots, just count blobs
		filepath.Walk(blobsDir, func(path string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() && !strings.HasSuffix(path, ".incomplete") && !strings.HasSuffix(path, ".meta") {
				totalSize += info.Size()
				files = append(files, CachedFileInfo{
					Name:      filepath.Base(path),
					Size:      info.Size(),
					SizeHuman: humanSizeBytes(info.Size()),
					IsLFS:     info.Size() > 10*1024*1024,
				})
			}
			return nil
		})
	}

	// Try to read commit and branch
	branch := "main"
	commit, _ := repoDir.ReadRef("main")
	if commit == "" {
		commit, _ = repoDir.ReadRef("master")
		if commit != "" {
			branch = "master"
		}
	}

	// Try to read manifest
	var manifest *ManifestInfo
	var downloadStatus string
	friendlyPath := repoDir.FriendlyPath()
	manifestPath := filepath.Join(friendlyPath, hfdownloader.ManifestFilename)
	if m, err := hfdownloader.ReadManifest(manifestPath); err == nil && manifestBelongsToRepo(cache, repoDir, m) {
		// Parse command for filter flags
		isFiltered, filters := parseCommandFilters(m.Command)

		manifest = &ManifestInfo{
			Branch:     m.Branch,
			Commit:     m.Commit,
			Downloaded: m.CompletedAt.Format("2006-01-02 15:04"),
			Command:    m.Command,
			TotalSize:  m.TotalSize,
			TotalFiles: m.TotalFiles,
			IsFiltered: isFiltered,
			Filters:    filters,
		}
		if m.Branch != "" {
			branch = m.Branch
		}
		if m.Commit != "" {
			commit = m.Commit
		}

		// Set download status based on manifest
		if isFiltered {
			downloadStatus = "filtered"
		} else {
			downloadStatus = "complete"
		}
	} else {
		// No manifest - either downloaded by Python or external tool
		downloadStatus = "unknown"
	}

	shortCommit := commit
	if len(shortCommit) > 7 {
		shortCommit = shortCommit[:7]
	}

	ggufMeta := collectHFFriendlyGGUFMetadata(cache, repoDir, cfg, true)

	info := CachedRepoInfo{
		Repo:           repoDir.RepoID(),
		Owner:          repoDir.Owner(),
		Name:           repoDir.Name(),
		Type:           string(repoDir.Type()),
		Path:           repoDir.Path(),
		FriendlyPath:   friendlyPath,
		Size:           totalSize,
		SizeHuman:      humanSizeBytes(totalSize),
		FileCount:      len(files),
		Branch:         branch,
		Commit:         shortCommit,
		DownloadStatus: downloadStatus,
		Snapshots:      snapshots,
		Files:          files,
		Manifest:       manifest,
		Source:         "HF cache",
		Quantizations:  ggufMeta.Quantizations,
		HasMMProj:      ggufMeta.HasMMProj,
		MMProjFiles:    ggufMeta.MMProjFiles,
		Capabilities:   ggufMeta.Capabilities,
	}

	writeJSON(w, http.StatusOK, info)
}

// RebuildResponse represents the result of a cache rebuild operation.
type RebuildResponse struct {
	Success         bool     `json:"success"`
	ReposScanned    int      `json:"reposScanned"`
	SymlinksCreated int      `json:"symlinksCreated"`
	SymlinksUpdated int      `json:"symlinksUpdated"`
	OrphansRemoved  int      `json:"orphansRemoved,omitempty"`
	Errors          []string `json:"errors,omitempty"`
	Message         string   `json:"message,omitempty"`
}

// handleCacheRebuild regenerates the friendly view symlinks from the hub cache.
func (s *Server) handleCacheRebuild(w http.ResponseWriter, r *http.Request) {
	cfg := s.snapshotConfig()
	release, ok := s.jobs.beginCacheMutation(
		filepath.Join(cfg.cacheRoot(), "models"),
		filepath.Join(cfg.cacheRoot(), "datasets"),
	)
	if !ok {
		writeError(w, http.StatusConflict, "Selected GGUF is busy", "A selected local GGUF group is being deleted")
		return
	}
	defer release()

	// Parse options from request body
	var req struct {
		Clean bool `json:"clean"` // Remove orphaned symlinks
	}
	_ = readJSON(r, &req) // Ignore errors, use defaults

	cache := cfg.cache()
	if _, err := cache.WriteRebuildScript(); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to write rebuild script", err.Error())
		return
	}

	opts := hfdownloader.SyncOptions{
		Clean:   req.Clean,
		Verbose: false,
	}

	result, err := cache.Sync(opts)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Rebuild failed", err.Error())
		return
	}

	resp := RebuildResponse{
		Success:         true,
		ReposScanned:    result.ReposScanned,
		SymlinksCreated: result.SymlinksCreated,
		SymlinksUpdated: result.SymlinksUpdated,
		OrphansRemoved:  result.OrphansRemoved,
	}

	for _, e := range result.Errors {
		resp.Errors = append(resp.Errors, e.Error())
	}

	if resp.SymlinksCreated == 0 && resp.SymlinksUpdated == 0 {
		resp.Message = "Friendly view is up to date"
	} else {
		resp.Message = fmt.Sprintf("Created %d symlinks, updated %d", resp.SymlinksCreated, resp.SymlinksUpdated)
	}

	writeJSON(w, http.StatusOK, resp)
}

// handleCacheDelete deletes a repository from the cache.
// SECURITY: This endpoint requires extensive validation to prevent:
// - Path traversal attacks (../, encoded variants)
// - Symlink attacks (symlinks pointing outside cache)
// - TOCTOU race conditions
// - Directory escape via prefix manipulation
func (s *Server) handleCacheDelete(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("repo")
	if repo == "" {
		writeError(w, http.StatusBadRequest, "Missing repo path", "")
		return
	}

	// Security Layer 1: Validate repo format strictly (owner/name)
	if !hfdownloader.IsValidModelName(repo) {
		writeError(w, http.StatusBadRequest, "Invalid repository ID format", "Expected format: owner/name")
		return
	}

	// Security Layer 2: Check for path traversal attempts (multiple encodings)
	// Check raw string for obvious traversal patterns
	if strings.Contains(repo, "..") || strings.Contains(repo, "//") {
		writeError(w, http.StatusBadRequest, "Invalid repository ID", "Path traversal not allowed")
		return
	}

	// Security Layer 3: Check for backslashes (Windows path traversal)
	if strings.Contains(repo, "\\") {
		writeError(w, http.StatusBadRequest, "Invalid repository ID", "Backslashes not allowed")
		return
	}

	// Security Layer 4: Validate characters in owner/name are safe
	// Only allow alphanumeric, dash, underscore, and period (standard HF naming)
	parts := strings.SplitN(repo, "/", 2)
	if !isValidRepoComponent(parts[0]) || !isValidRepoComponent(parts[1]) {
		writeError(w, http.StatusBadRequest, "Invalid repository ID", "Invalid characters in repository name")
		return
	}

	// Determine type from query param
	repoTypeStr := r.URL.Query().Get("type")
	repoType := hfdownloader.RepoTypeModel
	if repoTypeStr == "dataset" {
		repoType = hfdownloader.RepoTypeDataset
	}

	cfg := s.snapshotConfig()
	cacheDir := cfg.cacheRoot()
	cache := cfg.cache()

	// Find the repo directory
	repoDir, err := cache.Repo(repo, repoType)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid repository ID", err.Error())
		return
	}

	hubPath := repoDir.Path()
	friendlyPath := repoDir.FriendlyPath()

	// Security Layer 5: Resolve absolute paths
	absCacheDir, err := filepath.Abs(cacheDir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to resolve cache path", err.Error())
		return
	}
	// Ensure cache dir ends with separator to prevent /cache/huggingface-evil matching /cache/huggingface
	absCacheDirWithSep := absCacheDir + string(filepath.Separator)
	absHubDir, err := filepath.Abs(cache.HubDir())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to resolve Hub path", err.Error())
		return
	}

	absHubPath, err := filepath.Abs(hubPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to resolve path", err.Error())
		return
	}

	// Security Layer 6: Check if path exists and is not a symlink (TOCTOU mitigation)
	hubInfo, err := os.Lstat(absHubPath)
	if os.IsNotExist(err) {
		writeError(w, http.StatusNotFound, "Repository not found in cache", repo)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to check path", err.Error())
		return
	}

	// Security Layer 7: Reject if the hub path itself is a symlink (symlink attack prevention)
	if hubInfo.Mode()&os.ModeSymlink != 0 {
		writeError(w, http.StatusBadRequest, "Invalid path", "Cannot delete symlinked directories")
		return
	}

	// Security Layer 8: Verify path is within cache (using cleaned absolute path)
	if !hfdownloader.PathInside(absHubDir, absHubPath) || absHubPath == absHubDir {
		writeError(w, http.StatusBadRequest, "Invalid path", "Path outside cache directory")
		return
	}

	// Security Layer 9: Verify path follows expected HF cache structure
	// Must be exactly H/{models|datasets}--{owner}--{name}; H's parent
	// is not trusted, even when it happens to contain the friendly root.
	expectedPrefix := "models--"
	if repoType == hfdownloader.RepoTypeDataset {
		expectedPrefix = "datasets--"
	}
	hubSubpath, err := filepath.Rel(absHubDir, absHubPath)
	if err != nil || hubSubpath != expectedPrefix+parts[0]+"--"+parts[1] {
		writeError(w, http.StatusBadRequest, "Invalid path", "Path does not match expected cache structure")
		return
	}

	// Security Layer 10: Resolve symlinks to verify final destination is also within cache
	// This catches symlinks inside the directory structure
	realHubPath, err := filepath.EvalSymlinks(absHubPath)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid path", "Cannot resolve repository path safely")
		return
	}
	if realHubPath != absHubPath {
		// Keep the existing escape guard: a Hub/ancestor link must not
		// redefine the trusted deletion root. Compare to selected H, not
		// its parent and not a freshly trusted symlink target.
		if !hfdownloader.PathInside(absHubDir, realHubPath) || realHubPath == absHubDir {
			writeError(w, http.StatusBadRequest, "Invalid path", "Resolved path outside cache directory")
			return
		}
	}

	// All security checks passed - proceed with deletion
	release, ok := s.jobs.beginCacheMutation(absHubPath, friendlyPath)
	if !ok {
		writeError(w, http.StatusConflict, "Selected GGUF is busy", "A selected local GGUF group is being deleted")
		return
	}
	defer release()
	if err := os.RemoveAll(absHubPath); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to delete cache", err.Error())
		return
	}

	// Delete the friendly view directory (symlinks) with same security checks
	if friendlyPath != "" {
		if err := safeDeleteFriendlyPath(friendlyPath, absCacheDirWithSep); err != nil {
			if !os.IsNotExist(err) {
				writeJSON(w, http.StatusOK, map[string]any{"success": false, "message": "Hub repository deleted; friendly view removal failed", "errors": []string{err.Error()}})
				return
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"message": fmt.Sprintf("Deleted %s from cache", repo),
	})
}

// isValidRepoComponent checks if a repository owner or name contains only safe characters.
// Allows: alphanumeric, dash (-), underscore (_), and period (.)
// This matches HuggingFace's naming conventions.
func isValidRepoComponent(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !((r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	// Additional check: component must not be "." or ".."
	if s == "." || s == ".." {
		return false
	}
	return true
}

// safeDeleteFriendlyPath safely deletes the friendly view path with security checks.
func safeDeleteFriendlyPath(friendlyPath, absCacheDirWithSep string) error {
	absFriendlyPath, err := filepath.Abs(friendlyPath)
	if err != nil {
		return err
	}

	// Check it's within cache
	if !strings.HasPrefix(absFriendlyPath+string(filepath.Separator), absCacheDirWithSep) {
		return fmt.Errorf("friendly path outside cache")
	}

	// Check it's not a symlink at the top level
	info, err := os.Lstat(absFriendlyPath)
	if err != nil {
		return err // Doesn't exist, that's fine
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("friendly path is a symlink")
	}

	// Resolve symlinks and verify again
	realPath, err := filepath.EvalSymlinks(absFriendlyPath)
	if err == nil && realPath != absFriendlyPath {
		if !strings.HasPrefix(realPath+string(filepath.Separator), absCacheDirWithSep) {
			return fmt.Errorf("resolved friendly path outside cache")
		}
	}

	return os.RemoveAll(absFriendlyPath)
}

// parseCommandFilters extracts filter information from a manifest command string.
// Returns isFiltered (bool) and the filter string.
func parseCommandFilters(command string) (bool, string) {
	if command == "" {
		return false, ""
	}

	// Look for filter flags: -f, -F, --filters
	// Format examples:
	//   -f "q4_k_m,q5_k_m"
	//   -F q4_k_m
	//   --filters "q4_k_m"
	filters := ""
	isFiltered := false

	// Split command into parts (respecting quotes)
	parts := splitCommand(command)

	for i := 0; i < len(parts); i++ {
		part := parts[i]
		if part == "-f" || part == "-F" || part == "--filters" || part == "--include" {
			isFiltered = true
			// Next part is the filter value
			if i+1 < len(parts) {
				filters = parts[i+1]
				i++
			}
		} else if strings.HasPrefix(part, "-f=") || strings.HasPrefix(part, "-F=") || strings.HasPrefix(part, "--filters=") || strings.HasPrefix(part, "--include=") {
			isFiltered = true
			// Filter value is after =
			idx := strings.Index(part, "=")
			if idx != -1 {
				filters = part[idx+1:]
			}
		}
	}

	return isFiltered, filters
}

// splitCommand splits a command string into parts, respecting quoted strings.
func splitCommand(command string) []string {
	var parts []string
	var current strings.Builder
	inQuote := false
	quoteChar := rune(0)

	for _, r := range command {
		switch {
		case (r == '"' || r == '\'') && !inQuote:
			inQuote = true
			quoteChar = r
		case r == quoteChar && inQuote:
			inQuote = false
			quoteChar = 0
		case r == ' ' && !inQuote:
			if current.Len() > 0 {
				parts = append(parts, current.String())
				current.Reset()
			}
		default:
			current.WriteRune(r)
		}
	}
	if current.Len() > 0 {
		parts = append(parts, current.String())
	}

	return parts
}

// humanSizeBytes converts bytes to human-readable format.
func humanSizeBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
