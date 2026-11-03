// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

// Package server provides the HTTP server for the web UI and REST API.
package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/SnailStudioX/model-fetch/internal/assets/webui"
	"github.com/SnailStudioX/model-fetch/pkg/hfdownloader"
	"github.com/jarvisfriends/devices-common/devices"
)

// Config holds server configuration.
type Config struct {
	Addr string
	Port int
	// PrintURL writes "MODELFETCH_URL http://<addr>" once the listener is bound. The
	// desktop shell needs the resolved port when Port is 0.
	PrintURL    bool
	Token       string            // HuggingFace token
	ModelsDir   string            // Output directory for models (not configurable via API)
	DatasetsDir string            // Output directory for datasets (not configurable via API)
	CacheDir    string            // HuggingFace cache directory for v3 mode
	cacheEnv    *cacheEnvironment // Startup paths; immutable across config snapshots.
	// LocalDir, when set, puts the whole server in flat/local-file mode: every
	// download writes real files into <LocalDir>/<owner>/<repo> instead of the
	// HF cache layout. Set once at startup (serve --local-dir); not changeable
	// per request. Empty = HF cache mode.
	LocalDir string
	// LocalScanDirs are additional read-only roots scanned by the Cache browser
	// as <owner>/<model> folders, e.g. LM Studio or a manually managed model
	// library. Downloads still go to CacheDir/LocalDir unless a job overrides it.
	LocalScanDirs      []string
	Concurrency        int
	MaxActive          int
	MultipartThreshold string // Minimum size for multipart download
	MaxSpeed           string // Global download speed cap, e.g. "2MB" (empty/"0" = unlimited)
	// MaxStreamsPerHost bounds how many response streams this process reads from
	// one hub host at the same time, summed over every job and every file. It is
	// deliberately separate from Concurrency, which is the per-file multipart
	// connection count: a hub mirror throttles the client IP, not the job, so
	// "active files x connections" live streams is what makes it stall them.
	// 0 = follow Concurrency (the historical behavior).
	MaxStreamsPerHost int
	Verify            string   // Verification mode: none, size, sha256
	Retries           int      // Number of retry attempts
	StallTimeout      string   // Abort a stalled body read after this long (empty = "60s" default, "0" disables)
	AllowedOrigins    []string // CORS origins
	Endpoint          string   // Custom HuggingFace endpoint (e.g., for mirrors)
	Source            string   // Hub API shape for Endpoint ("hf", "modelscope", ...); empty = "hf"
	// HubSources is the saved list of hub access sources the user can pick from
	// the Settings tabs and the Models page selector. ActiveHub is the id of the
	// entry whose source, endpoint and token are the live triple below. The live
	// triple stays a single pair, so no download path has to know the list exists.
	HubSources []HubSource
	ActiveHub  string

	// DownloadRoutes maps a closed route key (see routes.go) to a destination
	// directory used when a download request supplies that routeKey. Opt-in:
	// empty means every download uses LocalDir/HF cache exactly as before.
	DownloadRoutes map[string]string

	// Hardware is the user's machine profile override for recommendations.
	// nil means recommendations use only what was detected.
	Hardware *HardwareConfig

	// Authentication
	AuthUser string // Basic auth username (empty = no auth)
	AuthPass string // Basic auth password

	// Proxy configuration
	Proxy *hfdownloader.ProxyConfig

	// tokenWrite is the pending explicit settings set/clear intent, not the
	// effective startup credential. It is inherited by ordinary updates and
	// retained across skipped side effects and failed saves; it is cleared
	// only once durably persisted by the persister whose snapshot carried the
	// same tokenWriteGen identity. nil preserves the disk token.
	tokenWrite *string
	// tokenWriteGen identifies the pending tokenWrite intent; bumped only when
	// an explicit set/clear replaces it, never by ordinary updates.
	tokenWriteGen uint64
}

