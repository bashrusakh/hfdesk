// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// strictSelectionTree carries the traps strict mode must not fall into: a
// non-LFS companion at the root, a basename alias in a subdirectory, and an
// LFS sibling sharing the selected directory prefix.
var strictSelectionTree = []hfNode{
	{Type: "file", Path: "config.json", Size: 10},
	{Type: "file", Path: "sub/config.json", Size: 11},
	{Type: "file", Path: "README.md", Size: 14},
	{Type: "file", Path: "weights/model.safetensors", LFS: &hfLfsInfo{Size: 100}},
	{Type: "file", Path: "other/weights/model.safetensors", LFS: &hfLfsInfo{Size: 101}},
}

// planStrict runs a strict selection against the fixture endpoint.
func planStrict(t *testing.T, job Job, endpoint string) (*Plan, error) {
	t.Helper()
	job.Repo, job.Revision, job.StrictFiles = "owner/repo", "main", true
	return PlanRepo(context.Background(), job, Settings{Endpoint: endpoint})
}

func TestPlanStrictFileSelection(t *testing.T) {
	srv := mockHFServerForPlan(t, strictSelectionTree)
	t.Cleanup(srv.Close)

	t.Run("exact selection", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			filters []string
			want    []string
		}{
			{"root file is exact, not a basename alias", []string{"config.json"}, []string{"config.json"}},
			{"./name root alias spelling", []string{"./config.json"}, []string{"config.json"}},
			{"repeated normalized spellings collapse", []string{"./config.json", "config.json"}, []string{"config.json"}},
			{"no non-LFS companion leak or prefix sibling", []string{"weights/model.safetensors"}, []string{"weights/model.safetensors"}},
			{"N paths plan exactly N files", []string{"./config.json", "README.md", "weights/model.safetensors"}, []string{"config.json", "README.md", "weights/model.safetensors"}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				plan, err := planStrict(t, Job{Filters: tc.filters}, srv.URL)
				if err != nil {
					t.Fatalf("PlanRepo: %v", err)
				}
				assertPaths(t, planPaths(plan), tc.want)
				for _, item := range plan.Items {
					if item.Subdir != "" || item.DestinationSubdir != "" {
						t.Fatalf("strict item %q carries attribution %q/%q; want repository-relative destination", item.RelativePath, item.Subdir, item.DestinationSubdir)
					}
				}
			})
		}
	})

	t.Run("failures are typed and fail closed", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			job     Job
			wantErr error
			inMsgs  []string
		}{
			{"absent path names the miss", Job{Filters: []string{"config.json", "gone.bin"}}, ErrNoFileMatch, []string{"gone.bin"}},
			{"case-variant spelling miss names the miss", Job{Filters: []string{"CONFIG.JSON"}}, ErrNoFileMatch, []string{"CONFIG.JSON"}},
			{"single segment is not a substring filter", Job{Filters: []string{"model"}}, ErrNoFileMatch, []string{"model"}},
			{"empty selection", Job{}, ErrInvalidFileSelection, []string{"empty"}},
			{"trailing slash directory entry", Job{Filters: []string{"weights/"}}, ErrInvalidFileSelection, []string{"weights/"}},
			{"absolute entry", Job{Filters: []string{"/config.json"}}, ErrInvalidFileSelection, []string{"/config.json"}},
			{"non-canonical entry", Job{Filters: []string{"a/../b"}}, ErrInvalidFileSelection, []string{"a/../b"}},
			{"case-colliding entries name both spellings", Job{Filters: []string{"config.json", "CONFIG.JSON"}}, ErrInvalidFileSelection, []string{`"config.json"`, `"CONFIG.JSON"`}},
			{"combined with excludes", Job{Filters: []string{"config.json"}, Excludes: []string{"x"}}, ErrInvalidFileSelection, []string{"excludes"}},
			{"combined with branch variant", Job{Filters: []string{"config.json"}, BranchVariant: true, Revision: "4_25"}, ErrInvalidFileSelection, []string{"strict file selection", "branch variant"}},
			{"combined with appendFilterSubdir", Job{Filters: []string{"config.json"}, AppendFilterSubdir: true}, ErrInvalidFileSelection, []string{"strict file selection", "appendFilterSubdir"}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				plan, err := planStrict(t, tc.job, srv.URL)
				if plan != nil || !errors.Is(err, tc.wantErr) {
					t.Fatalf("PlanRepo = (%v, %v), want nil plan and %v", planPaths(plan), err, tc.wantErr)
				}
				for _, want := range tc.inMsgs {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("PlanRepo error %q does not mention %q", err, want)
					}
				}
			})
		}
	})
}

