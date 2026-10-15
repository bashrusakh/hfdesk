// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"errors"
	"testing"
)

// bonsaiPlanTree is the reported defect shape: a PQ2_0 quant plus only
// metadata (README, LICENSE, an asset). A filtered request must succeed on a
// direct filter match, never on the metadata alone.
func bonsaiPlanTree() []hfNode {
	return []hfNode{
		{Type: "file", Path: "model-PQ2_0.gguf", Size: 100},
		{Type: "file", Path: "README.md", Size: 20},
		{Type: "file", Path: "LICENSE", Size: 10},
		{Type: "file", Path: "assets/logo.svg", Size: 5},
	}
}

// TestPlanRepo_NoDirectFilterMatchFails is the regression test for the
// selection-semantics defect: a filtered plan used to succeed (here with only
// metadata items) even though "q2_0" matches nothing — "q2_0" is not a
// delimiter-bounded segment of "model-PQ2_0.gguf" in exact mode. Companion
// files alone must never make a filtered request succeed.
func TestPlanRepo_NoDirectFilterMatchFails(t *testing.T) {
	srv := mockHFServerForPlan(t, bonsaiPlanTree())
	defer srv.Close()

	plan, err := PlanRepo(context.Background(), Job{
		Repo: "owner/repo", Revision: "main", Filters: []string{"q2_0"}, ExactMatch: true,
	}, Settings{Endpoint: srv.URL})
	if !errors.Is(err, ErrNoFilterMatch) {
		t.Fatalf("PlanRepo error = %v, want ErrNoFilterMatch", err)
	}
	if plan != nil {
		t.Fatalf("metadata-only plan returned despite no filter match: %+v", plan.Items)
	}
}

// TestPlanRepo_ExactDirectFilterMatchSucceeds pins the exact-match selection
// of the same tree: "pq2_0" is a whole segment of the quant name, so the plan
// must succeed and contain the PQ2_0 file.
func TestPlanRepo_ExactDirectFilterMatchSucceeds(t *testing.T) {
	srv := mockHFServerForPlan(t, bonsaiPlanTree())
	defer srv.Close()

	plan, err := PlanRepo(context.Background(), Job{
		Repo: "owner/repo", Revision: "main", Filters: []string{"pq2_0"}, ExactMatch: true,
	}, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("PlanRepo: %v", err)
	}
	found := false
	for _, it := range plan.Items {
		if it.RelativePath == "model-PQ2_0.gguf" {
			found = true
		}
	}
	if !found {
		t.Fatalf("plan does not contain PQ2_0: %+v", plan.Items)
	}
}

// TestPlanRepo_SubstringDirectFilterMatchSucceeds preserves the default
// substring semantics: without ExactMatch, "q2_0" matches PQ2_0 and the plan
// succeeds with the quant selected.
func TestPlanRepo_SubstringDirectFilterMatchSucceeds(t *testing.T) {
	srv := mockHFServerForPlan(t, bonsaiPlanTree())
	defer srv.Close()

	plan, err := PlanRepo(context.Background(), Job{
		Repo: "owner/repo", Revision: "main", Filters: []string{"q2_0"}, ExactMatch: false,
	}, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("PlanRepo: %v", err)
	}
	found := false
	for _, it := range plan.Items {
		if it.RelativePath == "model-PQ2_0.gguf" {
			found = true
		}
	}
	if !found {
		t.Fatalf("plan does not contain PQ2_0: %+v", plan.Items)
	}
}

// TestPlanRepo_NonLFSDirectFilterMatchSucceeds guards against deriving the
// selection proof from LFS-only or GGUF-only inclusion branches: a plain
// non-LFS config.json matched directly must yield a successful filtered plan,
// and its PlanItem.Subdir stays empty (non-LFS, non-GGUF-mode side-effect
// semantics are unchanged).
func TestPlanRepo_NonLFSDirectFilterMatchSucceeds(t *testing.T) {
	files := []hfNode{
		{Type: "file", Path: "config.json", Size: 20},
		{Type: "file", Path: "tokenizer.json", Size: 30},
	}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()

	plan, err := PlanRepo(context.Background(), Job{
		Repo: "owner/repo", Revision: "main", Filters: []string{"config.json"},
	}, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("PlanRepo: %v", err)
	}
	found := false
	for _, it := range plan.Items {
		if it.RelativePath == "config.json" {
			found = true
			if it.Subdir != "" {
				t.Errorf("config.json Subdir = %q, want no model destination prefix", it.Subdir)
			}
		}
	}
	if !found {
		t.Fatalf("plan does not contain config.json: %+v", plan.Items)
	}
}

// TestPlanRepo_ExcludedDirectMatchDoesNotConfirmSelection: excludes enter the
// effective selection. The only file the filter matches is excluded, so no
// direct match remains and the plan must fail rather than succeed on the
// remaining metadata.
func TestPlanRepo_ExcludedDirectMatchDoesNotConfirmSelection(t *testing.T) {
	srv := mockHFServerForPlan(t, bonsaiPlanTree())
	defer srv.Close()

	plan, err := PlanRepo(context.Background(), Job{
		Repo: "owner/repo", Revision: "main",
		Filters: []string{"pq2_0"}, ExactMatch: true, Excludes: []string{"pq2_0"},
	}, Settings{Endpoint: srv.URL})
	if !errors.Is(err, ErrNoFilterMatch) {
		t.Fatalf("PlanRepo error = %v, want ErrNoFilterMatch (excluded match must not count)", err)
	}
	if plan != nil {
		t.Fatalf("plan returned despite zero surviving matches: %+v", plan.Items)
	}
}

// TestPlanRepo_EmptyRepoWithoutFiltersSucceeds: no filters means no selection
// requirement — an empty repo keeps returning a successful empty plan, so
// ErrNoFilterMatch must never be derived from len(plan.Items).
func TestPlanRepo_EmptyRepoWithoutFiltersSucceeds(t *testing.T) {
	srv := mockHFServerForPlan(t, nil)
	defer srv.Close()

	plan, err := PlanRepo(context.Background(), Job{
		Repo: "owner/repo", Revision: "main",
	}, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("PlanRepo: %v", err)
	}
	if plan == nil || len(plan.Items) != 0 {
		t.Fatalf("plan = %+v, want successful empty plan", plan)
	}
}
