// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package smartdl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// TestAnalyze_AuthCheck covers the fail-open /auth-check contract: only an
// explicit 401/403 GatedRepo response blocks analysis; every other outcome
// falls through to the existing tree/metadata path.
func TestAnalyze_AuthCheck(t *testing.T) {
	cases := []struct {
		name         string
		dataset      bool
		token        string
		status       int
		headers      map[string]string
		body         string
		transportErr bool
		wantGated    bool
		wantHasToken bool
		wantMessage  string
	}{
		{name: "200 model with token", status: http.StatusOK, token: "hf_secret"},
		{name: "200 model without token", status: http.StatusOK},
		{
			name: "401 GatedRepo model", status: http.StatusUnauthorized, token: "hf_secret",
			headers:      map[string]string{"X-Error-Code": "GatedRepo"},
			body:         `{"error":"Access to this resource is gated."}`,
			wantGated:    true,
			wantHasToken: true,
			wantMessage:  "Access to this resource is gated.",
		},
		{
			name: "403 GatedRepo model without token", status: http.StatusForbidden,
			headers:     map[string]string{"X-Error-Code": "gatedrepo"},
			body:        `{"message":"Agreement required"}`,
			wantGated:   true,
			wantMessage: "Agreement required",
		},
		{
			name: "header message preferred", status: http.StatusUnauthorized,
			headers:     map[string]string{"X-Error-Code": "GatedRepo", "X-Error-Message": "Access to this resource is gated."},
			body:        `{"error":"ignored body"}`,
			wantGated:   true,
			wantMessage: "Access to this resource is gated.",
		},
		{
			name: "401 GatedRepo dataset", dataset: true, status: http.StatusUnauthorized, token: "hf_secret",
			headers:      map[string]string{"X-Error-Code": "GatedRepo"},
			body:         `{"error":"Dataset access is gated."}`,
			wantGated:    true,
			wantHasToken: true,
			wantMessage:  "Dataset access is gated.",
		},
		{name: "404 unsupported endpoint", status: http.StatusNotFound},
		{name: "500 unavailable endpoint", status: http.StatusInternalServerError},
		{name: "429 rate limited", status: http.StatusTooManyRequests},
		{name: "401 without gated code", status: http.StatusUnauthorized},
		{name: "403 without gated code", status: http.StatusForbidden},
		{name: "network failure", transportErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := "models"
			if tc.dataset {
				api = "datasets"
			}
			treePath := fmt.Sprintf("/api/%s/owner/repo/tree/main", api)
			authPath := fmt.Sprintf("/api/%s/owner/repo/auth-check", api)

			var mu sync.Mutex
			authRequests := 0
			authHeader := ""
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == authPath:
					mu.Lock()
					authRequests++
					authHeader = r.Header.Get("Authorization")
					mu.Unlock()
					for key, value := range tc.headers {
						w.Header().Set(key, value)
					}
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, tc.body)
				case r.URL.Path == treePath:
					_ = json.NewEncoder(w).Encode([]hfTreeNode{{Type: "file", Path: "data.txt", Size: 1}})
				case r.URL.Path == "/api/datasets/owner/repo/tree/main" && !tc.dataset:
					http.NotFound(w, r)
				case strings.HasSuffix(r.URL.Path, "/refs"):
					_, _ = io.WriteString(w, `{"branches":[],"tags":[]}`)
				default:
					t.Errorf("unexpected analysis request: %s", r.URL)
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()

			client := srv.Client()
			if tc.transportErr {
				base := client.Transport
				client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					if strings.HasSuffix(r.URL.Path, "/auth-check") {
						return nil, errors.New("auth check unavailable")
					}
					return base.RoundTrip(r)
				})}
			}
			analyzer := NewAnalyzer(AnalyzerOptions{Endpoint: srv.URL, HTTPClient: client, Token: tc.token})

			info, err := analyzer.Analyze(context.Background(), "owner/repo", tc.dataset)
			if tc.wantGated {
				var gated *GatedRepoError
				if !errors.As(err, &gated) {
					t.Fatalf("info = %+v, error = %v; want typed GatedRepoError", info, err)
				}
				if info != nil {
					t.Fatalf("gated analysis returned partial info: %+v", info)
				}
				if gated.Repo != "owner/repo" || gated.IsDataset != tc.dataset || gated.Endpoint != srv.URL || gated.HasToken != tc.wantHasToken {
					t.Errorf("gated = %+v; want repo/isDataset/endpoint/hasToken owner/repo/%v/%s/%v",
						gated, tc.dataset, srv.URL, tc.wantHasToken)
				}
				if !strings.Contains(gated.Message, tc.wantMessage) {
					t.Errorf("message = %q, want it to contain %q", gated.Message, tc.wantMessage)
				}
				wantAccept := srv.URL + "/owner/repo"
				if tc.dataset {
					wantAccept = srv.URL + "/datasets/owner/repo"
				}
				if gated.AcceptURL() != wantAccept {
					t.Errorf("accept URL = %q, want %q", gated.AcceptURL(), wantAccept)
				}
			} else {
				if err != nil {
					t.Fatalf("auth check blocked analysis: %v", err)
				}
				if info == nil || info.FileCount != 1 || info.IsDataset != tc.dataset {
					t.Fatalf("info = %+v; want one analyzed file (dataset=%v)", info, tc.dataset)
				}
			}

			wantAuthRequests := 1
			if tc.transportErr {
				wantAuthRequests = 0
			}
			if authRequests != wantAuthRequests {
				t.Errorf("auth-check requests = %d, want %d", authRequests, wantAuthRequests)
			}
			if !tc.transportErr {
				wantAuth := ""
				if tc.token != "" {
					wantAuth = "Bearer " + tc.token
				}
				if authHeader != wantAuth {
					t.Errorf("auth-check Authorization = %q, want %q", authHeader, wantAuth)
				}
			}
		})
	}
}

