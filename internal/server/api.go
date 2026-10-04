// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
	"github.com/bashrusakh/hfdesk/pkg/smartdl"
)

// Cache source labels. Kept as shared constants so the copy enumeration, the
// delete routing, and the cache list agree on the exact strings the UI and API
// expose.
const (
	cacheSourceHFCache      = "HF cache"
	cacheSourceFriendlyView = "Friendly view"
	cacheSourceLocal        = "Local"
)

// cacheDeleteIncompleteKey is the machine-readable field set on a delete
// success response when the primary copy was removed but a companion cleanup
// step did not complete (e.g. a protected friendly-view projection). The
// primary `success` stays true for backward compatibility; clients that can
// warn should inspect this flag.
const cacheDeleteIncompleteKey = "cleanupIncomplete"

// cacheDeleteSuccess builds a delete success body. When warnings is non-empty
// the primary delete succeeded but some companion cleanup did not, so the body
// carries cleanupIncomplete plus the individual warnings.
func cacheDeleteSuccess(repo, message string, warnings []string) map[string]any {
	resp := map[string]any{
		"success": true,
		"message": message,
	}
	if len(warnings) > 0 {
		resp[cacheDeleteIncompleteKey] = true
		resp["cleanupWarnings"] = warnings
	}
	return resp
}

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
		if errors.Is(err, errInvalidRouteKey) {
			writeError(w, http.StatusBadRequest, "Invalid routeKey", "routeKey must be one of the configured route keys")
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

	revision := req.Revision
	if revision == "" {
		revision = "main"
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

	// Take a snapshot of the server config; every read below uses it
	// instead of touching s.config directly to avoid racing a concurrent
	// settings update.
	cfg := s.snapshotConfig()

	// Use server-configured output directory (not from request for security)
	outputDir := cfg.ModelsDir
	if req.Dataset {
		outputDir = cfg.DatasetsDir
	}

	settings := hfdownloader.Settings{
		OutputDir: outputDir,
		Token:     cfg.Token,
		Endpoint:  cfg.Endpoint,
	}

	// Collect plan items
	var files []PlanFile
	var totalSize int64

	progressFunc := func(evt hfdownloader.ProgressEvent) {
		if evt.Event == "plan_item" {
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

	// Don't expose full token, just indicate if set
	tokenStatus := ""
	if cfg.Token != "" {
		tokenStatus = "********" + cfg.Token[max(0, len(cfg.Token)-4):]
	}

	cacheDir := cfg.CacheDir
	if cacheDir == "" {
		cacheDir = hfdownloader.DefaultCacheDir()
	}

	storageMode := "cache"
	if cfg.LocalDir != "" {
		storageMode = "local"
	}

	resp := SettingsResponse{
		Token:              tokenStatus,
		CacheDir:           cacheDir,
		Concurrency:        cfg.Concurrency,
		MaxActive:          cfg.MaxActive,
		MultipartThreshold: cfg.MultipartThreshold,
		MaxSpeed:           cfg.MaxSpeed,
		Verify:             cfg.Verify,
		Retries:            cfg.Retries,
		Endpoint:           cfg.Endpoint,
		StorageMode:        storageMode,
		LocalDir:           cfg.LocalDir,
		LocalScanDirs:      cfg.LocalScanDirs,
		DownloadRoutes:     cfg.DownloadRoutes,
		ConfigFile:         ConfigPath(),
		TargetsFile:        hfdownloader.DefaultTargetsPath(),
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
		DownloadRoutes     *map[string]string `json:"downloadRoutes,omitempty"`
		Concurrency        *int               `json:"connections,omitempty"`
		MaxActive          *int               `json:"maxActive,omitempty"`
		MultipartThreshold *string            `json:"multipartThreshold,omitempty"`
		MaxSpeed           *string            `json:"maxSpeed,omitempty"`
		Verify             *string            `json:"verify,omitempty"`
		Retries            *int               `json:"retries,omitempty"`
		Endpoint           *string            `json:"endpoint,omitempty"`
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
	if req.MaxSpeed != nil {
		trimmed := strings.TrimSpace(*req.MaxSpeed)
		if _, err := hfdownloader.ParseSizeStrict(trimmed); err != nil {
			writeError(w, http.StatusBadRequest, "Invalid maxSpeed", err.Error())
			return
		}
		newMaxSpeed = trimmed
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

	// Phase 2: build the new config on a local copy under withConfig (which
	// takes the write lock for the whole mutation, so a concurrent read sees
	// either the pre- or post-state, never a torn mix). The lock is released
	// before we touch the job manager or the file, so the critical section
	// stays short. myGen is the generation we just produced; the side
	// effects below drop themselves if a newer commit landed in between.
	_, myGen := s.withConfig(func(c *Config) {
		if req.Token != nil {
			c.Token = *req.Token
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
		if req.Verify != nil && *req.Verify != "" {
			c.Verify = *req.Verify
		}
		if req.Retries != nil && *req.Retries >= 0 {
			c.Retries = *req.Retries
		}
		if req.Endpoint != nil {
			c.Endpoint = *req.Endpoint
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
		Token:              finalCfg.Token,
		Connections:        finalCfg.Concurrency,
		MaxActive:          finalCfg.MaxActive,
		MultipartThreshold: finalCfg.MultipartThreshold,
		MaxSpeed:           finalCfg.MaxSpeed,
		Verify:             finalCfg.Verify,
		Retries:            &retries,
		Endpoint:           finalCfg.Endpoint,
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
	if err := SaveConfigFile(fileCfg); err != nil {
		// Log error but don't fail the request - settings are still applied in-memory
		writeJSON(w, http.StatusOK, SuccessResponse{
			Success: true,
			Message: "Settings updated (warning: could not persist to config file)",
		})
		return
	}

	writeJSON(w, http.StatusOK, SuccessResponse{
		Success: true,
		Message: "Settings saved",
	})
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

	// Get revision (defaults to "main")
	revision := r.URL.Query().Get("revision")
	if revision == "" {
		revision = "main"
	}

	// Create analyzer
	cfg := s.snapshotConfig()
	opts := smartdl.AnalyzerOptions{
		Token:    cfg.Token,
		Endpoint: cfg.Endpoint,
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
	if revision == "" {
		revision = "main"
	}
	isDataset := strings.EqualFold(r.URL.Query().Get("dataset"), "true")

	cfg := s.snapshotConfig()
	endpoint := strings.TrimRight(cfg.Endpoint, "/")
	if endpoint == "" {
		endpoint = "https://huggingface.co"
	}
	prefix := ""
	if isDataset {
		prefix = "datasets/"
	}

	client, err := hfdownloader.BuildHTTPClient(cfg.Proxy)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Invalid proxy configuration", err.Error())
		return
	}
	client.Timeout = 20 * time.Second

	candidates := []string{"README.md", "readme.md", "Readme.md"}
	for _, name := range candidates {
		rawURL := fmt.Sprintf("%s/%s%s/raw/%s/%s", endpoint, prefix, repo, revision, name)
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, rawURL, nil)
		if err != nil {
			continue
		}
		if cfg.Token != "" {
			req.Header.Set("Authorization", "Bearer "+cfg.Token)
		}
		req.Header.Set("User-Agent", "hfdesk/1")

		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 768<<10))
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK && readErr == nil && len(body) > 0 {
			baseRaw := fmt.Sprintf("%s/%s%s/resolve/%s/", endpoint, prefix, repo, revision)
			baseBlob := fmt.Sprintf("%s/%s%s/blob/%s/", endpoint, prefix, repo, revision)
			writeJSON(w, http.StatusOK, map[string]any{
				"repo":       repo,
				"revision":   revision,
				"path":       name,
				"markdown":   string(body),
				"baseRawURL": baseRaw,
				"html":       renderReadmeHTML(string(body), baseRaw, baseBlob, readmeEndpointHost(cfg.Endpoint)),
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
	// Copies enumerates every distinct deletable physical location for this
	// repo. The top-level fields above describe the primary/first copy for
	// backward compatibility; the UI uses Copies to offer per-location delete.
	// It intentionally has no omitempty: a current server always emits the key
	// (possibly as an empty []), so the UI can tell "current server, no
	// deletable copies" apart from "old server that does not send copies at
	// all" and only offer the legacy fallback delete button in the latter case.
	Copies        []CacheCopy `json:"copies"`
	Quantizations []string    `json:"quantizations,omitempty"`
	HasMMProj     bool        `json:"hasMMProj,omitempty"`
	MMProjFiles   []string    `json:"mmprojFiles,omitempty"`
	Capabilities  []string    `json:"capabilities,omitempty"`
}

// CacheCopy describes one deletable physical location of a cached repo.
type CacheCopy struct {
	Source    string `json:"source"` // "HF cache", "Friendly view", "Local"
	Path      string `json:"path"`   // exact deletable path for this copy
	Size      int64  `json:"size"`
	SizeHuman string `json:"sizeHuman"`
	FileCount int    `json:"fileCount"`
	// SelectiveVariants lists the artifact tokens this copy supports for
	// selective (per-variant) deletion, computed server-side from the copy's own
	// GGUF quantisation labels (Local) or its hfd.yaml manifest (HF/friendly).
	// Empty/omitted means only an entire-copy delete is available, so the UI can
	// avoid offering variant buttons the server would reject.
	SelectiveVariants []string `json:"selectiveVariants,omitempty"`
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
// whether to skip the HF-cache special subdirectories (hub, models,
// datasets, blobs, snapshots, refs) that aren't user-visible repos.
//
// A single physical directory can carry more than one role (for example an
// explicit Local root configured at <cache>/models). Roles are merged when
// paths collapse to the same physical directory: SkipSpecial wins as a safety
// exclusion, while the first spelling keeps the path/source/order. Delete
// authority is decided by path in localAuthorityKeys, not by this display
// Source, so an explicit Local root at <cache>/models still deletes its real
// folders even though it keeps the first-wins "Friendly view" label.
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

// pathIdentityKeyCaseInsensitive reports whether path-identity keys must fold
// case on the given platform. It is a platform decision, not a filesystem
// probe: filepath.EvalSymlinks canonicalizes case only on Windows, while on
// macOS (which is case-insensitive by default) it preserves the caller's
// casing. Fold on those two platforms so a case-insensitive filesystem still
// collapses aliases; Linux is treated as case-sensitive so genuinely distinct
// roots such as /Models and /models stay separate.
func pathIdentityKeyCaseInsensitive(goos string) bool {
	return goos == "windows" || goos == "darwin"
}

// pathIdentityKey returns the physical-identity key for a configured path on
// the current platform. It resolves symlinks and makes the path absolute so a
// symlinked root and its real path share one identity, then folds case on
// case-insensitive-default platforms (Windows, macOS). See
// pathIdentityKeyForGOOS for the platform-dependent case handling.
//
// This is the single path-identity helper shared by configured-path
// comparison (cleanPathList, download-route dedup), cache-root dedup, the
// local-delete boundary guards, and delete evidence, so every place that
// decides whether two spellings name the same physical path agrees.
func pathIdentityKey(path string) string {
	return pathIdentityKeyForGOOS(path, runtime.GOOS)
}

// pathIdentityKeyForGOOS is pathIdentityKey with the platform decision injected
// so the case-folding branch is testable without running on that OS. On
// platforms whose default filesystem is case-insensitive (Windows, macOS) the
// key is lowercased, because EvalSymlinks only canonicalizes case on Windows
// and would otherwise leave case-differing aliases of one physical directory
// un-deduped. On Linux the key preserves case, so two truly distinct roots such
// as /Models and /models stay separate.
func pathIdentityKeyForGOOS(path, goos string) string {
	cleaned := filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(cleaned); err == nil {
		cleaned = resolved
	}
	key := cleaned
	if abs, err := filepath.Abs(cleaned); err == nil {
		key = abs
	}
	if pathIdentityKeyCaseInsensitive(goos) {
		key = strings.ToLower(key)
	}
	return key
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
// layout is internal).
//
// Roots are deduped by physical identity, and the roles of all spellings that
// collapse to the same physical directory are merged rather than letting the
// first spelling win. That preserves SkipSpecial when the raw cache dir is also
// supplied as a scan dir, keeps an explicit Local role on <cache>/models when
// it is configured as a Local root, and prevents a symlinked root plus its real
// path from being listed twice.
func localCacheRoots(cacheDir, localDir string, localScanDirs []string, downloadRoutes map[string]string) []localCacheRoot {
	var roots []localCacheRoot
	index := make(map[string]int)
	add := func(path, source string, skipSpecial bool) {
		if path == "" {
			return
		}
		cleaned := filepath.Clean(path)
		key := pathIdentityKey(cleaned)
		if i, ok := index[key]; ok {
			// Same physical directory already represented: merge the
			// SkipSpecial safety exclusion with OR, but keep the first
			// spelling's path/source/order. Upstream lists <cache>/models as
			// the Friendly view even when it is also configured as a Local
			// root; delete authority is decided separately by localRootHasAuthority,
			// not by relabeling the Source.
			if skipSpecial {
				roots[i].SkipSpecial = true
			}
			return
		}
		index[key] = len(roots)
		roots = append(roots, localCacheRoot{
			Path:        cleaned,
			Source:      source,
			SkipSpecial: skipSpecial,
		})
	}

	localSkipSpecial := localDir != "" && pathIdentityKey(localDir) == pathIdentityKey(cacheDir)

	add(filepath.Join(cacheDir, "models"), cacheSourceFriendlyView, false)
	add(localDir, cacheSourceLocal, localSkipSpecial)
	for _, dir := range localScanDirs {
		add(dir, cacheSourceLocal, false)
	}
	for _, dir := range routeDirs(downloadRoutes) {
		add(dir, cacheSourceLocal, false)
	}
	add(cacheDir, cacheSourceLocal, true)
	return roots
}

// excludedSubroots keeps all scan roots, but gives each configured descendant
// exclusive ownership of its subtree. Absolute lexical keys also handle mixed
// relative/absolute configuration without resolving symlinks.
func (root localCacheRoot) excludedSubroots(roots []localCacheRoot) []string {
	rootPath, err := filepath.Abs(root.Path)
	if err != nil {
		return nil
	}
	rootKey := pathIdentityKey(rootPath)
	var excluded []string
	for _, candidate := range roots {
		candidatePath, err := filepath.Abs(candidate.Path)
		if err != nil {
			continue
		}
		candidateKey := pathIdentityKey(candidatePath)
		if candidateKey != rootKey && withinLocalRoot(rootKey, candidateKey) {
			excluded = append(excluded, candidateKey)
		}
	}
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

// localRepoDirAliased reports whether <root>/<owner>/<name> is reached through
// a symlinked intermediate component or is itself a symlink leaf. Both are
// aliases of other storage, not deletable Local copies, so they must not be
// advertised or resolved into.
func localRepoDirAliased(root, repoDir string) bool {
	if hfdownloader.RejectSymlinkedComponents(root, repoDir) != nil {
		return true
	}
	if info, err := os.Lstat(repoDir); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return true
	}
	return false
}

// localRepoSource returns the Source label to report for a repo directory found
// under rootSource. A directory that is a genuine friendly projection of the
// same repo is reported as the Friendly view even when its enclosing root also
// carries Local authority (e.g. localDir configured at <cache>/models), because
// it is the hub's projection rather than independent Local storage. Any other
// directory keeps the root's label.
func localRepoSource(cacheDir, owner, name, repoDir, rootSource string) string {
	if rootSource == cacheSourceFriendlyView {
		return rootSource
	}
	if rd, err := hfdownloader.NewHFCache(cacheDir, 0).Repo(owner+"/"+name, hfdownloader.RepoTypeModel); err == nil &&
		rd.FriendlyState() == hfdownloader.FriendlyProjection &&
		pathIdentityKey(rd.FriendlyPath()) == pathIdentityKey(repoDir) {
		return cacheSourceFriendlyView
	}
	return rootSource
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

// localUnitGuards returns the physical paths that a deletable Local leaf must
// not be, contain, or (except through its own configured root) be contained by:
// the cache dir, its friendly models/ and datasets/ namespaces, and every
// configured Local root (localDir, localScanDirs, and each download-route
// destination). Rejecting these keeps a Local delete from removing the whole
// cache, a friendly namespace, or a configured root through an
// ancestor/overlapping scan root.
func localUnitGuards(cacheDir, localDir string, localScanDirs []string, downloadRoutes map[string]string) []string {
	if cacheDir == "" {
		return nil
	}
	guards := []string{
		cacheDir,
		filepath.Join(cacheDir, "models"),
		filepath.Join(cacheDir, "datasets"),
	}
	if localDir != "" {
		guards = append(guards, localDir)
	}
	for _, dir := range localScanDirs {
		if dir != "" {
			guards = append(guards, dir)
		}
	}
	for _, dir := range routeDirs(downloadRoutes) {
		if dir != "" {
			guards = append(guards, dir)
		}
	}
	return guards
}

// pathIsPrefix reports whether parent equals child or is a directory ancestor
// of child. Both arguments must already be physical keys (absolute,
// symlink-resolved, and case-normalized for the platform), so pathIsPrefix
// never matches a sibling like /data/Models-evil against /data/Models.
func pathIsPrefix(parent, child string) bool {
	return parent == child || strings.HasPrefix(child, parent+string(filepath.Separator))
}

// localUnitGuardedPath reports whether absTarget collides with a guarded path:
// it equals a guard, is an ancestor of a guarded hard boundary (deleting it
// would delete the cache dir or a friendly namespace), or is a descendant of a
// guard reached through a different root. Descendant containment is allowed only
// through the candidate's own root (or an ancestor of it), because a deletable
// leaf is normally nested under its own configured root -- for example an
// explicit Local root at <cache>/models legitimately deletes
// <cache>/models/<owner>/<name>.
//
// A configured descendant root (localDir/scanDir/download-route destination)
// nested inside another root's owner/name folder does not by itself block the
// enclosing repo: upstream routing treats the nested root as owning its own
// subtree while the enclosing repo remains a real repo for its own files. Only
// the cache boundary (the cache dir and its models/ and datasets/ namespaces) is
// hard: an ancestor of it is always rejected, so deleting a container can never
// wipe the cache or a friendly namespace.
func localUnitGuardedPath(absTarget, rootKey string, guards []string) bool {
	targetKey := pathIdentityKey(absTarget)
	// localUnitGuards always places the cache dir first, so it identifies the
	// hard boundary used for ancestor checks.
	var cacheKey string
	if len(guards) > 0 {
		cacheKey = pathIdentityKey(guards[0])
	}
	for _, guard := range guards {
		guardKey := pathIdentityKey(guard)
		if targetKey == guardKey {
			return true
		}
		if pathIsPrefix(targetKey, guardKey) {
			// The target encloses this guard. Only the cache boundary is a hard
			// ancestor boundary; a nested configured Local root is owned by the
			// enclosing repo.
			if cacheKey != "" && pathIsPrefix(cacheKey, guardKey) {
				return true
			}
			continue
		}
		if pathIsPrefix(guardKey, targetKey) && guardKey != rootKey && !pathIsPrefix(guardKey, rootKey) {
			return true
		}
	}
	return false
}

// localLeafRepoIsDeletable reports whether absTarget is a genuine Local model
// directory that may be ADVERTISED by enumeration (localCopyCandidates, the
// cache list, and findLocalCachedRepo).
//
// A local model is exactly <root>/<owner>/<name>, and its ownership is proven
// recursively: it must contain at least one weight file (.gguf/.safetensors)
// anywhere inside it, including nested subdirectories, unless allowWeightless
// is set (used to complete a retained partial delete whose remainder no longer
// has a weight file). This keeps diffusers-style models (weights under unet/,
// vae/, text_encoder/) and filtered layouts (weights under q4_k_m/) as one
// deletable model. There is deliberately no notion of a nested repo deeper than
// <root>/<owner>/<name> under a single root.
//
// The only excluded targets are the hard boundaries: the cache dir, the
// friendly models/ and datasets/ namespaces (equal to, or an ancestor of, the
// cache boundary), and any path equal to, descendant of, or (for the cache
// boundary) ancestor of a configured root (localDir, localScanDirs, and
// download-route destinations) -- see localUnitGuardedPath. A configured
// descendant root nested inside another root's owner/name folder does not by
// itself block that enclosing repo (upstream routing owns the subtree while the
// enclosing folder stays a real repo); it is excluded from the weight
// qualification via excluded so the enclosure cannot inherit its weight file.
// Symlink/alias protections are applied separately.
//
// This predicate is intentionally RELAXED about a configured root nested inside
// the candidate: upstream #66 advertises such an enclosure as a real repo (it
// owns its own weights), and listing must keep that parity. A whole-copy
// deletion carries a stricter requirement -- see localWholeCopyDeleteAllowed.
//
// rootKey is the physical key of the configured root absTarget was resolved
// from, and excluded lists the physical keys of configured subroots whose
// subtrees another root owns, so a weight file inside one does not qualify this
// target.
func localLeafRepoIsDeletable(absTarget, rootKey string, guards, excluded []string, allowWeightless bool) bool {
	return (allowWeightless || hasLocalWeightFile(absTarget, excluded)) && !localUnitGuardedPath(absTarget, rootKey, guards)
}

// localWholeCopyDeleteAllowed reports whether absTarget may be removed in its
// entirety by a whole-copy delete ("Delete entire copy"). It is the strict
// superset of the listing predicate: the target must still be an advertised
// leaf model (localLeafRepoIsDeletable), AND it must enclose no configured
// guard at all.
//
// localLeafRepoIsDeletable deliberately permits a configured root nested inside
// the candidate (an enclosure that owns its own weight file is a real repo for
// discovery, and upstream #66 requires it to be listed). A whole-copy
// RemoveAll of that enclosure would also destroy the nested configured root, so
// delete must additionally reject any target that strictly encloses or equals a
// configured guard. The cache boundary is already covered (it is a guard) and
// the existing descendant/equal checks still apply through the listing
// predicate.
func localWholeCopyDeleteAllowed(absTarget, rootKey string, guards, excluded []string, allowWeightless bool) bool {
	if !localLeafRepoIsDeletable(absTarget, rootKey, guards, excluded, allowWeightless) {
		return false
	}
	targetKey := pathIdentityKey(absTarget)
	for _, guard := range guards {
		guardKey := pathIdentityKey(guard)
		// Equal guards are already rejected by the listing predicate, but keep
		// the explicit check so the strictness does not depend on guard order.
		if targetKey == guardKey || pathIsPrefix(targetKey, guardKey) {
			return false
		}
	}
	return true
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
		// Keep the key present (as []) in list responses too, so a current
		// server is distinguishable from one that omits copies.
		Copies:        []CacheCopy{},
		Quantizations: ggufMeta.Quantizations,
		HasMMProj:     ggufMeta.HasMMProj,
		MMProjFiles:   ggufMeta.MMProjFiles,
		Capabilities:  ggufMeta.Capabilities,
	}, nil
}

// scanLocalCachedRepos walks every local cache root (Friendly view,
// localDir, localScanDirs, raw cache) and returns the list of
// discovered repos. When includeFiles is true, each result includes
// the per-file list. Repos that look like HF-cache internals
// (hub/blobs/snapshots/refs) are skipped.
func scanLocalCachedRepos(cacheDir, localDir string, localScanDirs []string, downloadRoutes map[string]string, includeFiles bool) ([]CachedRepoInfo, error) {
	var repos []CachedRepoInfo
	roots := localCacheRoots(cacheDir, localDir, localScanDirs, downloadRoutes)
	guards := localUnitGuards(cacheDir, localDir, localScanDirs, downloadRoutes)
	for _, root := range roots {
		excluded := root.excludedSubroots(roots)
		if _, err := os.Stat(root.Path); os.IsNotExist(err) {
			continue
		}

		owners, err := os.ReadDir(root.Path)
		if err != nil {
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
				continue
			}
			for _, modelEntry := range models {
				if !modelEntry.IsDir() {
					continue
				}
				name := modelEntry.Name()
				repoDir := filepath.Join(ownerDir, name)
				// Do not advertise a repo reached through a symlinked
				// intermediate component (e.g. models/<owner> -> models/other)
				// or a symlinked leaf: it is an alias of another repo, not this
				// one, and must not be offered as a deletable copy.
				if localRepoDirAliased(root.Path, repoDir) {
					continue
				}
				if !hasLocalWeightFile(repoDir, excluded) {
					continue
				}
				// Advertise only leaf Local model directories: never the cache
				// dir, a friendly namespace, a configured root, or a container
				// of nested model repos. Friendly-view/raw-cache entries are
				// not Local-deletable units, so their listing is unchanged.
				if root.Source == cacheSourceLocal && !localLeafRepoIsDeletable(repoDir, pathIdentityKey(root.Path), guards, excluded, false) {
					continue
				}
				source := localRepoSource(cacheDir, owner, name, repoDir, root.Source)
				repo, err := buildLocalCacheRepo(owner, name, repoDir, source, includeFiles, excluded)
				if err == nil {
					repos = append(repos, *repo)
				}
			}
		}
	}
	return repos, nil
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
	guards := localUnitGuards(cacheDir, localDir, localScanDirs, downloadRoutes)
	for _, root := range roots {
		// Mirror the owner exclusion scanLocalCachedRepos and
		// localCopyCandidates apply for a raw cache root, so a single-repo
		// lookup agrees with the list and the delete routes: an owner such as
		// hub/models/datasets is HF-cache internals, not an independent Local
		// repo, and must not resolve to one.
		if root.skipsOwner(parts[0]) {
			continue
		}
		repoDir := filepath.Join(root.Path, parts[0], parts[1])
		excluded := root.excludedSubroots(roots)
		// A repo reached through a symlinked intermediate component (or a
		// symlinked leaf) is another repo's alias; do not resolve this ID to it.
		if localRepoDirAliased(root.Path, repoDir) {
			continue
		}
		if !hasLocalWeightFile(repoDir, excluded) {
			continue
		}
		// Only a leaf Local model directory resolves: a container of nested
		// model repos, the cache dir, a friendly namespace, or a configured
		// root is not this repo. Friendly-view/raw-cache entries are not
		// Local-deletable units, so their resolution is unchanged.
		if root.Source == cacheSourceLocal && !localLeafRepoIsDeletable(repoDir, pathIdentityKey(root.Path), guards, excluded, false) {
			continue
		}
		return buildLocalCacheRepo(parts[0], parts[1], repoDir, localRepoSource(cacheDir, parts[0], parts[1], repoDir, root.Source), includeFiles, excluded)
	}
	return nil, os.ErrNotExist
}

// handleCacheList lists all cached repositories with rich metadata.
func (s *Server) handleCacheList(w http.ResponseWriter, r *http.Request) {
	cfg := s.snapshotConfig()
	cacheDir := cfg.CacheDir
	if cacheDir == "" {
		cacheDir = hfdownloader.DefaultCacheDir()
	}

	// Get query params
	repoType := r.URL.Query().Get("type") // "model" or "dataset"
	search := strings.ToLower(r.URL.Query().Get("search"))

	cache := hfdownloader.NewHFCache(cacheDir, 0)
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
		if m, err := hfdownloader.ReadManifest(manifestPath); err == nil {
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
			// Keep the key present (as []) so a current server is
			// distinguishable from one that omits copies.
			Copies:        []CacheCopy{},
			Quantizations: ggufMeta.Quantizations,
			HasMMProj:     ggufMeta.HasMMProj,
			Capabilities:  ggufMeta.Capabilities,
		}
		repos = append(repos, repo)
		seenRepos[strings.ToLower(rdType+":"+repoID)] = true
	}

	localRepos, _ := scanLocalCachedRepos(cacheDir, cfg.LocalDir, cfg.LocalScanDirs, cfg.DownloadRoutes, false)
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

	writeJSON(w, http.StatusOK, map[string]any{
		"repos":    repos,
		"stats":    stats,
		"cacheDir": cacheDir,
	})
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

	cfg := s.snapshotConfig()
	cacheDir := cfg.CacheDir
	if cacheDir == "" {
		cacheDir = hfdownloader.DefaultCacheDir()
	}

	cache := hfdownloader.NewHFCache(cacheDir, 0)

	// Try as model first
	repoDir, err := cache.Repo(repo, hfdownloader.RepoTypeModel)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid repository format", err.Error())
		return
	}

	// Check if the path exists
	if _, err := os.Stat(repoDir.Path()); os.IsNotExist(err) {
		// Try as dataset
		repoDir, _ = cache.Repo(repo, hfdownloader.RepoTypeDataset)
		if _, err := os.Stat(repoDir.Path()); os.IsNotExist(err) {
			localRepo, localErr := findLocalCachedRepo(cacheDir, cfg.LocalDir, cfg.LocalScanDirs, cfg.DownloadRoutes, repo, true)
			if localErr == nil {
				localRepo.Copies = enumerateCacheCopies(cacheDir, cfg, repo, hfdownloader.RepoTypeModel)
				writeJSON(w, http.StatusOK, localRepo)
				return
			}
			writeError(w, http.StatusNotFound, "Repository not found in cache", "")
			return
		}
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
	if m, err := hfdownloader.ReadManifest(manifestPath); err == nil {
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
		Source:         cacheSourceHFCache,
		Copies:         enumerateCacheCopies(cacheDir, cfg, repo, repoDir.Type()),
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
	cacheDir := cfg.CacheDir
	if cacheDir == "" {
		cacheDir = hfdownloader.DefaultCacheDir()
	}

	// Parse options from request body
	var req struct {
		Clean bool `json:"clean"` // Remove orphaned symlinks
	}
	_ = readJSON(r, &req) // Ignore errors, use defaults

	cache := hfdownloader.NewHFCache(cacheDir, hfdownloader.DefaultStaleTimeout)

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
	cacheDir := cfg.CacheDir
	if cacheDir == "" {
		cacheDir = hfdownloader.DefaultCacheDir()
	}

	cache := hfdownloader.NewHFCache(cacheDir, hfdownloader.DefaultStaleTimeout)

	// Find the repo directory
	repoDir, err := cache.Repo(repo, repoType)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid repository ID", err.Error())
		return
	}

	owner, name := parts[0], parts[1]

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

	// Validate the source selector BEFORE any delete branch runs, including the
	// exact-path branch. An unknown non-empty label is a caller error and must
	// not fall through to a different copy's deletion merely because a valid
	// path was also supplied. Known labels keep the existing behavior: when a
	// path is supplied it selects the actual copy (valid-path precedence), and
	// the known source label does not have to equal the matched copy's source.
	source := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("source")))
	switch source {
	case "local", "friendly view", "", "hf cache":
		// Valid selectors; routed below.
	default:
		writeError(w, http.StatusBadRequest, "Invalid source", "source must be one of: HF cache, Friendly view, Local")
		return
	}

	// Variant deletes are a distinct, selective operation: remove only the
	// server-computed file set of one artifact instead of the whole copy. The
	// variant token is optional and orthogonal to path/source.
	variant := strings.TrimSpace(r.URL.Query().Get("variant"))
	pathParam := strings.TrimSpace(r.URL.Query().Get("path"))
	if variant != "" {
		s.handleCacheDeleteVariant(w, cacheDir, cfg, repo, repoType, repoDir, owner, name, source, pathParam, variant)
		return
	}

	// When the caller names an exact copy path, delete only that copy. The path
	// is matched against the same server-computed candidate set handleCacheInfo
	// lists, so a caller can never make the server delete an arbitrary path.
	if pathParam != "" {
		copySource, localCand, ok := s.matchCacheCopyPath(cacheDir, cfg, repo, repoType, pathParam)
		if !ok {
			writeError(w, http.StatusBadRequest, "Invalid path", "Path is not a known copy of this repository")
			return
		}
		switch copySource {
		case cacheSourceHFCache, cacheSourceFriendlyView:
			s.deleteFriendlyViewRepo(w, repoDir, repo, absCacheDir, absCacheDirWithSep, repoType)
			return
		case cacheSourceLocal:
			s.deleteLocalCopy(w, cacheDir, cfg, repo, repoType, owner, name, localCand)
			return
		}
		writeError(w, http.StatusBadRequest, "Invalid path", "Unsupported copy source")
		return
	}

	// Route by the cache-list source label. Local roots are model-management
	// areas, so deleting there removes the real <root>/<owner>/<name> folder.
	// The empty label and "hf cache" keep the legacy HF-cache-first behavior;
	// an empty label additionally falls back to the local roots so cached
	// clients that omit the param can still delete local repos.
	switch source {
	case "local":
		s.deleteLocalRootRepo(w, cacheDir, cfg, owner, name, repo, repoType)
		return
	case "friendly view":
		s.deleteFriendlyViewRepo(w, repoDir, repo, absCacheDir, absCacheDirWithSep, repoType)
		return
	case "", "hf cache":
		// Legacy HF-cache-first delete below. An empty label also reaches the
		// local-roots fallback at the end of this handler; "hf cache" does not.
	}

	absHubPath, err := filepath.Abs(hubPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to resolve path", err.Error())
		return
	}

	// Security Layer 6: Check if path exists and is not a symlink (TOCTOU mitigation)
	hubInfo, err := os.Lstat(absHubPath)
	if err != nil && !os.IsNotExist(err) {
		writeError(w, http.StatusInternalServerError, "Failed to check path", err.Error())
		return
	}

	if err == nil {
		// Security Layer 7: Reject if the hub path itself is a symlink (symlink attack prevention)
		if hubInfo.Mode()&os.ModeSymlink != 0 {
			writeError(w, http.StatusBadRequest, "Invalid path", "Cannot delete symlinked directories")
			return
		}

		// Security Layers 8-10: verify the path is within the cache and follows
		// the expected HF cache structure, including resolved symlink targets.
		if verr := validateHubDeletePath(absHubPath, absCacheDir, absCacheDirWithSep, repoType); verr != nil {
			writeCacheDeleteError(w, verr)
			return
		}

		// All security checks passed - proceed with deletion
		if err := os.RemoveAll(absHubPath); err != nil {
			writeError(w, http.StatusInternalServerError, "Failed to delete cache", err.Error())
			return
		}

		// Clean up the friendly-view projection. This only removes a genuine
		// whole-folder projection of this exact repo; a real folder or a link
		// into another repo is preserved and surfaced as incomplete cleanup so
		// the UI can warn. The hub delete already succeeded, so the primary
		// success stays true.
		var warnings []string
		if warning := s.cleanupFriendlyView(repoDir, absCacheDirWithSep); warning != "" {
			warnings = append(warnings, warning)
		}

		writeJSON(w, http.StatusOK, cacheDeleteSuccess(repo, fmt.Sprintf("Deleted %s from cache", repo), warnings))
		return
	}

	// Hub directory is absent. A friendly-view entry can still exist as a
	// genuine orphan (a directory of symlinks whose hub target is gone); delete
	// it rather than reporting "not found". A folder that is not a proven
	// projection of this exact repo is preserved instead: only shared storage
	// proven from the filesystem may be removed here.
	if friendlyPath != "" {
		switch repoDir.FriendlyState() {
		case hfdownloader.FriendlyProjection:
			if finfo, ferr := os.Lstat(friendlyPath); ferr == nil {
				if finfo.Mode()&os.ModeSymlink != 0 {
					writeError(w, http.StatusBadRequest, "Invalid path", "Cannot delete symlinked directories")
					return
				}
				if derr := safeDeleteFriendlyPath(friendlyPath, absCacheDirWithSep); derr != nil {
					writeError(w, http.StatusInternalServerError, "Failed to delete cache", derr.Error())
					return
				}
				writeJSON(w, http.StatusOK, cacheDeleteSuccess(repo, fmt.Sprintf("Deleted %s from cache", repo), nil))
				return
			} else if !os.IsNotExist(ferr) {
				writeError(w, http.StatusInternalServerError, "Failed to check path", ferr.Error())
				return
			}
		case hfdownloader.FriendlyNotProjection:
			// A real folder (or a link into another repo) is not the friendly
			// copy of this repo; fall through so nothing unrelated is removed.
		}
	}

	// Nothing found in the HF cache. For an empty source, fall back to
	// the local roots so older clients can still delete those entries.
	if source == "" {
		s.deleteLocalRootRepo(w, cacheDir, cfg, owner, name, repo, repoType)
		return
	}

	writeError(w, http.StatusNotFound, "Repository not found in cache", repo)
}

// cacheDeleteError describes a failed cache-delete attempt together with the
// HTTP status the API should report for it.
type cacheDeleteError struct {
	status int
	title  string
	detail string
}

func (e *cacheDeleteError) Error() string { return e.detail }

// writeCacheDeleteError maps a validation error from the cache-delete path to
// the corresponding JSON HTTP error. Unknown error types become a 500.
func writeCacheDeleteError(w http.ResponseWriter, err error) {
	var cde *cacheDeleteError
	if errors.As(err, &cde) {
		writeError(w, cde.status, cde.title, cde.detail)
		return
	}
	writeError(w, http.StatusInternalServerError, "Failed to delete cache", err.Error())
}

// validateHubDeletePath applies the HF-cache containment and structure checks
// (security layers 8-10) to an already-existing hub repo directory. It returns
// nil when the path is safe to delete.
func validateHubDeletePath(absHubPath, absCacheDir, absCacheDirWithSep string, repoType hfdownloader.RepoType) error {
	// Security Layer 8: Verify path is within cache (using cleaned absolute path)
	if !strings.HasPrefix(absHubPath+string(filepath.Separator), absCacheDirWithSep) {
		return &cacheDeleteError{http.StatusBadRequest, "Invalid path", "Path outside cache directory"}
	}

	// Security Layer 9: Verify path follows expected HF cache structure
	// Must be: {cacheDir}/hub/{models|datasets}--{owner}--{name}
	expectedPrefix := "models--"
	if repoType == hfdownloader.RepoTypeDataset {
		expectedPrefix = "datasets--"
	}
	hubSubpath, err := filepath.Rel(absCacheDir, absHubPath)
	if err != nil || !strings.HasPrefix(hubSubpath, filepath.Join("hub", expectedPrefix)) {
		return &cacheDeleteError{http.StatusBadRequest, "Invalid path", "Path does not match expected cache structure"}
	}

	// Security Layer 10: Resolve symlinks to verify final destination is also
	// within cache. This catches symlinks inside the directory structure.
	realHubPath, err := filepath.EvalSymlinks(absHubPath)
	if err == nil && realHubPath != absHubPath {
		if !strings.HasPrefix(realHubPath+string(filepath.Separator), absCacheDirWithSep) {
			return &cacheDeleteError{http.StatusBadRequest, "Invalid path", "Resolved path outside cache directory"}
		}
	}
	return nil
}

// cleanupFriendlyView removes the friendly-view directory for repoDir only when
// it is a genuine whole-folder projection of this exact repo (proven from the
// filesystem). It returns a warning string when cleanup did not complete; the
// caller keeps the primary success and surfaces the warning. A real folder or a
// link into another repo is preserved, never deleted.
func (s *Server) cleanupFriendlyView(repoDir *hfdownloader.RepoDir, absCacheDirWithSep string) string {
	friendlyPath := repoDir.FriendlyPath()
	if friendlyPath == "" {
		return ""
	}
	switch repoDir.FriendlyState() {
	case hfdownloader.FriendlyProjection:
		// Proven shared storage; safe to remove below.
	case hfdownloader.FriendlyNotProjection:
		return fmt.Sprintf("friendly view %s is not a whole-folder projection of this repository; left in place", friendlyPath)
	default:
		return ""
	}

	if s.deleteStepHook != nil {
		if err := s.deleteStepHook("hf:friendly-cleanup"); err != nil {
			log.Printf("warning: could not delete friendly view %s: %v", friendlyPath, err)
			return fmt.Sprintf("friendly view cleanup failed: %v", err)
		}
	}
	if err := safeDeleteFriendlyPath(friendlyPath, absCacheDirWithSep); err != nil {
		log.Printf("warning: could not delete friendly view %s: %v", friendlyPath, err)
		return fmt.Sprintf("friendly view cleanup failed: %v", err)
	}
	return ""
}

// deleteFriendlyViewRepo deletes the friendly-view projection of a repo (the
// <cache>/models/<owner>/<name> directory that holds symlinks), plus the HF hub
// directory when it still exists.
//
// The friendly directory is only removed when it is a proven whole-folder
// projection of this exact repo. When the hub was deleted but the friendly
// folder is a real folder or links into another repo, it is preserved and the
// success response carries cleanupIncomplete so the UI can warn. When neither
// exists, the request is a 404; when only a non-projection exists, it is a 400.
func (s *Server) deleteFriendlyViewRepo(w http.ResponseWriter, repoDir *hfdownloader.RepoDir, repo, absCacheDir, absCacheDirWithSep string, repoType hfdownloader.RepoType) {
	friendlyPath := repoDir.FriendlyPath()
	hubPath := repoDir.Path()

	deleted := false
	var warnings []string

	// Remove the hub directory first when present, using the same HF-cache
	// containment checks as the default path.
	absHubPath, err := filepath.Abs(hubPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to resolve path", err.Error())
		return
	}
	if hubInfo, herr := os.Lstat(absHubPath); herr == nil {
		if hubInfo.Mode()&os.ModeSymlink != 0 {
			writeError(w, http.StatusBadRequest, "Invalid path", "Cannot delete symlinked directories")
			return
		}
		if verr := validateHubDeletePath(absHubPath, absCacheDir, absCacheDirWithSep, repoType); verr != nil {
			writeCacheDeleteError(w, verr)
			return
		}
		if rerr := os.RemoveAll(absHubPath); rerr != nil {
			writeError(w, http.StatusInternalServerError, "Failed to delete cache", rerr.Error())
			return
		}
		deleted = true
	} else if !os.IsNotExist(herr) {
		writeError(w, http.StatusInternalServerError, "Failed to check path", herr.Error())
		return
	}

	if friendlyPath != "" {
		switch repoDir.FriendlyState() {
		case hfdownloader.FriendlyProjection:
			if s.deleteStepHook != nil {
				if herr := s.deleteStepHook("hf:friendly-cleanup"); herr != nil {
					if !deleted {
						writeError(w, http.StatusInternalServerError, "Failed to delete cache", herr.Error())
						return
					}
					log.Printf("warning: could not delete friendly view %s: %v", friendlyPath, herr)
					warnings = append(warnings, fmt.Sprintf("friendly view cleanup failed: %v", herr))
					break
				}
			}
			if derr := safeDeleteFriendlyPath(friendlyPath, absCacheDirWithSep); derr != nil {
				// The hub directory was already deleted, so the delete itself
				// succeeded; report the friendly-view cleanup failure as
				// incomplete instead of failing the whole request. Without a
				// hub delete this is still the primary operation and its
				// failure is reported.
				if !deleted {
					writeError(w, http.StatusInternalServerError, "Failed to delete cache", derr.Error())
					return
				}
				log.Printf("warning: could not delete friendly view %s: %v", friendlyPath, derr)
				warnings = append(warnings, fmt.Sprintf("friendly view cleanup failed: %v", derr))
			} else {
				deleted = true
			}
		case hfdownloader.FriendlyNotProjection:
			// Never delete a real folder or a link into another repo as if it
			// were this repo's friendly projection.
			if !deleted {
				writeError(w, http.StatusBadRequest, "Invalid path", "Friendly view path is not a projection of this repository")
				return
			}
			warnings = append(warnings, "friendly view is not a whole-folder projection of this repository; left in place")
		}
	}

	if !deleted {
		writeError(w, http.StatusNotFound, "Repository not found in cache", repo)
		return
	}

	writeJSON(w, http.StatusOK, cacheDeleteSuccess(repo, fmt.Sprintf("Deleted %s from cache", repo), warnings))
}

// localCopyCandidate is one deletable owner/name folder under a local root.
type localCopyCandidate struct {
	Root    string // configured root path, e.g. localDir
	RepoDir string // exact deletable <root>/<owner>/<name> path
	// Partial marks a retained partial-delete target whose remainder may no
	// longer directly own a weight file. Only such a retry may skip the
	// direct-weight requirement; a fresh candidate is always weight-bearing.
	Partial bool
}

// localAuthorityKeys returns the physical keys of roots that carry Local
// delete authority: the explicitly configured localDir, the scan dirs, every
// download-route destination, and the raw cache dir itself (whose non-special
// owners are user-managed folders). It deliberately excludes the implicit
// friendly namespace <cache>/models unless that exact path was also explicitly
// configured, so an implicit friendly projection is never treated as
// independent Local storage. Deduping by key keeps a symlinked root alias and
// its real path as one authority entry.
func localAuthorityKeys(cacheDir, localDir string, localScanDirs []string, downloadRoutes map[string]string) map[string]bool {
	keys := make(map[string]bool)
	if cacheDir != "" {
		keys[pathIdentityKey(cacheDir)] = true
	}
	if localDir != "" {
		keys[pathIdentityKey(localDir)] = true
	}
	for _, dir := range localScanDirs {
		if dir != "" {
			keys[pathIdentityKey(dir)] = true
		}
	}
	for _, dir := range routeDirs(downloadRoutes) {
		if dir != "" {
			keys[pathIdentityKey(dir)] = true
		}
	}
	return keys
}

// localCopyCandidates returns the deletable <root>/<owner>/<name> path for each
// local cache root that is a genuine LEAF model directory, in the same order
// the cache list uses. HF-cache internals under a raw cache root
// (hub/models/datasets/blobs/snapshots/refs) are skipped so the shown entry and
// the deleted entry agree. A friendly-view projection of this exact repo is not
// a Local copy: it is the same storage as the hub. A real folder that happens
// to live under the friendly namespace (e.g. an explicit Local root configured
// at <cache>/models) is still listed, because it is independent storage.
//
// A candidate must pass localLeafRepoIsDeletable against the cache dir,
// friendly namespaces, and every configured root, so the details modal never
// advertises the cache dir, a friendly namespace, a configured root, an
// overlapping root's container, or a container of nested model repos.
func localCopyCandidates(cacheDir, localDir string, localScanDirs []string, downloadRoutes map[string]string, owner, name string) []localCopyCandidate {
	var candidates []localCopyCandidate
	seen := make(map[string]bool)
	roots := localCacheRoots(cacheDir, localDir, localScanDirs, downloadRoutes)
	guards := localUnitGuards(cacheDir, localDir, localScanDirs, downloadRoutes)
	authority := localAuthorityKeys(cacheDir, localDir, localScanDirs, downloadRoutes)

	// Resolve whether <cache>/models/<owner>/<name> is a genuine projection of
	// this repo. Locals are models only, so the model friendly path applies.
	projectionKey := ""
	if cache := hfdownloader.NewHFCache(cacheDir, 0); cache != nil {
		if rd, err := cache.Repo(owner+"/"+name, hfdownloader.RepoTypeModel); err == nil &&
			rd.FriendlyState() == hfdownloader.FriendlyProjection {
			projectionKey = pathIdentityKey(rd.FriendlyPath())
		}
	}

	for _, root := range roots {
		// Only roots carrying Local delete authority are deletable as Local
		// storage. An implicit friendly namespace alone is not Local storage; it
		// only becomes one when the same physical directory is also explicitly
		// configured as a Local root (localDir/scanDir/route). Delete authority
		// is decided by path, not by the root's display Source, because an
		// explicitly configured <cache>/models keeps the first-wins "Friendly
		// view" display label while still being real Local storage.
		if !authority[pathIdentityKey(root.Path)] {
			continue
		}
		if root.SkipSpecial {
			switch strings.ToLower(owner) {
			case "hub", "models", "datasets", "blobs", "snapshots", "refs":
				continue
			}
		}
		repoDir := filepath.Join(root.Path, owner, name)
		excluded := root.excludedSubroots(roots)
		// Mirror the delete-side check: a candidate reached through a symlinked
		// or missing intermediate component, or a symlinked leaf, would be
		// advertised but never deletable, so skip it to keep the listed set
		// equal to the deletable set.
		if localRepoDirAliased(root.Path, repoDir) {
			continue
		}
		// A genuine friendly projection of this exact repo is the same storage
		// as the hub, not an independent Local copy.
		if projectionKey != "" && pathIdentityKey(repoDir) == projectionKey {
			continue
		}
		if !hasLocalWeightFile(repoDir, excluded) {
			continue
		}
		// The advertised entry must be a leaf repo, not the cache dir, a
		// friendly namespace, a configured root, or a container of nested
		// model repos. Applying the same predicate as the delete side keeps the
		// listed set equal to the deletable set.
		if !localLeafRepoIsDeletable(repoDir, pathIdentityKey(root.Path), guards, excluded, false) {
			continue
		}
		key := pathIdentityKey(repoDir)
		if seen[key] {
			continue
		}
		seen[key] = true
		candidates = append(candidates, localCopyCandidate{Root: root.Path, RepoDir: repoDir})
	}
	return candidates
}

// configuredLocalRoots returns the Local-authority roots currently in effect
// (localDir, the scan dirs, each download-route destination, and the raw cache
// dir), deduped by physical identity. This is exactly the set
// localCopyCandidates can resolve a deletable target from, so retained
// partial-delete evidence is validated against the same authorization. The
// implicit friendly namespace is included only when explicitly configured.
func configuredLocalRoots(cacheDir, localDir string, localScanDirs []string, downloadRoutes map[string]string) []string {
	var roots []string
	seen := make(map[string]bool)
	authority := localAuthorityKeys(cacheDir, localDir, localScanDirs, downloadRoutes)
	for _, root := range localCacheRoots(cacheDir, localDir, localScanDirs, downloadRoutes) {
		key := pathIdentityKey(root.Path)
		if seen[key] {
			continue
		}
		// Include only roots that are explicitly configured (or the raw cache
		// dir); the implicit friendly namespace is not Local authority.
		if !authority[key] {
			continue
		}
		seen[key] = true
		roots = append(roots, root.Path)
	}
	return roots
}

// localDeleteEvidence records that a validated Local target began deletion and
// may have been left partially deleted. It is deliberately in-memory only: the
// continuation horizon is the current server process, and a changed root or
// repository invalidates the record because it is keyed by all three.
type localDeleteEvidence struct {
	Root    string
	RepoDir string
}

// localDeleteEvidenceKey identifies one (type, root, owner, name) delete
// target. pathIdentityKey makes a symlinked root alias match its real path.
func localDeleteEvidenceKey(repoType hfdownloader.RepoType, root, owner, name string) string {
	return string(repoType) + "\x00" + pathIdentityKey(root) + "\x00" + owner + "\x00" + name
}

func (s *Server) rememberLocalDelete(key string, cand localCopyCandidate) {
	s.localDeleteMu.Lock()
	defer s.localDeleteMu.Unlock()
	if s.localDeleteEvidence == nil {
		s.localDeleteEvidence = make(map[string]localDeleteEvidence)
	}
	s.localDeleteEvidence[key] = localDeleteEvidence{Root: cand.Root, RepoDir: cand.RepoDir}
}

func (s *Server) forgetLocalDelete(key string) {
	s.localDeleteMu.Lock()
	defer s.localDeleteMu.Unlock()
	delete(s.localDeleteEvidence, key)
}

// retryableLocalTarget returns the exact target of a previous partially
// completed Local delete for this repo, when the owning root is still
// configured and the remainder still exists. It never redirects to a different
// copy: the target is matched only by the recorded root plus owner/name, and a
// changed root/config invalidates the record.
func (s *Server) retryableLocalTarget(cacheDir string, cfg Config, owner, name string, repoType hfdownloader.RepoType) (localCopyCandidate, bool) {
	if repoType != hfdownloader.RepoTypeModel {
		return localCopyCandidate{}, false
	}
	return s.matchLocalDeleteEvidence(cacheDir, cfg, owner, name, "")
}

// matchLocalDeleteEvidence looks up retained partial-delete evidence for a repo
// under the currently configured roots. When requestedAbs is non-empty the
// evidence must also name that exact absolute target (by-path retry); when it
// is empty any surviving remainder for the repo matches (no-path retry).
func (s *Server) matchLocalDeleteEvidence(cacheDir string, cfg Config, owner, name, requestedAbs string) (localCopyCandidate, bool) {
	roots := configuredLocalRoots(cacheDir, cfg.LocalDir, cfg.LocalScanDirs, cfg.DownloadRoutes)
	if len(roots) == 0 {
		return localCopyCandidate{}, false
	}
	s.localDeleteMu.Lock()
	defer s.localDeleteMu.Unlock()
	for _, root := range roots {
		key := localDeleteEvidenceKey(hfdownloader.RepoTypeModel, root, owner, name)
		ev, ok := s.localDeleteEvidence[key]
		if !ok {
			continue
		}
		if requestedAbs != "" {
			evAbs, aerr := filepath.Abs(ev.RepoDir)
			if aerr != nil || evAbs != requestedAbs {
				continue
			}
		}
		// The remainder must still physically exist, and must still validate
		// as the exact authorized target. A vanished remainder means there is
		// nothing left to retry.
		if _, err := os.Lstat(ev.RepoDir); err != nil {
			continue
		}
		// allowWeightless: a partial delete may have already removed the weight
		// file, leaving only the protected remainder. The retained target was
		// validated as a leaf when the delete began and is revalidated here for
		// guards/containment, so the weight requirement is the only part a
		// retry may skip.
		if _, err := resolveLocalDeleteTarget(cacheDir, cfg.LocalDir, cfg.LocalScanDirs, cfg.DownloadRoutes, ev.Root, owner, name, true); err != nil {
			continue
		}
		return localCopyCandidate{Root: ev.Root, RepoDir: ev.RepoDir, Partial: true}, true
	}
	return localCopyCandidate{}, false
}

// deleteLocalRootRepo deletes the exact <root>/<owner>/<name> folder for a repo
// stored under one of the local cache roots. Local entries are always models, so
// a non-model request type is reported as not found rather than deleting a
// folder it did not ask for. It skips friendly projections so a Local entry
// deletes real storage, and reports failures as 500. A previously started
// partial delete of the same target is retried within the current process even
// when weight-based discovery no longer recognizes the remainder.
func (s *Server) deleteLocalRootRepo(w http.ResponseWriter, cacheDir string, cfg Config, owner, name, repo string, repoType hfdownloader.RepoType) {
	if repoType != hfdownloader.RepoTypeModel {
		writeError(w, http.StatusNotFound, "Repository not found in cache", repo)
		return
	}
	candidates := localCopyCandidates(cacheDir, cfg.LocalDir, cfg.LocalScanDirs, cfg.DownloadRoutes, owner, name)
	if len(candidates) == 0 {
		if cand, ok := s.retryableLocalTarget(cacheDir, cfg, owner, name, repoType); ok {
			s.deleteLocalCopy(w, cacheDir, cfg, repo, repoType, owner, name, cand)
			return
		}
		writeError(w, http.StatusNotFound, "Repository not found in cache", repo)
		return
	}
	s.deleteLocalCopy(w, cacheDir, cfg, repo, repoType, owner, name, candidates[0])
}

// deleteLocalCopy deletes one specific local copy. Local entries are always
// models, so a non-model request type is reported as not found. cand.Partial
// marks a retained partial-delete retry whose remainder may no longer own a
// weight file; a fresh candidate is always revalidated as a weight-bearing
// leaf.
func (s *Server) deleteLocalCopy(w http.ResponseWriter, cacheDir string, cfg Config, repo string, repoType hfdownloader.RepoType, owner, name string, cand localCopyCandidate) {
	if repoType != hfdownloader.RepoTypeModel {
		writeError(w, http.StatusNotFound, "Repository not found in cache", repo)
		return
	}
	key := localDeleteEvidenceKey(repoType, cand.Root, owner, name)
	// Validate before recording anything, so a validation-only failure leaves
	// no stale evidence behind.
	absTarget, err := resolveLocalDeleteTarget(cacheDir, cfg.LocalDir, cfg.LocalScanDirs, cfg.DownloadRoutes, cand.Root, owner, name, cand.Partial)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to delete local folder", err.Error())
		return
	}
	// Record the validated target before mutating so a partial delete is
	// retryable within this process even though a failed RemoveAll leaves no
	// weight file behind for discovery.
	s.rememberLocalDelete(key, cand)
	if s.deleteStepHook != nil {
		if herr := s.deleteStepHook("local:before-remove"); herr != nil {
			writeError(w, http.StatusInternalServerError, "Failed to delete local folder", herr.Error())
			return
		}
	}
	if err := os.RemoveAll(absTarget); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to delete local folder", err.Error())
		return
	}
	s.forgetLocalDelete(key)
	writeJSON(w, http.StatusOK, cacheDeleteSuccess(repo, fmt.Sprintf("Deleted %s from disk", repo), nil))
}

// --- Selective (per-variant) delete ---
//
// A whole-copy delete (above) removes the entire <root>/<owner>/<name> folder.
// A variant delete removes exactly the server-computed file set of one artifact
// (e.g. a single GGUF quant), reusing the same selector semantics used at
// download time. The allowed tokens are recomputed server-side from the copy's
// own contents; a caller-supplied token that is not one of them is rejected
// before anything is touched.

// fileMatchesCacheVariant reports whether the base name of a GGUF file belongs
// to the artifact identified by variant. Matching is deliberately anchored to
// the file's own quantisation label (the exact inverse of how
// collectCacheGGUFMetadata derives the allowed tokens) so split shards
// (-00001-of-00002) and all files of one quant are selected together, while a
// different quant is never pulled in. The download selector
// hfdownloader.MatchesFilter is also consulted for segment-bounded matching.
// mmproj companions and non-GGUF files never match a quant variant.
func fileMatchesCacheVariant(base, variant string) bool {
	if isCacheMMProjFile(base) {
		return false
	}
	if !strings.EqualFold(filepath.Ext(base), ".gguf") {
		return false
	}
	if cacheQuantLabel(base) == variant {
		return true
	}
	return hfdownloader.MatchesFilter(base, variant, true)
}

// collectLocalVariantFiles returns the absolute paths of the GGUF files inside
// copyDir that belong to variant. It walks without following symlinks and skips
// subtrees owned by a nested configured root (excluded), so a variant delete
// never reaches outside the resolved copy.
func collectLocalVariantFiles(copyDir, variant string, excluded []string) []string {
	var files []string
	walkLocalCacheRepo(copyDir, excluded, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		// Never follow a symlinked file: it is an alias of other storage, not
		// this copy's bytes.
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		if !fileMatchesCacheVariant(info.Name(), variant) {
			return nil
		}
		files = append(files, path)
		return nil
	})
	return files
}

// collectLocalVariantTokens returns the distinct quantisation tokens a Local
// copy supports for selective deletion. It is the same detection used to
// advertise Quantizations, so the UI and the delete endpoint agree.
func collectLocalVariantTokens(copyDir string, excluded []string) []string {
	return collectCacheGGUFMetadata(copyDir, false, excluded).Quantizations
}

// readHFCopyManifest loads the hfd.yaml provenance for a friendly-copy path.
// Returns the parsed manifest and true when one exists.
func readHFCopyManifest(friendlyPath string) (*hfdownloader.DownloadManifest, bool) {
	if friendlyPath == "" {
		return nil, false
	}
	m, err := hfdownloader.ReadManifest(filepath.Join(friendlyPath, hfdownloader.ManifestFilename))
	if err != nil || m == nil || len(m.Files) == 0 {
		return nil, false
	}
	return m, true
}

// manifestVariantTokens returns the distinct quantisation tokens present in a
// manifest's file list. mmproj companions are skipped, matching the Local
// detection.
func manifestVariantTokens(m *hfdownloader.DownloadManifest) []string {
	seen := make(map[string]bool)
	var tokens []string
	for _, f := range m.Files {
		base := filepath.Base(f.Name)
		if isCacheMMProjFile(base) {
			continue
		}
		if q := cacheQuantLabel(base); q != "" && !seen[q] {
			seen[q] = true
			tokens = append(tokens, q)
		}
	}
	sort.Strings(tokens)
	return tokens
}

// localCopyExcludedSubroots returns the physical keys of nested configured
// roots owned by other roots, for the given copy root. Variant deletes use it
// so they never descend into a subtree another configured root owns.
func localCopyExcludedSubroots(cacheDir, localDir string, localScanDirs []string, downloadRoutes map[string]string, root string) []string {
	roots := localCacheRoots(cacheDir, localDir, localScanDirs, downloadRoutes)
	return localCacheRoot{Path: root}.excludedSubroots(roots)
}

// pruneEmptyDirs removes directories under root that became empty, bottom-up.
// Directories inside an excluded nested configured root are never visited, so
// pruning cannot disturb another root's subtree. The root itself is removed
// only when removeRoot is set and it is empty after pruning. Errors are
// ignored: pruning is best-effort cleanup and must never turn a successful file
// delete into a failure.
func pruneEmptyDirs(root string, excluded []string, removeRoot bool) {
	var dirs []string
	_ = walkLocalCacheRepo(root, excluded, func(path string, info os.FileInfo, err error) error {
		if err != nil || !info.IsDir() {
			return nil
		}
		dirs = append(dirs, path)
		return nil
	})
	// Deepest first so a parent can become empty once its children are gone.
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, dir := range dirs {
		if !removeRoot && pathIdentityKey(dir) == pathIdentityKey(root) {
			continue
		}
		if entries, err := os.ReadDir(dir); err == nil && len(entries) == 0 {
			_ = os.Remove(dir)
		}
	}
}

// deleteLocalVariant deletes exactly the files of variant inside copyDir,
// prunes directories that became empty, and reports the truthful counts. It
// returns false after writing an error response when variant is not one of the
// copy's detected tokens (deleting nothing).
func deleteLocalVariant(w http.ResponseWriter, repo, variant, copyDir string, excluded []string) {
	tokens := collectLocalVariantTokens(copyDir, excluded)
	if len(tokens) == 0 {
		// No GGUF artifacts detected: a Local non-GGUF folder without hfd.yaml
		// provenance cannot resolve an artifact to a file set, so only
		// entire-copy deletion is available.
		writeError(w, http.StatusBadRequest, "Selective delete unavailable", "selective delete needs provenance (no GGUF artifacts detected)")
		return
	}
	if !slicesContains(tokens, variant) {
		writeError(w, http.StatusBadRequest, "Unknown variant", "variant is not a detected artifact of this copy")
		return
	}
	files := collectLocalVariantFiles(copyDir, variant, excluded)
	if len(files) == 0 {
		writeError(w, http.StatusBadRequest, "Unknown variant", "no files match this variant in the copy")
		return
	}
	var failures []string
	var deleted int
	var deletedBytes int64
	for _, path := range files {
		info, err := os.Lstat(path)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", filepath.Base(path), err))
			continue
		}
		if err := os.Remove(path); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", filepath.Base(path), err))
			continue
		}
		deleted++
		deletedBytes += info.Size()
	}
	// Prune now-empty subdirectories; the copy root itself is removed only when
	// it is fully empty, so a copy holding other artifacts stays discoverable.
	pruneEmptyDirs(copyDir, excluded, true)
	resp := map[string]any{
		"success":      true,
		"message":      fmt.Sprintf("Deleted variant %s (%d files) from %s", variant, deleted, repo),
		"variant":      variant,
		"deletedFiles": deleted,
		"deletedBytes": deletedBytes,
	}
	if len(failures) > 0 {
		resp[cacheDeleteIncompleteKey] = true
		resp["cleanupWarnings"] = failures
	}
	writeJSON(w, http.StatusOK, resp)
}

// deleteHFVariant deletes the blob, snapshot links, and friendly-view link for
// each manifest entry of variant. It refuses when no usable provenance exists
// (the caller checks that first). Guarded so every path stays inside the hub
// or friendly copy root.
func deleteHFVariant(w http.ResponseWriter, repoDir *hfdownloader.RepoDir, repo, variant string, m *hfdownloader.DownloadManifest) {
	hubRoot, err := filepath.Abs(repoDir.Path())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to resolve path", err.Error())
		return
	}
	hubPrefix := hubRoot + string(filepath.Separator)
	friendlyPath := repoDir.FriendlyPath()
	var friendlyRoot, friendlyPrefix string
	if friendlyPath != "" {
		if abs, aerr := filepath.Abs(friendlyPath); aerr == nil {
			friendlyRoot = abs
			friendlyPrefix = abs + string(filepath.Separator)
		}
	}

	var failures []string
	var deleted int
	var deletedBytes int64
	snapshots, _ := repoDir.ListSnapshots()
	// Blobs referenced by entries that are NOT being deleted must be preserved
	// even if a deleted entry happens to share the same blob hash.
	keptBlobs := make(map[string]bool)
	for _, f := range m.Files {
		if fileMatchesCacheVariant(filepath.Base(f.Name), variant) {
			continue
		}
		if f.Blob != "" {
			keptBlobs[pathIdentityKey(filepath.Join(hubRoot, filepath.FromSlash(f.Blob)))] = true
		}
	}
	for _, f := range m.Files {
		base := filepath.Base(f.Name)
		if !fileMatchesCacheVariant(base, variant) {
			continue
		}
		// Blob file: manifest Blob is relative to the hub repo directory.
		if f.Blob != "" {
			blobPath, jerr := hfdownloader.SafeJoin(hubRoot, filepath.FromSlash(f.Blob))
			if jerr != nil || (blobPath != hubRoot && !strings.HasPrefix(blobPath+string(filepath.Separator), hubPrefix)) {
				failures = append(failures, fmt.Sprintf("%s: unsafe blob path", base))
			} else if keptBlobs[pathIdentityKey(blobPath)] {
				// Shared with a kept entry; leave it in place.
			} else if info, serr := os.Lstat(blobPath); serr == nil && info.Mode()&os.ModeSymlink == 0 {
				if rerr := os.Remove(blobPath); rerr != nil {
					failures = append(failures, fmt.Sprintf("%s: %v", base, rerr))
				} else {
					deleted++
					deletedBytes += info.Size()
				}
			}
		}
		// Snapshot links named after the original file.
		for _, commit := range snapshots {
			snapshotDir, derr := repoDir.SnapshotDir(commit)
			if derr != nil {
				continue
			}
			link := filepath.Join(snapshotDir, filepath.FromSlash(f.Name))
			if linkAbs, aerr := filepath.Abs(link); aerr != nil || !strings.HasPrefix(linkAbs+string(filepath.Separator), hubPrefix) {
				continue
			}
			if info, lerr := os.Lstat(link); lerr == nil && info.Mode()&os.ModeSymlink != 0 {
				_ = os.Remove(link)
			}
		}
		// Friendly-view link for the same original file.
		if friendlyRoot != "" {
			link := filepath.Join(friendlyRoot, filepath.FromSlash(f.Name))
			if linkAbs, aerr := filepath.Abs(link); aerr == nil &&
				strings.HasPrefix(linkAbs+string(filepath.Separator), friendlyPrefix) {
				if info, lerr := os.Lstat(link); lerr == nil && info.Mode()&os.ModeSymlink != 0 {
					_ = os.Remove(link)
				}
			}
		}
	}
	pruneEmptyDirs(hubRoot, nil, false)
	if friendlyRoot != "" {
		pruneEmptyDirs(friendlyRoot, nil, false)
	}
	resp := map[string]any{
		"success":      true,
		"message":      fmt.Sprintf("Deleted variant %s (%d files) from %s", variant, deleted, repo),
		"variant":      variant,
		"deletedFiles": deleted,
		"deletedBytes": deletedBytes,
	}
	if len(failures) > 0 {
		resp[cacheDeleteIncompleteKey] = true
		resp["cleanupWarnings"] = failures
	}
	writeJSON(w, http.StatusOK, resp)
}

// slicesContains reports whether ss contains want.
func slicesContains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// handleCacheDeleteVariant resolves the target copy and performs a selective
// variant delete. The variant token must be one the copy itself advertises
// (detected GGUF quantisation for Local, or a manifest filename token for
// HF/friendly); anything else is a 400 that deletes nothing. Local non-GGUF
// copies without provenance cannot support selective delete and are rejected.
func (s *Server) handleCacheDeleteVariant(w http.ResponseWriter, cacheDir string, cfg Config, repo string, repoType hfdownloader.RepoType, repoDir *hfdownloader.RepoDir, owner, name, source, pathParam, variant string) {
	// Resolve which copy the variant applies to, using the same server-computed
	// candidate sets as whole-copy delete. A caller-supplied path must match one
	// of them; without a path, route by source (Local first for an empty or
	// local label, otherwise the HF/friendly copy).
	resolvedSource := source
	localCand := localCopyCandidate{}
	haveLocal := false
	if pathParam != "" {
		cs, cand, ok := s.matchCacheCopyPath(cacheDir, cfg, repo, repoType, pathParam)
		if !ok {
			writeError(w, http.StatusBadRequest, "Invalid path", "Path is not a known copy of this repository")
			return
		}
		resolvedSource = cs
		localCand = cand
		haveLocal = cs == cacheSourceLocal
	} else {
		hasManifest := false
		if m, ok := readHFCopyManifest(repoDir.FriendlyPath()); ok && slicesContains(manifestVariantTokens(m), variant) {
			hasManifest = true
		}
		localCandidates := localCopyCandidates(cacheDir, cfg.LocalDir, cfg.LocalScanDirs, cfg.DownloadRoutes, owner, name)
		switch source {
		case "local":
			if len(localCandidates) > 0 {
				localCand = localCandidates[0]
				haveLocal = true
				resolvedSource = cacheSourceLocal
			} else {
				writeError(w, http.StatusNotFound, "Repository not found in cache", repo)
				return
			}
		case "friendly view", "hf cache":
			resolvedSource = cacheSourceHFCache
		default: // ""
			// Empty source prefers the copy that actually carries the artifact:
			// the HF/friendly manifest when it knows the variant, else Local.
			if hasManifest {
				resolvedSource = cacheSourceHFCache
			} else if len(localCandidates) > 0 {
				localCand = localCandidates[0]
				haveLocal = true
				resolvedSource = cacheSourceLocal
			} else {
				resolvedSource = cacheSourceHFCache
			}
		}
	}

	switch resolvedSource {
	case cacheSourceLocal:
		if !haveLocal {
			writeError(w, http.StatusNotFound, "Repository not found in cache", repo)
			return
		}
		// Revalidate the copy as an authorized target before touching any file.
		// A variant delete removes only matched files, never the container, so
		// the listing-level predicate is the correct authorization here.
		absTarget, err := resolveLocalVariantTarget(cacheDir, cfg.LocalDir, cfg.LocalScanDirs, cfg.DownloadRoutes, localCand.Root, owner, name)
		if err != nil {
			writeError(w, http.StatusBadRequest, "Invalid variant target", err.Error())
			return
		}
		excluded := localCopyExcludedSubroots(cacheDir, cfg.LocalDir, cfg.LocalScanDirs, cfg.DownloadRoutes, localCand.Root)
		deleteLocalVariant(w, repo, variant, absTarget, excluded)
		return
	case cacheSourceHFCache, cacheSourceFriendlyView:
		m, ok := readHFCopyManifest(repoDir.FriendlyPath())
		if !ok {
			writeError(w, http.StatusBadRequest, "Selective delete unavailable", "selective delete needs provenance (hfd.yaml)")
			return
		}
		if !slicesContains(manifestVariantTokens(m), variant) {
			writeError(w, http.StatusBadRequest, "Unknown variant", "variant is not a detected artifact of this copy")
			return
		}
		// Apply the same hub containment check as the whole-copy HF/friendly
		// route: a hub path outside the configured cache dir is not a deletable
		// copy (e.g. an HF_HUB_CACHE override), so selective delete must refuse
		// it too rather than operating on files outside the configured cache.
		if absCacheDir, aerr := filepath.Abs(cacheDir); aerr == nil {
			absHubPath, herr := filepath.Abs(repoDir.Path())
			if herr != nil {
				writeError(w, http.StatusInternalServerError, "Failed to resolve path", herr.Error())
				return
			}
			if verr := validateHubDeletePath(absHubPath, absCacheDir, absCacheDir+string(filepath.Separator), repoType); verr != nil {
				writeCacheDeleteError(w, verr)
				return
			}
		}
		deleteHFVariant(w, repoDir, repo, variant, m)
		return
	}
	writeError(w, http.StatusBadRequest, "Selective delete unavailable", "this copy does not support selective delete")
}

// cacheCopyUsage returns the total bytes stored under dir and the number of
// files. When resolveSymlinks is false the size reflects what deleting the
// directory removes in place: for an HF hub directory this measures blobs once
// instead of counting each snapshot symlink's target again. When true, symlinks
// are resolved to their targets so a friendly-view or local copy reports the
// real bytes it occupies (matching buildLocalCacheRepo). Dangling or unreadable
// entries are skipped so partial caches still report a usable size.
func cacheCopyUsage(dir string, resolveSymlinks bool) (int64, int) {
	var size int64
	var count int
	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			if !resolveSymlinks {
				return nil
			}
			realInfo, statErr := os.Stat(path)
			if statErr != nil {
				return nil
			}
			size += realInfo.Size()
			count++
			return nil
		}
		size += info.Size()
		count++
		return nil
	})
	return size, count
}

