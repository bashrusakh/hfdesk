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
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/bashrusakh/hfdesk/internal/assets"
	"github.com/bashrusakh/hfdesk/internal/logging"
	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
)

// serverLog is the logger of the HTTP server subsystem. It resolves the
// current default logger on every call, so records reach whichever pipeline
// the application installed.
var serverLog = logging.NewComponentLogger("server")

// serverShutdownTimeout bounds the graceful shutdown sequence: the HTTP
// drain and the job manager close each get this budget.
const serverShutdownTimeout = 10 * time.Second

// Config holds server configuration.
type Config struct {
	Addr        string
	Port        int
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
	MultipartThreshold string   // Minimum size for multipart download
	MaxSpeed           string   // Global download speed cap, e.g. "2MB" (empty/"0" = unlimited)
	Verify             string   // Verification mode: none, size, sha256
	Retries            int      // Number of retry attempts
	StallTimeout       string   // Abort a stalled body read after this long (empty = "60s" default, "0" disables)
	AllowedOrigins     []string // CORS origins
	Endpoint           string   // Custom HuggingFace endpoint (e.g., for mirrors)

	// DownloadRoutes maps a closed route key (see routes.go) to a destination
	// directory used when a download request supplies that routeKey. Opt-in:
	// empty means every download uses LocalDir/HF cache exactly as before.
	DownloadRoutes map[string]string

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
	}
}

// Server is the HTTP server for HFDesk.
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
	persistMu           sync.Mutex
	httpServer          *http.Server
	jobs                *JobManager
	wsHub               *WSHub
	selectedDeleteHooks selectedDeleteHooks
	// boundAddr holds the effective address the server is actually bound to.
	// It is written once the listener exists and read by BoundAddr and
	// BrowserURL, which must work before the server finished starting.
	boundAddrMu sync.RWMutex
	boundAddr   net.Addr
	// runID identifies the run of the server. It is generated once, when
	// the server is asked for it the first time, and it never changes
	// afterwards: a client compares it to tell a restart of the server
	// from a continued run.
	runIDMu sync.Mutex
	runID   string
	// pipelineProvider resolves the logging pipeline the runtime and log
	// endpoints read their records from. It is nil in production, where
	// the process wide default is used; the tests set it to install a
	// pipeline without touching the process wide default.
	pipelineProvider func() *logging.Pipeline
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

// ListenAndServe starts the HTTP server.
//
// It binds the configured address and serves the listener through Serve, so
// the effective bound address is known as soon as the listener exists.
func (s *Server) ListenAndServe(ctx context.Context) error {
	cfg := s.snapshotConfig()
	addr := fmt.Sprintf("%s:%d", cfg.Addr, cfg.Port)

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return s.closeJobsAfterListenError(fmt.Errorf("listen on %s: %w", addr, err), serverShutdownTimeout)
	}
	return s.Serve(ctx, listener)
}

// Serve runs the HFDesk HTTP server on a caller-provided listener.
//
// It uses the same handler chain and the same graceful shutdown sequence as
// ListenAndServe: the job manager is quiesced first, HTTP is drained within
// the shutdown timeout, and the job manager is closed afterwards. The
// effective bound address of the listener is recorded, so BoundAddr and
// BrowserURL report the real address even when the caller asked for an
// OS-assigned port.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	s.setBoundAddr(ln.Addr())
	s.logStartup(ln.Addr())
	return s.serveListener(ctx, ln, s.handler(), serverShutdownTimeout)
}

// BoundAddr returns the effective address the server is actually bound to.
//
// It reports the real listener address, including an OS-assigned port when
// the listener was bound to port 0. It returns nil while the server is not
// bound. It is safe to call concurrently with the running server.
func (s *Server) BoundAddr() net.Addr {
	s.boundAddrMu.RLock()
	defer s.boundAddrMu.RUnlock()
	return s.boundAddr
}

// BrowserURL returns a browser-safe URL for the running server.
//
// A wildcard bind (0.0.0.0, :: or an empty host) yields the loopback
// address 127.0.0.1 instead of the wildcard address, because a browser
// cannot open the wildcard address. Any other host is used as bound. The
// effective port is always included.
func (s *Server) BrowserURL() string {
	host, port := s.boundHostPort()
	return browserURLFor(host, port)
}

