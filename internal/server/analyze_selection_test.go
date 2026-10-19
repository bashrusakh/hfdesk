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
)

// TestAnalyzeExplicitNamespaceSelection pins the /api/analyze dataset
// tri-state: absent auto-detects, true forces the dataset namespace, and any
// other present value forces the model namespace without probing the other
// namespace (which would return needsSelection for a dual-namespace repo).
func TestAnalyzeExplicitNamespaceSelection(t *testing.T) {
	tests := []struct {
		name             string
		query            string
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
			name:           "explicit model",
			query:          "?dataset=false",
			wantModelTrees: 1,
		},
		{
			name:             "explicit dataset",
			query:            "?dataset=true",
			wantDataset:      true,
			wantDatasetTrees: 1,
		},
		{
			name:           "other present value forces model",
			query:          "?dataset=0",
			wantModelTrees: 1,
		},
		{
			name:           "empty present value forces model",
			query:          "?dataset=",
			wantModelTrees: 1,
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