// DefaultConfig returns sensible defaults.
func DefaultConfig() Config {
	return Config{
		Addr:               "0.0.0.0",
		Port:               8080,
		ModelsDir:          "./Models",
		DatasetsDir:        "./Datasets",
		Concurrency:        8,
		MaxActive:          3,
		MultipartThreshold: "32MiB",
		Verify:             "size",
		Retries:            4,
		StallTimeout:       "60s",
		// ModelScope is the default hub source: the users of this build sit on CN
		// networks where modelscope.cn is reachable and huggingface.co often is not.
		// A config file that names an Endpoint keeps the source that endpoint implies
		// (see ApplyConfigToServer), so an existing Hugging Face install is not rewritten.
		Source: "modelscope",
	}
}

// Server is the HTTP server for ModelFetch.
type Server struct {
	configMu sync.RWMutex
	config   Config
	// configGen is bumped on every successful withConfig. Callers
	// capture the value before releasing the lock and re-snapshot it
	// after the lock to detect whether a concurrent update committed
	// in between: if so, they drop the post-lock side effects so the
	// in-memory config stays the authoritative one and the job
	// manager / persisted file are not rolled back.
	configGen uint64
	// persistMu serializes the post-lock side effects of handleUpdateSettings
	// (jobs.UpdateConfig + SaveConfigFile). The generation re-check is taken
	// under this lock so that only the latest committed writer persists, and
	// an older writer cannot clobber a newer one's job-manager/file state.
	persistMu  sync.Mutex
	httpServer *http.Server
	jobs       *JobManager
	wsHub      *WSHub
	// avatarCache holds resolved search-row logos keyed by source, endpoint and
	// author. It is read and written only through resolveAvatars under avatarMu,
	// and is created lazily so a Server built without New (tests) still works.
	avatarMu    sync.Mutex
	avatarCache map[string]string
	// hubClientCache is the transport every API-side hub request shares. Building
	// a client per request (as hfdownloader.BuildHTTPClient does) gives each
	// search, analyze, README and avatar lookup its own connection pool: a fresh
	// TCP+TLS handshake every time, and an idle pool left behind. Replacing it on
	// a settings change closes the old one's idle connections. Guarded by
	// hubClientMu; see Server.hubClient.
	hubClientMu    sync.Mutex
	hubClientCache *http.Client
	hubProxy       *hfdownloader.ProxyConfig
	// mirrorTask is the running or most recent mirror sync, one at a time so two
	// of them cannot write the same destination. It is kept after it ends so a
	// page that reloads can still report how the last one finished; see
	// internal/server/mirror_task.go.
	mirrorTaskMu sync.Mutex
	mirrorTask   *MirrorTask
	// cacheCheckTask is the running or most recent cache integrity pass, one at a
	// time. A pass only reads, so it holds no cache-write reservation; see
	// internal/server/cache_check_task.go.
	cacheCheckTaskMu sync.Mutex
	cacheCheckTask   *CacheCheckTask
	// hwMu guards the device engine. The engine itself caches static values,
	// so the sidebar strip can refresh without re-reading the machine.
	hwMu     sync.Mutex
	hwEngine *devices.Engine
	// resolvedAddr is the address the listener actually bound. With --port 0 the
	// configured port is 0, so the desktop shell needs this instead of Config.Port.
	addrMu       sync.Mutex
	resolvedAddr string
}

// Addr returns the address the server actually bound. It is empty before the
// listener exists and reports the OS-chosen port when Config.Port is 0.
func (s *Server) Addr() string {
	s.addrMu.Lock()
	defer s.addrMu.Unlock()
	return s.resolvedAddr
}

func (s *Server) setResolvedAddr(addr string) {
	s.addrMu.Lock()
	s.resolvedAddr = addr
	s.addrMu.Unlock()
}

