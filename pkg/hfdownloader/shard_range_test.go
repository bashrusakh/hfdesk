// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

// shardMixedTree is the mixed matched set: a four-shard 1-based safetensors
// family (LFS, directly matched by the ".safetensors" filter), a three-shard
// 0-based parquet family (non-LFS companions), and two non-sharded
// companions that must survive narrowing untouched.
func shardMixedTree() []hfNode {
	return []hfNode{
		{Type: "file", Path: "model-00001-of-00004.safetensors", Size: 10, LFS: &hfLfsInfo{Size: 10}},
		{Type: "file", Path: "model-00002-of-00004.safetensors", Size: 10, LFS: &hfLfsInfo{Size: 10}},
		{Type: "file", Path: "model-00003-of-00004.safetensors", Size: 10, LFS: &hfLfsInfo{Size: 10}},
		{Type: "file", Path: "model-00004-of-00004.safetensors", Size: 10, LFS: &hfLfsInfo{Size: 10}},
		{Type: "file", Path: "data-00000-of-00003.parquet", Size: 30},
		{Type: "file", Path: "data-00001-of-00003.parquet", Size: 31},
		{Type: "file", Path: "data-00002-of-00003.parquet", Size: 32},
		{Type: "file", Path: "model.safetensors.index.json", Size: 7},
		{Type: "file", Path: "config.json", Size: 5},
	}
}

// shardGapTree is a gap family: the written shard numbers of one family are
// 1, 2 and 5, so written numbers and written-number positions differ. An
// unsharded sibling and a retained config companion complete the fixture.
func shardGapTree() []hfNode {
	return []hfNode{
		{Type: "file", Path: "model-00001-of-00005.safetensors", Size: 10, LFS: &hfLfsInfo{Size: 10}},
		{Type: "file", Path: "model-00002-of-00005.safetensors", Size: 11, LFS: &hfLfsInfo{Size: 11}},
		{Type: "file", Path: "model-00005-of-00005.safetensors", Size: 12, LFS: &hfLfsInfo{Size: 12}},
		{Type: "file", Path: "other.safetensors", Size: 13, LFS: &hfLfsInfo{Size: 13}},
		{Type: "file", Path: "config.json", Size: 5},
	}
}

func shardPlanPaths(plan *Plan) []string {
	paths := make([]string, 0, len(plan.Items))
	for _, item := range plan.Items {
		paths = append(paths, item.RelativePath)
	}
	return paths
}

func TestParseShardRanges(t *testing.T) {
	valid := []struct {
		expr string
		want []shardRange
	}{
		{"1-100,120-185", []shardRange{{1, 100, "1-100"}, {120, 185, "120-185"}}},
		{"5", []shardRange{{5, 5, "5"}}},
		{" 1-2 , 5 ", []shardRange{{1, 2, "1-2"}, {5, 5, "5"}}},
		{"10-20,1-5", []shardRange{{10, 20, "10-20"}, {1, 5, "1-5"}}},         // order is free
		{"999999-1000000", []shardRange{{999999, 1000000, "999999-1000000"}}}, // huge but well-formed
	}
	for _, tc := range valid {
		got, err := parseShardRanges(tc.expr)
		if err != nil {
			t.Errorf("parseShardRanges(%q) error = %v, want success", tc.expr, err)
		} else if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("parseShardRanges(%q) = %+v, want %+v", tc.expr, got, tc.want)
		}
	}
	// Empty expression/segment, non-numeric, zero/negative, reversed span,
	// structural junk and int overflow all fail closed.
	for _, expr := range []string{
		"", "   ", "1,,2", "1-2,", ",1-2", "abc", "1-x", "0", "0-5", "-5",
		"00", "10-5", "5-", "1-2-3", "99999999999999999999",
	} {
		if _, err := parseShardRanges(expr); !errors.Is(err, ErrInvalidShardRanges) {
			t.Errorf("parseShardRanges(%q) error = %v, want ErrInvalidShardRanges", expr, err)
		}
	}
	if err := ValidateShardRanges("1-2", []string{".safetensors"}); err != nil {
		t.Errorf("valid expression with a selection rejected: %v", err)
	}
	if err := ValidateShardRanges("1-2", nil); !errors.Is(err, ErrInvalidShardRanges) {
		t.Errorf("ranges without filters error = %v, want ErrInvalidShardRanges", err)
	}
}

