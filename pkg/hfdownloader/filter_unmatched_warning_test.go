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

// filterNotices returns every filters_unmatched notice of a plan's Notices
// (the `filter "…" matched nothing` / `filters "…" matched nothing`
// sentences), or nil when the plan carries none. Split-mention notices
// start with "excluded" and shard notices with "shard", so the prefix
// match is exact against the producer's own two spellings.
func filterNotices(plan *Plan) []string {
	if plan == nil {
		return nil
	}
	var found []string
	for _, notice := range plan.Notices {
		if strings.HasPrefix(notice, `filter "`) || strings.HasPrefix(notice, `filters "`) {
			found = append(found, notice)
		}
	}
	return found
}

// TestPlanRepoFilterUnmatchedNotice pins the D4 accounting at plan level:
// one notice names exactly the patterns that selected nothing, a fully
// matched or filterless request carries no such notice, a total-zero
// selection keeps ErrNoFilterMatch (with or without retained companions)
// instead of becoming a notice, shaped filters are each accounted through
// the established matching semantics, dataset-membership selection counts
// as a match for its own label, and an excluded-only path counts as
// unmatched.
func TestPlanRepoFilterUnmatchedNotice(t *testing.T) {
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
		name       string
		files      []hfNode
		job        Job
		wantPaths  []string // optional: pin the plan when preservation matters
		wantNotice string   // exact filters_unmatched notice; "" = none
		wantAllOf  []string // optional: exact Plan.Notices for ordinary model plans
		wantErr    error
	}{
		{
			name:  "one of two filters matched names only the unmatched one",
			files: ggufWithCompanion,
			job:   Job{Repo: "owner/repo", Filters: []string{"q4", "q5"}},
			// GGUF mode is matched-only: the config companion is dropped.
			wantPaths:  []string{"model-Q4_K_M.gguf"},
			wantNotice: `filter "q5" matched nothing`,
			wantAllOf:  []string{`filter "q5" matched nothing`},
		},
		{
			name:       "fully matched filter set carries no notice",
			files:      ggufWithCompanion,
			job:        Job{Repo: "owner/repo", Filters: []string{"q4"}},
			wantNotice: "",
			wantAllOf:  []string{},
		},
		{
			name:       "request without filters is notice-free",
			files:      ggufWithCompanion,
			job:        Job{Repo: "owner/repo"},
			wantPaths:  []string{"model-Q4_K_M.gguf", "config.json"},
			wantNotice: "",
			wantAllOf:  []string{},
		},
		{
			// No companion can retain the request: the plan is empty and the
			// total-zero selection keeps the hard error, never a notice.
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
			// and root-basename selectors did not. The bounded join names
			// both (≤ warningSegmentCap) in order.
			wantPaths:  []string{"weights/model.safetensors", "model.bin"},
			wantNotice: `filters "subdir/", "./absent.bin" matched nothing`,
			wantAllOf:  []string{`filters "subdir/", "./absent.bin" matched nothing`},
		},
		{
			name:  "shard-token substring filter counts as matched",
			files: shardTree,
			job:   Job{Repo: "owner/repo", Filters: []string{"00001-of", "q9"}},
			// Only the first shard path contains the token; the unmatched
			// LFS sibling drops, and only "q9" is named.
			wantPaths:  []string{"model-00001-of-00002.safetensors"},
			wantNotice: `filter "q9" matched nothing`,
			wantAllOf:  []string{`filter "q9" matched nothing`},
		},
		{
			name:  "excluded-only path counts as unmatched",
			files: quantTree,
			job: Job{Repo: "owner/repo",
				Filters:  []string{"q4", "fp16"},
				Excludes: []string{"fp16"}},
			// The fp16 file is excluded before selection, so its pattern
			// credited nothing while "q4" kept the request alive.
			wantPaths:  []string{"model-q4.gguf"},
			wantNotice: `filter "fp16" matched nothing`,
			wantAllOf:  []string{`filter "fp16" matched nothing`},
		},
		{
			name:  "dataset membership selection counts as matched for its label",
			files: datasetTree,
			job: Job{Repo: "owner/data", IsDataset: true,
				Filters: []string{"train", "q9"}, ExactMatch: true},
			// "train" selects its group through membership evidence — it is
			// matched — while the bogus direct pattern is named. The
			// split-mention notice family may report dropped mentions of
			// "train" alongside; it never reports "q9", so no double-report.
			wantPaths:  []string{"train/part-00000.csv"},
			wantNotice: `filter "q9" matched nothing`,
		},
		{
			name:  "dataset group filter alone carries no unmatched notice",
			files: datasetTree,
			job: Job{Repo: "owner/data", IsDataset: true,
				Filters: []string{"train"}, ExactMatch: true},
			wantPaths:  []string{"train/part-00000.csv"},
			wantNotice: "",
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
			found := filterNotices(plan)
			if tc.wantNotice == "" {
				if len(found) != 0 {
					t.Fatalf("notices = %q, want no filters_unmatched notice", plan.Notices)
				}
				return
			}
			if len(found) != 1 {
				t.Fatalf("filter notices = %q, want exactly one %q (all notices: %q)",
					found, tc.wantNotice, plan.Notices)
			}
			if found[0] != tc.wantNotice {
				t.Errorf("notice = %q, want %q", found[0], tc.wantNotice)
			}
			if tc.wantAllOf != nil && !reflect.DeepEqual(plan.Notices, tc.wantAllOf) {
				t.Errorf("notices = %q, want exactly %q", plan.Notices, tc.wantAllOf)
			}
		})
	}
}