// TestAnalyze_TreeGatedRepo pins the tree-status mapping: explicit GatedRepo
// responses become the typed error, dataset fallback still surfaces a gated
// dataset, and genuinely missing repositories are not misclassified.
func TestAnalyze_TreeGatedRepo(t *testing.T) {
	const message = "Access to this resource is gated."
	cases := []struct {
		name            string
		dataset         bool
		modelStatus     int
		modelGated      bool
		datasetStatus   int
		datasetGated    bool
		wantGated       bool
		wantDataset     bool
		wantNotFound    bool
		wantDatasetReqs int
	}{
		{name: "model tree gated 401", modelStatus: http.StatusUnauthorized, modelGated: true, wantGated: true},
		{name: "model tree gated 403", modelStatus: http.StatusForbidden, modelGated: true, wantGated: true},
		{
			name: "dataset gated through fallback", modelStatus: http.StatusUnauthorized,
			datasetStatus: http.StatusUnauthorized, datasetGated: true,
			wantGated: true, wantDataset: true, wantDatasetReqs: 1,
		},
		{
			name: "explicit dataset gated", dataset: true,
			datasetStatus: http.StatusForbidden, datasetGated: true,
			wantGated: true, wantDataset: true, wantDatasetReqs: 1,
		},
		{
			name: "missing is not gated", modelStatus: http.StatusNotFound,
			datasetStatus: http.StatusNotFound, wantNotFound: true, wantDatasetReqs: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const (
				modelTree   = "/api/models/owner/repo/tree/main"
				datasetTree = "/api/datasets/owner/repo/tree/main"
			)
			var modelReqs, datasetReqs, authReqs int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == modelTree:
					modelReqs++
					if tc.modelGated {
						w.Header().Set("X-Error-Code", "GatedRepo")
						w.Header().Set("X-Error-Message", message)
					}
					w.WriteHeader(tc.modelStatus)
					if tc.modelStatus == http.StatusOK {
						_ = json.NewEncoder(w).Encode([]hfTreeNode{{Type: "file", Path: "data.txt", Size: 1}})
					}
				case r.URL.Path == datasetTree:
					datasetReqs++
					if tc.datasetGated {
						w.Header().Set("X-Error-Code", "GatedRepo")
						w.Header().Set("X-Error-Message", message)
					}
					w.WriteHeader(tc.datasetStatus)
					if tc.datasetStatus == http.StatusOK {
						_ = json.NewEncoder(w).Encode([]hfTreeNode{{Type: "file", Path: "data.txt", Size: 1}})
					}
				case strings.HasSuffix(r.URL.Path, "/auth-check"):
					authReqs++
					http.NotFound(w, r)
				default:
					t.Errorf("unexpected analysis request: %s", r.URL)
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()

			analyzer := NewAnalyzer(AnalyzerOptions{Endpoint: srv.URL, HTTPClient: srv.Client(), Token: "hf_secret"})
			info, err := analyzer.Analyze(context.Background(), "owner/repo", tc.dataset)

			switch {
			case tc.wantGated:
				var gated *GatedRepoError
				if !errors.As(err, &gated) {
					t.Fatalf("info = %+v, error = %v; want typed GatedRepoError", info, err)
				}
				if info != nil {
					t.Fatalf("gated analysis returned partial info: %+v", info)
				}
				if gated.IsDataset != tc.wantDataset || !gated.HasToken || gated.Message != message {
					t.Errorf("gated = %+v; want dataset=%v, token present, message %q", gated, tc.wantDataset, message)
				}
				wantAccept := srv.URL + "/owner/repo"
				if tc.wantDataset {
					wantAccept = srv.URL + "/datasets/owner/repo"
				}
				if gated.AcceptURL() != wantAccept {
					t.Errorf("accept URL = %q, want %q", gated.AcceptURL(), wantAccept)
				}
			case tc.wantNotFound:
				var gated *GatedRepoError
				if errors.As(err, &gated) {
					t.Fatalf("missing repository misclassified as gated: %+v", gated)
				}
				if info != nil || err == nil || err.Error() != "fetch file tree: repository not found as model or dataset: owner/repo" {
					t.Fatalf("info = %v, error = %v; want missing-repository error", info, err)
				}
			default:
				t.Fatalf("unexpected test case shape: %+v", tc)
			}

			if authReqs != 0 {
				t.Errorf("auth-check requests = %d, want 0 after a failed tree", authReqs)
			}
			wantDatasetReqs := tc.wantDatasetReqs
			if tc.dataset {
				if modelReqs != 0 {
					t.Errorf("model tree requests = %d, want 0 for explicit dataset", modelReqs)
				}
			} else if modelReqs != 1 {
				t.Errorf("model tree requests = %d, want 1", modelReqs)
			}
			if datasetReqs != wantDatasetReqs {
				t.Errorf("dataset tree requests = %d, want %d", datasetReqs, wantDatasetReqs)
			}
		})
	}
}

