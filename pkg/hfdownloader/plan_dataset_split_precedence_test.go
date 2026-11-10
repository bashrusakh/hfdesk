// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// TestPlanRepoDatasetSplitDirectoryOutranksBasenameToken pins the documented
// split-membership precedence end to end through PlanRepo: a directory
// component that names a standard split outranks a split name in the file's
// basename, so a data file named for one split but placed inside another
// split's directory belongs to the directory's group.
func TestPlanRepoDatasetSplitDirectoryOutranksBasenameToken(t *testing.T) {
	files := []hfNode{
		{Type: "file", Path: "train/part-00000.csv", Size: 10},
		{Type: "file", Path: "test/train-part-00001.csv", Size: 11},
		{Type: "file", Path: "validation/train-part-00002.csv", Size: 12},
	}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()
	planFor := func(filters []string) (*Plan, error) {
		return PlanRepo(context.Background(), Job{Repo: "owner/data", IsDataset: true, Filters: filters, ExactMatch: true}, Settings{Endpoint: srv.URL})
	}

	// With the extension filter every file is selected, but only
	// train/part-00000.csv is a train member: the split directory outranks
	// the "train" basename token of the other two files, so their
	// attribution is the extension term and not "train".
	plan, err := planFor([]string{"train", ".csv"})
	if err != nil {
		t.Fatalf("PlanRepo(train, .csv): %v", err)
	}
	assertPaths(t, planPaths(plan), []string{
		"train/part-00000.csv",
		"test/train-part-00001.csv",
		"validation/train-part-00002.csv",
	})
	wantSubdirs := map[string]string{
		"train/part-00000.csv":            "train",
		"test/train-part-00001.csv":       ".csv",
		"validation/train-part-00002.csv": ".csv",
	}
	for _, item := range plan.Items {
		if want := wantSubdirs[item.RelativePath]; item.Subdir != want {
			t.Errorf("%s Subdir = %q, want %q", item.RelativePath, item.Subdir, want)
		}
	}

	// With the train filter alone the validation file is not a train member
	// (its directory names the split), so only the group member is selected.
	plan, err = planFor([]string{"train"})
	if err != nil {
		t.Fatalf("PlanRepo(train): %v", err)
	}
	if plan == nil {
		t.Fatal("PlanRepo(train) returned no plan")
	}
	assertPaths(t, planPaths(plan), []string{"train/part-00000.csv"})
}

// TestPlanRepoDatasetSplitDirectoryCollisionResolvesInsteadOfRefusing pins
// the user-visible half of the precedence change: before the split directory
// outranked the basename token, a "train" group filter over this tree refused
// the whole plan with ErrAmbiguousSelection because
// test/train-part-00001.csv could have been a train member through its
// basename token. The directory now decides membership, that file is
// unambiguously not a train member, and the group filter plans the group's
// own members instead of refusing.
func TestPlanRepoDatasetSplitDirectoryCollisionResolvesInsteadOfRefusing(t *testing.T) {
	files := []hfNode{
		{Type: "file", Path: "train/part-00000.csv", Size: 10},
		{Type: "file", Path: "test/train-part-00001.csv", Size: 11},
	}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()
	plan, err := PlanRepo(context.Background(), Job{Repo: "owner/data", IsDataset: true, Filters: []string{"train"}, ExactMatch: true}, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("directory-vs-token shape must resolve instead of refusing: %v", err)
	}
	if plan == nil {
		t.Fatal("PlanRepo(train) returned no plan")
	}
	assertPaths(t, planPaths(plan), []string{"train/part-00000.csv"})
	if got := plan.Items[0].Subdir; got != "train" {
		t.Errorf("train/part-00000.csv Subdir = %q, want %q", got, "train")
	}
}

