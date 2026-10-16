// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func BenchmarkPlanRepoDatasetSelectionInventory(b *testing.B) {
	for _, count := range []int{1000, 10000} {
		for _, mode := range []string{"one-train-rest-test", "all-train", "all-train-with-root-companions", "all-train-with-matching-gguf"} {
			b.Run(fmt.Sprintf("files-%d/%s", count, mode), func(b *testing.B) {
				files := make([]hfNode, 0, count+2)
				want := make([]string, 0, count+2)
				for i := 0; i < count; i++ {
					dir := "train"
					if mode == "one-train-rest-test" && i > 0 {
						dir = "test"
					}
					name := fmt.Sprintf("%s/part-%05d.parquet", dir, i)
					files = append(files, hfNode{Type: "file", Path: name, Size: 10})
					if dir == "train" {
						want = append(want, name)
					}
				}
				if mode == "all-train-with-root-companions" {
					files = append(files, hfNode{Type: "file", Path: "README.md", Size: 10}, hfNode{Type: "file", Path: "config.json", Size: 10})
					want = append(want, "README.md", "config.json")
				}
				if mode == "all-train-with-matching-gguf" {
					files = append(files, hfNode{Type: "file", Path: "train/model.gguf", Size: 10})
					want = append(want, "train/model.gguf")
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch {
					case r.URL.Path == "/api/datasets/owner/data/revision/main":
						_, _ = w.Write([]byte(`{"sha":"bench"}`))
					case r.URL.Path == "/api/datasets/owner/data/tree/main":
						_ = json.NewEncoder(w).Encode(files)
					default:
						http.NotFound(w, r)
					}
				}))
				defer server.Close()
				job := Job{Repo: "owner/data", IsDataset: true, Filters: []string{"train"}, ExactMatch: true}
				cfg := Settings{Endpoint: server.URL}
				plan, err := PlanRepo(context.Background(), job, cfg)
				if err != nil {
					b.Fatal(err)
				}
				if len(plan.Items) != len(want) {
					b.Fatalf("PlanRepo selected %d files; want %d", len(plan.Items), len(want))
				}
				for i, item := range plan.Items {
					if item.RelativePath != want[i] {
						b.Fatalf("PlanRepo item[%d]=%q; want %q", i, item.RelativePath, want[i])
					}
					wantSubdir := "train"
					if item.RelativePath == "README.md" || item.RelativePath == "config.json" {
						wantSubdir = ""
					}
					if item.Subdir != wantSubdir {
						b.Fatalf("PlanRepo item %q Subdir=%q; want %q", item.RelativePath, item.Subdir, wantSubdir)
					}
				}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, err := PlanRepo(context.Background(), job, cfg); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

func BenchmarkPlanRepoDatasetRootDefaultInventory(b *testing.B) {
	for _, count := range []int{100, 1000, 10000} {
		b.Run(fmt.Sprintf("files-%d", count), func(b *testing.B) {
			files := make([]hfNode, count+1)
			want := make([]string, count+1)
			files[0] = hfNode{Type: "file", Path: "records.parquet", Size: 10}
			want[0] = "records.parquet"
			for i := 1; i < len(files); i++ {
				name := fmt.Sprintf("records-%05d.json", i-1)
				files[i] = hfNode{Type: "file", Path: name, Size: 10}
				want[i] = name
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/api/datasets/owner/data/revision/main":
					_, _ = w.Write([]byte(`{"sha":"bench"}`))
				case r.URL.Path == "/api/datasets/owner/data/tree/main":
					_ = json.NewEncoder(w).Encode(files)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			job := Job{Repo: "owner/data", IsDataset: true, Filters: []string{"default"}, ExactMatch: true}
			cfg := Settings{Endpoint: server.URL}
			plan, err := PlanRepo(context.Background(), job, cfg)
			if err != nil {
				b.Fatal(err)
			}
			if len(plan.Items) != len(want) {
				b.Fatalf("PlanRepo selected %d; want %d", len(plan.Items), len(want))
			}
			for i, item := range plan.Items {
				if item.RelativePath != want[i] || item.Subdir != "default" {
					b.Fatalf("item[%d]=%q Subdir=%q; want %q Subdir=default", i, item.RelativePath, item.Subdir, want[i])
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := PlanRepo(context.Background(), job, cfg); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkPlanRepoDatasetDecisionShapes(b *testing.B) {
	for _, count := range []int{100, 1000, 10000} {
		for _, shape := range []string{"nested-seeded-companions", "path-mask-collisions", "case-fold-classes"} {
			b.Run(fmt.Sprintf("files-%d/%s", count, shape), func(b *testing.B) {
				files := make([]hfNode, 0, count)
				want := make([]string, 0, count)
				filters := []string{"train"}
				for i := 0; i < count; i++ {
					switch shape {
					case "nested-seeded-companions":
						pair := i / 2
						if i%2 == 0 {
							name := fmt.Sprintf("partition/train_dataset-%05d.csv", pair)
							files = append(files, hfNode{Type: "file", Path: name, Size: 10})
							want = append(want, name)
						} else {
							name := fmt.Sprintf("partition/README-part-%05d.md", pair)
							files = append(files, hfNode{Type: "file", Path: name, Size: 10})
							want = append(want, name)
						}
					case "path-mask-collisions":
						filters = []string{"train", ".csv"}
						name := fmt.Sprintf("train/part-%05d.csv", i)
						if i%2 == 1 {
							name = fmt.Sprintf("test/train-part-%05d.csv", i)
						}
						files = append(files, hfNode{Type: "file", Path: name, Size: 10})
						want = append(want, name)
					case "case-fold-classes":
						filters = []string{"train", ".csv"}
						pair := i / 2
						name := fmt.Sprintf("train/part-%05d.csv", pair)
						if i%2 == 1 {
							name = fmt.Sprintf("TRAIN/PART-%05d.CSV", pair)
						} else {
							want = append(want, name)
						}
						files = append(files, hfNode{Type: "file", Path: name, Size: 10})
					}
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch {
					case r.URL.Path == "/api/datasets/owner/data/revision/main":
						_, _ = w.Write([]byte(`{"sha":"bench"}`))
					case r.URL.Path == "/api/datasets/owner/data/tree/main":
						_ = json.NewEncoder(w).Encode(files)
					default:
						http.NotFound(w, r)
					}
				}))
				defer server.Close()
				job := Job{Repo: "owner/data", IsDataset: true, Filters: filters, ExactMatch: true}
				cfg := Settings{Endpoint: server.URL}
				plan, err := PlanRepo(context.Background(), job, cfg)
				if err != nil {
					b.Fatal(err)
				}
				if len(plan.Items) != len(want) {
					b.Fatalf("PlanRepo selected %d; want %d", len(plan.Items), len(want))
				}
				for i, item := range plan.Items {
					wantSubdir := "train"
					if shape == "nested-seeded-companions" && strings.Contains(item.RelativePath, "README-") {
						wantSubdir = ""
					}
					if item.RelativePath != want[i] || item.Subdir != wantSubdir {
						b.Fatalf("item[%d]=%q Subdir=%q; want %q Subdir=%q", i, item.RelativePath, item.Subdir, want[i], wantSubdir)
					}
				}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, err := PlanRepo(context.Background(), job, cfg); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

func BenchmarkPlanRepoDatasetOrdinarySelectorInventory(b *testing.B) {
	for _, count := range []int{1001, 3001} {
		b.Run(fmt.Sprintf("files-%d", count), func(b *testing.B) {
			files := make([]hfNode, 0, count)
			files = append(files, hfNode{Type: "file", Path: "selected.csv", Size: 10})
			for i := 1; i < count; i++ {
				files = append(files, hfNode{Type: "file", Path: fmt.Sprintf("records-%04d.json", i), Size: 10})
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/datasets/owner/data/revision/main":
					_, _ = w.Write([]byte(`{"sha":"bench"}`))
				case "/api/datasets/owner/data/tree/main":
					_ = json.NewEncoder(w).Encode(files)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			job := Job{Repo: "owner/data", IsDataset: true, Filters: []string{"selected.csv"}, ExactMatch: true}
			cfg := Settings{Endpoint: server.URL}
			if plan, err := PlanRepo(context.Background(), job, cfg); err != nil || len(plan.Items) != 1 || plan.Items[0].RelativePath != "selected.csv" {
				b.Fatalf("PlanRepo = %+v, err=%v; want only selected.csv", plan, err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := PlanRepo(context.Background(), job, cfg); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
