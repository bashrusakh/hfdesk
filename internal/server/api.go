// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
	"github.com/bashrusakh/hfdesk/pkg/smartdl"
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
		Token              *string  `json:"token,omitempty"`
		CacheDir           *string  `json:"cacheDir,omitempty"`
		LocalDir           *string  `json:"localDir,omitempty"`
		LocalScanDirs      []string `json:"localScanDirs,omitempty"`
		Concurrency        *int     `json:"connections,omitempty"`
		MaxActive          *int     `json:"maxActive,omitempty"`
		MultipartThreshold *string  `json:"multipartThreshold,omitempty"`
		MaxSpeed           *string  `json:"maxSpeed,omitempty"`
		Verify             *string  `json:"verify,omitempty"`
		Retries            *int     `json:"retries,omitempty"`
		Endpoint           *string  `json:"endpoint,omitempty"`
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

	// Phase 2: build the new config on a local copy under withConfig (which
	// takes the write lock for the whole mutation, so a concurrent read sees
	// either the pre- or post-state, never a torn mix). The lock is released
	// before we touch the job manager or the file, so the critical section
	// stays short.
	s.withConfig(func(c *Config) {
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

	// Re-snapshot under the read lock to feed the post-lock side effects.
	// Using newCfg (captured inside withConfig) is a lost-update hazard: a
	// concurrent handleUpdateSettings that runs its own withConfig between
	// the lock release and these side effects would leave the job manager
	// and the persisted file behind the latest in-memory state. The fresh
	// snapshot is the narrowest possible window.
	finalCfg := s.snapshotConfig()

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
			baseRaw := fmt.Sprintf("%s/%s%s/raw/%s/", endpoint, prefix, repo, revision)
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

type localCacheRoot struct {
	Path        string
	Source      string
	SkipSpecial bool
}

func cleanPathList(paths []string) []string {
	var cleaned []string
	seen := make(map[string]bool)
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		path = filepath.Clean(path)
		key := strings.ToLower(path)
		if seen[key] {
			continue
		}
		seen[key] = true
		cleaned = append(cleaned, path)
	}
	return cleaned
}

func localCacheRoots(cacheDir, localDir string, localScanDirs []string) []localCacheRoot {
	var roots []localCacheRoot
	seen := make(map[string]bool)
	add := func(path, source string, skipSpecial bool) {
		if path == "" {
			return
		}
		cleaned := filepath.Clean(path)
		key := strings.ToLower(cleaned)
		if seen[key] {
			return
		}
		seen[key] = true
		roots = append(roots, localCacheRoot{Path: cleaned, Source: source, SkipSpecial: skipSpecial})
	}

	cacheRoot := filepath.Clean(cacheDir)
	localSkipSpecial := localDir != "" && strings.EqualFold(filepath.Clean(localDir), cacheRoot)

	add(filepath.Join(cacheDir, "models"), "Friendly view", false)
	add(localDir, "Local", localSkipSpecial)
	for _, dir := range localScanDirs {
		add(dir, "Local", false)
	}
	add(cacheDir, "Local", true)
	return roots
}

func hasLocalWeightFile(dir string) bool {
	found := false
	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
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

var cacheQuantPattern = regexp.MustCompile(`(?i)(UD[-_])?(IQ[1-4]_(?:XXS|XS|S|M|NL)|Q[2-8]_(?:[01]|K(?:_(?:XXL|XL|L|M|S))?)|APEX[-_](?:I[-_])?(?:BALANCED|COMPACT|MINI|QUALITY)|MXFP4_MOE|F(?:16|32)|BF16)`)

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

func isCacheMMProjFile(name string) bool {
	base := strings.ToLower(filepath.Base(name))
	return strings.HasPrefix(base, "mmproj") || strings.Contains(base, "-mmproj")
}

type cacheGGUFMetadata struct {
	Quantizations []string
	HasMMProj     bool
	MMProjFiles   []string
	Capabilities  []string
}

func collectCacheGGUFMetadata(dir string, includeMMProjFiles bool) cacheGGUFMetadata {
	seen := make(map[string]bool)
	meta := cacheGGUFMetadata{}
	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
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

func buildLocalCacheRepo(owner, name, repoDir, source string, includeFiles bool) (*CachedRepoInfo, error) {
	var totalSize int64
	var fileCount int
	var files []CachedFileInfo
	var newest time.Time

	err := filepath.Walk(repoDir, func(path string, info os.FileInfo, err error) error {
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
	ggufMeta := collectCacheGGUFMetadata(repoDir, includeFiles)

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

func scanLocalCachedRepos(cacheDir, localDir string, localScanDirs []string, includeFiles bool) ([]CachedRepoInfo, error) {
	var repos []CachedRepoInfo
	for _, root := range localCacheRoots(cacheDir, localDir, localScanDirs) {
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
			if root.SkipSpecial {
				switch strings.ToLower(owner) {
				case "hub", "models", "datasets", "blobs", "snapshots", "refs":
					continue
				}
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
				if !hasLocalWeightFile(repoDir) {
					continue
				}
				repo, err := buildLocalCacheRepo(owner, name, repoDir, root.Source, includeFiles)
				if err == nil {
					repos = append(repos, *repo)
				}
			}
		}
	}
	return repos, nil
}

func findLocalCachedRepo(cacheDir, localDir string, localScanDirs []string, repoID string, includeFiles bool) (*CachedRepoInfo, error) {
	parts := strings.SplitN(repoID, "/", 2)
	if len(parts) != 2 {
		return nil, os.ErrNotExist
	}
	for _, root := range localCacheRoots(cacheDir, localDir, localScanDirs) {
		repoDir := filepath.Join(root.Path, parts[0], parts[1])
		if !hasLocalWeightFile(repoDir) {
			continue
		}
		return buildLocalCacheRepo(parts[0], parts[1], repoDir, root.Source, includeFiles)
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

		ggufMeta := collectCacheGGUFMetadata(friendlyPath, false)

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

	localRepos, _ := scanLocalCachedRepos(cacheDir, cfg.LocalDir, cfg.LocalScanDirs, false)
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
			localRepo, localErr := findLocalCachedRepo(cacheDir, cfg.LocalDir, cfg.LocalScanDirs, repo, true)
			if localErr == nil {
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
		snapshotDir := repoDir.SnapshotDir(snapshots[0])
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

	ggufMeta := collectCacheGGUFMetadata(friendlyPath, true)

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
	if !strings.HasPrefix(absHubPath+string(filepath.Separator), absCacheDirWithSep) {
		writeError(w, http.StatusBadRequest, "Invalid path", "Path outside cache directory")
		return
	}

	// Security Layer 9: Verify path follows expected HF cache structure
	// Must be: {cacheDir}/hub/{models|datasets}--{owner}--{name}
	expectedPrefix := "models--"
	if repoType == hfdownloader.RepoTypeDataset {
		expectedPrefix = "datasets--"
	}
	hubSubpath, err := filepath.Rel(absCacheDir, absHubPath)
	if err != nil || !strings.HasPrefix(hubSubpath, filepath.Join("hub", expectedPrefix)) {
		writeError(w, http.StatusBadRequest, "Invalid path", "Path does not match expected cache structure")
		return
	}

	// Security Layer 10: Resolve symlinks to verify final destination is also within cache
	// This catches symlinks inside the directory structure
	realHubPath, err := filepath.EvalSymlinks(absHubPath)
	if err == nil && realHubPath != absHubPath {
		// Path contained symlinks - verify resolved path is still within cache
		if !strings.HasPrefix(realHubPath+string(filepath.Separator), absCacheDirWithSep) {
			writeError(w, http.StatusBadRequest, "Invalid path", "Resolved path outside cache directory")
			return
		}
	}

	// All security checks passed - proceed with deletion
	if err := os.RemoveAll(absHubPath); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to delete cache", err.Error())
		return
	}

	// Delete the friendly view directory (symlinks) with same security checks
	if friendlyPath != "" {
		if err := safeDeleteFriendlyPath(friendlyPath, absCacheDirWithSep); err != nil {
			// Log but don't fail - hub directory was successfully deleted
			// The friendly path might not exist or might be invalid
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
