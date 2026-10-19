// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
)

// TestAnalyzeGatedRepoResponse pins the HTTP 403 contract for a gated
// repository: structured code, repository identity, configured-endpoint accept
// URL, and boolean-only token state with no token leakage.
func TestAnalyzeGatedRepoResponse(t *testing.T) {
	const (
		repo     = "owner/repo"
		secret   = "hf_supersecret"
		upstream = "Access to this resource is gated."
	)
	for _, tc := range []struct {
		name    string
		dataset bool
		token   string
	}{
		{name: "model without token", dataset: false, token: ""},
		{name: "model with token", dataset: false, token: secret},
		{name: "dataset without token", dataset: true, token: ""},
		{name: "dataset with token", dataset: true, token: secret},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := "models"
			if tc.dataset {
				api = "datasets"
			}
			treePath := fmt.Sprintf("/api/%s/%s/tree/main", api, repo)
			authPath := fmt.Sprintf("/api/%s/%s/auth-check", api, repo)

			hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == authPath:
					wantAuth := ""
					if tc.token != "" {
						wantAuth = "Bearer " + tc.token
					}
					if got := r.Header.Get("Authorization"); got != wantAuth {
						t.Errorf("auth-check Authorization = %q, want %q", got, wantAuth)
					}
					w.Header().Set("X-Error-Code", "GatedRepo")
					w.Header().Set("X-Error-Message", upstream)
					w.WriteHeader(http.StatusUnauthorized)
				case r.URL.Path == treePath:
					_, _ = w.Write([]byte(`[{"type":"file","path":"data.txt","size":1}]`))
				case r.URL.Path == "/api/datasets/owner/repo/tree/main" && !tc.dataset:
					http.NotFound(w, r)
				case strings.HasSuffix(r.URL.Path, "/refs"):
					_, _ = w.Write([]byte(`{"branches":[],"tags":[]}`))
				default:
					t.Errorf("unexpected analysis request: %s", r.URL)
					http.NotFound(w, r)
				}
			}))
			defer hub.Close()

			s := newTestServerWithConfig(t, Config{
				Endpoint: hub.URL,
				Token:    tc.token,
				CacheDir: t.TempDir(),
				Proxy:    &hfdownloader.ProxyConfig{NoEnvProxy: true},
			})

			r := httptest.NewRequest(http.MethodGet, "/api/analyze/"+repo, nil)
			r.SetPathValue("repo", repo)
			if tc.dataset {
				r.URL.RawQuery = "dataset=true"
			}
			w := httptest.NewRecorder()
			s.handleAnalyze(w, r)

			if w.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403; body=%s", w.Code, w.Body.String())
			}

			var resp struct {
				OK          bool   `json:"ok"`
				Error       string `json:"error"`
				Details     string `json:"details"`
				ErrorDetail *struct {
					Code    string `json:"code"`
					Message string `json:"message"`
					Details string `json:"details"`
				} `json:"error_detail"`
				Gated     bool   `json:"gated"`
				Repo      string `json:"repo"`
				RepoType  string `json:"repo_type"`
				AcceptURL string `json:"accept_url"`
				HasToken  *bool  `json:"has_token"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode response: %v; body=%s", err, w.Body.String())
			}

			if resp.OK || resp.Error != "Gated repository" {
				t.Errorf("ok/error = %v/%q, want false/%q", resp.OK, resp.Error, "Gated repository")
			}
			if resp.Details == "" {
				t.Error("details is empty")
			}
			if resp.ErrorDetail == nil || resp.ErrorDetail.Code != "gated_repo" || resp.ErrorDetail.Message != "Gated repository" || resp.ErrorDetail.Details != resp.Details {
				t.Errorf("error_detail = %+v; want gated_repo with matching details", resp.ErrorDetail)
			}
			if !resp.Gated || resp.Repo != repo {
				t.Errorf("gated/repo = %v/%q, want true/%q", resp.Gated, resp.Repo, repo)
			}
			wantType := "model"
			wantAccept := hub.URL + "/" + repo
			if tc.dataset {
				wantType = "dataset"
				wantAccept = hub.URL + "/datasets/" + repo
			}
			if resp.RepoType != wantType || resp.AcceptURL != wantAccept {
				t.Errorf("repo_type/accept_url = %q/%q, want %q/%q", resp.RepoType, resp.AcceptURL, wantType, wantAccept)
			}
			if resp.HasToken == nil || *resp.HasToken != (tc.token != "") {
				t.Fatalf("has_token = %v, want boolean %v", resp.HasToken, tc.token != "")
			}

			// Guidance must match the non-secret token state.
			if tc.token == "" {
				if !strings.Contains(resp.Details, "Settings") {
					t.Errorf("no-token guidance does not point at Settings: %q", resp.Details)
				}
			} else if !strings.Contains(resp.Details, "token") {
				t.Errorf("token guidance does not mention the token: %q", resp.Details)
			}
			if tc.token != "" && strings.Contains(w.Body.String(), tc.token) {
				t.Fatalf("response leaked the configured token: %s", w.Body.String())
			}
		})
	}
}

// TestAnalyzeErrorResponseOmitsGatedFields preserves the existing error shape:
// gated-only optional fields must not appear on ordinary analyze errors.
func TestAnalyzeErrorResponseOmitsGatedFields(t *testing.T) {
	s := newTestServer(t)
	r := httptest.NewRequest(http.MethodGet, "/api/analyze/invalid", nil)
	r.SetPathValue("repo", "invalid")
	w := httptest.NewRecorder()
	s.handleAnalyze(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"gated", "repo", "repo_type", "accept_url", "has_token"} {
		if _, ok := body[key]; ok {
			t.Errorf("ordinary error response contains gated-only field %q: %s", key, w.Body.String())
		}
	}
}

// TestAnalyzeDualTypeReturnsNeedsSelection pins the wrapped-sentinel handling:
// when both namespaces exist, auto-detection must return the existing 200
// selection response rather than a 500 analysis failure, and it must not carry
// gated-only fields.
func TestAnalyzeDualTypeReturnsNeedsSelection(t *testing.T) {
	const repo = "owner/repo"
	var modelReqs, datasetReqs, authReqs atomic.Int64
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/models/"+repo+"/tree/main":
			modelReqs.Add(1)
			_, _ = w.Write([]byte(`[{"type":"file","path":"data.txt","size":1}]`))
		case r.URL.Path == "/api/datasets/"+repo+"/tree/main":
			datasetReqs.Add(1)
			_, _ = w.Write([]byte(`[{"type":"file","path":"data.txt","size":1}]`))
		case strings.HasSuffix(r.URL.Path, "/auth-check"):
			authReqs.Add(1)
			http.NotFound(w, r)
		default:
			t.Errorf("unexpected analysis request: %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer hub.Close()

	s := newTestServerWithConfig(t, Config{
		Endpoint: hub.URL,
		CacheDir: t.TempDir(),
		Proxy:    &hfdownloader.ProxyConfig{NoEnvProxy: true},
	})

	r := httptest.NewRequest(http.MethodGet, "/api/analyze/"+repo, nil)
	r.SetPathValue("repo", repo)
	w := httptest.NewRecorder()
	s.handleAnalyze(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		NeedsSelection bool     `json:"needsSelection"`
		Repo           string   `json:"repo"`
		Options        []string `json:"options"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, w.Body.String())
	}
	if !resp.NeedsSelection || resp.Repo != repo {
		t.Errorf("needsSelection/repo = %v/%q, want true/%q", resp.NeedsSelection, resp.Repo, repo)
	}
	if len(resp.Options) != 2 || resp.Options[0] != "model" || resp.Options[1] != "dataset" {
		t.Errorf("options = %v, want [model dataset]", resp.Options)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"gated", "repo_type", "accept_url", "has_token"} {
		if _, ok := raw[key]; ok {
			t.Errorf("selection response contains gated-only field %q: %s", key, w.Body.String())
		}
	}
	if modelReqs.Load() != 1 || datasetReqs.Load() != 1 || authReqs.Load() != 0 {
		t.Errorf("model/dataset/auth requests = %d/%d/%d, want 1/1/0", modelReqs.Load(), datasetReqs.Load(), authReqs.Load())
	}
}

