package hfdownloader

import (
	"errors"
	"reflect"
	"testing"
)

func TestExactLiteralBasenameAndPath(t *testing.T) {
	for _, tc := range []struct {
		path, filter string
		want         bool
	}{
		{"model-Q4_0.gguf", "./model-Q4_0.gguf", true},
		{"other-model-Q4_0.gguf", "./model-Q4_0.gguf", false},
		{"nested/model-Q4_0.gguf", "./model-Q4_0.gguf", false},
		{"nested/model-Q4_0.gguf", "nested/model-Q4_0.gguf", true},
		{"nested/model-Q4_0.gguf", "model-Q4_0.gguf", true}, // unchanged legacy basename matching
		{"nested/model-Q4_0.gguf", "./nested/model-Q4_0.gguf", false},
		{"nested/model-Q4_0.gguf", "./../model-Q4_0.gguf", false},
		{"model-Q6_K_XL.gguf", "model-Q6_K.gguf", false},
	} {
		if got := filterMatchesPath(tc.path, tc.filter, true); got != tc.want {
			t.Errorf("filterMatchesPath(%q, %q, exact) = %v, want %v", tc.path, tc.filter, got, tc.want)
		}
	}
}

func TestPlanRepoRootSelectorAndLegacyBasename(t *testing.T) {
	files := []hfNode{
		{Type: "file", Path: "model-Q4_0.gguf", LFS: &hfLfsInfo{Size: 10}},
		{Type: "file", Path: "other-model-Q4_0.gguf", LFS: &hfLfsInfo{Size: 11}},
		{Type: "file", Path: "nested/model-Q4_0.gguf", LFS: &hfLfsInfo{Size: 12}},
	}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()

	planFor := func(filter string, appendSubdir bool) []PlanItem {
		t.Helper()
		plan, err := PlanRepo(t.Context(), Job{Repo: "owner/repo", Filters: []string{filter}, ExactMatch: true, AppendFilterSubdir: appendSubdir}, Settings{Endpoint: srv.URL})
		if err != nil {
			t.Fatalf("PlanRepo filter %q: %v", filter, err)
		}
		return plan.Items
	}
	root := planFor("./model-Q4_0.gguf", true)
	if len(root) != 1 || root[0].RelativePath != "model-Q4_0.gguf" {
		t.Fatalf("root selector planned %+v, want only root artifact", root)
	}
	if root[0].Subdir != "model-Q4_0.gguf" || root[0].DestinationSubdir != "model-Q4_0.gguf" {
		t.Fatalf("root selector marker leaked into attribution/destination: %+v", root[0])
	}
	nested := planFor("nested/model-Q4_0.gguf", false)
	if len(nested) != 1 || nested[0].RelativePath != "nested/model-Q4_0.gguf" {
		t.Fatalf("nested selector planned %+v, want only nested artifact", nested)
	}
	legacy := planFor("model-Q4_0.gguf", false)
	legacyPaths := make([]string, 0, len(legacy))
	for _, item := range legacy {
		legacyPaths = append(legacyPaths, item.RelativePath)
	}
	if got, want := legacyPaths, []string{"model-Q4_0.gguf", "other-model-Q4_0.gguf", "nested/model-Q4_0.gguf"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("legacy basename filters changed: got %v, want %v", got, want)
	}
}

func TestRootArtifactSelectorMatchesGGUFProjection(t *testing.T) {
	job := Job{Filters: []string{"./model-Q4_0.gguf"}, ExactMatch: true}
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"model-Q4_0.gguf", true},
		{"other-model-Q4_0.gguf", false},
		{"nested/model-Q4_0.gguf", false},
	} {
		if got := GGUFPathSelected(tc.path, job.Filters, nil, job.ExactMatch); got != tc.want {
			t.Errorf("GGUFPathSelected(%q) = %v, want %v", tc.path, got, tc.want)
		}
		// This API is intentionally a conservative MAY projection, not the exact
		// selection predicate; its safe superset includes these aliases.
		if got := GGUFPathMayBeEmitted(tc.path, job); !got {
			t.Errorf("GGUFPathMayBeEmitted(%q) = false, want conservative true", tc.path)
		}
	}
}

func TestDatasetRootArtifactSelectorAttributionDoesNotLeakMarker(t *testing.T) {
	files := []hfNode{
		{Type: "file", Path: "model-Q4_0.gguf", Size: 10},
		{Type: "file", Path: "dataset_info.json", Size: 2},
	}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()

	for _, appendSubdir := range []bool{false, true} {
		plan, err := PlanRepo(t.Context(), Job{
			Repo: "owner/data", IsDataset: true, Filters: []string{"./model-Q4_0.gguf"},
			ExactMatch: true, AppendFilterSubdir: appendSubdir,
		}, Settings{Endpoint: srv.URL})
		if err != nil {
			t.Fatalf("dataset GGUF PlanRepo append=%v: %v", appendSubdir, err)
		}
		var selected *PlanItem
		for i := range plan.Items {
			if plan.Items[i].RelativePath == "model-Q4_0.gguf" {
				selected = &plan.Items[i]
			}
		}
		if selected == nil {
			t.Fatalf("dataset GGUF plan append=%v omitted raw repository path: %+v", appendSubdir, plan.Items)
		}
		if selected.Subdir != "model-Q4_0.gguf" || selected.DestinationSubdir != "model-Q4_0.gguf" {
			t.Errorf("dataset GGUF attribution append=%v = %q/%q; want marker-free basename", appendSubdir, selected.Subdir, selected.DestinationSubdir)
		}
	}
	plan, err := PlanRepo(t.Context(), Job{
		Repo: "owner/data", IsDataset: true, Filters: []string{"./model-Q4_0.gguf"},
		Excludes: []string{"model-Q4_0.gguf"}, ExactMatch: true,
	}, Settings{Endpoint: srv.URL})
	if !errors.Is(err, ErrNoFilterMatch) || plan != nil {
		t.Fatalf("excluded literal artifact selection plan=%+v err=%v; want ErrNoFilterMatch", plan, err)
	}
}

func TestDatasetRootNonGGUFSelectorAttributionDoesNotLeakMarker(t *testing.T) {
	srv := mockHFServerForPlan(t, []hfNode{
		{Type: "file", Path: "data.csv", Size: 10},
		{Type: "file", Path: "dataset_info.json", Size: 2},
	})
	defer srv.Close()
	plan, err := PlanRepo(t.Context(), Job{
		Repo: "owner/data", IsDataset: true, Filters: []string{"./data.csv"}, ExactMatch: true, AppendFilterSubdir: true,
	}, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("dataset non-GGUF PlanRepo: %v", err)
	}
	for _, item := range plan.Items {
		if item.RelativePath == "data.csv" {
			if item.Subdir != "data.csv" || item.DestinationSubdir != "" {
				t.Fatalf("dataset data attribution/destination = %q/%q, want data.csv/empty", item.Subdir, item.DestinationSubdir)
			}
			return
		}
	}
	t.Fatalf("dataset plan omitted raw data path: %+v", plan.Items)
}