// TestAnalyzeModelWithRevision_ForcesModelNamespace pins the forced model path
// used to resolve a dual-type repository: only the model tree is read, then the
// model auth-check runs, and the dataset namespace is never probed.
func TestAnalyzeModelWithRevision_ForcesModelNamespace(t *testing.T) {
	const repo = "owner/repo"
	var modelReqs, datasetReqs, modelAuthReqs, datasetAuthReqs atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/models/"+repo+"/tree/main":
			modelReqs.Add(1)
			_ = json.NewEncoder(w).Encode([]hfTreeNode{{Type: "file", Path: "data.txt", Size: 1}})
		case r.URL.Path == "/api/datasets/"+repo+"/tree/main":
			datasetReqs.Add(1)
			_ = json.NewEncoder(w).Encode([]hfTreeNode{{Type: "file", Path: "data.txt", Size: 1}})
		case r.URL.Path == "/api/models/"+repo+"/auth-check":
			modelAuthReqs.Add(1)
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/api/datasets/"+repo+"/auth-check":
			datasetAuthReqs.Add(1)
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/api/models/"+repo+"/refs":
			_, _ = io.WriteString(w, `{"branches":[],"tags":[]}`)
		default:
			t.Errorf("unexpected analysis request: %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	analyzer := NewAnalyzer(AnalyzerOptions{Endpoint: srv.URL, HTTPClient: srv.Client()})
	info, err := analyzer.AnalyzeModelWithRevision(context.Background(), repo, "main")
	if err != nil {
		t.Fatalf("AnalyzeModelWithRevision: %v", err)
	}
	if info == nil || info.IsDataset || info.FileCount != 1 || info.Files[0].Path != "data.txt" {
		t.Fatalf("info = %+v; want one analyzed model file", info)
	}
	if modelReqs.Load() != 1 || datasetReqs.Load() != 0 || modelAuthReqs.Load() != 1 || datasetAuthReqs.Load() != 0 {
		t.Errorf("model/dataset/model-auth/dataset-auth requests = %d/%d/%d/%d, want 1/0/1/0",
			modelReqs.Load(), datasetReqs.Load(), modelAuthReqs.Load(), datasetAuthReqs.Load())
	}
}

// TestAnalyzeAutoDetectionStillReturnsBothExist preserves auto-detection:
// without a namespace selection, both trees are probed and the caller receives
// ErrBothExist so it can ask the user.
func TestAnalyzeAutoDetectionStillReturnsBothExist(t *testing.T) {
	const repo = "owner/repo"
	var modelReqs, datasetReqs, authReqs atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/models/"+repo+"/tree/main":
			modelReqs.Add(1)
			_ = json.NewEncoder(w).Encode([]hfTreeNode{{Type: "file", Path: "data.txt", Size: 1}})
		case r.URL.Path == "/api/datasets/"+repo+"/tree/main":
			datasetReqs.Add(1)
			_ = json.NewEncoder(w).Encode([]hfTreeNode{{Type: "file", Path: "data.txt", Size: 1}})
		case strings.HasSuffix(r.URL.Path, "/auth-check"):
			authReqs.Add(1)
			http.NotFound(w, r)
		default:
			t.Errorf("unexpected analysis request: %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	analyzer := NewAnalyzer(AnalyzerOptions{Endpoint: srv.URL, HTTPClient: srv.Client()})
	info, err := analyzer.Analyze(context.Background(), repo, false)
	if info != nil || !errors.Is(err, ErrBothExist) {
		t.Fatalf("info = %v, error = %v; want ErrBothExist", info, err)
	}
	if modelReqs.Load() != 1 || datasetReqs.Load() != 1 || authReqs.Load() != 0 {
		t.Errorf("model/dataset/auth requests = %d/%d/%d, want 1/1/0",
			modelReqs.Load(), datasetReqs.Load(), authReqs.Load())
	}
}