// enumerateCacheCopies lists every distinct deletable physical location for a
// repo, using server-computed paths. The delete endpoint recomputes this same
// set before removing anything, so a caller-supplied path can never delete a
// location outside it.
//
// Order: the HF cache copy (or an orphan Friendly view when the hub is gone)
// first, then one Local copy per local root that actually contains a weight
// file. The "Friendly view" projection is not listed alongside the hub because
// it is the same storage.
func enumerateCacheCopies(cacheDir string, cfg Config, repo string, repoType hfdownloader.RepoType) []CacheCopy {
	// Start from a non-nil empty slice so a current server always serializes
	// "copies": [] rather than null/omitted. The UI uses [] to mean "current
	// server, no deletable copies" and only shows the legacy fallback delete
	// button when the key is absent entirely.
	copies := []CacheCopy{}
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) != 2 {
		return copies
	}
	owner, name := parts[0], parts[1]

	cache := hfdownloader.NewHFCache(cacheDir, 0)
	if repoDir, err := cache.Repo(repo, repoType); err == nil {
		hubPath := repoDir.Path()
		// Only list copies the delete endpoint would actually accept: it
		// validates the hub path against the configured cacheDir, while
		// repoDir.Path() may point elsewhere (e.g. an HF_HUB_CACHE override).
		// Gate on the same predicate so a copy is never advertised as
		// deletable when both delete routes would reject it. Use Lstat so a
		// top-level symlink at the hub path is not followed: the delete path
		// rejects such a link (400), so it must not be listed as a deletable
		// copy either. os.Lstat reports symlinks as ModeSymlink, which fails
		// IsDir() below.
		absCacheDir, err := filepath.Abs(cacheDir)
		if err != nil {
			return copies
		}
		absCacheDirWithSep := absCacheDir + string(filepath.Separator)
		absHubPath, err := filepath.Abs(hubPath)
		if err != nil {
			return copies
		}
		if info, statErr := os.Lstat(hubPath); statErr == nil && info.IsDir() &&
			validateHubDeletePath(absHubPath, absCacheDir, absCacheDirWithSep, repoType) == nil {
			size, count := cacheCopyUsage(hubPath, false)
			copies = append(copies, CacheCopy{
				Source:            cacheSourceHFCache,
				Path:              hubPath,
				Size:              size,
				SizeHuman:         humanSizeBytes(size),
				FileCount:         count,
				SelectiveVariants: hfManifestVariantTokens(repoDir.FriendlyPath()),
			})
		} else if os.IsNotExist(statErr) {
			// Only consider the friendly view as an orphan copy when the hub
			// entry is genuinely absent AND the directory is a proven
			// whole-folder projection of this exact repo. A folder of real
			// files, a link into another repo, or a path reached through a
			// symlinked owner alias is not this copy and must not be listed
			// (nor deleted) as if it were.
			if friendlyPath := repoDir.FriendlyPath(); friendlyPath != "" &&
				repoDir.FriendlyState() == hfdownloader.FriendlyProjection {
				size, count := cacheCopyUsage(friendlyPath, true)
				copies = append(copies, CacheCopy{
					Source:            cacheSourceFriendlyView,
					Path:              friendlyPath,
					Size:              size,
					SizeHuman:         humanSizeBytes(size),
					FileCount:         count,
					SelectiveVariants: hfManifestVariantTokens(friendlyPath),
				})
			}
		}
	}

	// Local roots only ever hold models. Adding a Local copy to a dataset
	// response would advertise a copy whose Delete returns 404 (deleteLocalCopy
	// rejects non-model types), so omit them for datasets.
	if repoType == hfdownloader.RepoTypeModel {
		for _, cand := range localCopyCandidates(cacheDir, cfg.LocalDir, cfg.LocalScanDirs, cfg.DownloadRoutes, owner, name) {
			size, count := cacheCopyUsage(cand.RepoDir, true)
			excluded := localCopyExcludedSubroots(cacheDir, cfg.LocalDir, cfg.LocalScanDirs, cfg.DownloadRoutes, cand.Root)
			copies = append(copies, CacheCopy{
				Source:            cacheSourceLocal,
				Path:              cand.RepoDir,
				Size:              size,
				SizeHuman:         humanSizeBytes(size),
				FileCount:         count,
				SelectiveVariants: collectLocalVariantTokens(cand.RepoDir, excluded),
			})
		}
	}

	return copies
}