// uiHandler serves the embedded frontend. Release builds embed the built Svelte
// app; a Go-only build (no frontend build step) has no UI to serve, and a plain
// explanation beats an empty tree that looks like a broken dashboard.
func (s *Server) uiHandler() http.HandlerFunc {
	if !webui.HasBuild() {
		return func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "frontend not embedded in this binary; build it first (cd web && npm ci && npm run build) and rebuild the server", http.StatusServiceUnavailable)
		}
	}
	return s.staticFileHandler(webui.StaticFS())
}

// snapshotConfig returns a value copy of the current server config taken
// under the read lock. Handlers should call this once at the top and use
// the returned copy; that way a concurrent UpdateConfig (which takes the
// write lock for the whole mutation) cannot tear the read.
//
// This mirrors snapshotConfig on JobManager and is the read side of the
// config race fix: writes go through withConfig under the write lock.
func (s *Server) snapshotConfig() Config {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	return s.config
}

// snapshotConfigWithGen is snapshotConfig plus the current generation
// counter. Callers that need to do post-lock side effects (UpdateConfig,
// SaveConfigFile) use this to detect whether a newer write committed
// between the write lock release and the side-effect phase; if so, the
// in-memory config is already the newer one and the side effects would
// otherwise roll the job manager and the file back to an older state.
func (s *Server) snapshotConfigWithGen() (Config, uint64) {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	return s.config, s.configGen
}

// withConfig runs fn under the write lock on a private copy of the
// current config. The copy is stored back into s.config atomically when
// fn returns, so a panic inside fn leaves s.config in its pre-call
// state (no partially-updated corruption) and concurrent readers that
// hold a pre-call snapshotConfig() copy are unaffected.
//
// fn must REPLACE (not mutate in place) any slice, map, or pointer fields:
// s.config holds e.g. *ProxyConfig, []string LocalScanDirs, and
// map[string]string DownloadRoutes whose backing memory is shared with the
// copy. handleUpdateSettings handles this with copy-on-write for Proxy and
// cleanPathList for LocalScanDirs.
//
// The returned Config is the new effective value (a copy of the same
// struct fn mutated) and the returned uint64 is the post-mutation
// generation. Callers doing post-lock side effects re-snapshot the
// generation via snapshotConfigWithGen and drop the side effects if
// it has moved.
func (s *Server) withConfig(fn func(*Config)) (Config, uint64) {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	c := s.config
	fn(&c)
	c.cacheEnv = s.config.cacheEnv
	s.config = c
	s.configGen++
	return s.config, s.configGen
}

// consumeTokenWrite clears the pending explicit token write intent once the
// exact intent identified by intentGen has been durably persisted. Comparing
// the intent identity rather than configGen is what makes this generation-safe:
// ordinary updates inherit the intent and bump configGen without authoring a
// new one, so a newer inherited snapshot cannot clear an intent that a
// genuinely newer explicit set/clear has already replaced, and cannot re-apply
// an already-persisted value. It must only be called after saveSettingsConfig
// succeeded, while holding persistMu (the config lock is taken here). It does
// not bump configGen or tokenWriteGen: the in-memory config is semantically
// unchanged apart from dropping the already-persisted intent.
func (s *Server) consumeTokenWrite(intentGen uint64) {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	if s.config.tokenWrite == nil || s.config.tokenWriteGen != intentGen {
		return
	}
	c := s.config
	c.tokenWrite = nil
	s.config = c
}

// New creates a new server with the given configuration.
func New(cfg Config) *Server {
	cfg = cfg.captureCacheEnvironment()
	if isRedactedToken(cfg.Token) {
		cfg.Token = ""
	}
	// A server always has a hub list, including one built from DefaultConfig() in a
	// test or from a config file written by an older version.
	cfg.ensureHubSources()

	wsHub := NewWSHub()
	jobs := NewJobManager(cfg, wsHub)
	jobs.LoadState()
	s := &Server{
		config: cfg,
		jobs:   jobs,
		wsHub:  wsHub,
	}
	return s
}

