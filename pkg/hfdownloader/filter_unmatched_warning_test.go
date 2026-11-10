// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// planWithFilters runs PlanRepo against the shared mock hub for a single
// job under test.
func planWithFilters(t *testing.T, files []hfNode, job Job) (*Plan, error) {
	t.Helper()
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()
	return PlanRepo(context.Background(), job, Settings{Endpoint: srv.URL})
}

// findFilterWarning returns the single filters_unmatched warning of a plan,
// or nil when the plan carries none.
func findFilterWarning(plan *Plan) *PlanWarning {
	if plan == nil {
		return nil
	}
	var found *PlanWarning
	for i := range plan.Warnings {
		if plan.Warnings[i].Code == PlanWarningFiltersUnmatched {
			if found != nil {
				return &plan.Warnings[i] // more than one: fail on the caller's count check
			}
			found = &plan.Warnings[i]
		}
	}
	return found
}

// TestPlanRepoFilterUnmatchedWarning pins the D4 accounting at plan level:
// one warning names exactly the patterns that selected nothing, a fully
// matched or filterless request stays warning-free, a total-zero selection
// keeps ErrNoFilterMatch (with or without retained companions) instead of
// becoming a warning, shaped filters are each accounted through the
// established matching semantics, dataset-membership selection counts as a
// match for its own label, and an excluded-only path counts as unmatched.
func TestPlanRepoFilterUnmatchedWarning(t *testing.T) {
	ggufTree := []hfNode{
		{Type: "file", Path: "model-Q4_K_M.gguf", Size: 10, LFS: &hfLfsInfo{Size: 10}},
	}
	ggufWithCompanion := append(append([]hfNode{}, ggufTree...),
		hfNode{Type: "file", Path: "config.json", Size: 5})
	safetensorsTree := []hfNode{
		{Type: "file", Path: "weights/model.safetensors", Size: 10, LFS: &hfLfsInfo{Size: 10}},
		{Type: "file", Path: "model.bin", Size: 6},
	}
	shardTree := []hfNode{
		{Type: "file", Path: "model-00001-of-00002.safetensors", Size: 10, LFS: &hfLfsInfo{Size: 10}},
		{Type: "file", Path: "model-00002-of-00002.safetensors", Size: 11, LFS: &hfLfsInfo{Size: 11}},
	}
	quantTree := []hfNode{
		{Type: "file", Path: "model-q4.gguf", Size: 10, LFS: &hfLfsInfo{Size: 10}},
		{Type: "file", Path: "model-fp16.gguf", Size: 11, LFS: &hfLfsInfo{Size: 11}},
	}
	datasetTree := []hfNode{
		{Type: "file", Path: "train/part-00000.csv", Size: 10},
		{Type: "file", Path: "test/train-part-00001.csv", Size: 11},
		{Type: "file", Path: "validation/train-part-00002.csv", Size: 12},
	}

	cases := []struct {
		name      string
		files     []hfNode
		job       Job
		wantPaths []string // optional: pin the plan when preservation matters
		wantWarn  []string // segments of the one filters_unmatched warning; nil = none
		wantErr   error
	}{
		{
			name:  "one of two filters matched names only the unmatched one",
			files: ggufWithCompanion,
			job:   Job{Repo: "owner/repo", Filters: []string{"q4", "q5"}},
			// GGUF mode is matched-only: the config companion is dropped.
			wantPaths: []string{"model-Q4_K_M.gguf"},
			wantWarn:  []string{"q5"},
		},
		{
			name:     "fully matched filter set carries no warning",
			files:    ggufWithCompanion,
			job:      Job{Repo: "owner/repo", Filters: []string{"q4"}},
			wantWarn: nil,
		},
		{
			name:      "request without filters is warning-free",
			files:     ggufWithCompanion,
			job:       Job{Repo: "owner/repo"},
			wantPaths: []string{"model-Q4_K_M.gguf", "config.json"},
			wantWarn:  nil,
		},
		{
			// No companion can retain the request: the plan is empty and the
			// total-zero selection keeps the hard error, never a warning.
			name:    "total zero without companions errors",
			files:   ggufTree,
			job:     Job{Repo: "owner/repo", Filters: []string{"q9"}},
			wantErr: ErrNoFilterMatch,
		},
		{
			// A retained non-LFS config keeps the plan non-empty, but
			// retention never satisfies the filtered request: the error wins.
			name:    "total zero with a retained companion errors",
			files:   ggufWithCompanion,
			job:     Job{Repo: "owner/repo", Filters: []string{"q9"}},
			wantErr: ErrNoFilterMatch,
		},
		{
			name:  "shaped exact filters are each accounted",
			files: safetensorsTree,
			job: Job{Repo: "owner/repo",
				Filters:    []string{".safetensors", "subdir/", "./absent.bin"},
				ExactMatch: true},
			// Request order preserved; the extension matched, the directory
			// and root-basename selectors did not.
			wantPaths: []string{"weights/model.safetensors", "model.bin"},
			wantWarn:  []string{"subdir/", "./absent.bin"},
		},
		{
			name:  "shard-token substring filter counts as matched",
			files: shardTree,
			job:   Job{Repo: "owner/repo", Filters: []string{"00001-of", "q9"}},
			// Only the first shard path contains the token; the unmatched
			// LFS sibling drops, and only "q9" is named.
			wantPaths: []string{"model-00001-of-00002.safetensors"},
			wantWarn:  []string{"q9"},
		},
		{
			name:  "excluded-only path counts as unmatched",
			files: quantTree,
			job: Job{Repo: "owner/repo",
				Filters:  []string{"q4", "fp16"},
				Excludes: []string{"fp16"}},
			// The fp16 file is excluded before selection, so its pattern
			// credited nothing while "q4" kept the request alive.
			wantPaths: []string{"model-q4.gguf"},
			wantWarn:  []string{"fp16"},
		},
		{
			name:  "dataset membership selection counts as matched for its label",
			files: datasetTree,
			job: Job{Repo: "owner/data", IsDataset: true,
				Filters: []string{"train", "q9"}, ExactMatch: true},
			// "train" selects its group through membership evidence — it is
			// matched — while the bogus direct pattern is named.
			wantPaths: []string{"train/part-00000.csv"},
			wantWarn:  []string{"q9"},
		},
		{
			name:  "dataset group filter alone stays warning-free",
			files: datasetTree,
			job: Job{Repo: "owner/data", IsDataset: true,
				Filters: []string{"train"}, ExactMatch: true},
			wantPaths: []string{"train/part-00000.csv"},
			wantWarn:  nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := planWithFilters(t, tc.files, tc.job)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) || plan != nil {
					t.Fatalf("PlanRepo = (%v, %v), want nil plan with %v", plan, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("PlanRepo: %v", err)
			}
			if tc.wantPaths != nil {
				assertPaths(t, planPaths(plan), tc.wantPaths)
			}
			warning := findFilterWarning(plan)
			if tc.wantWarn == nil {
				if warning != nil {
					t.Fatalf("warnings = %+v, want no filters_unmatched warning", plan.Warnings)
				}
				if len(plan.Warnings) != 0 {
					t.Fatalf("warnings = %+v, want none", plan.Warnings)
				}
				return
			}
			if warning == nil {
				t.Fatalf("warnings = %+v, want one filters_unmatched %v", plan.Warnings, tc.wantWarn)
			}
			if len(plan.Warnings) != 1 {
				t.Errorf("warnings = %+v, want exactly the filters_unmatched warning", plan.Warnings)
			}
			if !reflect.DeepEqual(warning.Segments, tc.wantWarn) {
				t.Errorf("segments = %v, want %v", warning.Segments, tc.wantWarn)
			}
			for _, segment := range tc.wantWarn {
				if !strings.Contains(warning.Message, segment) {
					t.Errorf("message %q does not name %q", warning.Message, segment)
				}
			}
		})
	}
}

