// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDatasetAnalyzeSelectionAndPlanAPI(t *testing.T) {
	files := []map[string]any{
		{"type": "file", "path": "train.csv", "size": 10},
		{"type": "file", "path": "test_data.csv", "size": 10},
		{"type": "file", "path": "data/customdata/chunk.arrow", "size": 10},
		{"type": "file", "path": "default.parquet", "size": 10},
		{"type": "file", "path": "unknown.json", "size": 10},
		{"type": "file", "path": "config.json", "size": 10},
		{"type": "file", "path": "dataset_info.json", "size": 10},
		{"type": "file", "path": "README.md", "size": 10},
	}
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/api/datasets/owner/data/tree/rev1"):
			_ = json.NewEncoder(w).Encode(files)
		case strings.Contains(r.URL.Path, "/api/datasets/owner/data/revision/rev1"):
			_, _ = w.Write([]byte(`{"sha":"rev1"}`))
		case strings.Contains(r.URL.Path, "/api/datasets/owner/data/refs"):
			_, _ = w.Write([]byte(`{"branches":[]}`))
		case strings.Contains(r.URL.Path, "/raw/"):
			http.NotFound(w, r) // Optional dataset metadata is absent in this fixture.
		default:
			http.NotFound(w, r)
		}
	}))
	defer hub.Close()

	srv := newTestServerWithConfig(t, Config{CacheDir: t.TempDir(), Endpoint: hub.URL})
	mux := http.NewServeMux()
	srv.registerAPIRoutes(mux)

	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/analyze/owner/data?dataset=true&revision=rev1", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("analyze status=%d body=%s", response.Code, response.Body.String())
	}
	var analyzed struct {
		SelectableItems []struct {
			ID          string `json:"id"`
			FilterValue string `json:"filter_value"`
		} `json:"selectable_items"`
		RecommendedDownload struct {
			Filters    []string `json:"filters"`
			ExactMatch bool     `json:"exactMatch"`
		} `json:"recommended_download"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &analyzed); err != nil {
		t.Fatal(err)
	}
	if !analyzed.RecommendedDownload.ExactMatch || !reflect.DeepEqual(analyzed.RecommendedDownload.Filters, []string{"train"}) {
		t.Fatalf("recommended selection = %+v", analyzed.RecommendedDownload)
	}
	filters := map[string]string{}
	for _, item := range analyzed.SelectableItems {
		filters[item.ID] = item.FilterValue
	}
	for _, group := range []string{"train", "test", "customdata", "default"} {
		if filters[group] != group {
			t.Errorf("selectable %s filter=%q; want %q", group, filters[group], group)
		}
	}
	if _, ok := filters["unknown"]; ok {
		t.Fatal("ambiguous JSON was advertised as a selectable group")
	}

	postPlan := func(filters, excludes []string) *httptest.ResponseRecorder {
		t.Helper()
		body, _ := json.Marshal(DownloadRequest{Repo: "owner/data", Revision: "rev1", Dataset: true, Filters: filters, Excludes: excludes, ExactMatch: true})
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/plan", bytes.NewReader(body)))
		return w
	}
	response = postPlan(nil, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("unfiltered plan status=%d body=%s", response.Code, response.Body.String())
	}
	var unfiltered PlanResponse
	if err := json.Unmarshal(response.Body.Bytes(), &unfiltered); err != nil {
		t.Fatal(err)
	}
	if len(unfiltered.Files) != len(files) {
		t.Fatalf("unfiltered plan has %d entries; want all %d: %+v", len(unfiltered.Files), len(files), unfiltered.Files)
	}
	response = postPlan(analyzed.RecommendedDownload.Filters, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("plan status=%d body=%s; unknown root JSON must be skipped, not refused", response.Code, response.Body.String())
	}
	var trainPlan PlanResponse
	if err := json.Unmarshal(response.Body.Bytes(), &trainPlan); err != nil {
		t.Fatal(err)
	}
	var trainPaths []string
	for _, file := range trainPlan.Files {
		trainPaths = append(trainPaths, file.Path)
	}
	if want := []string{"train.csv", "config.json", "dataset_info.json", "README.md"}; !reflect.DeepEqual(trainPaths, want) {
		t.Fatalf("train plan paths=%v, want %v", trainPaths, want)
	}
	response = postPlan([]string{"customdata", "default"}, []string{"unknown.json"})
	if response.Code != http.StatusOK {
		t.Fatalf("excluded ambiguity plan status=%d body=%s", response.Code, response.Body.String())
	}
	var plan PlanResponse
	if err := json.Unmarshal(response.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, file := range plan.Files {
		got = append(got, file.Path)
	}
	want := []string{"data/customdata/chunk.arrow", "default.parquet", "config.json", "dataset_info.json", "README.md"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("selected plan paths=%v, want %v", got, want)
	}
}

func TestDatasetAlternativeAmbiguityPlanAndDryRunAPI(t *testing.T) {
	files := []map[string]any{{"type": "file", "path": "train_test.csv", "size": 10}}
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/api/datasets/owner/data/tree/rev1"):
			_ = json.NewEncoder(w).Encode(files)
		case strings.Contains(r.URL.Path, "/api/datasets/owner/data/revision/rev1"):
			_, _ = w.Write([]byte(`{"sha":"rev1"}`))
		case strings.Contains(r.URL.Path, "/api/datasets/owner/data/refs"):
			_, _ = w.Write([]byte(`{"branches":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer hub.Close()
	srv := newTestServerWithConfig(t, Config{CacheDir: t.TempDir(), Endpoint: hub.URL})
	mux := http.NewServeMux()
	srv.registerAPIRoutes(mux)
	for _, label := range []string{"train", "test"} {
		for _, route := range []struct {
			path string
			dry  bool
		}{{"/api/plan", false}, {"/api/download", true}} {
			body, _ := json.Marshal(DownloadRequest{Repo: "owner/data", Revision: "rev1", Dataset: true, Filters: []string{label}, ExactMatch: true, DryRun: route.dry})
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, route.path, bytes.NewReader(body)))
			var response ErrorResponse
			if w.Code != http.StatusBadRequest || json.Unmarshal(w.Body.Bytes(), &response) != nil || response.ErrorDetail == nil || response.ErrorDetail.Code != "ambiguous_selection" {
				t.Errorf("%s label=%s status=%d body=%s; want 400 ambiguous_selection", route.path, label, w.Code, w.Body.String())
			}
		}
	}
}