// TestPlanRepoDatasetSplitFilterNeverSelectsAMereMention pins the
// label-absent inventory corner: the tree's only split evidence is a file
// living in another split's directory. The split name its basename merely
// mentions stays a recognized group label, so the filter naming it remains a
// membership selector and never a literal path match: the train-only request
// fails closed with ErrNoFilterMatch instead of downloading the mentioning
// file, and with an extension filter the file is selected but attributed to
// the extension, not to train.
func TestPlanRepoDatasetSplitFilterNeverSelectsAMereMention(t *testing.T) {
	files := []hfNode{
		{Type: "file", Path: "test/train-part-00001.csv", Size: 11},
	}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()
	planFor := func(filters []string) (*Plan, error) {
		return PlanRepo(context.Background(), Job{Repo: "owner/data", IsDataset: true, Filters: filters, ExactMatch: true}, Settings{Endpoint: srv.URL})
	}

	plan, err := planFor([]string{"train"})
	if err == nil || !errors.Is(err, ErrNoFilterMatch) {
		t.Fatalf("PlanRepo(train) = (%v, %v), want nil plan and ErrNoFilterMatch", plan, err)
	}
	if plan != nil {
		t.Fatalf("PlanRepo(train) returned a plan (%v), want nil", planPaths(plan))
	}
	wantErr := `no files matched requested filters: filters=["train"] exact=true`
	if got := err.Error(); got != wantErr {
		t.Errorf("PlanRepo(train) error = %q, want %q", got, wantErr)
	}

	plan, err = planFor([]string{"train", ".csv"})
	if err != nil {
		t.Fatalf("PlanRepo(train, .csv): %v", err)
	}
	if plan == nil {
		t.Fatal("PlanRepo(train, .csv) returned no plan")
	}
	assertPaths(t, planPaths(plan), []string{"test/train-part-00001.csv"})
	if got := plan.Items[0].Subdir; got != ".csv" {
		t.Errorf("test/train-part-00001.csv Subdir = %q, want %q (the mentioned split must not attribute it)", got, ".csv")
	}
}

// TestPlanRepoDatasetSplitMentionNoticeNamesTheExcludedFile pins the
// visible half of the precedence rule: the same one-item train plan as
// TestPlanRepoDatasetSplitDirectoryCollisionResolvesInsteadOfRefusing, now
// carrying a notice that names the split label, the count, the directory and
// the exact excluded path — while the plan's items stay unchanged.
func TestPlanRepoDatasetSplitMentionNoticeNamesTheExcludedFile(t *testing.T) {
	files := []hfNode{
		{Type: "file", Path: "train/part-00000.csv", Size: 10},
		{Type: "file", Path: "test/train-part-00001.csv", Size: 11},
	}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()
	plan, err := PlanRepo(context.Background(), Job{Repo: "owner/data", IsDataset: true, Filters: []string{"train"}}, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("PlanRepo(train): %v", err)
	}
	if plan == nil {
		t.Fatal("PlanRepo(train) returned no plan")
	}
	// The plan itself is exactly the plain group selection: one item, the
	// group's own member.
	assertPaths(t, planPaths(plan), []string{"train/part-00000.csv"})
	if got := plan.Items[0].Subdir; got != "train" {
		t.Errorf("train/part-00000.csv Subdir = %q, want %q", got, "train")
	}
	wantNotice := "excluded 1 file that mentions train but lives in test/: test/train-part-00001.csv"
	if len(plan.Notices) != 1 {
		t.Fatalf("plan.Notices = %q, want exactly one notice %q", plan.Notices, wantNotice)
	}
	if plan.Notices[0] != wantNotice {
		t.Errorf("plan.Notices[0] = %q, want %q", plan.Notices[0], wantNotice)
	}
}

// TestPlanRepoDatasetSplitMentionNoticeAbsentWhenNothingDropped pins the
// negative half: a plain group member selection carries no notice, so the
// warning cannot become noise on ordinary plans.
func TestPlanRepoDatasetSplitMentionNoticeAbsentWhenNothingDropped(t *testing.T) {
	files := []hfNode{
		{Type: "file", Path: "train/part-00000.csv", Size: 10},
		{Type: "file", Path: "train/part-00002.csv", Size: 12},
	}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()
	plan, err := PlanRepo(context.Background(), Job{Repo: "owner/data", IsDataset: true, Filters: []string{"train"}}, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("PlanRepo(train): %v", err)
	}
	if plan == nil {
		t.Fatal("PlanRepo(train) returned no plan")
	}
	assertPaths(t, planPaths(plan), []string{"train/part-00000.csv", "train/part-00002.csv"})
	if len(plan.Notices) != 0 {
		t.Errorf("plan.Notices = %q, want none for a plain group member selection", plan.Notices)
	}
}

