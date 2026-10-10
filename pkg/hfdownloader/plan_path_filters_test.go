// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"testing"
)

func TestFilterMatchesPath(t *testing.T) {
	tests := []struct {
		name, path, filter string
		exact, want        bool
	}{
		{"nested directory", "diffusers/unet/model.safetensors", "unet", true, true},
		{"directory slash", "diffusers/unet/model.safetensors", "unet/", true, true},
		{"directory boundary", "diffusers/myunet/model.safetensors", "unet/", true, false},
		{"extension", "train/data-00001.parquet", ".parquet", true, true},
		{"exact relative path", "text_encoder/config.json", "text_encoder/config.json", true, true},
		{"substring nested path", "dataset/train/data.parquet", "train/data", false, true},
		{"quant exact", "model-q6_k.gguf", "q6_k", true, true},
		{"quant suffix rejected", "model-q6_k_xl.gguf", "q6_k", true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := filterMatchesPath(tt.path, tt.filter, tt.exact); got != tt.want {
				t.Fatalf("filterMatchesPath(%q, %q, exact=%v) = %v, want %v", tt.path, tt.filter, tt.exact, got, tt.want)
			}
		})
	}
}

func TestPlanRepoPathFiltersKeepCompanionsAndSkipUnmatchedLFS(t *testing.T) {
	files := []hfNode{
		{Type: "file", Path: "config.json", Size: 10},
		{Type: "file", Path: "README.md", Size: 10},
		{Type: "file", Path: "diffusers/unet/diffusion_pytorch_model.safetensors", LFS: &hfLfsInfo{Size: 100}},
		{Type: "file", Path: "diffusers/vae/diffusion_pytorch_model.safetensors", LFS: &hfLfsInfo{Size: 100}},
		{Type: "file", Path: "diffusers/unet/README.md", Size: 20},
		{Type: "file", Path: "diffusers/config.json", Size: 10},
		{Type: "file", Path: "diffusers/vae/config.json", Size: 10},
		{Type: "file", Path: "diffusers/vae/model.msgpack", LFS: &hfLfsInfo{Size: 10}},
		{Type: "file", Path: "diffusers/vae/model.onnx", LFS: &hfLfsInfo{Size: 10}},
		{Type: "file", Path: "diffusers/vae/model.h5", LFS: &hfLfsInfo{Size: 10}},
		{Type: "file", Path: "diffusers/vae/model.tflite", LFS: &hfLfsInfo{Size: 10}},
		{Type: "file", Path: "metadata.json", LFS: &hfLfsInfo{Size: 10}},
	}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()

	plan, err := PlanRepo(context.Background(), Job{
		Repo: "owner/repo", Revision: "main", Filters: []string{"diffusers/unet/"}, ExactMatch: true,
	}, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("PlanRepo: %v", err)
	}
	got := planPaths(plan)
	want := []string{"config.json", "README.md", "diffusers/unet/diffusion_pytorch_model.safetensors", "diffusers/unet/README.md", "diffusers/config.json"}
	assertPaths(t, got, want)
}

func TestPlanRepoDatasetSplitPathFilter(t *testing.T) {
	files := []hfNode{
		{Type: "file", Path: "dataset_info.json", Size: 10},
		{Type: "file", Path: "train/data-00000-of-00001.parquet", LFS: &hfLfsInfo{Size: 100}},
		{Type: "file", Path: "test/data-00000-of-00001.parquet", LFS: &hfLfsInfo{Size: 100}},
	}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()

	plan, err := PlanRepo(context.Background(), Job{
		Repo: "owner/dataset", Revision: "main", IsDataset: true, Filters: []string{"train/"}, ExactMatch: true,
	}, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("PlanRepo: %v", err)
	}
	assertPaths(t, planPaths(plan), []string{"dataset_info.json", "train/data-00000-of-00001.parquet"})
}

func TestPlanRepoExactGGUFAndNoFilterRegression(t *testing.T) {
	files := []hfNode{
		{Type: "file", Path: "model-q6_k.gguf", LFS: &hfLfsInfo{Size: 100}},
		{Type: "file", Path: "model-q6_k_xl.gguf", LFS: &hfLfsInfo{Size: 100}},
		{Type: "file", Path: "config.json", Size: 10},
	}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()

	filtered, err := PlanRepo(context.Background(), Job{
		Repo: "owner/gguf", Revision: "main", Filters: []string{"q6_k"}, ExactMatch: true,
	}, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("filtered PlanRepo: %v", err)
	}
	assertPaths(t, planPaths(filtered), []string{"model-q6_k.gguf"})

	unfiltered, err := PlanRepo(context.Background(), Job{Repo: "owner/gguf", Revision: "main"}, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("unfiltered PlanRepo: %v", err)
	}
	assertPaths(t, planPaths(unfiltered), []string{"model-q6_k.gguf", "model-q6_k_xl.gguf", "config.json"})
}

func TestPlanRepoMixedGGUFFolderFilterKeepsCompanions(t *testing.T) {
	files := []hfNode{
		{Type: "file", Path: "weights/model.gguf", LFS: &hfLfsInfo{Size: 100}},
		{Type: "file", Path: "weights/model.safetensors", LFS: &hfLfsInfo{Size: 100}},
		{Type: "file", Path: "weights/README.md", Size: 10},
		{Type: "file", Path: "config.json", Size: 10},
	}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()

	plan, err := PlanRepo(context.Background(), Job{
		Repo: "owner/mixed", Revision: "main", Filters: []string{"weights/"}, ExactMatch: true,
	}, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("PlanRepo: %v", err)
	}
	assertPaths(t, planPaths(plan), []string{"weights/model.gguf", "weights/model.safetensors", "weights/README.md", "config.json"})
}

func TestPlanRepoPureGGUFFolderFilterRemainsMatchedOnly(t *testing.T) {
	files := []hfNode{
		{Type: "file", Path: "weights/model.gguf", LFS: &hfLfsInfo{Size: 100}},
		{Type: "file", Path: "config.json", Size: 10},
	}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()

	plan, err := PlanRepo(context.Background(), Job{
		Repo: "owner/pure-gguf", Revision: "main", Filters: []string{"weights/"}, ExactMatch: true,
	}, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("PlanRepo: %v", err)
	}
	assertPaths(t, planPaths(plan), []string{"weights/model.gguf"})
}

func TestPlanRepoExcludedNonGGUFDoesNotDisableGGUFMode(t *testing.T) {
	files := []hfNode{
		{Type: "file", Path: "weights/model.gguf", LFS: &hfLfsInfo{Size: 100}},
		{Type: "file", Path: "weights/model.safetensors", LFS: &hfLfsInfo{Size: 100}},
		{Type: "file", Path: "config.json", Size: 10},
	}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()

	plan, err := PlanRepo(context.Background(), Job{
		Repo: "owner/excluded-mixed", Revision: "main", Filters: []string{"weights/"}, Excludes: []string{".safetensors"}, ExactMatch: true,
	}, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("PlanRepo: %v", err)
	}
	assertPaths(t, planPaths(plan), []string{"weights/model.gguf"})
}

func planPaths(plan *Plan) []string {
	paths := make([]string, 0, len(plan.Items))
	for _, item := range plan.Items {
		paths = append(paths, item.RelativePath)
	}
	return paths
}

func assertPaths(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("paths = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("paths = %v, want %v", got, want)
		}
	}
}