// staticFileHandler serves the embedded UI. Content-hashed build assets never
// change name, so they can be cached for a year; index.html is what decides
// which build a browser runs, so it must always be revalidated. Without that
// split a page left open across a server restart keeps running the previous
// bundle, and the user sees behaviour that no longer exists in the binary.
//
// http.FileServer answers the request itself: it derives Content-Type from the
// file, serves Last-Modified/ETag and honours Range. Choosing the type from the
// path suffix here used to label anything that was not .css/.js/.json/.svg —
// the build's icons and fonts among them — as text/html, and it read each file
// whole just to decide whether it existed.
func (s *Server) staticFileHandler(staticFS fs.FS) http.HandlerFunc {
	fileServer := http.FileServer(http.FS(staticFS))

	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		fileServer.ServeHTTP(w, r)
	}
}

// ListenAndServe starts the HTTP server.
func (s *Server) ListenAndServe(ctx context.Context) error {
	mux := http.NewServeMux()

	// API routes
	s.registerAPIRoutes(mux)

	// Static files (embedded)
	mux.HandleFunc("/", s.uiHandler())

	addr := fmt.Sprintf("%s:%d", s.config.Addr, s.config.Port)

	// Build middleware chain: CORS -> Auth -> Logging -> Handler
	handler := s.corsMiddleware(s.basicAuthMiddleware(s.bodyLimitMiddleware(s.loggingMiddleware(mux))))

	log.Printf("🚀 Server starting on http://%s", addr)
	log.Printf("   Dashboard: http://localhost:%d", s.config.Port)
	log.Printf("   API:       http://localhost:%d/api", s.config.Port)
	if !webui.HasBuild() {
		log.Printf("   Frontend:  NOT embedded (this is a Go-only build); run 'npm run build' in web/ to include the dashboard")
	}
	if s.config.AuthUser != "" {
		log.Printf("   Auth:      enabled (user: %s)", s.config.AuthUser)
	}

	return s.serveHTTP(ctx, addr, handler, 10*time.Second)
}

func (s *Server) serveHTTP(ctx context.Context, addr string, handler http.Handler, shutdownTimeout time.Duration) error {
	s.httpServer = &http.Server{
		Addr:        addr,
		Handler:     handler,
		ReadTimeout: 30 * time.Second,
		// WriteTimeout arms a write deadline (now + WriteTimeout) when each
		// request's headers are read, so it bounds handler compute plus the
		// response write. The synchronous analyze/plan-preview handlers grant
		// themselves 60s; this must stay above that budget (plus margin) or a
		// complete result is computed but the response is cut off. A walk that
		// exceeds the handler budget still fails loudly via its own context.
		WriteTimeout: 70 * time.Second,
		IdleTimeout:  120 * time.Second,
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return s.closeJobsAfterListenError(fmt.Errorf("listen on %s: %w", addr, err), shutdownTimeout)
	}
	resolved := listener.Addr().String()
	s.setResolvedAddr(resolved)
	if s.config.PrintURL {
		// Machine-readable line for the desktop shell, which needs the port the OS
		// picked when --port 0 is used.
		fmt.Printf("MODELFETCH_URL http://%s\n", resolved)
	}
	if s.wsHub != nil {
		go s.wsHub.Run()
	}
	return s.serveHTTPListener(ctx, listener, shutdownTimeout)
}

