// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

// DownloadRequest is the request body for starting a download.
type DownloadRequest struct {
	Repo               string   `json:"repo"`
	Revision           string   `json:"revision,omitempty"`
	Dataset            bool     `json:"dataset,omitempty"`
	Filters            []string `json:"filters,omitempty"`
	Excludes           []string `json:"excludes,omitempty"`
	AppendFilterSubdir bool     `json:"appendFilterSubdir,omitempty"`
	DryRun             bool     `json:"dryRun,omitempty"`
	// ExactMatch matches filters against whole name segments instead of
	// substrings, so "q6_k" selects Q6_K but not Q6_K_XL (github issue #78).
	ExactMatch bool `json:"exactMatch,omitempty"`
	// LocalDir overrides the download destination for this specific request,
	// writing real files (flat mode) instead of the HF cache layout.
	// When empty, the server-global LocalDir (if set) or HF cache is used.
	LocalDir string `json:"localDir,omitempty"`
	// LocalRepo overrides the folder name used for storing downloaded files.
	// When set, files fetched from Repo are stored as if they belong to LocalRepo.
	// Used when downloading upstream mmproj files alongside the current model's quants.
	LocalRepo string `json:"localRepo,omitempty"`
	// RouteKey selects a configured download route (a key from the closed,
	// server-defined set, never a path). When set and configured, the resolved
	// destination is used as the job's flat/local directory. An explicit
	// localDir takes priority; an unknown key is rejected with 400.
	RouteKey string `json:"routeKey,omitempty"`
}

// PlanResponse is the response for a dry-run/plan request.
type PlanResponse struct {
	Repo       string     `json:"repo"`
	Revision   string     `json:"revision"`
	Files      []PlanFile `json:"files"`
	TotalSize  int64      `json:"totalSize"`
	TotalFiles int        `json:"totalFiles"`
}

// PlanFile represents a file in the plan.
type PlanFile struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
	LFS  bool   `json:"lfs"`
}

// SettingsResponse represents current settings.
type SettingsResponse struct {
	Token              string `json:"token,omitempty"`
	CacheDir           string `json:"cacheDir"`
	ConfiguredCacheDir string `json:"configuredCacheDir"`
	EffectiveHubDir    string `json:"effectiveHubDir"`
	HubDirSource       string `json:"hubDirSource"`
	Concurrency        int    `json:"connections"`
	MaxActive          int    `json:"maxActive"`
	MultipartThreshold string `json:"multipartThreshold"`
	MaxSpeed           string `json:"maxSpeed"`
	// MaxStreamsPerHost is the configured cap on how many response streams this
	// process reads from one hub host at the same time, summed over all jobs.
	// 0 means "follow connections".
	MaxStreamsPerHost int `json:"maxStreamsPerHost"`
	// EffectiveMaxStreamsPerHost is the budget the stream gate applies right now,
	// after the fallback to the connection count for an unset budget.
	EffectiveMaxStreamsPerHost int    `json:"effectiveMaxStreamsPerHost"`
	Verify                     string `json:"verify"`
	Retries                    int    `json:"retries"`
	StallTimeout               string `json:"stallTimeout"`
	Endpoint                   string `json:"endpoint,omitempty"`
	// Source is the hub API shape used for Endpoint ("hf", "modelscope", ...).
	// Empty means "hf".
	Source string `json:"source,omitempty"`
	// SourceDefaults maps each registered source id to the base URL it uses when
	// the endpoint is left empty. An endpoint fits exactly one source's API shape,
	// so the UI needs this to prefill when the source changes.
	SourceDefaults map[string]string `json:"source_defaults,omitempty"`
	// SourceHosts lists the hosts each registered source is known to serve. The UI
	// uses it to decide whether a saved endpoint was written by an earlier source
	// switch (replace it) or typed by the user (keep it).
	SourceHosts map[string][]string `json:"source_hosts,omitempty"`
	// SourcePageTemplates maps repo kind ("model" or "dataset") to the repository
	// page URL of the selected source, with "{repo}" left as a placeholder. The UI
	// builds its "open in hub" links from this instead of assuming Hugging Face.
	SourcePageTemplates map[string]string `json:"source_page_templates,omitempty"`
	// HubSources is the saved list of hub access sources: the Settings tabs and the
	// Models page selector. Every credential in it is masked, the same way Token is.
	HubSources []HubSource `json:"hub_sources,omitempty"`
	// ActiveHubSource is the id of the entry whose source, endpoint and token the
	// server is using right now; Token above belongs to it.
	ActiveHubSource string `json:"active_hub_source,omitempty"`
	// StorageMode is "local" when the server writes real files into LocalDir,
	// or "cache" when it uses the HF cache layout. Set at startup, read-only.
	StorageMode   string   `json:"storageMode"`
	LocalDir      string   `json:"localDir,omitempty"`
	LocalScanDirs []string `json:"localScanDirs,omitempty"`
	// DownloadRoutes maps internal route keys to destination directories.
	// Opt-in; empty/omitted means routed downloads are disabled.
	DownloadRoutes map[string]string `json:"downloadRoutes,omitempty"`
	// Hardware is the user's machine profile override for recommendations.
	// nil means recommendations use only what was detected.
	Hardware *HardwareConfig `json:"hardware,omitempty"`
	// Proxy settings
	Proxy *ProxySettingsResponse `json:"proxy,omitempty"`
	// Config file paths
	ConfigFile  string `json:"configFile,omitempty"`
	TargetsFile string `json:"targetsFile,omitempty"`
}

// ProxySettingsResponse represents proxy configuration in API responses.
type ProxySettingsResponse struct {
	URL                string `json:"url,omitempty"`
	Username           string `json:"username,omitempty"`
	NoProxy            string `json:"noProxy,omitempty"`
	NoEnvProxy         bool   `json:"noEnvProxy,omitempty"`
	InsecureSkipVerify bool   `json:"insecureSkipVerify,omitempty"`
	// Note: Password is intentionally omitted from response for security.
}

// ErrorResponse represents an API error.
type ErrorResponse struct {
	OK          bool            `json:"ok"`
	Error       string          `json:"error"` // Legacy flat message for existing UI/tests.
	Details     string          `json:"details,omitempty"`
	ErrorDetail *APIErrorDetail `json:"error_detail,omitempty"`
}

// APIErrorDetail is the structured error payload for API clients.
type APIErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details string `json:"details,omitempty"`
}

// SuccessResponse represents a simple success message.
type SuccessResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message,omitempty"`
}
