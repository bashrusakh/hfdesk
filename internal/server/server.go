// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

// Package server provides the HTTP server for the web UI and REST API.
package server

import (
	"context"
	"crypto/subtle"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/bashrusakh/hfdesk/internal/assets"
	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
)

// Config holds server configuration.
type Config struct {
	Addr        string
	Port        int
	Token       string // HuggingFace token
	ModelsDir   string // Output directory for models (not configurable via API)
	DatasetsDir string // Output directory for datasets (not configurable via API)
	CacheDir    string // HuggingFace cache directory for v3 mode
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
	AllowedOrigins     []string // CORS origins
	Endpoint           string   // Custom HuggingFace endpoint (e.g., for mirrors)

	// Authentication
	AuthUser string // Basic auth username (empty = no auth)
	AuthPass string // Basic auth password

	// Proxy configuration
	Proxy *hfdownloader.ProxyConfig
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
	persistMu  sync.Mutex
	httpServer *http.Server
	jobs       *JobManager
	wsHub      *WSHub
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
// fn must REPLACE (not mutate in place) any slice or pointer fields:
// s.config holds e.g. *ProxyConfig and []string LocalScanDirs whose
// backing memory is shared with the copy. handleUpdateSettings handles
// this with copy-on-write for Proxy and cleanPathList for LocalScanDirs.
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
	s.config = c
	s.configGen++
	return s.config, s.configGen
}

// New creates a new server with the given configuration.
func New(cfg Config) *Server {
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
func (s *Server) ListenAndServe(ctx context.Context) error {
	// Start WebSocket hub
	go s.wsHub.Run()

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

	addr := fmt.Sprintf("%s:%d", s.config.Addr, s.config.Port)

	// Build middleware chain: CORS -> Auth -> Logging -> Handler
	handler := s.corsMiddleware(s.basicAuthMiddleware(s.loggingMiddleware(mux)))

	s.httpServer = &http.Server{
		Addr:         addr,
		Handler:      handler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// Graceful shutdown
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		s.httpServer.Shutdown(shutdownCtx)
	}()

	log.Printf("🚀 Server starting on http://%s", addr)
	log.Printf("   Dashboard: http://localhost:%d", s.config.Port)
	log.Printf("   API:       http://localhost:%d/api", s.config.Port)
	if s.config.AuthUser != "" {
		log.Printf("   Auth:      enabled (user: %s)", s.config.AuthUser)
	}

	err := s.httpServer.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
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

	// Cache browser
	mux.HandleFunc("GET /api/cache", s.handleCacheList)
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
			w.Header().Set("WWW-Authenticate", `Basic realm="HFDesk"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r)
	})
}