// hfManifestVariantTokens loads the hfd.yaml at friendlyPath and returns its
// selective-delete tokens, or nil when there is no usable provenance.
func hfManifestVariantTokens(friendlyPath string) []string {
	m, ok := readHFCopyManifest(friendlyPath)
	if !ok {
		return nil
	}
	return manifestVariantTokens(m)
}

// matchCacheCopyPath resolves a caller-supplied path to one of the
// server-computed copies for this repo. It compares normalized absolute paths,
// so only an exact match against a path the server itself would list is
// accepted. For a Local match it also returns the owning candidate so the
// correct root can be used for the safe delete.
//
// A Local path that is no longer listed (because a previous partial delete
// removed the weight file) is still accepted when this server retained
// evidence that the exact target was partially deleted; that keeps an identical
// retry of the same validated target possible for the current process.
func (s *Server) matchCacheCopyPath(cacheDir string, cfg Config, repo string, repoType hfdownloader.RepoType, pathParam string) (string, localCopyCandidate, bool) {
	requested, err := filepath.Abs(strings.TrimSpace(pathParam))
	if err != nil {
		return "", localCopyCandidate{}, false
	}
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) != 2 {
		return "", localCopyCandidate{}, false
	}
	owner, name := parts[0], parts[1]

	for _, cand := range localCopyCandidates(cacheDir, cfg.LocalDir, cfg.LocalScanDirs, cfg.DownloadRoutes, owner, name) {
		if candAbs, aerr := filepath.Abs(cand.RepoDir); aerr == nil && candAbs == requested {
			return cacheSourceLocal, cand, true
		}
	}
	// A Local path that is no longer listed (the weight file is already gone)
	// is still accepted when this server retained evidence that the exact
	// target was partially deleted; that keeps an identical retry of the same
	// validated target possible for the current process.
	if cand, ok := s.matchLocalDeleteEvidence(cacheDir, cfg, owner, name, requested); ok {
		return cacheSourceLocal, cand, true
	}
	for _, copy := range enumerateCacheCopies(cacheDir, cfg, repo, repoType) {
		if candAbs, aerr := filepath.Abs(copy.Path); aerr == nil && candAbs == requested {
			return copy.Source, localCopyCandidate{}, true
		}
	}
	return "", localCopyCandidate{}, false
}

