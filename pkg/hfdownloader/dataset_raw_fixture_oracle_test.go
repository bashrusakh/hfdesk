// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/bashrusakh/hfdesk/internal/datasetselection"
)

// This is intentionally a raw-input oracle: expected roles, memberships and
// emitted paths are authored here, not obtained from the decision compiler or
// its world/query helpers. Compiler output is checked against the role table
// before PlanRepo is treated as the subject under test.
func TestDatasetRawFixtureLiteralMembershipOracle(t *testing.T) {
	type roleLabel struct {
		role  datasetselection.EvidenceRole
		label string
	}
	tests := []struct {
		name        string
		files       []hfNode
		filter      string
		exact       bool
		excludes    []string
		roles       [][]roleLabel
		direct      []bool // hand-authored literal/path matches, independent of membership
		want        []string
		wantNoMatch bool
		worldSets   [][]string // independent final sets for role alternatives when relevant
		wantRefuse  bool
	}{
		{
			name: "literal train substring is additive to membership",
			files: []hfNode{
				{Type: "file", Path: "train.csv", Size: 10},
				{Type: "file", Path: "pretrain.csv", LFS: &hfLfsInfo{Size: 11}},
				{Type: "file", Path: "validation.csv", Size: 12},
			},
			filter: "train", exact: false,
			roles: [][]roleLabel{
				{{datasetselection.EvidenceRoleData, "train"}},
				{{datasetselection.EvidenceRoleData, "default"}},
				{{datasetselection.EvidenceRoleData, "validation"}},
			},
			direct: []bool{true, true, false},
			want:   []string{"train.csv", "pretrain.csv"},
		},
		{
			name: "exact literal does not collapse pretrain membership",
			files: []hfNode{
				{Type: "file", Path: "train.csv", Size: 10},
				{Type: "file", Path: "pretrain.csv", LFS: &hfLfsInfo{Size: 11}},
				{Type: "file", Path: "validation.csv", Size: 12},
			},
			filter: "train", exact: true,
			roles: [][]roleLabel{
				{{datasetselection.EvidenceRoleData, "train"}},
				{{datasetselection.EvidenceRoleData, "default"}},
				{{datasetselection.EvidenceRoleData, "validation"}},
			},
			direct: []bool{true, false, false},
			want:   []string{"train.csv"},
		},
		{
			name: "explicit exclude overrides direct literal match",
			files: []hfNode{
				{Type: "file", Path: "train.csv", Size: 10},
				{Type: "file", Path: "pretrain.csv", LFS: &hfLfsInfo{Size: 11}},
			},
			filter: "train", exact: false,
			excludes: []string{"train.csv"},
			roles: [][]roleLabel{
				{{datasetselection.EvidenceRoleData, "train"}},
				{{datasetselection.EvidenceRoleData, "default"}},
			},
			direct:      []bool{true, true},
			wantNoMatch: true,
		},
		{
			name: "duplicate direct and membership path emits once",
			files: []hfNode{
				{Type: "file", Path: "train.csv", Size: 10},
				{Type: "file", Path: "train.csv", Size: 10},
			},
			filter: "train", exact: false,
			roles: [][]roleLabel{
				{{datasetselection.EvidenceRoleData, "train"}},
				{{datasetselection.EvidenceRoleData, "train"}},
			},
			direct: []bool{true, true},
			want:   []string{"train.csv"},
		},
		{
			name: "custom membership and underscore group remain distinct",
			files: []hfNode{
				{Type: "file", Path: "data/custom_group/part.arrow", LFS: &hfLfsInfo{Size: 20}},
				{Type: "file", Path: "data/customgroup/part.arrow", LFS: &hfLfsInfo{Size: 21}},
				{Type: "file", Path: "README.md", Size: 2},
			},
			filter: "custom_group", exact: true,
			roles: [][]roleLabel{
				{{datasetselection.EvidenceRoleData, "custom_group"}},
				{{datasetselection.EvidenceRoleData, "customgroup"}},
				{{datasetselection.EvidenceRoleCompanion, ""}},
			},
			direct: []bool{true, false, false},
			want:   []string{"data/custom_group/part.arrow", "README.md"},
		},
		{
			name: "unresolved metadata ownership remains retained",
			files: []hfNode{
				{Type: "file", Path: "weights/model.safetensors", LFS: &hfLfsInfo{Size: 100}},
				{Type: "file", Path: "tokenizer/README.md", Size: 7},
			},
			filter: "weights/", exact: true,
			roles: [][]roleLabel{
				{{datasetselection.EvidenceRoleCompanion, ""}},
				{{datasetselection.EvidenceRoleCompanion, ""}},
			},
			direct: []bool{true, false},
			want:   []string{"weights/model.safetensors", "tokenizer/README.md"},
		},
		{
			name:   "default membership selects without a literal path match",
			files:  []hfNode{{Type: "file", Path: "records.parquet", LFS: &hfLfsInfo{Size: 30}}},
			filter: "default", exact: true,
			roles:  [][]roleLabel{{{datasetselection.EvidenceRoleData, "default"}}},
			direct: []bool{false},
			want:   []string{"records.parquet"},
		},
		{
			name:   "role alternatives with different final sets refuse",
			files:  []hfNode{{Type: "file", Path: "nested/data.json", Size: 5}},
			filter: "default", exact: true,
			roles: [][]roleLabel{{
				{datasetselection.EvidenceRoleData, "default"},
				{datasetselection.EvidenceRoleCompanion, ""},
			}},
			direct:     []bool{false},
			worldSets:  [][]string{{"nested/data.json"}, {}},
			wantRefuse: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if len(tt.files) != len(tt.roles) || len(tt.files) != len(tt.direct) {
				t.Fatalf("fixture paths=%d hand-authored role rows=%d direct rows=%d", len(tt.files), len(tt.roles), len(tt.direct))
			}
			membershipFiles := make([]datasetselection.File, len(tt.files))
			for i := range tt.files {
				membershipFiles[i] = datasetselection.File{Path: tt.files[i].Path}
			}
			c := compileDatasetDecisionContext(tt.files, datasetselection.Recognize(membershipFiles), []string{tt.filter}, tt.excludes, tt.exact, true, false)
			for i, want := range tt.roles {
				got := c.Inputs[i].Alternatives
				if len(got) != len(want) {
					t.Fatalf("raw path %q compiled alternatives=%+v; hand-authored=%+v", tt.files[i].Path, got, want)
				}
				for j := range want {
					if got[j].Role != want[j].role || got[j].Label != want[j].label {
						t.Fatalf("raw path %q alternative %d compiled=%+v; hand-authored=%+v", tt.files[i].Path, j, got[j], want[j])
					}
				}
				if gotDirect := len(c.Inputs[i].DirectFilterIndexes) != 0; gotDirect != tt.direct[i] {
					t.Fatalf("raw path %q compiled literal-direct=%t, hand-authored=%t", tt.files[i].Path, gotDirect, tt.direct[i])
				}
			}
			if len(tt.worldSets) != 0 {
				if len(tt.worldSets) != len(tt.roles[0]) {
					t.Fatalf("hand-authored role worlds=%d final-set worlds=%d", len(tt.roles[0]), len(tt.worldSets))
				}
				if reflect.DeepEqual(tt.worldSets[0], tt.worldSets[1]) {
					t.Fatalf("fixture no longer represents differing hand-authored final sets: %v", tt.worldSets)
				}
				// Compile each actual admissible role choice into a concrete input
				// world. The expected emitted sets above remain the oracle; the
				// production evaluator is only the subject compared against them.
				for worldIndex, alternative := range tt.roles[0] {
					world := datasetDecisionWorld{
						DataLabels: map[int]string{}, Companion: map[int]bool{}, SharedToken: map[uint]bool{},
					}
					switch alternative.role {
					case datasetselection.EvidenceRoleData:
						world.DataLabels[0] = alternative.label
					case datasetselection.EvidenceRoleCompanion:
						world.Companion[0] = true
					default:
						t.Fatalf("hand-authored world %d has unsupported role %q", worldIndex, alternative.role)
					}
					evaluation, evalErr := evaluateDatasetDecisionWorld(c, world)
					if evalErr != nil {
						t.Fatalf("evaluate hand-authored world %d (%+v): %v", worldIndex, alternative, evalErr)
					}
					var gotSet []string
					for i, record := range evaluation.Records {
						if record.Selected {
							gotSet = append(gotSet, tt.files[i].Path)
						}
					}
					if !slices.Equal(gotSet, tt.worldSets[worldIndex]) {
						t.Fatalf("world %d role=%+v evaluator final set=%v; hand-authored=%v", worldIndex, alternative, gotSet, tt.worldSets[worldIndex])
					}
				}
				// The query stage is per-input feasibility, not the final plan.
				// Check both uncached and cache-populated paths against the same
				// hand-authored world sets.
				for _, cached := range []bool{false, true} {
					queryContext := c
					if !cached {
						queryContext.uniqueEvaluation = nil
					}
					can, cannot := datasetDecisionRefusal(queryContext, 0)
					wantCan, wantCannot := false, false
					for _, finalSet := range tt.worldSets {
						selected := containsString(finalSet, tt.files[0].Path)
						wantCan = wantCan || selected
						wantCannot = wantCannot || !selected
					}
					wantCannot = wantCan && wantCannot
					if can.Err != nil || cannot.Err != nil || can.Possible != wantCan || cannot.Possible != wantCannot {
						t.Fatalf("cached=%t query can/cannot=%t/%t errors=%v/%v; hand-world oracle=%t/%t", cached, can.Possible, cannot.Possible, can.Err, cannot.Err, wantCan, wantCannot)
					}
					if !can.Record.Selected || cannot.Record.Selected {
						t.Fatalf("cached=%t query witnesses selected=%t/%t; expected emitting/non-emitting witnesses", cached, can.Record.Selected, cannot.Record.Selected)
					}
				}
			}

			srv := mockHFServerForPlan(t, tt.files)
			defer srv.Close()
			plan, err := PlanRepo(context.Background(), Job{
				Repo: "owner/fixture", Revision: "main", IsDataset: true,
				Filters: []string{tt.filter}, Excludes: tt.excludes, ExactMatch: tt.exact,
			}, Settings{Endpoint: srv.URL})
			if tt.wantRefuse {
				wantErr := fmt.Sprintf("dataset filter selection has ambiguous emitted set for %q", tt.files[0].Path)
				if err == nil || !strings.Contains(err.Error(), wantErr) {
					t.Fatalf("PlanRepo error=%v; want path-specific ambiguity %q (plan=%v)", err, wantErr, planPaths(plan))
				}
				return
			}
			if tt.wantNoMatch {
				if !errors.Is(err, ErrNoFilterMatch) || plan != nil {
					t.Fatalf("PlanRepo plan=%v error=%v; want nil plan and ErrNoFilterMatch", planPaths(plan), err)
				}
				return
			}
			if err != nil {
				t.Fatalf("PlanRepo: %v", err)
			}
			if got := planPaths(plan); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("PlanRepo paths=%v; independently authored final set=%v", got, tt.want)
			}
		})
	}
}