// TestPlanRepo_ShardRangeRejectionMakesNoHubContact pins the fail-closed
// boundary: malformed or selection-less ranges fail in validate(), before
// the HTTP client is built, so no request leaves the process.
func TestPlanRepo_ShardRangeRejectionMakesNoHubContact(t *testing.T) {
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected hub request", http.StatusInternalServerError)
	}))
	defer srv.Close()

	for _, job := range []Job{
		{Repo: "owner/repo", Revision: "main", Filters: []string{".safetensors"}, ExactMatch: true, ShardRanges: "10-5"},
		{Repo: "owner/repo", Revision: "main", Filters: []string{".safetensors"}, ExactMatch: true, ShardRanges: "  "},
		{Repo: "owner/repo", Revision: "main", ShardRanges: "1-2"},
	} {
		plan, err := PlanRepo(context.Background(), job, Settings{Endpoint: srv.URL})
		if !errors.Is(err, ErrInvalidShardRanges) || plan != nil {
			t.Errorf("PlanRepo(%+v) = (%v, %v), want nil plan with ErrInvalidShardRanges", job, plan, err)
		}
	}
	if got := requests.Load(); got != 0 {
		t.Errorf("rejected requests reached the hub %d time(s), want none", got)
	}
}

// TestPlanRepo_ShardRangeSelection is the plan-semantics suite: narrowing a
// mixed set (1-based shards, 0-based companions, non-sharded files), typed
// warnings for ineffective/no-shard requests, the zero-match error, and the
// preserved no-ranges behavior — all against the same fixture family.
func TestPlanRepo_ShardRangeSelection(t *testing.T) {
	type wantWarning struct {
		code     string
		segments []string
	}
	cases := []struct {
		name     string
		files    []hfNode
		job      Job
		want     []string
		wantErr  error
		wantWarn []wantWarning
	}{
		{
			name:  "narrowed mixed set keeps companions",
			files: shardMixedTree(),
			job: Job{Repo: "owner/repo", Revision: "main", Filters: []string{".safetensors"},
				ExactMatch: true, ShardRanges: "1-2"},
			// Only out-of-range MATCHED shards drop. The 0-based parquet
			// family never matched the filter: it is retained, so ranges
			// leave every member untouched.
			want: []string{
				"model-00001-of-00004.safetensors", "model-00002-of-00004.safetensors",
				"data-00000-of-00003.parquet", "data-00001-of-00003.parquet",
				"data-00002-of-00003.parquet",
				"model.safetensors.index.json", "config.json",
			},
		},
		{
			name:  "ineffective segment warns and is named",
			files: shardMixedTree(),
			job: Job{Repo: "owner/repo", Revision: "main", Filters: []string{".safetensors"},
				ExactMatch: true, ShardRanges: "1,9-10"},
			want: []string{
				"model-00001-of-00004.safetensors",
				"data-00000-of-00003.parquet", "data-00001-of-00003.parquet",
				"data-00002-of-00003.parquet",
				"model.safetensors.index.json", "config.json",
			},
			wantWarn: []wantWarning{{PlanWarningShardRangesIneffectiveSegments, []string{"9-10"}}},
		},
		{
			name:  "no shard files warn once",
			files: []hfNode{{Type: "file", Path: "config.json", Size: 5}, {Type: "file", Path: "README.md", Size: 9}},
			job: Job{Repo: "owner/repo", Revision: "main", Filters: []string{"config.json"},
				ShardRanges: "1-5"},
			want:     []string{"config.json", "README.md"},
			wantWarn: []wantWarning{{PlanWarningShardRangesNoShards, []string{"1-5"}}},
		},
		{
			name: "narrowed to zero fails with no filter match",
			files: []hfNode{
				{Type: "file", Path: "model-00001-of-00002.safetensors", Size: 10},
				{Type: "file", Path: "model-00002-of-00002.safetensors", Size: 10},
			},
			job: Job{Repo: "owner/repo", Revision: "main", Filters: []string{".safetensors"},
				ExactMatch: true, ShardRanges: "5-9"},
			wantErr: ErrNoFilterMatch,
		},
		{
			// Companion-present variant of the zero case: the ranges exclude
			// every MATCHED shard while the retained config companion stays
			// planned. Retention must never satisfy the filtered request, so
			// this still fails with ErrNoFilterMatch.
			name: "ranges emptying the selection fail despite a retained companion",
			files: []hfNode{
				{Type: "file", Path: "model-00001-of-00002.safetensors", Size: 10},
				{Type: "file", Path: "model-00002-of-00002.safetensors", Size: 10},
				{Type: "file", Path: "config.json", Size: 5},
			},
			job: Job{Repo: "owner/repo", Revision: "main", Filters: []string{".safetensors"},
				ExactMatch: true, ShardRanges: "5-9"},
			wantErr: ErrNoFilterMatch,
		},
		{
			// F2 discriminator: the parquet shards are shard-shaped but were
			// never matched by the filter, so ranges must leave them exactly
			// as the unfiltered retention rule keeps them.
			name: "never-matched shard-shaped retained files survive narrowing unchanged",
			files: []hfNode{
				{Type: "file", Path: "model-00001-of-00002.safetensors", Size: 10, LFS: &hfLfsInfo{Size: 10}},
				{Type: "file", Path: "model-00002-of-00002.safetensors", Size: 10, LFS: &hfLfsInfo{Size: 10}},
				{Type: "file", Path: "other-00001-of-00003.parquet", Size: 20},
				{Type: "file", Path: "other-00002-of-00003.parquet", Size: 21},
				{Type: "file", Path: "other-00003-of-00003.parquet", Size: 22},
			},
			job: Job{Repo: "owner/repo", Revision: "main", Filters: []string{".safetensors"},
				ExactMatch: true, ShardRanges: "1"},
			want: []string{
				"model-00001-of-00002.safetensors",
				"other-00001-of-00003.parquet", "other-00002-of-00003.parquet",
				"other-00003-of-00003.parquet",
			},
		},
		{
			// The 0-based family is the MATCHED set here: members are ordered
			// by their written numbers (00000, 00001, 00002) and indexed 1..3,
			// so positions 1-2 keep the first two 0-based members. Unmatched
			// LFS shards drop from the base plan and non-shard companions stay.
			name:  "0-based selected family narrows by written-number position",
			files: shardMixedTree(),
			job: Job{Repo: "owner/repo", Revision: "main", Filters: []string{".parquet"},
				ExactMatch: true, ShardRanges: "1-2"},
			want: []string{
				"data-00000-of-00003.parquet", "data-00001-of-00003.parquet",
				"model.safetensors.index.json", "config.json",
			},
		},
		{
			// Gap family: written numbers 1, 2, 5 — ranges address POSITIONS
			// in written-number order, so position 3 is the 00005-named file.
			name:  "gap family ranges address written-number positions",
			files: shardGapTree(),
			job: Job{Repo: "owner/repo", Revision: "main", Filters: []string{".safetensors"},
				ExactMatch: true, ShardRanges: "3"},
			want: []string{
				"model-00005-of-00005.safetensors", "other.safetensors", "config.json",
			},
		},
		{
			// Same gap family: positions only reach 1..3, so "4-5" selects no
			// position and surfaces as the ineffective-segments warning while
			// the selected unsharded sibling keeps the plan alive.
			name:  "gap family out-of-position segment warns",
			files: shardGapTree(),
			job: Job{Repo: "owner/repo", Revision: "main", Filters: []string{".safetensors"},
				ExactMatch: true, ShardRanges: "4-5"},
			want:     []string{"other.safetensors", "config.json"},
			wantWarn: []wantWarning{{PlanWarningShardRangesIneffectiveSegments, []string{"4-5"}}},
		},
		{
			name:  "without ranges the base plan is unchanged",
			files: shardMixedTree(),
			job: Job{Repo: "owner/repo", Revision: "main", Filters: []string{".safetensors"},
				ExactMatch: true},
			want: []string{
				"model-00001-of-00004.safetensors", "model-00002-of-00004.safetensors",
				"model-00003-of-00004.safetensors", "model-00004-of-00004.safetensors",
				"data-00000-of-00003.parquet", "data-00001-of-00003.parquet",
				"data-00002-of-00003.parquet",
				"model.safetensors.index.json", "config.json",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := mockHFServerForPlan(t, tc.files)
			defer srv.Close()
			plan, err := PlanRepo(context.Background(), tc.job, Settings{Endpoint: srv.URL})
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) || plan != nil {
					t.Fatalf("PlanRepo = (%v, %v), want nil plan with %v", plan, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("PlanRepo: %v", err)
			}
			if got := shardPlanPaths(plan); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("plan paths = %v, want %v", got, tc.want)
			}
			if plan.Notices != nil {
				t.Errorf("model-repo plan carries notices: %v", plan.Notices)
			}
			if len(plan.Warnings) != len(tc.wantWarn) {
				t.Fatalf("warnings = %+v, want %+v", plan.Warnings, tc.wantWarn)
			}
			for i, want := range tc.wantWarn {
				w := plan.Warnings[i]
				if w.Code != want.code || !reflect.DeepEqual(w.Segments, want.segments) {
					t.Errorf("warning %d = %+v, want code=%s segments=%v", i, w, want.code, want.segments)
				}
				// Every warning must name its ineffective input in the message.
				if len(want.segments) > 0 && !strings.Contains(w.Message, want.segments[0]) {
					t.Errorf("warning message %q does not name %q", w.Message, want.segments[0])
				}
			}
		})
	}
}