// TestPlanStrictFileSelectionIsCaseExact pins the no-substitution rule for a
// tree that contains case-variant files: the plan holds the selected
// spelling only, never the case-variant file the API lists first, and a
// selection whose spelling is absent from the tree is a miss — not a silent
// fallthrough to the case-variant sibling.
func TestPlanStrictFileSelectionIsCaseExact(t *testing.T) {
	srv := mockHFServerForPlan(t, []hfNode{
		{Type: "file", Path: "A.bin", Size: 10},
		{Type: "file", Path: "a.bin", Size: 11},
		{Type: "file", Path: "B.bin", Size: 12},
	})
	t.Cleanup(srv.Close)

	for _, tc := range []struct {
		name    string
		filters []string
		want    []string
	}{
		{"lowercase selection ignores the earlier A.bin", []string{"a.bin"}, []string{"a.bin"}},
		{"uppercase selection plans A.bin only", []string{"A.bin"}, []string{"A.bin"}},
		{"unselected case-variant sibling never joins the plan", []string{"B.bin"}, []string{"B.bin"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := planStrict(t, Job{Filters: tc.filters}, srv.URL)
			if err != nil {
				t.Fatalf("PlanRepo: %v", err)
			}
			assertPaths(t, planPaths(plan), tc.want)
		})
	}
}

// TestPlanStrictFileSelectionIsExactForDatasets pins the gate that keeps the
// dataset membership/companion machinery out of strict mode.
func TestPlanStrictFileSelectionIsExactForDatasets(t *testing.T) {
	srv := mockHFServerForPlan(t, []hfNode{
		{Type: "file", Path: "train.csv", Size: 10},
		{Type: "file", Path: "dataset_info.json", Size: 11},
		{Type: "file", Path: "README.md", Size: 12},
	})
	t.Cleanup(srv.Close)
	plan, err := planStrict(t, Job{IsDataset: true, Filters: []string{"train.csv"}}, srv.URL)
	if err != nil {
		t.Fatalf("PlanRepo: %v", err)
	}
	assertPaths(t, planPaths(plan), []string{"train.csv"})
}

// TestLegacyFilterPlanKeepsCompanionsBesideStrictMode pins the preserved
// behavior: the same filter list with StrictFiles off keeps the established
// companion policy byte-identical, while strict mode plans only the path.
func TestLegacyFilterPlanKeepsCompanionsBesideStrictMode(t *testing.T) {
	srv := mockHFServerForPlan(t, strictSelectionTree)
	t.Cleanup(srv.Close)

	legacy, err := PlanRepo(context.Background(), Job{
		Repo: "owner/repo", Revision: "main", Filters: []string{"weights/model.safetensors"}, ExactMatch: true,
	}, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("legacy PlanRepo: %v", err)
	}
	assertPaths(t, planPaths(legacy), []string{
		"config.json", "sub/config.json", "README.md", "weights/model.safetensors",
	})

	strict, err := planStrict(t, Job{Filters: []string{"weights/model.safetensors"}, ExactMatch: true}, srv.URL)
	if err != nil {
		t.Fatalf("strict PlanRepo: %v", err)
	}
	assertPaths(t, planPaths(strict), []string{"weights/model.safetensors"})
}