// TestPlanRepoFilterNoticeOrdersAfterShardNotices pins the array order when
// one request produces both diagnostic families on the shared notices
// surface: the established shard-range notices keep their leading position
// and the filters_unmatched notice joins the same array after them.
func TestPlanRepoFilterNoticeOrdersAfterShardNotices(t *testing.T) {
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
	wantNotices := []string{
		"shard range segments selected no shard (indices present: 1-4): 9-10",
		`filter "q9" matched nothing`,
	}
	if !reflect.DeepEqual(plan.Notices, wantNotices) {
		t.Fatalf("notices = %q, want %q", plan.Notices, wantNotices)
	}
}

// TestPlanRepoUnmatchedNoticeSkipsSplitCoveredPattern pins the
// no-double-reporting boundary: when a split-labelled pattern's miss is
// already reported by main's split-mention notice family (dropped files
// mention the standard split, nothing selected the pattern), the
// filters_unmatched producer skips exactly that pattern — the notices
// array carries the split notice alone, never a second sentence about the
// same pattern.
func TestPlanRepoUnmatchedNoticeSkipsSplitCoveredPattern(t *testing.T) {
	files := []hfNode{
		{Type: "file", Path: "test/train-part-00001.csv", Size: 11},
		{Type: "file", Path: "other.csv", Size: 7},
	}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()
	plan, err := PlanRepo(context.Background(),
		Job{Repo: "owner/data", IsDataset: true, Filters: []string{"train", "other"}},
		Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("PlanRepo: %v", err)
	}
	// The split-mention family reports the dropped train mention...
	var split []string
	for _, n := range plan.Notices {
		if strings.HasPrefix(n, "excluded ") {
			split = append(split, n)
		}
	}
	if len(split) != 1 {
		t.Fatalf("split-mention notices = %q, want exactly one (all: %q)", split, plan.Notices)
	}
	// ...and the unmatched-filter family must stay silent about "train":
	// its pattern-level miss is that same fact, already reported.
	if found := filterNotices(plan); len(found) != 0 {
		t.Errorf("filter notices = %q, want none (the split notice already covers %q; all: %q)",
			found, "train", plan.Notices)
	}
	if len(plan.Notices) != 1 {
		t.Errorf("notices = %q, want only the split-mention notice", plan.Notices)
	}
}

// TestPlanRepoUnmatchedNoticeReportsUncoveredDatasetLabel is the control:
// the same split-labelled pattern when NO split-mention notice exists (no
// file mentions the standard split) is reported by the unmatched-filter
// family — the boundary suppresses only patterns the split family
// actually covers, extending coverage to everything else.
func TestPlanRepoUnmatchedNoticeReportsUncoveredDatasetLabel(t *testing.T) {
	files := []hfNode{
		{Type: "file", Path: "test/data-part-00001.csv", Size: 11},
		{Type: "file", Path: "other.csv", Size: 7},
	}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()
	plan, err := PlanRepo(context.Background(),
		Job{Repo: "owner/data", IsDataset: true, Filters: []string{"train", "other"}},
		Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("PlanRepo: %v", err)
	}
	found := filterNotices(plan)
	if len(found) != 1 || found[0] != `filter "train" matched nothing` {
		t.Fatalf("filter notices = %q, want exactly [%q] (all: %q)",
			found, `filter "train" matched nothing`, plan.Notices)
	}
}
