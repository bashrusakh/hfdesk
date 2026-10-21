// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
	"github.com/bashrusakh/hfdesk/pkg/smartdl"
)

// TestAnalyzeExplicitNamespaceSelection pins the /api/analyze namespace
// contract: type=model|dataset is the one explicit selector and neither value
// probes the other namespace. An absent type auto-detects, and the legacy
// dataset parameter keeps its pre-#135 behavior (only "true" forces dataset;
// every other value auto-detects, including false). repoType is no longer a
// selector.
func TestAnalyzeExplicitNamespaceSelection(t *testing.T) {
	tests := []struct {
		name             string
		query            string
		wantStatus       int
		wantError        string
		wantDataset      bool
		wantNeedsSelect  bool
		wantModelTrees   int64
		wantDatasetTrees int64
	}{
		{
			name:             "absent auto-detects both",
			wantNeedsSelect:  true,
			wantModelTrees:   1,
			wantDatasetTrees: 1,
		},
		{
			name:           "type model forces model without dataset probe",
			query:          "?type=model",
			wantModelTrees: 1,
		},
		{
			name:             "type dataset forces dataset without model probe",
			query:            "?type=dataset",
			wantDataset:      true,
			wantDatasetTrees: 1,
		},
		{
			name:           "type model with legacy dataset=false selects model",
			query:          "?type=model&dataset=false",
			wantModelTrees: 1,
		},
		{
			name:             "type dataset with legacy dataset=false selects dataset",
			query:            "?type=dataset&dataset=false",
			wantDataset:      true,
			wantDatasetTrees: 1,
		},
		{
			name:             "type dataset with legacy dataset=true stays dataset",
			query:            "?type=dataset&dataset=true",
			wantDataset:      true,
			wantDatasetTrees: 1,
		},
		{
			name:       "type model with legacy dataset=true conflicts",
			query:      "?type=model&dataset=true",
			wantStatus: http.StatusBadRequest,
			wantError:  "Conflicting repository type selection",
		},
		{
			name:             "legacy dataset=false auto-detects the dual repository",
			query:            "?dataset=false",
			wantNeedsSelect:  true,
			wantModelTrees:   1,
			wantDatasetTrees: 1,
		},
		{
			name:             "legacy dataset=true forces dataset",
			query:            "?dataset=true",
			wantDataset:      true,
			wantDatasetTrees: 1,
		},
		{
			name:             "legacy dataset=0 auto-detects",
			query:            "?dataset=0",
			wantNeedsSelect:  true,
			wantModelTrees:   1,
			wantDatasetTrees: 1,
		},
		{
			name:             "legacy empty dataset auto-detects",
			query:            "?dataset=",
			wantNeedsSelect:  true,
			wantModelTrees:   1,
			wantDatasetTrees: 1,
		},
		{
			name:             "legacy dataset=TRUE does not force dataset",
			query:            "?dataset=TRUE",
			wantNeedsSelect:  true,
			wantModelTrees:   1,
			wantDatasetTrees: 1,
		},
		{
			name:       "invalid type is rejected",
			query:      "?type=invalid",
			wantStatus: http.StatusBadRequest,
			wantError:  "Invalid repository type",
		},
		{
			name:       "type matching stays case-sensitive",
			query:      "?type=MODEL",
			wantStatus: http.StatusBadRequest,
			wantError:  "Invalid repository type",
		},
		{
			name:             "repoType is no longer a selector",
			query:            "?repoType=dataset",
			wantNeedsSelect:  true,
			wantModelTrees:   1,
			wantDatasetTrees: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var modelTrees, datasetTrees atomic.Int64
			hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/api/models/owner/both/tree/main":
					modelTrees.Add(1)
					io.WriteString(w, `[{"type":"file","path":"model.safetensors","size":10}]`)
				case r.URL.Path == "/api/datasets/owner/both/tree/main":
					datasetTrees.Add(1)
					io.WriteString(w, `[{"type":"file","path":"data.parquet","size":20}]`)
				case strings.HasSuffix(r.URL.Path, "/refs"):
					io.WriteString(w, `{"branches":[]}`)
				case strings.HasSuffix(r.URL.Path, "/auth-check"):
					// The merged analyzer probes the optional gated-access
					// endpoint; this fake mirror does not implement it.
					http.NotFound(w, r)
				default:
					t.Errorf("unexpected request: %s", r.URL)
					http.NotFound(w, r)
				}
			}))
			defer hub.Close()

			s := newTestServerWithConfig(t, Config{
				Endpoint: hub.URL,
				CacheDir: t.TempDir(),
				Proxy:    &hfdownloader.ProxyConfig{NoEnvProxy: true},
			})
			r := httptest.NewRequest("GET", "/api/analyze/owner/both"+tt.query, nil)
			r.SetPathValue("repo", "owner/both")
			w := httptest.NewRecorder()
			s.handleAnalyze(w, r)

			wantStatus := tt.wantStatus
			if wantStatus == 0 {
				wantStatus = http.StatusOK
			}
			if w.Code != wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", w.Code, wantStatus, w.Body.String())
			}
			if wantStatus != http.StatusOK {
				if tt.wantError != "" && !strings.Contains(w.Body.String(), tt.wantError) {
					t.Fatalf("body = %s, want a %q error", w.Body.String(), tt.wantError)
				}
				if modelTrees.Load() != tt.wantModelTrees || datasetTrees.Load() != tt.wantDatasetTrees {
					t.Fatalf("model/dataset tree requests = %d/%d, want %d/%d",
						modelTrees.Load(), datasetTrees.Load(), tt.wantModelTrees, tt.wantDatasetTrees)
				}
				return
			}
			var resp struct {
				IsDataset      bool `json:"is_dataset"`
				NeedsSelection bool `json:"needsSelection"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode response: %v; body=%s", err, w.Body.String())
			}
			if resp.NeedsSelection != tt.wantNeedsSelect {
				t.Fatalf("needsSelection = %v, want %v; body=%s", resp.NeedsSelection, tt.wantNeedsSelect, w.Body.String())
			}
			if !tt.wantNeedsSelect && resp.IsDataset != tt.wantDataset {
				t.Fatalf("is_dataset = %v, want %v; body=%s", resp.IsDataset, tt.wantDataset, w.Body.String())
			}
			if modelTrees.Load() != tt.wantModelTrees || datasetTrees.Load() != tt.wantDatasetTrees {
				t.Fatalf("model/dataset tree requests = %d/%d, want %d/%d",
					modelTrees.Load(), datasetTrees.Load(), tt.wantModelTrees, tt.wantDatasetTrees)
			}
		})
	}
}

func TestAnalyzeEXLBranchSelectionUsesBranchRevision(t *testing.T) {
	const (
		repo     = "owner/exl3-model"
		revision = "SC_6.00bpw_H6_V6"
	)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/tree/"):
			branch := strings.SplitN(r.URL.Path, "/tree/", 2)[1]
			switch branch {
			case "main":
				io.WriteString(w, `[{"type":"file","path":"README.md","size":1},{"type":"file","path":"config.json","size":10}]`)
			case revision:
				io.WriteString(w, `[{"type":"file","path":"config.json","size":10},{"type":"file","path":"model-6.safetensors","size":600},{"type":"file","path":"quantization_config.json","size":20}]`)
			default:
				t.Errorf("unexpected model tree revision %q", branch)
				http.NotFound(w, r)
			}
		case strings.HasSuffix(r.URL.Path, "/refs"):
			io.WriteString(w, `{"branches":[{"name":"main","targetCommit":"main-sha"},{"name":"`+revision+`","targetCommit":"branch-sha"}],"tags":[{"name":"fp16","targetCommit":"tag-sha"}]}`)
		case strings.HasSuffix(r.URL.Path, "/auth-check"):
			http.NotFound(w, r)
		case strings.Contains(r.URL.Path, "/raw/"):
			switch {
			case strings.HasSuffix(r.URL.Path, "/main/config.json"):
				io.WriteString(w, `{"model_type":"llama"}`)
			case strings.HasSuffix(r.URL.Path, "/"+revision+"/quantization_config.json"):
				io.WriteString(w, `{"quant_method":"exl3","bits":4,"bits_per_weight":6,"head_bits":6}`)
			default:
				http.NotFound(w, r)
			}
		default:
			t.Errorf("unexpected Hub request: %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer hub.Close()

	s := newTestServerWithConfig(t, Config{
		Endpoint: hub.URL,
		CacheDir: t.TempDir(),
		Proxy:    &hfdownloader.ProxyConfig{NoEnvProxy: true},
	})
	getAnalysis := func(query string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/api/analyze/"+repo+query, nil)
		r.SetPathValue("repo", repo)
		w := httptest.NewRecorder()
		s.handleAnalyze(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("analyze %q status = %d body=%s", query, w.Code, w.Body.String())
		}
		return w
	}

	mainResponse := getAnalysis("?type=model")
	var main struct {
		SelectableItems []smartdl.SelectableItem   `json:"selectable_items"`
		Recommended     *smartdl.DownloadSelection `json:"recommended_download"`
	}
	if err := json.Unmarshal(mainResponse.Body.Bytes(), &main); err != nil {
		t.Fatalf("decode main analysis: %v", err)
	}
	if len(main.SelectableItems) != 1 || main.SelectableItems[0].Revision != revision || main.SelectableItems[0].Method != "exl3" || main.SelectableItems[0].BitsPerWeight != 6 || main.SelectableItems[0].HeadBits != 6 {
		t.Fatalf("branch item API projection = %+v", main.SelectableItems)
	}
	if main.Recommended == nil || main.Recommended.Revision != revision || len(main.Recommended.Filters) != 0 {
		t.Fatalf("recommended_download = %+v, want unfiltered revision %q", main.Recommended, revision)
	}

	branchResponse := getAnalysis("?type=model&revision=" + revision)
	var selected struct {
		Branch string             `json:"branch"`
		Files  []smartdl.FileInfo `json:"files"`
	}
	if err := json.Unmarshal(branchResponse.Body.Bytes(), &selected); err != nil {
		t.Fatalf("decode selected branch analysis: %v", err)
	}
	if selected.Branch != revision || len(selected.Files) != 3 {
		t.Fatalf("selected branch response = branch %q files=%+v", selected.Branch, selected.Files)
	}
	for _, file := range selected.Files {
		if file.Path == "README.md" {
			t.Fatal("selected branch analysis returned main-only README")
		}
	}
}

// TestAnalyzeLegacyDatasetFalseDatasetOnly pins the #135 backward-compatibility
// regression: a repository that exists only as a dataset must still be analyzed
// by the legacy dataset=false request, because false historically means
// auto-detect rather than force model.
func TestAnalyzeLegacyDatasetFalseDatasetOnly(t *testing.T) {
	var modelTrees, datasetTrees atomic.Int64
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/models/owner/data-only/tree/main":
			modelTrees.Add(1)
			http.NotFound(w, r)
		case r.URL.Path == "/api/datasets/owner/data-only/tree/main":
			datasetTrees.Add(1)
			io.WriteString(w, `[{"type":"file","path":"data.parquet","size":20}]`)
		case strings.HasSuffix(r.URL.Path, "/refs"):
			io.WriteString(w, `{"branches":[]}`)
		case strings.HasSuffix(r.URL.Path, "/auth-check"):
			// Optional gated-access probe: this fake mirror does not
			// implement it, so analysis stays fail-open.
			http.NotFound(w, r)
		default:
			t.Errorf("unexpected request: %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer hub.Close()

	s := newTestServerWithConfig(t, Config{
		Endpoint: hub.URL,
		CacheDir: t.TempDir(),
		Proxy:    &hfdownloader.ProxyConfig{NoEnvProxy: true},
	})
	r := httptest.NewRequest("GET", "/api/analyze/owner/data-only?dataset=false", nil)
	r.SetPathValue("repo", "owner/data-only")
	w := httptest.NewRecorder()
	s.handleAnalyze(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		IsDataset      bool `json:"is_dataset"`
		NeedsSelection bool `json:"needsSelection"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, w.Body.String())
	}
	if resp.NeedsSelection || !resp.IsDataset {
		t.Fatalf("needsSelection/is_dataset = %v/%v, want false/true; body=%s", resp.NeedsSelection, resp.IsDataset, w.Body.String())
	}
	if modelTrees.Load() != 1 || datasetTrees.Load() != 1 {
		t.Fatalf("model/dataset tree requests = %d/%d, want 1/1 (model first, then dataset fallback)",
			modelTrees.Load(), datasetTrees.Load())
	}
}
