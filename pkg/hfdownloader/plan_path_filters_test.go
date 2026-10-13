// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"reflect"
	"strings"
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

func TestPlanRepoDatasetGroupAmbiguityIsRelevantAndExcludesResolveIt(t *testing.T) {
	files := []hfNode{
		{Type: "file", Path: "train.csv", Size: 10},
		{Type: "file", Path: "unknown.json", Size: 10},
		{Type: "file", Path: "elsewhere/notes.txt", Size: 10},
		{Type: "file", Path: "metadata.json", LFS: &hfLfsInfo{Size: 10}},
	}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()
	job := Job{Repo: "owner/data", IsDataset: true, Filters: []string{"train"}, ExactMatch: true}
	if plan, err := PlanRepo(context.Background(), job, Settings{Endpoint: srv.URL}); err == nil || plan != nil {
		t.Fatalf("ambiguous root companion plan = %+v, err=%v; want whole-plan refusal", plan, err)
	}
	job.Excludes = []string{"unknown.json"}
	plan, err := PlanRepo(context.Background(), job, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("excluded ambiguity: %v", err)
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

func TestPlanRepoDatasetGroupLabelPathCollisionRefuses(t *testing.T) {
	files := []hfNode{
		{Type: "file", Path: "train.csv", Size: 10},
		{Type: "file", Path: "test/train.csv", Size: 10},
	}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()
	plan, err := PlanRepo(context.Background(), Job{Repo: "owner/data", IsDataset: true, Filters: []string{"train"}, ExactMatch: true}, Settings{Endpoint: srv.URL})
	if err == nil || plan != nil {
		t.Fatalf("colliding label/path plan = %+v, err=%v; want refusal", plan, err)
	}
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
		if item.Subdir != "train" {
			t.Errorf("%s attribution=%q, want longest applicable filter train", item.RelativePath, item.Subdir)
		}
	}
}

func TestPlanRepoDatasetGroupCollisionWithNonDatasetInterpretationRefuses(t *testing.T) {
	files := []hfNode{
		{Type: "file", Path: "train.csv", Size: 10},
		{Type: "file", Path: "test/train.csv", Size: 20},
		{Type: "file", Path: "README.md", Size: 5},
	}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()

	plan, err := PlanRepo(context.Background(), Job{Repo: "owner/data", IsDataset: true, Filters: []string{"train"}, ExactMatch: true}, Settings{Endpoint: srv.URL})
	if err == nil || plan != nil || !strings.Contains(strings.ToLower(err.Error()), "ambiguous") {
		t.Fatalf("group/path interpretation plan=%+v err=%v; want whole-plan ambiguity refusal", plan, err)
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
		{"conventional root train", "train", []string{"unknown.json"}, []string{"train.csv", "README.md"}},
		{"underscore custom group with companions", "train_data", []string{"unknown.json"}, []string{"data/train_data/part.parquet", "data/train_data/schema.json", "data/train_data/README.md", "README.md"}},
		{"unnamed default", "default", []string{"unknown.json", "metadata-lfs.json"}, []string{"other/data.parquet", "README.md"}},
		{"custom data group", "english", []string{"unknown.json"}, []string{"data/english/chunk.arrow", "README.md"}},
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
	if plan, err := planFor([]string{"train"}, nil); err == nil || plan != nil {
		t.Fatalf("ambiguous root JSON should refuse whole plan: plan=%+v err=%v", plan, err)
	}
	plan, err := planFor([]string{"train"}, []string{"unknown.json"})
	if err != nil {
		t.Fatalf("exclude resolves relevant root ambiguity: %v", err)
	}
	if got, want := planPaths(plan), []string{"train.csv", "README.md"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("excluded ambiguity paths=%v, want %v", got, want)
	}
	plan, err = planFor(nil, nil)
	if err != nil {
		t.Fatalf("unfiltered dataset plan: %v", err)
	}
	if got, want := planPaths(plan), []string{"train.csv", "data/train_data/part.parquet", "data/train_data/schema.json", "data/train_data/README.md", "other/data.parquet", "data/english/chunk.arrow", "unknown.json", "unrelated/unknown.json", "metadata-lfs.json", "README.md"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unfiltered paths=%v, want %v", got, want)
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