// ActiveJobCounts returns how many jobs are running and how many are queued.
//
// A job that has been dispatched but is not marked running yet counts as
// running, which is the same notion of an active job the scheduler uses to
// enforce the max-active limit. The counts are safe to call concurrently
// with the scheduler.
func (s *Server) ActiveJobCounts() (running, queued int) {
	if s.jobs == nil {
		return 0, 0
	}
	return s.jobs.activeCounts()
}

// setBoundAddr records the effective bound address of the server.
func (s *Server) setBoundAddr(addr net.Addr) {
	s.boundAddrMu.Lock()
	s.boundAddr = addr
	s.boundAddrMu.Unlock()
}

// boundHostPort returns the host and the effective port of the running
// server. Before the server is bound it falls back to the configured
// address and port.
func (s *Server) boundHostPort() (string, int) {
	if addr := s.BoundAddr(); addr != nil {
		return splitAddrHostPort(addr)
	}
	cfg := s.snapshotConfig()
	return cfg.Addr, cfg.Port
}

// browserURLFor builds the browser-safe URL for a bind host and an effective
// port.
func browserURLFor(host string, port int) string {
	return "http://" + net.JoinHostPort(browserHost(host), strconv.Itoa(port))
}

// browserHost maps a bind host to a host a browser can open. A wildcard or
// empty bind host becomes the loopback address; every other host is kept.
func browserHost(host string) string {
	if host == "" {
		return "127.0.0.1"
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		return "127.0.0.1"
	}
	return host
}

// splitAddrHostPort splits a listener address into its host and port.
func splitAddrHostPort(addr net.Addr) (string, int) {
	if addr == nil {
		return "", 0
	}
	if tcp, ok := addr.(*net.TCPAddr); ok {
		host := ""
		if tcp.IP != nil {
			host = tcp.IP.String()
		}
		return host, tcp.Port
	}
	host, portText, err := net.SplitHostPort(addr.String())
	if err != nil {
		return addr.String(), 0
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return host, 0
	}
	return host, port
}

// logStartup records the startup information that used to be printed as a
// banner: the listening address, the dashboard and API URLs, and the fact
// that authentication is enabled.
//
// The dashboard URL comes from BrowserURL, the same source the application
// opens and the runtime API reports, so the product has exactly one spelling
// of the dashboard URL. A server that binds a routable address reports that
// address, because the loopback name of the machine is not the address a
// user reaches.
func (s *Server) logStartup(addr net.Addr) {
	cfg := s.snapshotConfig()
	listenURL := "http://" + addr.String()
	dashboardURL := s.BrowserURL()
	serverLog.Info("server starting",
		"addr", listenURL,
		"dashboard", dashboardURL,
		"api", dashboardURL+"/api",
	)
	if cfg.AuthUser != "" {
		serverLog.Info("auth enabled", "user", cfg.AuthUser)
	}
}

// handler builds the HTTP handler chain: the API routes, the embedded static
// files and the middleware chain (CORS -> Auth -> Logging -> mux).
func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()

	// API routes
	s.registerAPIRoutes(mux)

	// Static files (embedded)
	staticFS := assets.StaticFS()
	fileServer := http.FileServer(http.FS(staticFS))

	// Serve index.html for SPA routes
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Try to serve the file directly
		path := r.URL.Path
		if path == "/" {
			path = "/index.html"
		}

		// Check if file exists
		if f, err := staticFS.(fs.ReadFileFS).ReadFile(path[1:]); err == nil {
			// Serve with correct content type
			contentType := "text/html; charset=utf-8"
			switch {
			case len(path) > 4 && path[len(path)-4:] == ".css":
				contentType = "text/css; charset=utf-8"
			case len(path) > 3 && path[len(path)-3:] == ".js":
				contentType = "application/javascript; charset=utf-8"
			case len(path) > 5 && path[len(path)-5:] == ".json":
				contentType = "application/json; charset=utf-8"
			case len(path) > 4 && path[len(path)-4:] == ".svg":
				contentType = "image/svg+xml"
			}
			w.Header().Set("Content-Type", contentType)
			w.Write(f)
			return
		}

		// Fallback to index.html for SPA routing
		fileServer.ServeHTTP(w, r)
	})

	// Build middleware chain: CORS -> Auth -> Logging -> Handler
	return s.corsMiddleware(s.basicAuthMiddleware(s.loggingMiddleware(mux)))
}