// TestPlanRepoDatasetSplitMentionNoticeIsBoundedAndDeterministic pins the
// diagnostic bound: seven dropped mentions across two directories produce
// one sentence with sorted directories and sorted paths, naming only the
// first splitMentionNoticePathCap paths and stating how many more there
// are — never one path per excluded file without a bound.
func TestPlanRepoDatasetSplitMentionNoticeIsBoundedAndDeterministic(t *testing.T) {
	files := []hfNode{
		{Type: "file", Path: "train/part-00000.csv", Size: 10},
		{Type: "file", Path: "test/train-part-00001.csv", Size: 11},
		{Type: "file", Path: "test/train-part-00002.csv", Size: 11},
		{Type: "file", Path: "test/train-part-00003.csv", Size: 11},
		{Type: "file", Path: "test/train-part-00004.csv", Size: 11},
		{Type: "file", Path: "eval/train-0.csv", Size: 11},
		{Type: "file", Path: "eval/train-1.csv", Size: 11},
		{Type: "file", Path: "eval/train-2.csv", Size: 11},
	}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()
	plan, err := PlanRepo(context.Background(), Job{Repo: "owner/data", IsDataset: true, Filters: []string{"train"}}, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("PlanRepo(train): %v", err)
	}
	if plan == nil {
		t.Fatal("PlanRepo(train) returned no plan")
	}
	assertPaths(t, planPaths(plan), []string{"train/part-00000.csv"})
	wantNotice := "excluded 7 files that mention train but live in eval/ and test/: " +
		"eval/train-0.csv, eval/train-1.csv, eval/train-2.csv, " +
		"test/train-part-00001.csv, test/train-part-00002.csv, and 2 more"
	if len(plan.Notices) != 1 {
		t.Fatalf("plan.Notices = %q, want exactly one notice %q", plan.Notices, wantNotice)
	}
	if plan.Notices[0] != wantNotice {
		t.Errorf("plan.Notices[0] = %q, want %q", plan.Notices[0], wantNotice)
	}
	// Re-running the same plan must produce byte-identical notices.
	again, err := PlanRepo(context.Background(), Job{Repo: "owner/data", IsDataset: true, Filters: []string{"train"}}, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("PlanRepo(train) second run: %v", err)
	}
	if len(again.Notices) != 1 || again.Notices[0] != wantNotice {
		t.Errorf("second run plan.Notices = %q, want %q", again.Notices, wantNotice)
	}
}

// TestPlanRepoDatasetSplitMentionNoticeCountsLogicalFilesOnce pins the
// planner's dedup model in the notice: an exact duplicate inventory entry
// and a case-fold variant are one logical file, so the notice counts and
// names it once, under the first-occurrence spelling the planner keeps.
func TestPlanRepoDatasetSplitMentionNoticeCountsLogicalFilesOnce(t *testing.T) {
	files := []hfNode{
		{Type: "file", Path: "train/part-00000.csv", Size: 10},
		{Type: "file", Path: "test/train-part.csv", Size: 11},
		{Type: "file", Path: "test/train-part.csv", Size: 11}, // exact duplicate entry
		{Type: "file", Path: "TEST/train-part.csv", Size: 11}, // case-fold variant
	}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()
	plan, err := PlanRepo(context.Background(), Job{Repo: "owner/data", IsDataset: true, Filters: []string{"train"}}, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("PlanRepo(train): %v", err)
	}
	if plan == nil {
		t.Fatal("PlanRepo(train) returned no plan")
	}
	assertPaths(t, planPaths(plan), []string{"train/part-00000.csv"})
	wantNotice := "excluded 1 file that mentions train but lives in test/: test/train-part.csv"
	if len(plan.Notices) != 1 {
		t.Fatalf("plan.Notices = %q, want exactly one notice %q", plan.Notices, wantNotice)
	}
	if plan.Notices[0] != wantNotice {
		t.Errorf("plan.Notices[0] = %q, want %q", plan.Notices[0], wantNotice)
	}
}

// TestPlanRepoDatasetSplitMentionNoticeBoundsDirectories pins the other
// half of the bounded claim: excluded files spread over more directories
// than the cap produce a notice that names only the first
// splitMentionNoticePathCap directories and states how many more there
// are, beside the already-capped path list.
func TestPlanRepoDatasetSplitMentionNoticeBoundsDirectories(t *testing.T) {
	files := []hfNode{{Type: "file", Path: "train/part-00000.csv", Size: 10}}
	for i := 0; i < 7; i++ {
		files = append(files, hfNode{Type: "file", Path: fmt.Sprintf("eval/d%d/train-%d.csv", i+1, i), Size: 11})
	}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()
	plan, err := PlanRepo(context.Background(), Job{Repo: "owner/data", IsDataset: true, Filters: []string{"train"}}, Settings{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("PlanRepo(train): %v", err)
	}
	if plan == nil {
		t.Fatal("PlanRepo(train) returned no plan")
	}
	assertPaths(t, planPaths(plan), []string{"train/part-00000.csv"})
	wantNotice := "excluded 7 files that mention train but live in " +
		"eval/d1/, eval/d2/, eval/d3/, eval/d4/, eval/d5/ (and 2 more directories): " +
		"eval/d1/train-0.csv, eval/d2/train-1.csv, eval/d3/train-2.csv, " +
		"eval/d4/train-3.csv, eval/d5/train-4.csv, and 2 more"
	if len(plan.Notices) != 1 {
		t.Fatalf("plan.Notices = %q, want exactly one notice %q", plan.Notices, wantNotice)
	}
	if plan.Notices[0] != wantNotice {
		t.Errorf("plan.Notices[0] = %q, want %q", plan.Notices[0], wantNotice)
	}
}
