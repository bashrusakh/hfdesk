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
	for _, count := range []int{100, 1000} {
		b.Run(fmt.Sprintf("files-%d", count), func(b *testing.B) {
			files := make([]hfNode, 0, count)
			files = append(files, hfNode{Type: "file", Path: "train/part-00000.parquet", Size: 10})
			for i := 1; i < count; i++ {
				files = append(files, hfNode{Type: "file", Path: fmt.Sprintf("test/part-%05d.parquet", i), Size: 10})
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
