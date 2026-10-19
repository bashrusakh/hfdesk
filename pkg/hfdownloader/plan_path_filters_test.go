// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

type inventoryEOFBody struct {
	io.ReadCloser
	read *atomic.Bool
}

func (b *inventoryEOFBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err == io.EOF {
		b.read.Store(true)
	}
	return n, err
}

func (b *inventoryEOFBody) Close() error {
	b.read.Store(true)
	return b.ReadCloser.Close()
}

type inventoryBodyTransport struct {
	base http.RoundTripper
	read atomic.Bool
}

func (t *inventoryBodyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err == nil && strings.Contains(req.URL.Path, "/tree/") {
		resp.Body = &inventoryEOFBody{ReadCloser: resp.Body, read: &t.read}
	}
	return resp, err
}

type cancelAfterInventoryContext struct {
	context.Context
	read   *atomic.Bool
	checks atomic.Int64
	limit  int64
	cancel context.CancelFunc
}

func (c *cancelAfterInventoryContext) Err() error {
	if c.read.Load() && c.checks.Add(1) >= c.limit {
		c.cancel()
	}
	return c.Context.Err()
}

func TestFilterMatchesPath(t *testing.T) {
	tests := []struct {
		name, path, filter string
		exact, want        bool
	}{
		{"nested directory", "diffusers/unet/model.safetensors", "unet", true, true},
		{"hyphen-delimited directory token", "variants/model-Q6_K/weights.gguf", "q6_k", true, true},
		{"dot-delimited directory token", "variants/model.Q6_K/weights.gguf", "q6_k", true, true},
		{"space-delimited directory token", "variants/model Q6_K/weights.gguf", "q6_k", true, true},
		{"underscore extension in directory is not delimiter", "variants/model-Q6_K_XL/weights.gguf", "q6_k", true, false},
		{"directory slash", "diffusers/unet/model.safetensors", "unet/", true, true},
		{"directory slash does not select directory leaf", "unet", "unet/", true, false},
		{"directory slash selects descendants of same-named directory", "unet/model.safetensors", "unet/", true, true},
		{"directory slash selects nested directory descendants", "diffusers/unet/model.safetensors", "unet/", true, true},
		{"directory slash does not select similarly named directory", "diffusers/myunet/model.safetensors", "unet/", true, false},
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

func TestGGUFPathSelectedUsesDirectoryTokenMatcher(t *testing.T) {
	for _, tc := range []struct {
		name    string
		path    string
		exclude []string
		want    bool
	}{
		{"delimiter-bounded directory token", "variants/model-Q6_K/weights.gguf", nil, true},
		{"underscore suffix remains distinct", "variants/model-Q6_K_XL/weights.gguf", nil, false},
		{"explicit exclude still wins", "variants/model-Q6_K/weights.gguf", []string{"weights"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := GGUFPathSelected(tc.path, []string{"q6_k"}, tc.exclude, true); got != tc.want {
				t.Fatalf("GGUFPathSelected(%q) = %v, want %v", tc.path, got, tc.want)
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
		{Type: "file", Path: "tokenizer.model", Size: 20},
		{Type: "file", Path: "assets/tokenizer.model", Size: 21},
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
	want := []string{"config.json", "README.md", "diffusers/unet/diffusion_pytorch_model.safetensors", "diffusers/unet/README.md", "diffusers/config.json", "diffusers/vae/config.json", "tokenizer.model", "assets/tokenizer.model"}
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

func TestPlanRepoDatasetGroupLabelIsMembershipOnly(t *testing.T) {
	files := []hfNode{
		{Type: "file", Path: "train.csv", Size: 10},
		{Type: "file", Path: "pretrain.csv", Size: 11},
		{Type: "file", Path: "other/xtrain.csv", LFS: &hfLfsInfo{Size: 12}},
	}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()

	for _, tc := range []struct {
		name  string
		exact bool
		want  []string
	}{
		{"substring group label ignores literal matches", false, []string{"train.csv"}},
		{"exact group label ignores literal matches", true, []string{"train.csv"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := PlanRepo(context.Background(), Job{
				Repo: "owner/dataset", IsDataset: true, Filters: []string{"train"}, ExactMatch: tc.exact,
			}, Settings{Endpoint: srv.URL})
			if err != nil {
				t.Fatalf("PlanRepo: %v", err)
			}
			assertPaths(t, planPaths(plan), tc.want)
		})
	}

	// Explicit path forms remain available for literal selection of a file
	// whose spelling merely contains a recognized group label.
	for _, tc := range []struct {
		name   string
		filter string
		exact  bool
		want   []string
	}{
		{"plain basename", "pretrain.csv", true, []string{"pretrain.csv"}},
		{"root literal", "./pretrain.csv", true, []string{"pretrain.csv"}},
		{"directory", "other/", true, []string{"other/xtrain.csv"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := PlanRepo(context.Background(), Job{
				Repo: "owner/dataset", IsDataset: true, Filters: []string{tc.filter}, ExactMatch: tc.exact,
			}, Settings{Endpoint: srv.URL})
			if err != nil {
				t.Fatalf("PlanRepo filter %q: %v", tc.filter, err)
			}
			assertPaths(t, planPaths(plan), tc.want)
		})
	}
}

func TestPlanRepoDatasetGGUFGuardComposesWithRootCompanions(t *testing.T) {
	files := []hfNode{
		{Type: "file", Path: "train.csv", Size: 10},
		{Type: "file", Path: "model-test.gguf", Size: 20},
		{Type: "file", Path: "model-train.gguf", Size: 30},
		{Type: "file", Path: "blocked-test.gguf", Size: 40},
	}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()
	job := Job{Repo: "owner/data", IsDataset: true, Filters: []string{"train"}, ExactMatch: true, Excludes: []string{"blocked"}}
	plan, err := PlanRepo(context.Background(), job, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	// A recognized group label is membership-only; an unmatched GGUF whose
	// name merely contains the label is not a companion selection.
	assertPaths(t, planPaths(plan), []string{"train.csv"})

	job.Filters = []string{"train", "model-train.gguf"}
	plan, err = PlanRepo(context.Background(), job, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	assertPaths(t, planPaths(plan), []string{"train.csv", "model-train.gguf"})

	job.Filters = []string{"train"}
	if GGUFPathMayBeEmitted("model-test.gguf", job) {
		t.Fatal("pre-plan projection admitted an unmatched root GGUF")
	}
	job.Excludes = []string{"model-test"}
	if GGUFPathMayBeEmitted("model-test.gguf", job) {
		t.Fatal("explicit exclude must dominate conservative projection")
	}
}

func TestPlanRepoDatasetExplicitFileAndGGUFFilterUnion(t *testing.T) {
	for _, tc := range []struct {
		name   string
		file   hfNode
		filter string
	}{
		{"non-LFS explicit data", hfNode{Type: "file", Path: "train.csv", Size: 10}, "train.csv"},
		{"LFS explicit data", hfNode{Type: "file", Path: "train.csv", LFS: &hfLfsInfo{Size: 10}}, "train.csv"},
		{"extension-selected non-LFS data", hfNode{Type: "file", Path: "train.parquet", Size: 11}, ".parquet"},
		{"extension-selected LFS data", hfNode{Type: "file", Path: "train.parquet", LFS: &hfLfsInfo{Size: 11}}, ".parquet"},
		{"directory-selected non-LFS data", hfNode{Type: "file", Path: "data/train.csv", Size: 12}, "data/"},
		{"directory-selected LFS data", hfNode{Type: "file", Path: "data/train.csv", LFS: &hfLfsInfo{Size: 12}}, "data/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := []hfNode{tc.file, {Type: "file", Path: "model-Q4_K_M.gguf", Size: 20}}
			srv := mockHFServerForPlan(t, files)
			defer srv.Close()
			filters := []string{"Q4_K_M", tc.filter}
			plan, err := PlanRepo(context.Background(), Job{Repo: "owner/data", IsDataset: true, Filters: filters, ExactMatch: true}, Settings{Endpoint: srv.URL})
			if err != nil {
				t.Fatalf("PlanRepo: %v", err)
			}
			assertPaths(t, planPaths(plan), []string{tc.file.Path, "model-Q4_K_M.gguf"})
			for _, item := range plan.Items {
				if item.RelativePath == tc.file.Path && tc.file.LFS == nil && item.DestinationSubdir != "" {
					t.Fatalf("explicit dataset path destination subdir = %q; want repo-relative destination", item.DestinationSubdir)
				}
			}
			job := Job{Repo: "owner/data", IsDataset: true, Filters: filters, Excludes: []string{tc.file.Path}, ExactMatch: true}
			excludedPlan, err := PlanRepo(context.Background(), job, Settings{Endpoint: srv.URL})
			if err != nil {
				t.Fatalf("PlanRepo with exclude: %v", err)
			}
			assertPaths(t, planPaths(excludedPlan), []string{"model-Q4_K_M.gguf"})
		})
	}
}

func TestPlanRepoDatasetOrdinaryFiltersDoNotAdmitUnmatchedPayload(t *testing.T) {
	files := []hfNode{
		{Type: "file", Path: "train.csv", Size: 10},
		{Type: "file", Path: "validation.csv", Size: 11},
		{Type: "file", Path: "other.arrow", Size: 12},
		{Type: "file", Path: "other/large.arrow", LFS: &hfLfsInfo{Size: 17}},
		{Type: "file", Path: "model-test.gguf", Size: 13},
		{Type: "file", Path: "config.json", Size: 14},
		{Type: "file", Path: "nested/config.json", Size: 15},
		{Type: "file", Path: "nested/other.csv", Size: 16},
	}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()
	for _, tc := range []struct {
		name   string
		filter string
		exact  bool
		want   []string
	}{
		{"exact filename retains root metadata companions", "train.csv", true, []string{"train.csv", "config.json"}},
		{"substring group selects data and root companions", "train", false, []string{"train.csv", "config.json"}},
		{"extension selection retains root metadata companions", ".csv", true, []string{"train.csv", "validation.csv", "config.json", "nested/other.csv"}},
		{"folder directly selects metadata in both roles", "nested/", true, []string{"config.json", "nested/config.json", "nested/other.csv"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := PlanRepo(context.Background(), Job{Repo: "owner/data", IsDataset: true, Filters: []string{tc.filter}, ExactMatch: tc.exact}, Settings{Endpoint: srv.URL})
			if err != nil {
				t.Fatalf("PlanRepo: %v", err)
			}
			assertPaths(t, planPaths(plan), tc.want)
		})
	}
	job := Job{Repo: "owner/data", IsDataset: true, Filters: []string{"train.csv"}, Excludes: []string{"nested/config.json"}, ExactMatch: true}
	if GGUFPathMayBeEmitted("model-test.gguf", job) {
		t.Fatal("pre-plan upper bound admitted an unmatched dataset root GGUF")
	}
	plan, err := PlanRepo(context.Background(), job, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("PlanRepo: %v", err)
	}
	if containsString(planPaths(plan), "model-test.gguf") {
		t.Fatal("actual plan emitted GGUF admitted only by the conservative projection")
	}
}

func TestPlanRepoDatasetSubstringAndExactPathFiltersRemainDistinct(t *testing.T) {
	srv := mockHFServerForPlan(t, []hfNode{
		{Type: "file", Path: "samples/trainish.csv", Size: 10},
		{Type: "file", Path: "samples/other.csv", Size: 11},
	})
	defer srv.Close()
	for _, tc := range []struct {
		name  string
		exact bool
		want  []string
	}{
		{"substring", false, []string{"samples/trainish.csv"}},
		{"exact", true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := PlanRepo(context.Background(), Job{Repo: "owner/data", IsDataset: true, Filters: []string{"traini"}, ExactMatch: tc.exact}, Settings{Endpoint: srv.URL})
			if tc.exact {
				if !errors.Is(err, ErrNoFilterMatch) || plan != nil {
					t.Fatalf("PlanRepo plan=%v error=%v; want nil plan and ErrNoFilterMatch", planPaths(plan), err)
				}
				return
			}
			if err != nil {
				t.Fatalf("PlanRepo: %v", err)
			}
			assertPaths(t, planPaths(plan), tc.want)
		})
	}
}

func TestPlanRepoDatasetGGUFFallbackUsesMatchedOnlySelection(t *testing.T) {
	files := []hfNode{
		{Type: "file", Path: "weights/model.gguf", Size: 20},
		{Type: "file", Path: "nested/config.json", Size: 15},
		{Type: "file", Path: "dataset_info.json", Size: 16},
	}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()
	job := Job{Repo: "owner/data", IsDataset: true, Filters: []string{"weights/model.gguf"}, ExactMatch: true}
	plan, err := PlanRepo(context.Background(), job, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("explicit GGUF path must not be refused due to unrelated metadata role uncertainty: %v", err)
	}
	assertPaths(t, planPaths(plan), []string{"weights/model.gguf"})

	job.Filters = []string{"weights/model.gguf", "nested/config.json"}
	plan, err = PlanRepo(context.Background(), job, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("explicit path resolves role alternatives: %v", err)
	}
	assertPaths(t, planPaths(plan), []string{"weights/model.gguf", "nested/config.json"})

	job.Filters = []string{"weights/model.gguf"}
	job.Excludes = []string{"dataset_info.json", "nested/config.json"}
	plan, err = PlanRepo(context.Background(), job, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("PlanRepo with explicit metadata exclude: %v", err)
	}
	assertPaths(t, planPaths(plan), []string{"weights/model.gguf"})
}

func TestPlanRepoDatasetSubstringAndGGUFMatchedOnly(t *testing.T) {
	files := []hfNode{
		{Type: "file", Path: "train.csv", Size: 10},
		{Type: "file", Path: "validation.csv", Size: 10},
		{Type: "file", Path: "model-Q4_K_M.gguf", Size: 20},
		{Type: "file", Path: "tokenizer/tokenizer.json", Size: 30},
		{Type: "file", Path: "other/records.json", Size: 40},
		{Type: "file", Path: "tokenizer/config.json", Size: 50},
	}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()
	plan, err := PlanRepo(context.Background(), Job{Repo: "owner/data", IsDataset: true, Filters: []string{"alid"}}, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("substring split with known companions: %v", err)
	}
	assertPaths(t, planPaths(plan), []string{"validation.csv", "tokenizer/tokenizer.json", "tokenizer/config.json"})

	modelFiles := []hfNode{
		{Type: "file", Path: "model-Q4_K_M.gguf", Size: 20},
		{Type: "file", Path: "tokenizer/tokenizer.json", Size: 30},
		{Type: "file", Path: "tokenizer/config.json", Size: 40},
		{Type: "file", Path: "tokenizer/records.json", Size: 50},
	}
	modelServer := mockHFServerForPlan(t, modelFiles)
	defer modelServer.Close()
	plan, err = PlanRepo(context.Background(), Job{Repo: "owner/model", Filters: []string{"Q4_K_M"}}, Settings{Endpoint: modelServer.URL})
	if err != nil {
		t.Fatal(err)
	}
	assertPaths(t, planPaths(plan), []string{"model-Q4_K_M.gguf"})
}

func TestPlanRepoMetadataFallbackRespectsExcludesAndDoesNotSeedData(t *testing.T) {
	files := []hfNode{
		{Type: "file", Path: "train/data.parquet", Size: 10},
		{Type: "file", Path: "other/config.json", Size: 20},
		{Type: "file", Path: "other/tokenizer/tokenizer.json", Size: 30},
		{Type: "file", Path: "other/records.json", Size: 40},
		{Type: "file", Path: "other/blocked_config.json", Size: 50},
		{Type: "file", Path: "other/large.json", LFS: &hfLfsInfo{Size: 60}},
	}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()
	plan, err := PlanRepo(context.Background(), Job{
		Repo: "owner/model", ExactMatch: true,
		Filters: []string{"train/"}, Excludes: []string{"blocked"},
	}, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	assertPaths(t, planPaths(plan), []string{"train/data.parquet", "other/config.json", "other/tokenizer/tokenizer.json", "other/records.json"})
}

// Model-context tokenizer recognition is established; dataset basename alone
// is not enough to assign that role. Dataset ambiguity handling is tested in
// dataset_decision_test.go without selecting an unsupported policy.
func TestPlanRepoModelTokenizerMetadataRetention(t *testing.T) {
	model := mockHFServerForPlan(t, []hfNode{{Type: "file", Path: "model-Q4_K_M.gguf", Size: 20}, {Type: "file", Path: "tokenizer.json", Size: 12}})
	defer model.Close()
	plan, err := PlanRepo(context.Background(), Job{Repo: "owner/model", Filters: []string{"Q4_K_M"}, ExactMatch: true}, Settings{Endpoint: model.URL})
	if err != nil {
		t.Fatal(err)
	}
	assertPaths(t, planPaths(plan), []string{"model-Q4_K_M.gguf"})
}

func TestPlanRepoModelTokenizerVocabularyMetadataRetention(t *testing.T) {
	files := []hfNode{
		{Type: "file", Path: "weights/model.safetensors", LFS: &hfLfsInfo{Size: 100}},
		{Type: "file", Path: "arbitrary/assets/vocab.json", Size: 12},
		{Type: "file", Path: "arbitrary/assets/merges.txt", Size: 13},
		{Type: "file", Path: "arbitrary/assets/vocab.txt", Size: 13},
		{Type: "file", Path: "arbitrary/assets/unrelated.json", Size: 14},
		{Type: "file", Path: "arbitrary/assets/unrelated.txt", Size: 15},
		{Type: "file", Path: "arbitrary/assets/vocab-lfs.txt", LFS: &hfLfsInfo{Size: 16}},
	}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()
	plan, err := PlanRepo(context.Background(), Job{Repo: "owner/model", Filters: []string{"weights/model.safetensors"}, ExactMatch: true}, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	assertPaths(t, planPaths(plan), []string{"weights/model.safetensors", "arbitrary/assets/vocab.json", "arbitrary/assets/merges.txt", "arbitrary/assets/vocab.txt", "arbitrary/assets/unrelated.json", "arbitrary/assets/unrelated.txt"})

	plan, err = PlanRepo(context.Background(), Job{
		Repo: "owner/model", Filters: []string{"weights/model.safetensors"}, Excludes: []string{"vocab.txt"}, ExactMatch: true,
	}, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	assertPaths(t, planPaths(plan), []string{"weights/model.safetensors", "arbitrary/assets/vocab.json", "arbitrary/assets/merges.txt", "arbitrary/assets/unrelated.json", "arbitrary/assets/unrelated.txt"})
}

func TestPlanRepoDatasetRetainsKnownVocabTxt(t *testing.T) {
	files := []hfNode{
		{Type: "file", Path: "train.csv", Size: 10},
		{Type: "file", Path: "tokenizer/vocab.txt", Size: 11},
		{Type: "file", Path: "elsewhere/unrelated.txt", Size: 12},
	}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()
	plan, err := PlanRepo(context.Background(), Job{
		Repo: "owner/data", IsDataset: true, Filters: []string{"train.csv"}, ExactMatch: true,
	}, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	assertPaths(t, planPaths(plan), []string{"train.csv", "tokenizer/vocab.txt"})
}

func TestPlanRepoDatasetOrdinaryPathSelectionSurvivesMetadataRoleAmbiguity(t *testing.T) {
	files := []hfNode{
		{Type: "file", Path: "selected.csv", Size: 10},
		{Type: "file", Path: "train/config.json", Size: 11},
		{Type: "file", Path: "nested/config.json", Size: 12},
		{Type: "file", Path: "validation/data.parquet", Size: 13},
	}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()
	job := Job{Repo: "owner/data", IsDataset: true, Filters: []string{"selected.csv"}, ExactMatch: true}
	plan, err := PlanRepo(context.Background(), job, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("ordinary path selection must ignore out-of-scope metadata: %v", err)
	}
	assertPaths(t, planPaths(plan), []string{"selected.csv"})

	job.Filters = []string{"selected.csv", "nested/config.json"}
	plan, err = PlanRepo(context.Background(), job, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("explicit metadata path should resolve alternatives: %v", err)
	}
	assertPaths(t, planPaths(plan), []string{"selected.csv", "nested/config.json"})

	job.Filters = []string{"selected.csv"}
	job.Excludes = []string{"config.json"}
	plan, err = PlanRepo(context.Background(), job, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("exclude should resolve metadata ambiguity: %v", err)
	}
	assertPaths(t, planPaths(plan), []string{"selected.csv"})
}

func TestPlanRepoDatasetNoMatchPrecedesRetainedAmbiguity(t *testing.T) {
	files := []hfNode{
		{Type: "file", Path: "train.csv", Size: 10},
		{Type: "file", Path: "notes.json", Size: 11},
	}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()
	plan, err := PlanRepo(context.Background(), Job{
		Repo: "owner/data", IsDataset: true, Filters: []string{"train"}, Excludes: []string{"train.csv"}, ExactMatch: true,
	}, Settings{Endpoint: srv.URL})
	if plan != nil || !errors.Is(err, ErrNoFilterMatch) {
		t.Fatalf("PlanRepo = (%+v, %v), want nil plan and ErrNoFilterMatch before companion ambiguity", plan, err)
	}
}

func TestPlanRepoDatasetAmbiguityDoesNotDependOnFirstAlternative(t *testing.T) {
	for _, filter := range []string{"train", "test"} {
		t.Run(filter, func(t *testing.T) {
			srv := mockHFServerForPlan(t, []hfNode{{Type: "file", Path: "train_test.csv", Size: 10}})
			defer srv.Close()
			_, err := PlanRepo(context.Background(), Job{Repo: "owner/data", IsDataset: true, Filters: []string{filter}, ExactMatch: true}, Settings{Endpoint: srv.URL})
			if !errors.Is(err, ErrAmbiguousSelection) {
				t.Fatalf("PlanRepo error = %v, want ErrAmbiguousSelection", err)
			}
		})
	}
}

func TestPlanRepoDatasetKnownMetadataOwnershipUnknownIsRetained(t *testing.T) {
	files := []hfNode{
		{Type: "file", Path: "train.csv", Size: 10},
		{Type: "file", Path: "elsewhere/README.md", Size: 11},
	}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()
	plan, err := PlanRepo(context.Background(), Job{Repo: "owner/data", IsDataset: true, Filters: []string{"train"}, ExactMatch: true}, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("uncertain metadata ownership must retain without refusing: %v", err)
	}
	assertPaths(t, planPaths(plan), []string{"train.csv", "elsewhere/README.md"})
}

func TestPlanRepoDatasetKnownRootMetadataResolvesAsCompanion(t *testing.T) {
	for _, metadata := range []string{"README.txt", "README.md", "LICENSE.txt", "LICENSE.md", "LICENSE", "dataset_infos.json", "dataset_info.json", "config.json"} {
		t.Run("group/"+metadata, func(t *testing.T) {
			srv := mockHFServerForPlan(t, []hfNode{
				{Type: "file", Path: "train.csv", Size: 10},
				{Type: "file", Path: metadata, Size: 11},
			})
			defer srv.Close()
			plan, err := PlanRepo(context.Background(), Job{Repo: "owner/data", IsDataset: true, Filters: []string{"train"}, ExactMatch: true}, Settings{Endpoint: srv.URL})
			if err != nil {
				t.Fatalf("group filter with known root metadata %q: %v", metadata, err)
			}
			assertPaths(t, planPaths(plan), []string{"train.csv", metadata})
		})
		t.Run("ordinary/"+metadata, func(t *testing.T) {
			srv := mockHFServerForPlan(t, []hfNode{
				{Type: "file", Path: "train.csv", Size: 10},
				{Type: "file", Path: metadata, Size: 11},
			})
			defer srv.Close()
			plan, err := PlanRepo(context.Background(), Job{Repo: "owner/data", IsDataset: true, Filters: []string{"train.csv"}, ExactMatch: true}, Settings{Endpoint: srv.URL})
			if err != nil {
				t.Fatalf("ordinary path filter with known root metadata %q: %v", metadata, err)
			}
			assertPaths(t, planPaths(plan), []string{"train.csv", metadata})
		})
	}
}

func TestPlanRepoDatasetBareReadmeCompanion(t *testing.T) {
	files := []hfNode{
		{Type: "file", Path: "train.csv", Size: 10},
		{Type: "file", Path: "README", Size: 11},
		{Type: "file", Path: "train/README", Size: 12},
		{Type: "file", Path: "README.md", Size: 13},
		{Type: "file", Path: "MAINTAINERS", Size: 14},
	}
	want := []string{"train.csv", "README", "train/README", "README.md"}
	for _, tc := range []struct {
		name    string
		filters []string
	}{
		{name: "recognized group", filters: []string{"train"}},
		{name: "ordinary path", filters: []string{"train.csv"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := mockHFServerForPlan(t, files)
			defer srv.Close()
			plan, err := PlanRepo(context.Background(), Job{Repo: "owner/data", IsDataset: true, Filters: tc.filters, ExactMatch: true}, Settings{Endpoint: srv.URL})
			if err != nil {
				t.Fatalf("PlanRepo filters=%v: %v", tc.filters, err)
			}
			// Bare README variants are companions at the root and any depth,
			// matching README.md's established behavior. Arbitrary bare names
			// such as MAINTAINERS remain unknown and are not retained.
			assertPaths(t, planPaths(plan), want)
		})
	}
}

func TestPlanRepoDatasetRetainsKnownCompanions(t *testing.T) {
	files := []hfNode{
		{Type: "file", Path: "train.csv", Size: 10},
		{Type: "file", Path: "docs/guide.md", Size: 4},
		{Type: "file", Path: "utils/tool.py", Size: 5},
		{Type: "file", Path: "elsewhere/notes.txt", Size: 6},
		{Type: "file", Path: "config.json", Size: 7},
		{Type: "file", Path: "README.md", Size: 8},
		{Type: "file", Path: "tokenizer/tokenizer.bin", Size: 9},
		{Type: "file", Path: "weights/model.safetensors", Size: 100},
		{Type: "file", Path: "weights/model.onnx", Size: 101},
		{Type: "file", Path: "weights/model.pt", Size: 102},
		{Type: "file", Path: "weights/training-model.safetensors", Size: 103},
		{Type: "file", Path: "weights/custom.msgpack", Size: 104},
		{Type: "file", Path: "weights/checkpoint.h5", Size: 105},
		{Type: "file", Path: "weights/weights.ggml", Size: 106},
		{Type: "file", Path: "weights/model.npy", Size: 107},
		{Type: "file", Path: "weights/model.pkl", Size: 108},
		{Type: "file", Path: "weights/custom.dat", Size: 109},
		{Type: "file", Path: "weights/custom.foo", Size: 110},
	}
	rejected := []string{
		"weights/model.safetensors",
		"weights/model.onnx",
		"weights/model.pt",
		"weights/training-model.safetensors",
		"weights/custom.msgpack",
		"weights/checkpoint.h5",
		"weights/weights.ggml",
		"weights/model.npy",
		"weights/model.pkl",
		"weights/custom.dat",
		"weights/custom.foo",
	}
	want := []string{"train.csv", "docs/guide.md", "utils/tool.py", "config.json", "README.md", "tokenizer/tokenizer.bin"}
	for _, tc := range []struct {
		name    string
		filters []string
	}{
		{name: "recognized group", filters: []string{"train"}},
		{name: "ordinary path", filters: []string{"train.csv"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := mockHFServerForPlan(t, files)
			defer srv.Close()
			plan, err := PlanRepo(context.Background(), Job{Repo: "owner/data", IsDataset: true, Filters: tc.filters, ExactMatch: true}, Settings{Endpoint: srv.URL})
			if err != nil {
				t.Fatalf("PlanRepo filters=%v: %v", tc.filters, err)
			}
			// Allowlisted companions are retained, while unknown nested
			// .txt/.json and payload paths keep their unselected behavior.
			assertPaths(t, planPaths(plan), want)
			// Unmatched model payload/unknown paths are not safe companions.
			for _, path := range planPaths(plan) {
				for _, reject := range rejected {
					if path == reject {
						t.Errorf("unmatched model payload %q was retained by filters=%v", path, tc.filters)
					}
				}
			}
		})
	}

	t.Run("excluded companion", func(t *testing.T) {
		srv := mockHFServerForPlan(t, files)
		defer srv.Close()
		plan, err := PlanRepo(context.Background(), Job{Repo: "owner/data", IsDataset: true, Filters: []string{"train"}, Excludes: []string{"docs/guide.md"}, ExactMatch: true}, Settings{Endpoint: srv.URL})
		if err != nil {
			t.Fatalf("PlanRepo with excluded companion: %v", err)
		}
		assertPaths(t, planPaths(plan), []string{"train.csv", "utils/tool.py", "config.json", "README.md", "tokenizer/tokenizer.bin"})
	})

	t.Run("LFS companion", func(t *testing.T) {
		lfsFiles := []hfNode{
			{Type: "file", Path: "train.csv", Size: 10},
			{Type: "file", Path: "docs/guide.md", LFS: &hfLfsInfo{Size: 4}},
			{Type: "file", Path: "utils/tool.py", Size: 5},
		}
		srv := mockHFServerForPlan(t, lfsFiles)
		defer srv.Close()
		plan, err := PlanRepo(context.Background(), Job{Repo: "owner/data", IsDataset: true, Filters: []string{"train"}, ExactMatch: true}, Settings{Endpoint: srv.URL})
		if err != nil {
			t.Fatalf("PlanRepo with LFS companion: %v", err)
		}
		assertPaths(t, planPaths(plan), []string{"train.csv", "utils/tool.py"})
	})

	t.Run("GGUF-only remains matched-only", func(t *testing.T) {
		ggufFiles := []hfNode{
			{Type: "file", Path: "model-Q4_K_M.gguf", Size: 100},
			{Type: "file", Path: "docs/guide.md", Size: 4},
			{Type: "file", Path: "utils/tool.py", Size: 5},
		}
		srv := mockHFServerForPlan(t, ggufFiles)
		defer srv.Close()
		plan, err := PlanRepo(context.Background(), Job{Repo: "owner/data", IsDataset: true, Filters: []string{"Q4_K_M"}, ExactMatch: true}, Settings{Endpoint: srv.URL})
		if err != nil {
			t.Fatalf("PlanRepo GGUF-only: %v", err)
		}
		assertPaths(t, planPaths(plan), []string{"model-Q4_K_M.gguf"})
	})
}

// TestPlanRepoDatasetKnownConfigAndTokenizerCompanions covers the .json/.txt
// names that membership would otherwise treat as data(default)-or-companion.
func TestPlanRepoDatasetKnownConfigAndTokenizerCompanions(t *testing.T) {
	paths := []string{
		"requirements.txt",
		"requirements-dev.txt",
		"merges.txt",
		"vocab.json",
		"vocab.txt",
		"tokenizer.json",
		"tokenizer_config.json",
		"special_tokens_map.json",
		"added_tokens.json",
		"dataset_info.json",
		"dataset_infos.json",
		"CITATION.cff",
		"config.json",
		"README.md",
		"LICENSE.md",
		"docs/guide.md",
		"Docs/guide.md",
		"notebooks/run.ipynb",
		"docs/config.json",
		"tokenizer/vocab.txt",
		"COPYING",
		"NOTICE",
		"utils/tool.py",
	}
	for _, path := range paths {
		for _, tc := range []struct {
			name    string
			filters []string
		}{
			{name: "recognized group", filters: []string{"train"}},
			{name: "ordinary path", filters: []string{"train.csv"}},
		} {
			t.Run(tc.name+"/"+path, func(t *testing.T) {
				files := []hfNode{
					{Type: "file", Path: "train.csv", Size: 10},
					{Type: "file", Path: path, Size: 11},
				}
				srv := mockHFServerForPlan(t, files)
				defer srv.Close()
				plan, err := PlanRepo(context.Background(), Job{Repo: "owner/data", IsDataset: true, Filters: tc.filters, ExactMatch: true}, Settings{Endpoint: srv.URL})
				if err != nil {
					t.Fatalf("PlanRepo filters=%v path=%q: %v", tc.filters, path, err)
				}
				assertPaths(t, planPaths(plan), []string{"train.csv", path})
			})
		}
	}
}

// TestPlanRepoDatasetCompanionPathScope keeps docs/config/scripts out of
// split-like and arbitrary nested directories while retaining root and
// conventional companion directories.
func TestPlanRepoDatasetCompanionPathScope(t *testing.T) {
	positives := []string{
		"analysis.ipynb",
		"scripts/run.sh",
		"docs/guide.md",
		"Docs/other.md",
		"notebooks/run.ipynb",
		"utils/tool.py",
		"configs/train.yaml",
		"examples/demo.py",
		"tools/helper.sh",
		"config/ci.yml",
		"COPYING",
		"NOTICE",
		"test/README.md",
		"test/LICENSE.txt",
	}
	negatives := []string{
		"test/code.js",
		"test/page.html",
		"test/query.sql",
		"test/run.ipynb",
		"test/config.yaml",
		"test/config.json",
		"test/tokenizer.json",
		"test/vocab.txt",
		"test/requirements.txt",
		"tests/requirements.txt",
		"test/setup.py",
		"test/CITATION.cff",
		"test/COPYING",
		"test/NOTICE",
		"validation/query.sql",
		"dev/run.ipynb",
		"eval/helper.py",
		"elsewhere/tool.py",
		"data/other/script.sh",
	}
	files := []hfNode{{Type: "file", Path: "train.csv", Size: 10}}
	want := []string{"train.csv"}
	for _, path := range positives {
		files = append(files, hfNode{Type: "file", Path: path, Size: 11})
		want = append(want, path)
	}
	for _, path := range negatives {
		files = append(files, hfNode{Type: "file", Path: path, Size: 12})
	}
	for _, tc := range []struct {
		name    string
		filters []string
	}{
		{name: "recognized group", filters: []string{"train"}},
		{name: "ordinary path", filters: []string{"train.csv"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := mockHFServerForPlan(t, files)
			defer srv.Close()
			plan, err := PlanRepo(context.Background(), Job{Repo: "owner/data", IsDataset: true, Filters: tc.filters, ExactMatch: true}, Settings{Endpoint: srv.URL})
			if err != nil {
				t.Fatalf("PlanRepo filters=%v: %v", tc.filters, err)
			}
			assertPaths(t, planPaths(plan), want)
		})
	}
}

// TestPlanRepoDatasetSkipsUnmatchedModelPayloads keeps the positive companion
// controls in every case: only payload safety changes, not ordinary dataset
// companion retention.
func TestPlanRepoDatasetSkipsUnmatchedModelPayloads(t *testing.T) {
	payloads := []string{
		"weights/model.safetensors",
		"weights/model.onnx",
		"weights/model.pt",
		"weights/model.pth",
		"weights/custom.msgpack",
		"weights/checkpoint.h5",
		"weights/custom.tflite",
		"weights/model.ot",
		"weights/model.ggml",
		"weights/training-model.safetensors",
		"weights/model-training.pt",
		"weights/model.ckpt",
		"weights/checkpoint.ckpt",
		"weights/model.gguf-split",
		"weights/optimizer.bin",
		"weights/training_state.bin",
		"weights/training_args.bin",
		"weights/model.npy",
		"weights/model.pkl",
		"weights/custom.dat",
		"weights/custom.foo",
	}
	want := []string{"train.csv", "docs/guide.md", "utils/tool.py", "config.json", "README.md", "tokenizer/tokenizer.bin"}
	for _, payload := range payloads {
		for _, tc := range []struct {
			name    string
			filters []string
		}{
			{name: "recognized group", filters: []string{"train"}},
			{name: "ordinary path", filters: []string{"train.csv"}},
		} {
			t.Run(tc.name+"/"+payload, func(t *testing.T) {
				files := []hfNode{
					{Type: "file", Path: "train.csv", Size: 10},
					{Type: "file", Path: "docs/guide.md", Size: 4},
					{Type: "file", Path: "utils/tool.py", Size: 5},
					{Type: "file", Path: "config.json", Size: 7},
					{Type: "file", Path: "README.md", Size: 8},
					{Type: "file", Path: "tokenizer/tokenizer.bin", Size: 9},
					{Type: "file", Path: payload, Size: 100},
				}
				srv := mockHFServerForPlan(t, files)
				defer srv.Close()
				plan, err := PlanRepo(context.Background(), Job{Repo: "owner/data", IsDataset: true, Filters: tc.filters, ExactMatch: true}, Settings{Endpoint: srv.URL})
				if err != nil {
					t.Fatalf("PlanRepo filters=%v payload=%q: %v", tc.filters, payload, err)
				}
				assertPaths(t, planPaths(plan), want)
				for _, path := range planPaths(plan) {
					if path == payload {
						t.Fatalf("unmatched model payload %q was retained by filters=%v", payload, tc.filters)
					}
				}
			})
		}
	}
}

// TestPlanRepoDatasetSkipsPayloadsInRetainedCompanionScopes covers the root
// and directory-seed fallbacks, not only definite role evidence.
func TestPlanRepoDatasetSkipsPayloadsInRetainedCompanionScopes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		files   []hfNode
		filters []string
		want    []string
	}{
		{
			name: "root",
			files: []hfNode{
				{Type: "file", Path: "train.csv", Size: 10},
				{Type: "file", Path: "model.safetensors", Size: 100},
				{Type: "file", Path: "optimizer.bin", Size: 101},
				{Type: "file", Path: "checkpoint.ckpt", Size: 102},
				{Type: "file", Path: "config.json", Size: 7},
			},
			filters: []string{"train"},
			want:    []string{"train.csv", "config.json"},
		},
		{
			name: "seeded ancestor directory",
			files: []hfNode{
				{Type: "file", Path: "foo/train/part.csv", Size: 10},
				{Type: "file", Path: "foo/model.safetensors", Size: 100},
				{Type: "file", Path: "foo/optimizer.bin", Size: 101},
				{Type: "file", Path: "foo/checkpoint.ckpt", Size: 102},
				{Type: "file", Path: "foo/README.md", Size: 7},
			},
			filters: []string{"train"},
			want:    []string{"foo/train/part.csv", "foo/README.md"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := mockHFServerForPlan(t, tc.files)
			defer srv.Close()
			plan, err := PlanRepo(context.Background(), Job{Repo: "owner/data", IsDataset: true, Filters: tc.filters, ExactMatch: true}, Settings{Endpoint: srv.URL})
			if err != nil {
				t.Fatalf("PlanRepo filters=%v: %v", tc.filters, err)
			}
			assertPaths(t, planPaths(plan), tc.want)
		})
	}
}

// TestPlanRepoDatasetExplicitPayloadSelectionRemainsSelected keeps payload
// exclusion limited to unmatched companion fallback.
func TestPlanRepoDatasetExplicitPayloadSelectionRemainsSelected(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload string
		filters []string
		want    []string
	}{
		{
			name:    "group and payload",
			payload: "weights/model.ckpt",
			filters: []string{"train", "weights/model.ckpt"},
			want:    []string{"train.csv", "weights/model.ckpt", "config.json", "tokenizer/tokenizer.bin"},
		},
		{
			name:    "payload only",
			payload: "weights/optimizer.bin",
			filters: []string{"weights/optimizer.bin"},
			want:    []string{"weights/optimizer.bin", "config.json", "tokenizer/tokenizer.bin"},
		},
		{
			name:    "payload and tokenizer companion",
			payload: "weights/training_state.bin",
			filters: []string{"train", "weights/training_state.bin"},
			want:    []string{"train.csv", "weights/training_state.bin", "config.json", "tokenizer/tokenizer.bin"},
		},
		{
			name:    "unknown extension direct selection",
			payload: "weights/model.npy",
			filters: []string{"train", "weights/model.npy"},
			want:    []string{"train.csv", "weights/model.npy", "config.json", "tokenizer/tokenizer.bin"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := []hfNode{
				{Type: "file", Path: "train.csv", Size: 10},
				{Type: "file", Path: tc.payload, Size: 100},
				{Type: "file", Path: "config.json", Size: 7},
				{Type: "file", Path: "tokenizer/tokenizer.bin", Size: 9},
			}
			srv := mockHFServerForPlan(t, files)
			defer srv.Close()
			plan, err := PlanRepo(context.Background(), Job{Repo: "owner/data", IsDataset: true, Filters: tc.filters, ExactMatch: true}, Settings{Endpoint: srv.URL})
			if err != nil {
				t.Fatalf("PlanRepo filters=%v: %v", tc.filters, err)
			}
			assertPaths(t, planPaths(plan), tc.want)
		})
	}
}

func TestPlanRepoDatasetUnknownRootMetadataIsNotRetained(t *testing.T) {
	for _, metadata := range []string{"notes.txt", "data.json"} {
		t.Run("group/"+metadata, func(t *testing.T) {
			srv := mockHFServerForPlan(t, []hfNode{
				{Type: "file", Path: "train.csv", Size: 10},
				{Type: "file", Path: metadata, Size: 11},
			})
			defer srv.Close()
			plan, err := PlanRepo(context.Background(), Job{Repo: "owner/data", IsDataset: true, Filters: []string{"train"}, ExactMatch: true}, Settings{Endpoint: srv.URL})
			if err != nil {
				t.Fatalf("unknown root metadata %q group filter: %v", metadata, err)
			}
			assertPaths(t, planPaths(plan), []string{"train.csv"})
		})
		t.Run("ordinary/"+metadata, func(t *testing.T) {
			srv := mockHFServerForPlan(t, []hfNode{
				{Type: "file", Path: "train.csv", Size: 10},
				{Type: "file", Path: metadata, Size: 11},
			})
			defer srv.Close()
			plan, err := PlanRepo(context.Background(), Job{Repo: "owner/data", IsDataset: true, Filters: []string{"train.csv"}, ExactMatch: true}, Settings{Endpoint: srv.URL})
			if err != nil {
				t.Fatalf("ordinary path filter with unknown root metadata %q: %v", metadata, err)
			}
			assertPaths(t, planPaths(plan), []string{"train.csv"})
		})
	}
}

func TestPlanRepoDatasetReadmeLicenseAndDatasetInfoAnywhere(t *testing.T) {
	files := []hfNode{
		{Type: "file", Path: "train.csv", Size: 10},
		{Type: "file", Path: "train/README.txt", Size: 11},
		{Type: "file", Path: "train/LICENSE.txt", Size: 12},
		{Type: "file", Path: "data/dataset_info.json", Size: 13},
		{Type: "file", Path: "data/dataset_infos.json", Size: 14},
		{Type: "file", Path: "nested/dataset_info.json", Size: 15},
		{Type: "file", Path: "nested/README.md", Size: 16},
	}
	want := []string{"train.csv", "train/README.txt", "train/LICENSE.txt", "data/dataset_info.json", "data/dataset_infos.json", "nested/dataset_info.json", "nested/README.md"}
	for _, tc := range []struct {
		name    string
		filters []string
	}{
		{name: "group", filters: []string{"train"}},
		{name: "ordinary", filters: []string{"train.csv"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := mockHFServerForPlan(t, files)
			defer srv.Close()
			plan, err := PlanRepo(context.Background(), Job{Repo: "owner/data", IsDataset: true, Filters: tc.filters, ExactMatch: true}, Settings{Endpoint: srv.URL})
			if err != nil {
				t.Fatalf("PlanRepo filters=%v: %v", tc.filters, err)
			}
			assertPaths(t, planPaths(plan), want)
		})
	}
}

func TestPlanRepoDatasetKnownConfigPathScope(t *testing.T) {
	t.Run("arbitrary nested config is not retained", func(t *testing.T) {
		srv := mockHFServerForPlan(t, []hfNode{
			{Type: "file", Path: "train.csv", Size: 10},
			{Type: "file", Path: "nested/config.json", Size: 11},
		})
		defer srv.Close()
		plan, err := PlanRepo(context.Background(), Job{Repo: "owner/data", IsDataset: true, Filters: []string{"train"}, ExactMatch: true}, Settings{Endpoint: srv.URL})
		if err != nil {
			t.Fatalf("nested config PlanRepo: %v", err)
		}
		assertPaths(t, planPaths(plan), []string{"train.csv"})
	})
	t.Run("split-directory config and unknown skipped", func(t *testing.T) {
		srv := mockHFServerForPlan(t, []hfNode{
			{Type: "file", Path: "train.csv", Size: 10},
			{Type: "file", Path: "test/config.json", Size: 11},
			{Type: "file", Path: "partition/notes.txt", Size: 12},
		})
		defer srv.Close()
		plan, err := PlanRepo(context.Background(), Job{Repo: "owner/data", IsDataset: true, Filters: []string{"train"}, ExactMatch: true}, Settings{Endpoint: srv.URL})
		if err != nil {
			t.Fatalf("split-directory config/unknown PlanRepo: %v", err)
		}
		assertPaths(t, planPaths(plan), []string{"train.csv"})
	})
	t.Run("train config keeps data role, not unmatched companion", func(t *testing.T) {
		srv := mockHFServerForPlan(t, []hfNode{
			{Type: "file", Path: "validation.csv", Size: 10},
			{Type: "file", Path: "train/config.json", Size: 11},
		})
		defer srv.Close()
		plan, err := PlanRepo(context.Background(), Job{Repo: "owner/data", IsDataset: true, Filters: []string{"validation.csv"}, ExactMatch: true}, Settings{Endpoint: srv.URL})
		if err != nil {
			t.Fatalf("train config PlanRepo: %v", err)
		}
		assertPaths(t, planPaths(plan), []string{"validation.csv"})
	})
}

func TestPlanRepoDatasetKnownRootMetadataDoesNotBroadenGGUFOnly(t *testing.T) {
	srv := mockHFServerForPlan(t, []hfNode{
		{Type: "file", Path: "model-Q4_K_M.gguf", Size: 100},
		{Type: "file", Path: "README.txt", Size: 10},
		{Type: "file", Path: "LICENSE.txt", Size: 11},
		{Type: "file", Path: "dataset_infos.json", Size: 12},
		{Type: "file", Path: "dataset_info.json", Size: 13},
		{Type: "file", Path: "config.json", Size: 14},
		{Type: "file", Path: "README.md", Size: 15},
		{Type: "file", Path: "train/README.txt", Size: 16},
		{Type: "file", Path: "train/LICENSE.txt", Size: 17},
		{Type: "file", Path: "data/dataset_infos.json", Size: 18},
	})
	defer srv.Close()

	plan, err := PlanRepo(context.Background(), Job{Repo: "owner/data", IsDataset: true, Filters: []string{"Q4_K_M"}, ExactMatch: true}, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("PlanRepo: %v", err)
	}
	assertPaths(t, planPaths(plan), []string{"model-Q4_K_M.gguf"})
}

func TestPlanRepoDatasetKnownConfigDoesNotBroadenGGUFFallback(t *testing.T) {
	files := []hfNode{
		{Type: "file", Path: "validation/data.parquet", Size: 10},
		{Type: "file", Path: "train/config.json", Size: 11},
		{Type: "file", Path: "nested/dataset_info.json", Size: 12},
		{Type: "file", Path: "model-Q4_K_M.gguf", Size: 13},
	}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()
	plan, err := PlanRepo(context.Background(), Job{Repo: "owner/data", IsDataset: true, Filters: []string{"validation/"}, Excludes: []string{"dataset_info.json"}, ExactMatch: true}, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("validation path plan: %v", err)
	}
	assertPaths(t, planPaths(plan), []string{"validation/data.parquet"})

	plan, err = PlanRepo(context.Background(), Job{Repo: "owner/data", IsDataset: true, Filters: []string{"Q4_K_M"}, Excludes: []string{"dataset_info.json"}, ExactMatch: true}, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("GGUF fallback plan: %v", err)
	}
	if containsString(planPaths(plan), "train/config.json") {
		t.Fatalf("definite train data escaped through metadata fallback: %v", planPaths(plan))
	}
}

func TestPlanRepoDatasetGroupFiltersSelectMembership(t *testing.T) {
	files := []hfNode{
		{Type: "file", Path: "train.csv", Size: 10},
		{Type: "file", Path: "test_data.csv", Size: 10},
		{Type: "file", Path: "data/customdata/chunk.arrow", Size: 10},
		{Type: "file", Path: "default.parquet", Size: 10},
		{Type: "file", Path: "config.json", Size: 10},
		{Type: "file", Path: "dataset_info.json", Size: 10},
		{Type: "file", Path: "README.md", Size: 10},
	}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()
	plan, err := PlanRepo(context.Background(), Job{Repo: "owner/data", IsDataset: true, Filters: []string{"train"}, ExactMatch: true}, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("PlanRepo: %v", err)
	}
	assertPaths(t, planPaths(plan), []string{"train.csv", "config.json", "dataset_info.json", "README.md"})

	plan, err = PlanRepo(context.Background(), Job{Repo: "owner/data", IsDataset: true, Filters: []string{"customdata", "default"}, ExactMatch: true}, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("multi-group PlanRepo: %v", err)
	}
	assertPaths(t, planPaths(plan), []string{"data/customdata/chunk.arrow", "default.parquet", "config.json", "dataset_info.json", "README.md"})
}

func TestPlanRepoDatasetRootDefaultRoleVarianceRefuses(t *testing.T) {
	files := make([]hfNode, 1001)
	files[0] = hfNode{Type: "file", Path: "records.parquet", Size: 10}
	for i := 1; i < len(files); i++ {
		files[i] = hfNode{Type: "file", Path: fmt.Sprintf("records-%04d.json", i-1), Size: 10}
	}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()

	// Unnamed root JSON keeps its data/companion ambiguity. The data role is
	// selected by "default" while the companion role is skipped, so the final
	// emitted set is not invariant and the plan must refuse.
	plan, err := PlanRepo(context.Background(), Job{Repo: "owner/data", IsDataset: true, Filters: []string{"default"}, ExactMatch: true}, Settings{Endpoint: srv.URL})
	if plan != nil || !errors.Is(err, ErrAmbiguousSelection) {
		t.Fatalf("PlanRepo = (%+v, %v), want nil plan and ErrAmbiguousSelection", plan, err)
	}
}

func TestPlanRepoDatasetCancellationDuringDecisionReturnsNoPlan(t *testing.T) {
	const count = 1000
	files := make([]hfNode, count)
	for i := range files {
		files[i] = hfNode{Type: "file", Path: fmt.Sprintf("records-%04d.json", i), Size: 10}
	}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()
	transport := &inventoryBodyTransport{base: http.DefaultTransport}
	baseCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &cancelAfterInventoryContext{Context: baseCtx, read: &transport.read, limit: count + 10, cancel: cancel}
	httpc := &http.Client{Transport: transport}
	plan, err := scanRepo(ctx, httpc, "", Job{Repo: "owner/data", Revision: "main", IsDataset: true, Filters: []string{"default"}, ExactMatch: true}, Settings{Endpoint: srv.URL})
	if plan != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("scanRepo returned plan=%t err=%v; inventoryRead=%t checks=%d; want nil plan and context.Canceled", plan != nil, err, transport.read.Load(), ctx.checks.Load())
	}
	if !transport.read.Load() || ctx.checks.Load() < ctx.limit {
		t.Fatalf("cancellation was not triggered after the complete inventory: read=%t checks=%d limit=%d", transport.read.Load(), ctx.checks.Load(), ctx.limit)
	}
}

func TestPlanRepoDatasetUnknownRootMetadataSkippedAndExplicitSelectionWorks(t *testing.T) {
	files := []hfNode{
		{Type: "file", Path: "train.csv", Size: 10},
		{Type: "file", Path: "unknown.json", Size: 10},
		{Type: "file", Path: "elsewhere/notes.txt", Size: 10},
		{Type: "file", Path: "metadata.json", LFS: &hfLfsInfo{Size: 10}},
	}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()
	job := Job{Repo: "owner/data", IsDataset: true, Filters: []string{"train"}, ExactMatch: true}
	plan, err := PlanRepo(context.Background(), job, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("unknown root metadata should be skipped, not refused: %v", err)
	}
	assertPaths(t, planPaths(plan), []string{"train.csv"})
	job.Excludes = []string{"unknown.json"}
	plan, err = PlanRepo(context.Background(), job, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("excluded unknown metadata: %v", err)
	}
	assertPaths(t, planPaths(plan), []string{"train.csv"})
	job.Excludes = nil
	job.Filters = []string{"train", "unknown.json"}
	plan, err = PlanRepo(context.Background(), job, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("explicitly requested ambiguous file: %v", err)
	}
	assertPaths(t, planPaths(plan), []string{"train.csv", "unknown.json"})
	job.Filters = nil
	plan, err = PlanRepo(context.Background(), job, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("unfiltered plan: %v", err)
	}
	assertPaths(t, planPaths(plan), []string{"train.csv", "unknown.json", "elsewhere/notes.txt", "metadata.json"})
}

func TestPlanRepoDatasetGroupLabelPathCollisionIsMembershipOnly(t *testing.T) {
	files := []hfNode{
		{Type: "file", Path: "train.csv", Size: 10},
		{Type: "file", Path: "test/train.csv", Size: 10},
	}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()
	plan, err := PlanRepo(context.Background(), Job{Repo: "owner/data", IsDataset: true, Filters: []string{"train"}, ExactMatch: true}, Settings{Endpoint: srv.URL})
	if plan != nil || !errors.Is(err, ErrAmbiguousSelection) {
		t.Fatalf("PlanRepo = (%v, %v), want nil plan and fail-closed ErrAmbiguousSelection; test/train.csv is a conflicting membership, not a literal train selection", planPaths(plan), err)
	}

	// The conflicting file is still selectable through an explicit path form.
	plan, err = PlanRepo(context.Background(), Job{Repo: "owner/data", IsDataset: true, Filters: []string{"test/train.csv"}, ExactMatch: true}, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("explicit conflicting path PlanRepo: %v", err)
	}
	assertPaths(t, planPaths(plan), []string{"test/train.csv"})
}

func TestPlanRepoDatasetGroupCollisionCanBeResolvedByIndependentFilter(t *testing.T) {
	files := []hfNode{
		{Type: "file", Path: "train.csv", Size: 10},
		{Type: "file", Path: "test/train.csv", Size: 20},
	}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()

	plan, err := PlanRepo(context.Background(), Job{Repo: "owner/data", IsDataset: true, Filters: []string{"train", ".csv"}, ExactMatch: true, AppendFilterSubdir: true}, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("independent extension filter should resolve group/path collision: %v", err)
	}
	if got, want := planPaths(plan), []string{"train.csv", "test/train.csv"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("paths=%v, want %v", got, want)
	}
	for _, item := range plan.Items {
		want := ".csv"
		if item.RelativePath == "train.csv" {
			want = "train"
		}
		if item.Subdir != want {
			t.Errorf("%s attribution=%q, want longest applicable filter %q", item.RelativePath, item.Subdir, want)
		}
	}
}

func TestPlanRepoDatasetGroupCollisionWithRootCompanionRefuses(t *testing.T) {
	files := []hfNode{
		{Type: "file", Path: "train.csv", Size: 10},
		{Type: "file", Path: "test/train.csv", Size: 20},
		{Type: "file", Path: "README.md", Size: 5},
	}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()

	plan, err := PlanRepo(context.Background(), Job{Repo: "owner/data", IsDataset: true, Filters: []string{"train"}, ExactMatch: true}, Settings{Endpoint: srv.URL})
	if plan != nil || !errors.Is(err, ErrAmbiguousSelection) {
		t.Fatalf("PlanRepo = (%v, %v), want nil plan and fail-closed ErrAmbiguousSelection", planPaths(plan), err)
	}
}

func TestPlanRepoDatasetGroupsUseCompleteMembershipAndCompanionWorld(t *testing.T) {
	files := []hfNode{
		{Type: "file", Path: "train.csv", Size: 10},
		{Type: "file", Path: "data/train_data/part.parquet", LFS: &hfLfsInfo{Size: 100}},
		{Type: "file", Path: "data/train_data/schema.json", Size: 8},
		{Type: "file", Path: "data/train_data/README.md", Size: 4},
		{Type: "file", Path: "other/data.parquet", Size: 20},
		{Type: "file", Path: "data/english/chunk.arrow", Size: 30},
		{Type: "file", Path: "unknown.json", Size: 5},
		{Type: "file", Path: "unrelated/unknown.json", Size: 6},
		{Type: "file", Path: "metadata-lfs.json", LFS: &hfLfsInfo{Size: 7}},
		{Type: "file", Path: "README.md", Size: 3},
	}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()
	planFor := func(filters, excludes []string) (*Plan, error) {
		return PlanRepo(context.Background(), Job{Repo: "owner/data", IsDataset: true, Filters: filters, Excludes: excludes, ExactMatch: true}, Settings{Endpoint: srv.URL})
	}
	tests := []struct {
		name, filter string
		excludes     []string
		want         []string
	}{
		{"conventional root train", "train", []string{"unknown.json"}, []string{"train.csv", "data/train_data/README.md", "README.md"}},
		{"underscore custom group with companions", "train_data", []string{"unknown.json"}, []string{"data/train_data/part.parquet", "data/train_data/schema.json", "data/train_data/README.md", "README.md"}},
		{"unnamed default", "default", []string{"unknown.json", "metadata-lfs.json"}, []string{"data/train_data/README.md", "other/data.parquet", "README.md"}},
		{"custom data group", "english", []string{"unknown.json"}, []string{"data/train_data/README.md", "data/english/chunk.arrow", "README.md"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := planFor([]string{tc.filter}, tc.excludes)
			if err != nil {
				t.Fatalf("PlanRepo: %v", err)
			}
			if got := planPaths(plan); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("paths=%v, want %v", got, tc.want)
			}
		})
	}
	plan, err := planFor([]string{"train"}, nil)
	if err != nil {
		t.Fatalf("unknown root JSON should be skipped, not refused: %v", err)
	}
	if got, want := planPaths(plan), []string{"train.csv", "data/train_data/README.md", "README.md"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("skip unknown root JSON paths=%v, want %v", got, want)
	}
	plan, err = planFor([]string{"train"}, []string{"unknown.json"})
	if err != nil {
		t.Fatalf("exclude unknown root JSON: %v", err)
	}
	if got, want := planPaths(plan), []string{"train.csv", "data/train_data/README.md", "README.md"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("excluded unknown paths=%v, want %v", got, want)
	}
	plan, err = planFor(nil, nil)
	if err != nil {
		t.Fatalf("unfiltered dataset plan: %v", err)
	}
	if got, want := planPaths(plan), []string{"train.csv", "data/train_data/part.parquet", "data/train_data/schema.json", "data/train_data/README.md", "other/data.parquet", "data/english/chunk.arrow", "unknown.json", "unrelated/unknown.json", "metadata-lfs.json", "README.md"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unfiltered paths=%v, want %v", got, want)
	}
}

func TestPlanRepoDatasetExactQuantFilterIsMatchedOnly(t *testing.T) {
	files := []hfNode{
		{Type: "file", Path: "model-Q4_K_M.gguf", LFS: &hfLfsInfo{Size: 100}},
		{Type: "file", Path: "other.safetensors", Size: 20},
		{Type: "file", Path: "dataset_info.json", Size: 8},
		{Type: "file", Path: "README.md", Size: 5},
		{Type: "file", Path: "config.json", Size: 3},
		{Type: "file", Path: "tokenizer.json", Size: 4},
	}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()

	plan, err := PlanRepo(context.Background(), Job{Repo: "owner/data", IsDataset: true, Filters: []string{"Q4_K_M"}, ExactMatch: true}, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("PlanRepo: %v", err)
	}
	if got, want := planPaths(plan), []string{"model-Q4_K_M.gguf"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("paths=%v, want matched-only GGUF selection %v", got, want)
	}

	for _, metadata := range []string{"dataset_info.json", "README.md", "config.json", "tokenizer.json"} {
		plan, err := PlanRepo(context.Background(), Job{Repo: "owner/data", IsDataset: true, Filters: []string{metadata}, ExactMatch: true}, Settings{Endpoint: srv.URL})
		if err != nil {
			t.Fatalf("PlanRepo explicit metadata %q: %v", metadata, err)
		}
		if !containsString(planPaths(plan), metadata) {
			t.Errorf("explicit metadata %q was not selected: %v", metadata, planPaths(plan))
		}
	}
}

func TestPlanRepoDatasetTrainGroupIgnoresMatchedGGUFCompanions(t *testing.T) {
	files := []hfNode{
		{Type: "file", Path: "train_records.csv", Size: 10},
		{Type: "file", Path: "model-train.gguf", LFS: &hfLfsInfo{Size: 100}},
		{Type: "file", Path: "model-train-lfs.gguf", Size: 20},
		{Type: "file", Path: "test_records.csv", Size: 10},
		{Type: "file", Path: "model-test.gguf", LFS: &hfLfsInfo{Size: 100}},
	}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()

	plan, err := PlanRepo(context.Background(), Job{Repo: "owner/data", Revision: "main", IsDataset: true, Filters: []string{"train"}, ExactMatch: true, AppendFilterSubdir: true}, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("PlanRepo: %v", err)
	}
	// The group label selects membership only. GGUF companions whose names
	// merely contain "train" are not selected; explicit path filters are.
	if got, want := planPaths(plan), []string{"train_records.csv"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("paths=%v, want %v", got, want)
	}
	for _, item := range plan.Items {
		if item.Subdir != "train" {
			t.Errorf("%s attribution=%q, want train", item.RelativePath, item.Subdir)
		}
	}

	plan, err = PlanRepo(context.Background(), Job{Repo: "owner/data", Revision: "main", IsDataset: true, Filters: []string{"train", "model-train.gguf", "model-train-lfs.gguf"}, ExactMatch: true, AppendFilterSubdir: true}, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("PlanRepo explicit GGUF companions: %v", err)
	}
	if got, want := planPaths(plan), []string{"train_records.csv", "model-train.gguf", "model-train-lfs.gguf"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("explicit paths=%v, want %v", got, want)
	}
	for _, item := range plan.Items {
		if (item.RelativePath == "model-train.gguf" || item.RelativePath == "model-train-lfs.gguf") && item.DestinationSubdir == "" {
			t.Errorf("%s explicit GGUF destination subdir is empty, want a matched-filter prefix", item.RelativePath)
		}
	}
}

func TestPlanRepoDatasetCaseSuppressedEligibleInputStillValidatesHash(t *testing.T) {
	files := []hfNode{
		{Type: "file", Path: "Train.csv", Size: 10},
		{Type: "file", Path: "train.csv", Size: 10, Sha256: "not-a-canonical-sha256"},
	}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()
	plan, err := PlanRepo(context.Background(), Job{Repo: "owner/data", IsDataset: true, Filters: []string{"train"}, ExactMatch: true}, Settings{Endpoint: srv.URL})
	if err == nil || plan != nil || !strings.Contains(strings.ToLower(err.Error()), "non-canonical sha256") {
		t.Fatalf("case-suppressed eligible record was not metadata-validated: plan=%+v err=%v", plan, err)
	}
}

func TestPlanRepoDatasetAttributionUsesLongestThenEarliestFilter(t *testing.T) {
	srv := mockHFServerForPlan(t, []hfNode{{Type: "file", Path: "train.json", Size: 10}})
	defer srv.Close()
	for _, tc := range []struct {
		filters []string
		want    string
	}{
		{[]string{".json", "train"}, ".json"},
		{[]string{"train", ".json"}, "train"},
	} {
		plan, err := PlanRepo(context.Background(), Job{Repo: "owner/data", IsDataset: true, Filters: tc.filters, ExactMatch: true}, Settings{Endpoint: srv.URL})
		if err != nil {
			t.Fatalf("filters %v: PlanRepo: %v", tc.filters, err)
		}
		if len(plan.Items) != 1 || plan.Items[0].Subdir != tc.want {
			t.Errorf("filters %v: items=%+v; want one item attributed to %q", tc.filters, plan.Items, tc.want)
		}
	}
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

func TestPlanRepoGGUFQuantFilterKeepsMatchedREADMEOnly(t *testing.T) {
	files := []hfNode{
		{Type: "file", Path: "model-Q4_K_M.gguf", LFS: &hfLfsInfo{Size: 100}},
		{Type: "file", Path: "README-Q4_K_M.md", Size: 10},
		{Type: "file", Path: "config.json", Size: 10},
		{Type: "file", Path: "README.md", Size: 10},
	}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()

	plan, err := PlanRepo(context.Background(), Job{
		Repo: "owner/gguf", Revision: "main", Filters: []string{"Q4_K_M"}, ExactMatch: true,
	}, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("PlanRepo: %v", err)
	}
	assertPaths(t, planPaths(plan), []string{"model-Q4_K_M.gguf", "README-Q4_K_M.md"})
}

func TestPlanRepoREADMEOnlyFilterDoesNotEnableGGUFMode(t *testing.T) {
	files := []hfNode{
		{Type: "file", Path: "model-Q4_K_M.gguf", LFS: &hfLfsInfo{Size: 100}},
		{Type: "file", Path: "README-Q4_K_M.md", Size: 10},
		{Type: "file", Path: "config.json", Size: 10},
		{Type: "file", Path: "other.safetensors", Size: 10},
	}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()

	plan, err := PlanRepo(context.Background(), Job{
		Repo: "owner/gguf", Revision: "main", Filters: []string{"README-Q4_K_M.md"}, ExactMatch: true,
	}, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("PlanRepo: %v", err)
	}
	assertPaths(t, planPaths(plan), []string{"README-Q4_K_M.md", "config.json", "other.safetensors"})
}

func TestPlanRepoMixedGGUFFolderFilterKeepsCompanions(t *testing.T) {
	files := []hfNode{
		{Type: "file", Path: "weights/model.gguf", LFS: &hfLfsInfo{Size: 100}},
		{Type: "file", Path: "weights/model.safetensors", LFS: &hfLfsInfo{Size: 100}},
		{Type: "file", Path: "weights/other.safetensors", Size: 20},
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
	assertPaths(t, planPaths(plan), []string{"weights/model.gguf", "weights/model.safetensors", "weights/other.safetensors", "weights/README.md", "config.json"})
}

func TestPlanRepoOrdinaryModelRetainsUnmatchedNonLFSGGUF(t *testing.T) {
	files := []hfNode{
		{Type: "file", Path: "weights/model.safetensors", LFS: &hfLfsInfo{Size: 100}},
		{Type: "file", Path: "model-Q4_K_M.gguf", Size: 20},
		{Type: "file", Path: "large-unmatched.gguf", Size: 30},
		{Type: "file", Path: "excluded.gguf", Size: 40},
		{Type: "file", Path: "unmatched-lfs.gguf", LFS: &hfLfsInfo{Size: 50}},
	}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()
	job := Job{Repo: "owner/mixed-model", Filters: []string{"weights/model.safetensors"}, ExactMatch: true, Excludes: []string{"excluded"}}
	plan, err := PlanRepo(context.Background(), job, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("PlanRepo: %v", err)
	}
	assertPaths(t, planPaths(plan), []string{"weights/model.safetensors", "model-Q4_K_M.gguf", "large-unmatched.gguf"})
	if !GGUFPathMayBeEmitted("large-unmatched.gguf", job) {
		t.Fatal("pre-plan projection ruled out a non-LFS ordinary-model companion")
	}
	if GGUFPathMayBeEmitted("excluded.gguf", job) {
		t.Fatal("exclude must dominate conservative model projection")
	}
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

func TestPlanRepoTrailingSlashDoesNotSelectDirectoryLeaf(t *testing.T) {
	srv := mockHFServerForPlan(t, []hfNode{{Type: "file", Path: "unet", LFS: &hfLfsInfo{Size: 10}}})
	defer srv.Close()
	plan, err := PlanRepo(context.Background(), Job{Repo: "owner/model", Filters: []string{"unet/"}, ExactMatch: true}, Settings{Endpoint: srv.URL})
	if !errors.Is(err, ErrNoFilterMatch) {
		t.Fatalf("PlanRepo error = %v, want ErrNoFilterMatch", err)
	}
	if plan != nil {
		t.Fatalf("directory leaf unexpectedly selected: %+v", plan.Items)
	}
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

// TestPlanRepoRejectsNonCanonicalTreePath is the canonical-path intake
// regression: a tree listing whose file path carries a non-canonical spelling
// (a "." segment, an empty "//" segment, an in-root ".." segment, or a
// trailing "/") must fail the whole plan loudly, naming the offending path and
// its canonical form. SafeJoin cleans before joining, so two spellings of one
// remote file would both be planned and the two transfers would race to write
// a single cache entry (raw "Q5_K_M/../actual/model-Q4_K_M.gguf" next to the
// cleaned "actual/model-Q4_K_M.gguf", or "weights//model.gguf" next to
// "weights/model.gguf"). Dirty-key mirrors now fail loudly instead of being
// silently rewritten.
func TestPlanRepoRejectsNonCanonicalTreePath(t *testing.T) {
	cases := []struct {
		name string
		node hfNode
		want string // canonical form the error must also name
	}{
		{"dot segment", hfNode{Type: "file", Path: "foo/./bar.txt", Size: 10}, "foo/bar.txt"},
		{"in-root parent segment", hfNode{Type: "file", Path: "a/../b.txt", Size: 10}, "b.txt"},
		{"empty segment", hfNode{Type: "file", Path: "weights//model.gguf", LFS: &hfLfsInfo{Size: 10}}, "weights/model.gguf"},
		{"quant alias traversal", hfNode{Type: "file", Path: "Q5_K_M/../actual/model-Q4_K_M.gguf", LFS: &hfLfsInfo{Size: 10}}, "actual/model-Q4_K_M.gguf"},
		{"trailing slash", hfNode{Type: "file", Path: "weights/model.gguf/", Size: 10}, "weights/model.gguf"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := mockHFServerForPlan(t, []hfNode{c.node})
			defer srv.Close()
			_, err := PlanRepo(context.Background(), Job{Repo: "owner/repo", Revision: "main"}, Settings{Endpoint: srv.URL})
			if err == nil {
				t.Fatal("PlanRepo accepted a non-canonical tree path")
			}
			if !strings.Contains(err.Error(), "non-canonical") {
				t.Errorf("error should identify the non-canonical path, got: %v", err)
			}
			if !strings.Contains(err.Error(), c.node.Path) || !strings.Contains(err.Error(), c.want) {
				t.Errorf("error should identify the offending path %q and its canonical form %q, got: %v", c.node.Path, c.want, err)
			}
		})
	}
}

// TestPlanRepoLegacyGGUFSplitFilterSelectsCompleteQuant is the planner
// regression for the old ".gguf-split" shard extension: the selected quant's
// complete split group is planned, while another quant and ordinary model
// metadata are not.
func TestPlanRepoLegacyGGUFSplitFilterSelectsCompleteQuant(t *testing.T) {
	files := []hfNode{
		{Type: "file", Path: "model-Q4_K_M-00001-of-00002.gguf-split", Size: 100},
		{Type: "file", Path: "model-Q4_K_M-00002-of-00002.gguf-split", Size: 110},
		{Type: "file", Path: "model-Q6_K-00001-of-00002.gguf-split", Size: 90},
		{Type: "file", Path: "config.json", Size: 10},
	}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()

	plan, err := PlanRepo(context.Background(), Job{
		Repo: "owner/model", Revision: "main", Filters: []string{"Q4_K_M"}, ExactMatch: true,
	}, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("PlanRepo: %v", err)
	}
	assertPaths(t, planPaths(plan), []string{
		"model-Q4_K_M-00001-of-00002.gguf-split",
		"model-Q4_K_M-00002-of-00002.gguf-split",
	})

	// An ordinary non-GGUF model selection keeps its established non-LFS
	// companion retention; the legacy split recognition must not narrow it.
	plan, err = PlanRepo(context.Background(), Job{
		Repo: "owner/model", Revision: "main", Filters: []string{"config.json"}, ExactMatch: true,
	}, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("PlanRepo ordinary selection: %v", err)
	}
	assertPaths(t, planPaths(plan), []string{
		"model-Q4_K_M-00001-of-00002.gguf-split",
		"model-Q4_K_M-00002-of-00002.gguf-split",
		"model-Q6_K-00001-of-00002.gguf-split",
		"config.json",
	})
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