// TestPlanRepo_ShardRangeDatasetMembershipZeroFails applies the same
// selection-survival rule to dataset-membership selections: a "train" group
// filter selects the two membership shards, config.json is retained
// metadata, and ranges that exclude both shards must fail with
// ErrNoFilterMatch — retained metadata alone never satisfies the request.
// On code that derives the zero-narrowed error from len(plan.Items) this
// passes a plan of [config.json] instead of failing.
func TestPlanRepo_ShardRangeDatasetMembershipZeroFails(t *testing.T) {
	files := []hfNode{
		{Type: "file", Path: "train/part-00001-of-00002.parquet", Size: 10},
		{Type: "file", Path: "train/part-00002-of-00002.parquet", Size: 11},
		{Type: "file", Path: "config.json", Size: 5},
	}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()

	plan, err := PlanRepo(context.Background(), Job{
		Repo: "owner/data", Revision: "main", IsDataset: true,
		Filters: []string{"train"}, ExactMatch: true, ShardRanges: "5-9",
	}, Settings{Endpoint: srv.URL})
	if !errors.Is(err, ErrNoFilterMatch) {
		t.Fatalf("PlanRepo = (%v, %v), want nil plan with ErrNoFilterMatch (retained metadata must not survive the emptied selection)", plan, err)
	}
	if plan != nil {
		t.Fatalf("plan returned despite ranges emptying the membership selection: %v", shardPlanPaths(plan))
	}
}