func (s *Server) serveHTTPListener(ctx context.Context, listener net.Listener, shutdownTimeout time.Duration) error {
	serveDone := make(chan error, 1)
	go func() { serveDone <- s.httpServer.Serve(listener) }()

	stoppingForContext := false
	serveReturned := false
	var serveErr error
	select {
	case <-ctx.Done():
		stoppingForContext = true
	case serveErr = <-serveDone:
		serveReturned = true
	}
	if ctx.Err() != nil {
		stoppingForContext = true
	}

	// Quiesce before HTTP drain: accepted handlers may still mutate manager
	// state, but neither their mutations nor runner completion may dispatch work.
	s.jobs.Quiesce()
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownTimeout)
	shutdownErr := s.httpServer.Shutdown(shutdownCtx)
	cancelShutdown()
	if shutdownErr != nil {
		if closeErr := s.httpServer.Close(); closeErr != nil {
			shutdownErr = errors.Join(shutdownErr, fmt.Errorf("close HTTP server: %w", closeErr))
		}
	}

	managerCtx, cancelManager := context.WithTimeout(context.Background(), shutdownTimeout)
	managerErr := s.jobs.Close(managerCtx)
	cancelManager()
	// The long operations that run outside the job manager - a mirror sync and a
	// cache check - are stopped here, after the HTTP drain: a handler still
	// running could otherwise start a new one behind the stop.
	tasksErr := s.stopBackgroundTasks(shutdownTimeout)
	if !serveReturned {
		serveErr = <-serveDone
	}

	var resultErr error
	if serveErr != nil && (!stoppingForContext || !errors.Is(serveErr, http.ErrServerClosed)) {
		resultErr = errors.Join(resultErr, fmt.Errorf("serve HTTP: %w", serveErr))
	}
	if shutdownErr != nil {
		resultErr = errors.Join(resultErr, fmt.Errorf("shutdown HTTP server: %w", shutdownErr))
	}
	if managerErr != nil {
		resultErr = errors.Join(resultErr, fmt.Errorf("close job manager: %w", managerErr))
	}
	if tasksErr != nil {
		resultErr = errors.Join(resultErr, fmt.Errorf("stop background tasks: %w", tasksErr))
	}
	return resultErr
}

// stopBackgroundTasks stops the operations that outlive the request which started
// them. A download job is stopped by the job manager; a mirror sync and a cache
// check run as their own tasks with their own contexts, so shutdown has to reach
// them separately or the process exits in the middle of one. Only the sync has
// anything to finish: see stopMirrorTaskForShutdown and
// stopCacheCheckForShutdown for what each waits for and why.
func (s *Server) stopBackgroundTasks(wait time.Duration) error {
	s.stopCacheCheckForShutdown()
	return s.stopMirrorTaskForShutdown(wait)
}

func (s *Server) closeJobsAfterListenError(listenErr error, timeout time.Duration) error {
	s.jobs.Quiesce()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return errors.Join(listenErr, s.jobs.Close(ctx))
}

