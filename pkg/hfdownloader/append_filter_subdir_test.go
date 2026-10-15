// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDownload_NonLFSModelFilterDoesNotChangeDestination(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/revision/main"):
			_ = json.NewEncoder(w).Encode(RepoInfo{SHA: "deadbeef"})
		case strings.Contains(r.URL.Path, "/tree/"):
			_ = json.NewEncoder(w).Encode([]hfNode{{Type: "file", Path: "config.json", Size: 4}})
		case r.URL.Path == "/owner/model/raw/main/config.json":
			_, _ = w.Write([]byte("data"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	for _, tc := range []struct {
		name     string
		append   bool
		rootFile bool
	}{
		{name: "append with existing root file", append: true, rootFile: true},
		{name: "append into clean target", append: true},
		{name: "append disabled control", append: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := t.TempDir()
			base, err := DestinationBase(out, "owner/model")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(base, 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.rootFile {
				if err := os.WriteFile(filepath.Join(base, "config.json"), []byte("old"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			err = Download(context.Background(), Job{
				Repo: "owner/model", Revision: "main", Filters: []string{"config.json"}, AppendFilterSubdir: tc.append,
			}, Settings{Endpoint: server.URL, OutputDir: out, Concurrency: 1, MaxActiveDownloads: 1}, nil)
			if err != nil {
				t.Fatalf("Download: %v", err)
			}
			got, err := os.ReadFile(filepath.Join(base, "config.json"))
			if err != nil {
				t.Fatalf("read destination config.json: %v", err)
			}
			if string(got) != "data" {
				t.Errorf("config.json = %q, want downloaded content", got)
			}
		})
	}
}

func TestDownload_RetainedModelVocabKeepsRepoRelativeDestination(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/revision/main"):
			_ = json.NewEncoder(w).Encode(RepoInfo{SHA: "deadbeef"})
		case strings.Contains(r.URL.Path, "/tree/"):
			_ = json.NewEncoder(w).Encode([]hfNode{
				{Type: "file", Path: "weights/model.safetensors", LFS: &hfLfsInfo{Size: 6}},
				{Type: "file", Path: "arbitrary/assets/vocab.txt", Size: 4},
			})
		case r.URL.Path == "/owner/model/resolve/main/weights/model.safetensors":
			_, _ = w.Write([]byte("weight"))
		case r.URL.Path == "/owner/model/raw/main/arbitrary/assets/vocab.txt":
			_, _ = w.Write([]byte("vocab"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	out := t.TempDir()
	base, err := DestinationBase(out, "owner/model")
	if err != nil {
		t.Fatal(err)
	}
	err = Download(context.Background(), Job{
		Repo: "owner/model", Revision: "main", Filters: []string{"weights/model.safetensors"}, ExactMatch: true, AppendFilterSubdir: true,
	}, Settings{Endpoint: server.URL, OutputDir: out, Concurrency: 1, MaxActiveDownloads: 1}, nil)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(base, "arbitrary/assets/vocab.txt"))
	if err != nil || string(got) != "vocab" {
		t.Fatalf("repo-relative vocab = %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(base, "weights", "arbitrary/assets/vocab.txt")); !os.IsNotExist(err) {
		t.Fatalf("retained vocab unexpectedly relocated under filter directory (stat err=%v)", err)
	}
}

func TestDownload_DatasetOrdinaryNonLFSFilterPreservesRootDestinations(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/revision/main"):
			_ = json.NewEncoder(w).Encode(RepoInfo{SHA: "deadbeef"})
		case strings.Contains(r.URL.Path, "/tree/"):
			_ = json.NewEncoder(w).Encode([]hfNode{
				{Type: "file", Path: "train.csv", Size: 4},
				{Type: "file", Path: "config.json", Size: 4},
			})
		case r.URL.Path == "/datasets/owner/data/raw/main/train.csv":
			_, _ = w.Write([]byte("data"))
		case r.URL.Path == "/datasets/owner/data/raw/main/config.json":
			_, _ = w.Write([]byte("meta"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	out := t.TempDir()
	base, err := DestinationBase(out, "owner/data")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "train.csv"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "config.json"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanRepo(context.Background(), Job{
		Repo: "owner/data", Revision: "main", IsDataset: true,
		Filters: []string{"train.csv"}, ExactMatch: true, AppendFilterSubdir: true,
	}, Settings{Endpoint: server.URL})
	if err != nil {
		t.Fatalf("PlanRepo: %v", err)
	}
	var selected *PlanItem
	for i := range plan.Items {
		if plan.Items[i].RelativePath == "train.csv" {
			selected = &plan.Items[i]
		}
	}
	if selected == nil || selected.Subdir != "train.csv" || selected.destinationSubdir != "" {
		t.Fatalf("dataset selection proof/attribution/destination = %+v; want attribution train.csv and unchanged destination", selected)
	}
	err = Download(context.Background(), Job{
		Repo: "owner/data", Revision: "main", IsDataset: true,
		Filters: []string{"train.csv"}, ExactMatch: true, AppendFilterSubdir: true,
	}, Settings{Endpoint: server.URL, OutputDir: out, Concurrency: 1, MaxActiveDownloads: 1}, nil)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	for name, want := range map[string]string{"train.csv": "data", "config.json": "meta"} {
		got, err := os.ReadFile(filepath.Join(base, name))
		if err != nil {
			t.Fatalf("read preserved root destination %s: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("%s=%q, want %q", name, got, want)
		}
	}

	controlOut := t.TempDir()
	controlBase, err := DestinationBase(controlOut, "owner/data")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(controlBase, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(controlBase, "train.csv"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Download(context.Background(), Job{
		Repo: "owner/data", Revision: "main", IsDataset: true,
		Filters: []string{"train.csv"}, ExactMatch: true, AppendFilterSubdir: false,
	}, Settings{Endpoint: server.URL, OutputDir: controlOut, Concurrency: 1, MaxActiveDownloads: 1}, nil); err != nil {
		t.Fatalf("Download with AppendFilterSubdir=false: %v", err)
	}
	controlBytes, err := os.ReadFile(filepath.Join(controlBase, "train.csv"))
	if err != nil || string(controlBytes) != "data" {
		t.Fatalf("append-disabled control destination = %q, err=%v; want downloaded data at repo root", controlBytes, err)
	}
}

func TestDownload_DatasetGGUFNonLFSFilterKeepsAppendedDestination(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/revision/main"):
			_ = json.NewEncoder(w).Encode(RepoInfo{SHA: "deadbeef"})
		case strings.Contains(r.URL.Path, "/tree/"):
			_ = json.NewEncoder(w).Encode([]hfNode{
				{Type: "file", Path: "Q4_K_M/model-Q4_K_M.gguf", Size: 4},
				{Type: "file", Path: "README-Q4_K_M.md", Size: 4},
				{Type: "file", Path: "train.csv", Size: 4},
			})
		case r.URL.Path == "/datasets/owner/data/raw/main/Q4_K_M/model-Q4_K_M.gguf":
			_, _ = w.Write([]byte("gguf"))
		case r.URL.Path == "/datasets/owner/data/raw/main/README-Q4_K_M.md":
			_, _ = w.Write([]byte("read"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	out := t.TempDir()
	base, err := DestinationBase(out, "owner/data")
	if err != nil {
		t.Fatal(err)
	}
	job := Job{
		Repo: "owner/data", Revision: "main", IsDataset: true,
		Filters: []string{"Q4_K_M"}, ExactMatch: true, AppendFilterSubdir: true,
	}
	settings := Settings{Endpoint: server.URL, OutputDir: out, Concurrency: 1, MaxActiveDownloads: 1}
	plan, err := PlanRepo(context.Background(), job, settings)
	if err != nil {
		t.Fatalf("PlanRepo: %v", err)
	}
	for _, rel := range []string{"Q4_K_M/model-Q4_K_M.gguf", "README-Q4_K_M.md"} {
		found := false
		for _, item := range plan.Items {
			if item.RelativePath == rel {
				found = true
				if item.Subdir != "Q4_K_M" || item.destinationSubdir != "Q4_K_M" {
					t.Errorf("%s attribution/destination = %q/%q, want Q4_K_M/Q4_K_M", rel, item.Subdir, item.destinationSubdir)
				}
			}
		}
		if !found {
			t.Errorf("plan omitted direct GGUF-mode selection %s", rel)
		}
	}
	var planPaths []string
	if err := Download(context.Background(), job, settings, func(event ProgressEvent) {
		if event.Event == "plan_item" {
			planPaths = append(planPaths, event.Path)
		}
	}); err != nil {
		t.Fatalf("Download: %v", err)
	}
	wantPaths := map[string]bool{
		"Q4_K_M/Q4_K_M/model-Q4_K_M.gguf": true,
		"Q4_K_M/README-Q4_K_M.md":         true,
	}
	for _, p := range planPaths {
		if !wantPaths[p] {
			t.Errorf("unexpected plan_item path %q", p)
		}
		delete(wantPaths, p)
	}
	for p := range wantPaths {
		t.Errorf("missing plan_item path %q", p)
	}
	for rel, want := range map[string]string{
		"Q4_K_M/Q4_K_M/model-Q4_K_M.gguf": "gguf",
		"Q4_K_M/README-Q4_K_M.md":         "read",
	} {
		got, err := os.ReadFile(filepath.Join(base, filepath.FromSlash(rel)))
		if err != nil || string(got) != want {
			t.Errorf("destination %s = %q, err=%v; want %q", rel, got, err, want)
		}
	}
	if _, err := os.Stat(filepath.Join(base, "train.csv")); !os.IsNotExist(err) {
		t.Errorf("unmatched dataset payload exists at root: err=%v", err)
	}

	cacheRoot := t.TempDir()
	cacheSettings := Settings{
		Endpoint: server.URL, CacheDir: cacheRoot,
		Concurrency: 1, MaxActiveDownloads: 1,
	}
	if err := Download(context.Background(), job, cacheSettings, nil); err != nil {
		t.Fatalf("Download to HF cache: %v", err)
	}
	for _, rel := range []string{"Q4_K_M/model-Q4_K_M.gguf", "README-Q4_K_M.md"} {
		friendly := filepath.Join(cacheRoot, "datasets", "owner", "data", "Q4_K_M", filepath.FromSlash(rel))
		if _, err := os.Stat(friendly); err != nil {
			t.Errorf("HF-friendly destination %s: %v", friendly, err)
		}
		snapshot := filepath.Join(cacheRoot, "hub", "datasets--owner--data", "snapshots", "deadbeef", filepath.FromSlash(rel))
		if _, err := os.Stat(snapshot); err != nil {
			t.Errorf("HF snapshot path %s: %v", snapshot, err)
		}
	}

	noAppendOut := t.TempDir()
	noAppendSettings := Settings{Endpoint: server.URL, OutputDir: noAppendOut, Concurrency: 1, MaxActiveDownloads: 1}
	noAppendJob := job
	noAppendJob.AppendFilterSubdir = false
	if err := Download(context.Background(), noAppendJob, noAppendSettings, nil); err != nil {
		t.Fatalf("Download with AppendFilterSubdir=false: %v", err)
	}
	noAppendBase, err := DestinationBase(noAppendOut, "owner/data")
	if err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]string{
		"Q4_K_M/model-Q4_K_M.gguf": "gguf",
		"README-Q4_K_M.md":         "read",
	} {
		got, err := os.ReadFile(filepath.Join(noAppendBase, filepath.FromSlash(rel)))
		if err != nil || string(got) != want {
			t.Errorf("append-disabled destination %s = %q, err=%v; want %q", rel, got, err, want)
		}
	}
}

func TestDownload_DatasetGroupDestinationIndependentOfMatchedGGUF(t *testing.T) {
	for _, withGGUF := range []bool{false, true} {
		t.Run(fmt.Sprintf("matched_gguf_%t", withGGUF), func(t *testing.T) {
			files := []hfNode{{Type: "file", Path: "train_records.csv", Size: 4}}
			if withGGUF {
				files = append(files, hfNode{Type: "file", Path: "model-train.gguf", Size: 4})
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/revision/main"):
					_ = json.NewEncoder(w).Encode(RepoInfo{SHA: "deadbeef"})
				case strings.Contains(r.URL.Path, "/tree/"):
					_ = json.NewEncoder(w).Encode(files)
				case strings.HasSuffix(r.URL.Path, "/train_records.csv"):
					_, _ = w.Write([]byte("data"))
				case strings.HasSuffix(r.URL.Path, "/model-train.gguf"):
					_, _ = w.Write([]byte("model"))
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			out := t.TempDir()
			job := Job{Repo: "owner/data", Revision: "main", IsDataset: true, Filters: []string{"train"}, ExactMatch: true, AppendFilterSubdir: true}
			settings := Settings{Endpoint: server.URL, OutputDir: out, Concurrency: 1, MaxActiveDownloads: 1}
			if err := Download(context.Background(), job, settings, nil); err != nil {
				t.Fatalf("Download: %v", err)
			}
			base, err := DestinationBase(out, job.Repo)
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(base, "train_records.csv"))
			if err != nil || string(data) != "data" {
				t.Fatalf("dataset destination = %q, err=%v; want repo root independent of GGUF matches", data, err)
			}
			_, statErr := os.Stat(filepath.Join(base, "train", "train_records.csv"))
			if !os.IsNotExist(statErr) {
				t.Errorf("dataset was relocated under train/: stat err=%v", statErr)
			}
			if withGGUF {
				model, err := os.ReadFile(filepath.Join(base, "train", "model-train.gguf"))
				if err != nil || string(model) != "model" {
					t.Errorf("selected GGUF destination = %q, err=%v; want train/model-train.gguf", model, err)
				}
			}
		})
	}
}

func TestDownload_MixedFormatKeepsPerFileGGUFDestination(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/revision/main"):
			_ = json.NewEncoder(w).Encode(RepoInfo{SHA: "deadbeef"})
		case strings.Contains(r.URL.Path, "/tree/"):
			_ = json.NewEncoder(w).Encode([]hfNode{
				{Type: "file", Path: "model-Q4_K_M.gguf", Size: 4},
				{Type: "file", Path: "other.safetensors", Size: 4, LFS: &hfLfsInfo{Size: 4}},
			})
		case r.URL.Path == "/owner/model/raw/main/model-Q4_K_M.gguf":
			_, _ = w.Write([]byte("gguf"))
		case r.URL.Path == "/owner/model/resolve/main/other.safetensors":
			_, _ = w.Write([]byte("safe"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	out := t.TempDir()
	job := Job{Repo: "owner/model", Revision: "main", Filters: []string{"Q4_K_M", "safetensors"}, ExactMatch: true, AppendFilterSubdir: true}
	settings := Settings{Endpoint: server.URL, OutputDir: out, Concurrency: 1, MaxActiveDownloads: 1}
	plan, err := PlanRepo(context.Background(), job, settings)
	if err != nil {
		t.Fatalf("PlanRepo: %v", err)
	}
	if len(plan.Items) != 2 {
		t.Fatalf("selected %d files, want both mixed-format matches: %+v", len(plan.Items), plan.Items)
	}
	for _, item := range plan.Items {
		if item.RelativePath == "model-Q4_K_M.gguf" && (item.Subdir != "Q4_K_M" || item.destinationSubdir != "Q4_K_M") {
			t.Errorf("mixed-format GGUF attribution/destination = %q/%q; want Q4_K_M/Q4_K_M", item.Subdir, item.destinationSubdir)
		}
	}
	var eventPaths []string
	if err := Download(context.Background(), job, settings, func(event ProgressEvent) {
		if event.Event == "plan_item" {
			eventPaths = append(eventPaths, event.Path)
		}
	}); err != nil {
		t.Fatalf("Download: %v", err)
	}
	wantPaths := map[string]bool{"Q4_K_M/model-Q4_K_M.gguf": true, "safetensors/other.safetensors": true}
	for _, p := range eventPaths {
		if !wantPaths[p] {
			t.Errorf("unexpected plan_item path %q", p)
		}
		delete(wantPaths, p)
	}
	for p := range wantPaths {
		t.Errorf("missing plan_item path %q", p)
	}
	base, err := DestinationBase(out, job.Repo)
	if err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]string{
		"Q4_K_M/model-Q4_K_M.gguf":      "gguf",
		"safetensors/other.safetensors": "safe",
	} {
		got, err := os.ReadFile(filepath.Join(base, filepath.FromSlash(rel)))
		if err != nil || string(got) != want {
			t.Errorf("destination %s = %q, err=%v; want %q", rel, got, err, want)
		}
	}
}

func TestPlanRepo_DatasetGGUFLFSFilterKeepsAppendedDestination(t *testing.T) {
	srv := mockHFServerForPlan(t, []hfNode{{
		Type: "file", Path: "Q4_K_M/model-Q4_K_M.gguf", Size: 4,
		LFS: &hfLfsInfo{Size: 4},
	}})
	defer srv.Close()

	plan, err := PlanRepo(context.Background(), Job{
		Repo: "owner/data", IsDataset: true, Filters: []string{"Q4_K_M"},
		ExactMatch: true, AppendFilterSubdir: true,
	}, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("PlanRepo: %v", err)
	}
	if len(plan.Items) != 1 {
		t.Fatalf("plan has %d items, want 1: %+v", len(plan.Items), plan.Items)
	}
	item := plan.Items[0]
	if !item.LFS || item.Subdir != "Q4_K_M" || item.destinationSubdir != "Q4_K_M" {
		t.Fatalf("LFS GGUF attribution/destination = %+v; want LFS with Q4_K_M prefix", item)
	}
}