// resolveLocalDeleteTarget validates that deleting <root>/<owner>/<name> is
// safe and returns the absolute target path. The target must be exactly a
// two-level owner/name directory inside the specific root it was resolved from;
// every intermediate component must be a real directory (not a symlink) and the
// leaf must not be a symlink. Prefix checks use the root plus a separator so a
// sibling like /data/Models-evil never matches root /data/Models.
//
// Beyond the structural checks, the target must be a genuine LEAF Local repo:
// it must not be, contain, or (through its own root) sit inside the cache dir, a
// friendly namespace, or another configured root, and it must not be a
// container of nested weight-bearing model directories. It must directly own a
// weight file unless allowWeightless is set, which only the retained
// partial-delete retry uses (the remainder may no longer carry a weight file).
// This is a WHOLE-COPY delete, so the strict predicate additionally rejects any
// target that encloses a configured root: listing may advertise such an
// enclosure (upstream #66), but RemoveAll on it would destroy the nested root.
func resolveLocalDeleteTarget(cacheDir, localDir string, localScanDirs []string, downloadRoutes map[string]string, root, owner, name string, allowWeightless bool) (string, error) {
	absTarget, err := localTargetStructuralPath(root, owner, name)
	if err != nil {
		return "", err
	}
	absRoot, _ := filepath.Abs(root)
	roots := localCacheRoots(cacheDir, localDir, localScanDirs, downloadRoutes)
	targetRoot := localCacheRoot{Path: absRoot}
	if !localWholeCopyDeleteAllowed(absTarget, pathIdentityKey(absRoot),
		localUnitGuards(cacheDir, localDir, localScanDirs, downloadRoutes),
		targetRoot.excludedSubroots(roots), allowWeightless) {
		return "", fmt.Errorf("target is not a deletable leaf model directory")
	}
	return absTarget, nil
}