// TestParseShardFile pins the recognizer behind narrowing: the family key
// separates identical basenames by directory, the written number is parsed
// for ordering, and every malformed shape is refused so ranges can never
// drop a file on a name they only half-understand.
func TestParseShardFile(t *testing.T) {
	valid := []struct {
		rel    string
		number int
		family string
	}{
		{"model-00001-of-00004.safetensors", 1, "/model.safetensors"},
		{"data-00000-of-00003.parquet", 0, "/data.parquet"}, // 0-based spelling
		{"weights/model-00002-of-00004.safetensors", 2, "weights/model.safetensors"},
		{"other/model-00002-of-00004.safetensors", 2, "other/model.safetensors"}, // same basename, other family
		{"a/model-Q4_K_M-1-of-2.gguf", 1, "a/model-Q4_K_M.gguf"},                 // unpadded numbers
		{"weights-1-of-2", 1, "/weights"},                                        // no extension
		{"model-00001-of-00002.gguf-split", 1, "/model.gguf-split"},              // compound extension
	}
	for _, tc := range valid {
		number, family, ok := parseShardFile(tc.rel)
		if !ok || number != tc.number || family != tc.family {
			t.Errorf("parseShardFile(%q) = (%d, %q, %v), want (%d, %q, true)",
				tc.rel, number, family, ok, tc.number, tc.family)
		}
	}
	_, familyA, _ := parseShardFile("weights/model-00002-of-00004.safetensors")
	_, familyB, _ := parseShardFile("other/model-00002-of-00004.safetensors")
	if familyA == familyB {
		t.Errorf("shards in different directories share family key %q", familyA)
	}
	for _, rel := range []string{
		"config.json", "model-00001-of-.safetensors", "model-00001-of-00000.safetensors",
		"model-of-00002.safetensors", "model 00001-of-00002.safetensors",
		"model-00001-of-00002x", "model-99999999999999999999-of-00002.safetensors",
	} {
		if number, family, ok := parseShardFile(rel); ok {
			t.Errorf("parseShardFile(%q) = (%d, %q, true), want refused", rel, number, family)
		}
	}
}