// TestAnalyzeForceModelResolvesDualType verifies that type=model analyzes the
// model namespace directly instead of probing the dataset tree and returning
// needsSelection again.
func TestAnalyzeForceModelResolvesDualType(t *testing.T) {
	const repo = "owner/repo"
	var modelReqs, datasetReqs, modelAuthReqs, refsReqs atomic.Int64
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/models/"+repo+"/tree/main":
			modelReqs.Add(1)
			_, _ = w.Write([]byte(`[{"type":"file","path":"data.txt","size":1}]`))
		case r.URL.Path == "/api/datasets/"+repo+"/tree/main":
			datasetReqs.Add(1)
			_, _ = w.Write([]byte(`[{"type":"file","path":"data.txt","size":1}]`))
		case r.URL.Path == "/api/models/"+repo+"/auth-check":
			modelAuthReqs.Add(1)
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/api/models/"+repo+"/refs":
			refsReqs.Add(1)
			_, _ = w.Write([]byte(`{"branches":[],"tags":[]}`))
		default:
			t.Errorf("unexpected analysis request: %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer hub.Close()

	s := newTestServerWithConfig(t, Config{
		Endpoint: hub.URL,
		CacheDir: t.TempDir(),
		Proxy:    &hfdownloader.ProxyConfig{NoEnvProxy: true},
	})

	r := httptest.NewRequest(http.MethodGet, "/api/analyze/"+repo+"?type=model", nil)
	r.SetPathValue("repo", repo)
	w := httptest.NewRecorder()
	s.handleAnalyze(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Repo      string `json:"repo"`
		IsDataset bool   `json:"is_dataset"`
		FileCount int    `json:"file_count"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, w.Body.String())
	}
	if resp.Repo != repo || resp.IsDataset || resp.FileCount != 1 {
		t.Errorf("response = %+v, want model analysis of %q with one file", resp, repo)
	}
	if modelReqs.Load() != 1 || datasetReqs.Load() != 0 || modelAuthReqs.Load() != 1 || refsReqs.Load() != 1 {
		t.Errorf("model/dataset/auth/refs requests = %d/%d/%d/%d, want 1/0/1/1",
			modelReqs.Load(), datasetReqs.Load(), modelAuthReqs.Load(), refsReqs.Load())
	}
}

// TestAnalyzeForceModelGatedRepoResponse verifies that the forced model path
// still runs the model auth-check and returns the structured 403 for a gated
// repository, without probing the dataset namespace or leaking the token.
func TestAnalyzeForceModelGatedRepoResponse(t *testing.T) {
	const (
		repo      = "owner/repo"
		secret    = "hf_force_model_secret"
		upstream  = "Access to this resource is gated."
		acceptURL = "/owner/repo"
	)
	var modelReqs, datasetReqs, authReqs atomic.Int64
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/models/"+repo+"/tree/main":
			modelReqs.Add(1)
			_, _ = w.Write([]byte(`[{"type":"file","path":"data.txt","size":1}]`))
		case r.URL.Path == "/api/datasets/"+repo+"/tree/main":
			datasetReqs.Add(1)
			_, _ = w.Write([]byte(`[{"type":"file","path":"data.txt","size":1}]`))
		case r.URL.Path == "/api/models/"+repo+"/auth-check":
			authReqs.Add(1)
			if got := r.Header.Get("Authorization"); got != "Bearer "+secret {
				t.Errorf("auth-check Authorization = %q, want Bearer token", got)
			}
			w.Header().Set("X-Error-Code", "GatedRepo")
			w.Header().Set("X-Error-Message", upstream)
			w.WriteHeader(http.StatusUnauthorized)
		default:
			t.Errorf("unexpected analysis request: %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer hub.Close()

	s := newTestServerWithConfig(t, Config{
		Endpoint: hub.URL,
		Token:    secret,
		CacheDir: t.TempDir(),
		Proxy:    &hfdownloader.ProxyConfig{NoEnvProxy: true},
	})

	r := httptest.NewRequest(http.MethodGet, "/api/analyze/"+repo+"?type=model", nil)
	r.SetPathValue("repo", repo)
	w := httptest.NewRecorder()
	s.handleAnalyze(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		ErrorDetail *struct {
			Code string `json:"code"`
		} `json:"error_detail"`
		Gated     bool   `json:"gated"`
		Repo      string `json:"repo"`
		RepoType  string `json:"repo_type"`
		AcceptURL string `json:"accept_url"`
		HasToken  *bool  `json:"has_token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, w.Body.String())
	}
	if resp.ErrorDetail == nil || resp.ErrorDetail.Code != "gated_repo" {
		t.Errorf("error_detail = %+v, want gated_repo", resp.ErrorDetail)
	}
	if !resp.Gated || resp.Repo != repo || resp.RepoType != "model" {
		t.Errorf("gated/repo/repo_type = %v/%q/%q, want true/%q/model", resp.Gated, resp.Repo, resp.RepoType, repo)
	}
	if resp.AcceptURL != hub.URL+acceptURL {
		t.Errorf("accept_url = %q, want %q", resp.AcceptURL, hub.URL+acceptURL)
	}
	if resp.HasToken == nil || !*resp.HasToken {
		t.Errorf("has_token = %v, want true", resp.HasToken)
	}
	if strings.Contains(w.Body.String(), secret) {
		t.Fatalf("response leaked the configured token: %s", w.Body.String())
	}
	if modelReqs.Load() != 1 || datasetReqs.Load() != 0 || authReqs.Load() != 1 {
		t.Errorf("model/dataset/auth requests = %d/%d/%d, want 1/0/1",
			modelReqs.Load(), datasetReqs.Load(), authReqs.Load())
	}
}

// TestAnalyzeDatasetSelectionStaysDataset preserves the historical
// dataset=true path: the model namespace is not probed and the dataset
// auth-check runs for the selected namespace.
func TestAnalyzeDatasetSelectionStaysDataset(t *testing.T) {
	const repo = "owner/repo"
	var modelReqs, datasetReqs, datasetAuthReqs, refsReqs atomic.Int64
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/models/"+repo+"/tree/main":
			modelReqs.Add(1)
			_, _ = w.Write([]byte(`[{"type":"file","path":"data.txt","size":1}]`))
		case r.URL.Path == "/api/datasets/"+repo+"/tree/main":
			datasetReqs.Add(1)
			_, _ = w.Write([]byte(`[{"type":"file","path":"data.txt","size":1}]`))
		case r.URL.Path == "/api/datasets/"+repo+"/auth-check":
			datasetAuthReqs.Add(1)
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/api/datasets/"+repo+"/refs":
			refsReqs.Add(1)
			_, _ = w.Write([]byte(`{"branches":[],"tags":[]}`))
		default:
			t.Errorf("unexpected analysis request: %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer hub.Close()

	s := newTestServerWithConfig(t, Config{
		Endpoint: hub.URL,
		CacheDir: t.TempDir(),
		Proxy:    &hfdownloader.ProxyConfig{NoEnvProxy: true},
	})

	r := httptest.NewRequest(http.MethodGet, "/api/analyze/"+repo+"?dataset=true", nil)
	r.SetPathValue("repo", repo)
	w := httptest.NewRecorder()
	s.handleAnalyze(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Repo      string `json:"repo"`
		IsDataset bool   `json:"is_dataset"`
		FileCount int    `json:"file_count"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, w.Body.String())
	}
	if resp.Repo != repo || !resp.IsDataset || resp.FileCount != 1 {
		t.Errorf("response = %+v, want dataset analysis of %q with one file", resp, repo)
	}
	if modelReqs.Load() != 0 || datasetReqs.Load() != 1 || datasetAuthReqs.Load() != 1 || refsReqs.Load() != 1 {
		t.Errorf("model/dataset/auth/refs requests = %d/%d/%d/%d, want 0/1/1/1",
			modelReqs.Load(), datasetReqs.Load(), datasetAuthReqs.Load(), refsReqs.Load())
	}
}

// TestAnalyzeRejectsInvalidTypeSelection pins the 400 contract for an invalid
// type value and for conflicting type/dataset selections.
func TestAnalyzeRejectsInvalidTypeSelection(t *testing.T) {
	const repo = "owner/repo"
	for _, tc := range []struct {
		name        string
		query       string
		wantDetails string
	}{
		{name: "conflicting model and dataset", query: "type=model&dataset=true", wantDetails: "cannot be combined"},
		{name: "dataset type value", query: "type=dataset", wantDetails: "Expected type=model"},
		{name: "unknown type value", query: "type=invalid", wantDetails: "Expected type=model"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			r := httptest.NewRequest(http.MethodGet, "/api/analyze/"+repo+"?"+tc.query, nil)
			r.SetPathValue("repo", repo)
			w := httptest.NewRecorder()
			s.handleAnalyze(w, r)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
			}
			var resp struct {
				Details     string `json:"details"`
				ErrorDetail *struct {
					Code string `json:"code"`
				} `json:"error_detail"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode response: %v; body=%s", err, w.Body.String())
			}
			if resp.ErrorDetail == nil || resp.ErrorDetail.Code != "bad_request" {
				t.Errorf("error_detail = %+v, want bad_request", resp.ErrorDetail)
			}
			if !strings.Contains(resp.Details, tc.wantDetails) {
				t.Errorf("details = %q, want it to contain %q", resp.Details, tc.wantDetails)
			}
		})
	}
}