// serveListener serves handler on listener with the shared HTTP server
// configuration and the graceful shutdown sequence.
func (s *Server) serveListener(ctx context.Context, listener net.Listener, handler http.Handler, shutdownTimeout time.Duration) error {
	s.httpServer = &http.Server{
		Addr:        listener.Addr().String(),
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
	if s.wsHub != nil {
		go s.wsHub.Run()
	}
	return s.serveHTTPListener(ctx, listener, shutdownTimeout)
}

func (s *Server) serveHTTP(ctx context.Context, addr string, handler http.Handler, shutdownTimeout time.Duration) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return s.closeJobsAfterListenError(fmt.Errorf("listen on %s: %w", addr, err), shutdownTimeout)
	}
	return s.serveListener(ctx, listener, handler, shutdownTimeout)
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
	return resultErr
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
	mux.HandleFunc("GET /api/readme/{repo...}", s.handleReadme)
	mux.HandleFunc("GET /api/readme-asset", s.handleReadmeAsset)

	// Community (read-only Discussions / Pull Requests proxy)
	mux.HandleFunc("GET /api/community/{repo...}", s.handleCommunity)

	// Cache browser
	mux.HandleFunc("GET /api/cache", s.handleCacheList)
	mux.HandleFunc("GET /api/cache-selection", s.handleCacheSelection)
	mux.HandleFunc("DELETE /api/cache-selection", s.handleSelectedCacheDelete)
	mux.HandleFunc("GET /api/cache/{repo...}", s.handleCacheInfo)
	mux.HandleFunc("POST /api/cache/rebuild", s.handleCacheRebuild)
	mux.HandleFunc("DELETE /api/cache/{repo...}", s.handleCacheDelete)

	// Mirror - Target management
	mux.HandleFunc("GET /api/mirror/targets", s.handleMirrorTargetsList)
	mux.HandleFunc("POST /api/mirror/targets", s.handleMirrorTargetAdd)
	mux.HandleFunc("DELETE /api/mirror/targets/{name}", s.handleMirrorTargetRemove)

	// Mirror - Operations
	mux.HandleFunc("POST /api/mirror/diff", s.handleMirrorDiff)
	mux.HandleFunc("POST /api/mirror/push", s.handleMirrorPush)
	mux.HandleFunc("POST /api/mirror/pull", s.handleMirrorPull)

	// Search
	mux.HandleFunc("GET /api/search", s.handleSearch)

	// Download history
	mux.HandleFunc("GET /api/history", s.handleHistory)

	// Disk free space
	mux.HandleFunc("GET /api/diskfree", s.handleDiskFree)
	mux.HandleFunc("POST /api/diskfree", s.handleDownloadDiskFree)

	// WebSocket
	mux.HandleFunc("GET /api/ws", s.handleWebSocket)

	// Runtime status and logs. These endpoints expose the local process
	// diagnostics, so they are guarded by the local-only rule and stay
	// reachable from the local machine only, whatever the server binds to.
	mux.HandleFunc("GET /api/runtime", s.localOnly(s.handleRuntimeStatus))
	mux.HandleFunc("GET /api/runtime/logs", s.localOnly(s.handleRuntimeLogs))
	mux.HandleFunc("GET /api/runtime/logs/stream", s.localOnly(s.handleRuntimeLogStream))
	// The open-logs action changes the state of the desktop, so it is a
	// POST, and it is guarded by the same local-only rule as the other
	// runtime endpoints.
	mux.HandleFunc("POST /api/runtime/open-logs", s.localOnly(s.handleRuntimeOpenLogs))
	// The open-ui action opens the web UI of the running instance in the
	// default browser of the operating system. A page cannot do that by
	// itself, so the runtime view asks the server to do it. It changes the
	// state of the desktop, so it is a POST, and it is guarded by the same
	// local-only rule as the other runtime endpoints.
	mux.HandleFunc("POST /api/runtime/open-ui", s.localOnly(s.handleRuntimeOpenUI))
}

// Middleware

func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		// The path is logged without the query string: a query can carry a
		// credential the caller put there.
		serverLog.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"duration", time.Since(start).Round(time.Millisecond),
		)
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
			w.Header().Set("WWW-Authenticate", `Basic realm="HFDesk"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r)
	})
}