// resolveLocalVariantTarget validates the same structural invariants as
// resolveLocalDeleteTarget but applies the listing predicate instead of the
// strict whole-copy predicate. A variant delete removes only the matched files
// inside the copy (never the container with RemoveAll), so a copy that merely
// encloses a configured root remains a valid selective-delete target; the walk
// skips excluded subroots so the nested root is untouched.
func resolveLocalVariantTarget(cacheDir, localDir string, localScanDirs []string, downloadRoutes map[string]string, root, owner, name string) (string, error) {
	absTarget, err := localTargetStructuralPath(root, owner, name)
	if err != nil {
		return "", err
	}
	absRoot, _ := filepath.Abs(root)
	roots := localCacheRoots(cacheDir, localDir, localScanDirs, downloadRoutes)
	targetRoot := localCacheRoot{Path: absRoot}
	if !localLeafRepoIsDeletable(absTarget, pathIdentityKey(absRoot),
		localUnitGuards(cacheDir, localDir, localScanDirs, downloadRoutes),
		targetRoot.excludedSubroots(roots), false) {
		return "", fmt.Errorf("target is not a deletable leaf model directory")
	}
	return absTarget, nil
}

// localTargetStructuralPath applies the structural half of local-delete
// validation: the target must be exactly a two-level owner/name directory inside
// root, every intermediate component must be a real directory (not a symlink),
// the leaf must not be a symlink, and the resolved leaf must stay inside the
// resolved root. It deliberately does not apply the ownership predicate; callers
// choose listing-level or whole-copy-level strictness.
func localTargetStructuralPath(root, owner, name string) (string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	absTarget, err := filepath.Abs(filepath.Join(absRoot, owner, name))
	if err != nil {
		return "", err
	}

	rootWithSep := absRoot + string(filepath.Separator)
	if absTarget == absRoot || !strings.HasPrefix(absTarget+string(filepath.Separator), rootWithSep) {
		return "", fmt.Errorf("target outside local root")
	}

	rel, err := filepath.Rel(absRoot, absTarget)
	if err != nil {
		return "", err
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("target escapes local root")
	}
	if strings.Count(rel, string(filepath.Separator)) != 1 {
		return "", fmt.Errorf("target is not an owner/name directory")
	}

	// Validate every path component between the root and the target, not just
	// the leaf. An intermediate symlink such as <root>/<owner> -> <root>/other
	// would otherwise make the leaf Lstat below resolve to the real
	// <root>/other/<name>; os.RemoveAll would then follow the intermediate link
	// and delete the sibling instead of the intended <root>/<owner>/<name>.
	if err := hfdownloader.RejectSymlinkedComponents(absRoot, absTarget); err != nil {
		return "", err
	}

	info, err := os.Lstat(absTarget)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("cannot delete symlinked directory")
	}

	realTarget, err := filepath.EvalSymlinks(absTarget)
	if err == nil {
		base := absRoot
		if realRoot, rerr := filepath.EvalSymlinks(absRoot); rerr == nil {
			base = realRoot
		}
		if realTarget != base && !strings.HasPrefix(realTarget+string(filepath.Separator), base+string(filepath.Separator)) {
			return "", fmt.Errorf("resolved target outside local root")
		}
	}

	return absTarget, nil
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
