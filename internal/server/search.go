// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
)

// SearchResult is one model returned by /api/search.
type SearchResult struct {
	ID           string    `json:"id"`
	Author       string    `json:"author,omitempty"`
	Downloads    int64     `json:"downloads"`
	Likes        int       `json:"likes"`
	TrendingScore float64  `json:"trendingScore,omitempty"`
	Private      bool      `json:"private,omitempty"`
	Gated        bool      `json:"gated,omitempty"`
	PipelineTag  string    `json:"pipelineTag,omitempty"`
	LibraryName  string    `json:"libraryName,omitempty"`
	Tags         []string  `json:"tags,omitempty"`
	LastModified time.Time `json:"lastModified,omitempty"`
	CreatedAt    time.Time `json:"createdAt,omitempty"`
}

// SearchResponse wraps a list of results and metadata.
type SearchResponse struct {
	Results []SearchResult `json:"results"`
	Total   int            `json:"total"`
	Query   string         `json:"query,omitempty"`
}

// hfAPIModel is the raw shape returned by huggingface.co/api/models.
type hfAPIModel struct {
	ID           string    `json:"id"`
	Author       string    `json:"author"`
	Downloads    int64     `json:"downloads"`
	Likes        int       `json:"likes"`
	TrendingScore float64  `json:"trendingScore"`
	Private      bool      `json:"private"`
	Gated        any       `json:"gated"` // bool or string ("auto","manual")
	PipelineTag  string    `json:"pipeline_tag"`
	LibraryName  string    `json:"library_name"`
	Tags         []string  `json:"tags"`
	LastModified time.Time `json:"lastModified"`
	CreatedAt    time.Time `json:"createdAt"`
}

// handleSearch proxies model search to the HuggingFace Hub API.
//
// Query parameters:
//
//	q        – free-text search
//	sort     – downloads | likes | lastModified | trendingScore (default: downloads)
//	limit    – max results (default 20, max 100)
//	filter   – comma-separated tag filters forwarded to HF (e.g. "gguf,text-generation")
//	datasets – "true" to search datasets instead of models
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	query := strings.TrimSpace(q.Get("q"))
	sort := q.Get("sort")
	if sort == "" {
		sort = "downloads"
	}
	limitStr := q.Get("limit")
	limit := 20
	if n, err := strconv.Atoi(limitStr); err == nil && n > 0 {
		if n > 100 {
			n = 100
		}
		limit = n
	}
	filterParam := q.Get("filter")
	isDataset := strings.EqualFold(q.Get("datasets"), "true")

	// Build HF API URL
	endpoint := s.config.Endpoint
	if endpoint == "" {
		endpoint = "https://huggingface.co"
	}
	endpoint = strings.TrimRight(endpoint, "/")

	apiPath := "/api/models"
	if isDataset {
		apiPath = "/api/datasets"
	}

	params := url.Values{}
	if query != "" {
		params.Set("search", query)
	}
	params.Set("sort", sort)
	params.Set("direction", "-1")
	params.Set("limit", strconv.Itoa(limit))
	if filterParam != "" {
		// HF supports multiple filter params; we join with comma for a single param
		params.Set("filter", filterParam)
	}

	hfURL := fmt.Sprintf("%s%s?%s", endpoint, apiPath, params.Encode())

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, hfURL, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to build upstream request", err.Error())
		return
	}
	if s.config.Token != "" {
		req.Header.Set("Authorization", "Bearer "+s.config.Token)
	}
	req.Header.Set("User-Agent", "hfdesk/1")

	httpClient, err := hfdownloader.BuildHTTPClient(s.config.Proxy)
	if err != nil {
		log.Printf("search: invalid proxy config: %v", err)
		writeError(w, http.StatusInternalServerError, "Invalid proxy configuration", err.Error())
		return
	}
	httpClient.Timeout = 20 * time.Second
	resp, err := httpClient.Do(req)
	if err != nil {
		log.Printf("search: upstream error: %v", err)
		writeError(w, http.StatusBadGateway, "Upstream request failed", err.Error())
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20)) // 4 MiB cap
	if err != nil {
		writeError(w, http.StatusBadGateway, "Failed to read upstream response", err.Error())
		return
	}

	if resp.StatusCode != http.StatusOK {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		w.Write(body)
		return
	}

	var raw []hfAPIModel
	if err := json.Unmarshal(body, &raw); err != nil {
		writeError(w, http.StatusBadGateway, "Failed to parse upstream response", err.Error())
		return
	}

	results := make([]SearchResult, 0, len(raw))
	for _, m := range raw {
		gated := false
		switch v := m.Gated.(type) {
		case bool:
			gated = v
		case string:
			gated = v != "" && v != "false"
		}
		results = append(results, SearchResult{
			ID:            m.ID,
			Author:        m.Author,
			Downloads:     m.Downloads,
			Likes:         m.Likes,
			TrendingScore: m.TrendingScore,
			Private:       m.Private,
			Gated:         gated,
			PipelineTag:   m.PipelineTag,
			LibraryName:   m.LibraryName,
			Tags:          m.Tags,
			LastModified:  m.LastModified,
			CreatedAt:     m.CreatedAt,
		})
	}

	writeJSON(w, http.StatusOK, SearchResponse{
		Results: results,
		Total:   len(results),
		Query:   query,
	})
}

// handleDiskFree returns free/total disk space for a given path.
// Query param: path (optional — defaults to CacheDir or RunDir).
func (s *Server) handleDiskFree(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		path = s.config.CacheDir
	}
	// Resolve the same way the storage badge does: when CacheDir is unset, fall
	// back to the default HF cache location (HF_HOME/HF_HUB_CACHE/~), not the run
	// directory — otherwise the disk indicator reports the exe's drive while the
	// badge shows the cache drive (bug: cache on I:, size from C:).
	if path == "" {
		path = hfdownloader.DefaultCacheDir()
	}
	if path == "" {
		path = RunDir()
	}
	free, total, err := diskFreeBytes(path)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Could not stat disk", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path":  path,
		"free":  free,
		"total": total,
	})
}

// handleHistory returns entries from download_history.json.
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	entries, err := LoadHistory()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to load history", err.Error())
		return
	}
	if entries == nil {
		entries = []HistoryEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entries": entries,
		"total":   len(entries),
	})
}
