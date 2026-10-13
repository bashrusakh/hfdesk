// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func BenchmarkPlanRepoDatasetSelectionInventory(b *testing.B) {
	for _, count := range []int{1000, 10000} {
		for _, mode := range []string{"one-train-rest-test", "all-train", "all-train-with-root-companions"} {
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