// TestPlanRepoFilterWarningOrdersAfterShardWarnings pins the array order
// when one request produces both warning families: the established
// shard-range warnings keep their leading positions and the filters_unmatched
// warning joins the same extensible array after them.
func TestPlanRepoFilterWarningOrdersAfterShardWarnings(t *testing.T) {
	files := []hfNode{
		{Type: "file", Path: "model-00001-of-00004.safetensors", Size: 10, LFS: &hfLfsInfo{Size: 10}},
		{Type: "file", Path: "model-00002-of-00004.safetensors", Size: 10, LFS: &hfLfsInfo{Size: 10}},
		{Type: "file", Path: "model-00003-of-00004.safetensors", Size: 10, LFS: &hfLfsInfo{Size: 10}},
		{Type: "file", Path: "model-00004-of-00004.safetensors", Size: 10, LFS: &hfLfsInfo{Size: 10}},
	}
	plan, err := planWithFilters(t, files, Job{
		Repo:        "owner/repo",
		Filters:     []string{".safetensors", "q9"},
		ExactMatch:  true,
		ShardRanges: "1,9-10",
	})
	if err != nil {
		t.Fatalf("PlanRepo: %v", err)
	}
	wantCodes := []string{
		PlanWarningShardRangesIneffectiveSegments,
		PlanWarningFiltersUnmatched,
	}
	if len(plan.Warnings) != len(wantCodes) {
		t.Fatalf("warnings = %+v, want %v", plan.Warnings, wantCodes)
	}
	for i, code := range wantCodes {
		if plan.Warnings[i].Code != code {
			t.Errorf("warning %d code = %q, want %q (all: %+v)", i, plan.Warnings[i].Code, code, plan.Warnings)
		}
	}
	if !reflect.DeepEqual(plan.Warnings[1].Segments, []string{"q9"}) {
		t.Errorf("filter warning segments = %v, want [q9]", plan.Warnings[1].Segments)
	}
}
