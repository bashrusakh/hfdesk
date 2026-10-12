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
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("ambiguous plan status=%d body=%s; want explicit refusal", response.Code, response.Body.String())
	}
	var refusal map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &refusal); err != nil || refusal["error"] == nil || !strings.Contains(strings.ToLower(response.Body.String()), "ambiguous") {
		t.Fatalf("ambiguous plan error payload=%s err=%v", response.Body.String(), err)
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
