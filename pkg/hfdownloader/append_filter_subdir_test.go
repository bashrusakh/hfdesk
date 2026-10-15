// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"encoding/json"
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