func TestDatasetVocabTxtDoesNotBecomeMetadataFeasibility(t *testing.T) {
	files := []hfNode{
		{Type: "file", Path: "train.csv", Size: 10},
		{Type: "file", Path: "elsewhere/vocab.txt", Size: 11},
	}
	membershipFiles := []datasetselection.File{{Path: files[0].Path}, {Path: files[1].Path}}
	membership := datasetselection.Recognize(membershipFiles)
	c := compileDatasetDecisionContext(files, membership, []string{"train.csv"}, nil, true, true, false)

	// The dataset recognizer still considers TXT data-or-companion, but the
	// model-only metadata spelling must not make its nested companion eligible.
	if got := c.Inputs[1].Alternatives; len(got) != 2 || got[0].Role != datasetselection.EvidenceRoleData || got[1].Role != datasetselection.EvidenceRoleCompanion {
		t.Fatalf("vocab.txt dataset alternatives = %+v; want data/companion alternatives", got)
	}
	worlds := []datasetDecisionWorld{
		{DataLabels: map[int]string{0: "train", 1: "default"}, Companion: map[int]bool{}, SharedToken: map[uint]bool{}},
		{DataLabels: map[int]string{0: "train"}, Companion: map[int]bool{1: true}, SharedToken: map[uint]bool{}},
	}
	for i, world := range worlds {
		evaluation, err := evaluateDatasetDecisionWorld(c, world)
		if err != nil {
			t.Fatalf("evaluate role world %d: %v", i, err)
		}
		if evaluation.Records[1].Selected {
			t.Fatalf("role world %d selected nested vocab.txt without direct filter or dataset directory seed", i)
		}
	}
	for _, cached := range []bool{false, true} {
		queryContext := c
		if !cached {
			queryContext.uniqueEvaluation = nil
		}
		can, cannot := datasetDecisionRefusal(queryContext, 1)
		if can.Err != nil || cannot.Err != nil || can.Possible || cannot.Possible {
			t.Fatalf("cached=%t vocab.txt feasibility can/cannot=%t/%t errors=%v/%v; want neither", cached, can.Possible, cannot.Possible, can.Err, cannot.Err)
		}
	}
}