// registerAPIRoutes sets up all API endpoints.
func (s *Server) registerAPIRoutes(mux *http.ServeMux) {
	// Health check
	mux.HandleFunc("GET /api/health", s.handleHealth)

	// Downloads
	mux.HandleFunc("POST /api/download", s.handleStartDownload)
	mux.HandleFunc("GET /api/jobs", s.handleListJobs)
	mux.HandleFunc("GET /api/jobs/{id}", s.handleGetJob)
	mux.HandleFunc("DELETE /api/jobs/{id}", s.handleCancelJob)
	mux.HandleFunc("POST /api/jobs/{id}/pause", s.handlePauseJob)
	mux.HandleFunc("POST /api/jobs/{id}/resume", s.handleResumeJob)
	mux.HandleFunc("POST /api/jobs/{id}/retry", s.handleRetryJob)
	mux.HandleFunc("POST /api/jobs/{id}/dismiss", s.handleDismissJob)

	// Settings
	mux.HandleFunc("GET /api/settings", s.handleGetSettings)
	mux.HandleFunc("POST /api/settings", s.handleUpdateSettings)

	// Plan (dry-run)
	mux.HandleFunc("POST /api/plan", s.handlePlan)

	// Smart Analyzer
	mux.HandleFunc("GET /api/analyze/{repo...}", s.handleAnalyze)
	mux.HandleFunc("GET /api/hardware", s.handleHardware)
	mux.HandleFunc("GET /api/readme/{repo...}", s.handleReadme)
	mux.HandleFunc("GET /api/readme-asset", s.handleReadmeAsset)

	// Cache browser
	mux.HandleFunc("GET /api/cache", s.handleCacheList)
	mux.HandleFunc("GET /api/cache-selection", s.handleCacheSelection)
	mux.HandleFunc("DELETE /api/cache-selection", s.handleSelectedCacheDelete)
	mux.HandleFunc("GET /api/cache/{repo...}", s.handleCacheInfo)
	mux.HandleFunc("POST /api/cache/rebuild", s.handleCacheRebuild)
	mux.HandleFunc("DELETE /api/cache/{repo...}", s.handleCacheDelete)

	// Cache integrity check. A byte pass re-reads the whole library, so it runs as
	// a task rather than inside the request that asked for it.
	mux.HandleFunc("POST /api/cache-check", s.startCacheCheck)
	mux.HandleFunc("GET /api/cache-check", s.handleCacheCheckCurrent)
	mux.HandleFunc("GET /api/cache-check/{id}", s.handleCacheCheckByID)
	mux.HandleFunc("POST /api/cache-check/{id}/cancel", s.handleCacheCheckCancel)

	// Mirror - Target management
	mux.HandleFunc("GET /api/mirror/targets", s.handleMirrorTargetsList)
	mux.HandleFunc("POST /api/mirror/targets", s.handleMirrorTargetAdd)
	mux.HandleFunc("DELETE /api/mirror/targets/{name}", s.handleMirrorTargetRemove)

	// Mirror - Operations
	mux.HandleFunc("POST /api/mirror/diff", s.handleMirrorDiff)
	mux.HandleFunc("POST /api/mirror/push", func(w http.ResponseWriter, r *http.Request) { s.startMirrorSync(w, r, "push") })
	mux.HandleFunc("POST /api/mirror/pull", func(w http.ResponseWriter, r *http.Request) { s.startMirrorSync(w, r, "pull") })
	mux.HandleFunc("GET /api/mirror/task", s.handleMirrorTaskCurrent)
	mux.HandleFunc("GET /api/mirror/task/{id}", s.handleMirrorTaskByID)
	mux.HandleFunc("POST /api/mirror/task/{id}/cancel", s.handleMirrorTaskCancel)

	// Search
	mux.HandleFunc("GET /api/search", s.handleSearch)

	// Download history
	mux.HandleFunc("GET /api/history", s.handleHistory)

	// Disk free space
	mux.HandleFunc("GET /api/diskfree", s.handleDiskFree)
	mux.HandleFunc("POST /api/diskfree", s.handleDownloadDiskFree)

	// Folder listing for the Settings folder picker
	mux.HandleFunc("GET /api/browse", s.handleBrowse)
	mux.HandleFunc("POST /api/browse/pick", s.handleNativePick)

	// WebSocket
	mux.HandleFunc("GET /api/ws", s.handleWebSocket)
}

// Middleware

func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}

func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		cfg := s.snapshotConfig()

		// Allow same-origin and configured origins
		if origin != "" {
			allowed := false
			if len(cfg.AllowedOrigins) == 0 {
				// Default: allow same host
				allowed = true
			} else {
				for _, o := range cfg.AllowedOrigins {
					if o == "*" || o == origin {
						allowed = true
						break
					}
				}
			}

			if allowed {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
				w.Header().Set("Access-Control-Max-Age", "86400")
			}
		}

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// basicAuthMiddleware provides HTTP Basic Authentication.
func (s *Server) basicAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := s.snapshotConfig()

		// Skip auth if not configured
		if cfg.AuthUser == "" {
			next.ServeHTTP(w, r)
			return
		}

		user, pass, ok := r.BasicAuth()
		// Constant-time comparison avoids leaking the configured credentials
		// through response-timing differences. Both comparisons always run.
		userOK := subtle.ConstantTimeCompare([]byte(user), []byte(cfg.AuthUser)) == 1
		passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(cfg.AuthPass)) == 1
		if !ok || !userOK || !passOK {
			w.Header().Set("WWW-Authenticate", `Basic realm="ModelFetch"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r)
	})
}