func TestDatasetAlternativeAsyncAmbiguityDownloadFailsJob(t *testing.T) {
	files := []map[string]any{{"type": "file", "path": "train_test.csv", "size": 10}}
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/api/datasets/owner/data/tree/rev1"):
			_ = json.NewEncoder(w).Encode(files)
		case strings.Contains(r.URL.Path, "/api/datasets/owner/data/revision/rev1"):
			_, _ = w.Write([]byte(`{"sha":"rev1"}`))
		case strings.Contains(r.URL.Path, "/api/datasets/owner/data/refs"):
			_, _ = w.Write([]byte(`{"branches":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer hub.Close()
	srv := newTestServerWithConfig(t, Config{CacheDir: t.TempDir(), Endpoint: hub.URL})
	mux := http.NewServeMux()
	srv.registerAPIRoutes(mux)
	for _, label := range []string{"train", "test"} {
		body, _ := json.Marshal(DownloadRequest{Repo: "owner/data", Revision: "rev1", Dataset: true, Filters: []string{label}, ExactMatch: true})
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/download", bytes.NewReader(body)))
		if w.Code != http.StatusAccepted {
			t.Fatalf("download label=%s status=%d body=%s; want 202 admitted job", label, w.Code, w.Body.String())
		}
		var admitted Job
		if err := json.Unmarshal(w.Body.Bytes(), &admitted); err != nil {
			t.Fatalf("download label=%s response: %v", label, err)
		}
		if admitted.ID == "" {
			t.Fatalf("download label=%s admitted without job id: %s", label, w.Body.String())
		}
		job := waitJobStatus(t, srv.jobs, admitted.ID, JobStatusFailed, 5*time.Second)
		if job.Status != JobStatusFailed {
			t.Fatalf("job label=%s status=%s err=%q; want failed", label, job.Status, job.Error)
		}
		if !strings.Contains(job.Error, "ambiguous emitted set") || !strings.Contains(job.Error, "train_test.csv") {
			t.Fatalf("job label=%s error=%q; want ambiguous emitted-set error", label, job.Error)
		}
	}
}

func TestDatasetRecommendedTrainPlanKeepsMatchedGGUFCompanions(t *testing.T) {
	files := []map[string]any{
		{"type": "file", "path": "train_records.csv", "size": 10},
		{"type": "file", "path": "model-train.gguf", "size": 100, "lfs": map[string]any{"size": 100}},
		{"type": "file", "path": "model-train-lfs.gguf", "size": 20},
	}
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/api/datasets/owner/data/tree/rev1"):
			_ = json.NewEncoder(w).Encode(files)
		case strings.Contains(r.URL.Path, "/api/datasets/owner/data/revision/rev1"):
			_, _ = w.Write([]byte(`{"sha":"rev1"}`))
		case strings.Contains(r.URL.Path, "/api/datasets/owner/data/refs"):
			_, _ = w.Write([]byte(`{"branches":[]}`))
		case strings.Contains(r.URL.Path, "/raw/"):
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer hub.Close()

	srv := newTestServerWithConfig(t, Config{CacheDir: t.TempDir(), Endpoint: hub.URL})
	mux := http.NewServeMux()
	srv.registerAPIRoutes(mux)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/analyze/owner/data?dataset=true&revision=rev1", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("analyze status=%d body=%s", response.Code, response.Body.String())
	}
	var analyzed struct {
		SelectableItems []struct {
			ID string `json:"id"`
		} `json:"selectable_items"`
		RecommendedDownload struct {
			Filters    []string `json:"filters"`
			ExactMatch bool     `json:"exactMatch"`
		} `json:"recommended_download"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &analyzed); err != nil {
		t.Fatal(err)
	}
	if !analyzed.RecommendedDownload.ExactMatch || !reflect.DeepEqual(analyzed.RecommendedDownload.Filters, []string{"train"}) {
		t.Fatalf("recommended selection=%+v", analyzed.RecommendedDownload)
	}
	trainAdvertised := false
	for _, item := range analyzed.SelectableItems {
		trainAdvertised = trainAdvertised || item.ID == "train"
	}
	if !trainAdvertised {
		t.Fatal("analyzer did not advertise the train group")
	}
	request := DownloadRequest{Repo: "owner/data", Revision: "rev1", Dataset: true, Filters: analyzed.RecommendedDownload.Filters, ExactMatch: analyzed.RecommendedDownload.ExactMatch, AppendFilterSubdir: true}
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/plan", bytes.NewReader(body)))
	if response.Code != http.StatusOK {
		t.Fatalf("plan status=%d body=%s", response.Code, response.Body.String())
	}
	var plan PlanResponse
	if err := json.Unmarshal(response.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	paths := make([]string, 0, len(plan.Files))
	for _, file := range plan.Files {
		paths = append(paths, file.Path)
	}
	if want := []string{"train_records.csv", "model-train.gguf", "model-train-lfs.gguf"}; !reflect.DeepEqual(paths, want) {
		t.Fatalf("selected plan paths=%v, want %v", paths, want)
	}
	lfsByPath := make(map[string]bool, len(plan.Files))
	for _, file := range plan.Files {
		lfsByPath[file.Path] = file.LFS
	}
	if !lfsByPath["model-train.gguf"] || lfsByPath["model-train-lfs.gguf"] {
		t.Fatalf("plan lost LFS/non-LFS representation: %v", lfsByPath)
	}
	request.AppendFilterSubdir = false
	body, err = json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/plan", bytes.NewReader(body)))
	if response.Code != http.StatusOK {
		t.Fatalf("append-disabled plan status=%d body=%s", response.Code, response.Body.String())
	}
	if err := json.Unmarshal(response.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	paths = paths[:0]
	for _, file := range plan.Files {
		paths = append(paths, file.Path)
	}
	if want := []string{"train_records.csv", "model-train.gguf", "model-train-lfs.gguf"}; !reflect.DeepEqual(paths, want) {
		t.Fatalf("append-disabled plan paths=%v; want legacy repository-relative paths %v", paths, want)
	}
}
