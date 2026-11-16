// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import "testing"

// TestCreateJobDedupFiltersOrderInert pins that filter list order alone does
// not split a job in the order-inert configuration (no appendFilterSubdir):
// the planner's selection is any-match over the set, and for a strict
// request the selection is already normalized at the request boundary.
func TestCreateJobDedupFiltersOrderInert(t *testing.T) {
	mgr, cleanup := newRouteTestManager(t, Config{CacheDir: t.TempDir()})
	defer cleanup()

	first, existing, err := mgr.CreateJob(DownloadRequest{
		Repo: "dedup/filters-order", Filters: []string{"q4_k_m", "mmproj-f16"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if existing {
		t.Fatal("first request should create a new job")
	}
	swapped, existing, err := mgr.CreateJob(DownloadRequest{
		Repo: "dedup/filters-order", Filters: []string{"mmproj-f16", "q4_k_m"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !existing || swapped.ID != first.ID {
		t.Errorf("order-inert filter swap should dedup; existing=%v ids %s/%s", existing, swapped.ID, first.ID)
	}

	strict, existing, err := mgr.CreateJob(DownloadRequest{
		Repo: "dedup/strict-order", StrictFiles: true,
		Filters: []string{"a/one.bin", "b/two.bin"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if existing {
		t.Fatal("first strict request should create a new job")
	}
	strictSwapped, existing, err := mgr.CreateJob(DownloadRequest{
		Repo: "dedup/strict-order", StrictFiles: true,
		Filters: []string{"b/two.bin", "a/one.bin"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !existing || strictSwapped.ID != strict.ID {
		t.Errorf("strict selection swap should dedup; existing=%v ids %s/%s", existing, strictSwapped.ID, strict.ID)
	}
}

// TestCreateJobDedupExcludesOrderSwap pins that excludes are compared as the
// pure matchers they are: swapping their order keeps one job.
func TestCreateJobDedupExcludesOrderSwap(t *testing.T) {
	mgr, cleanup := newRouteTestManager(t, Config{CacheDir: t.TempDir()})
	defer cleanup()

	first, existing, err := mgr.CreateJob(DownloadRequest{
		Repo: "dedup/excludes-order", Filters: []string{"q4_k_m"},
		Excludes: []string{"mmproj", "*.bin"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if existing {
		t.Fatal("first request should create a new job")
	}
	swapped, existing, err := mgr.CreateJob(DownloadRequest{
		Repo: "dedup/excludes-order", Filters: []string{"q4_k_m"},
		Excludes: []string{"*.bin", "mmproj"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !existing || swapped.ID != first.ID {
		t.Errorf("excludes order swap should dedup; existing=%v ids %s/%s", existing, swapped.ID, first.ID)
	}
}

// TestCreateJobShardRangeSegmentOrderIdentity pins shardRanges identity as a
// sorted multiset of per-span spellings: segment order is inert per
// parseShardRanges' contract, while a different span set, a different
// spelling of the same coverage, and a duplicated span all stay distinct.
func TestCreateJobShardRangeSegmentOrderIdentity(t *testing.T) {
	mgr, cleanup := newRouteTestManager(t, Config{CacheDir: t.TempDir()})
	defer cleanup()

	base := DownloadRequest{
		Repo: "dedup/shard-order", Filters: []string{".safetensors"},
		ExactMatch: true, ShardRanges: "1-3,5",
	}
	first, existing, err := mgr.CreateJob(base)
	if err != nil {
		t.Fatal(err)
	}
	if existing {
		t.Fatal("first request should create a new job")
	}

	reordered := base
	reordered.ShardRanges = "5,1-3"
	same, existing, err := mgr.CreateJob(reordered)
	if err != nil {
		t.Fatal(err)
	}
	if !existing || same.ID != first.ID {
		t.Errorf("segment order swap should dedup; existing=%v ids %s/%s", existing, same.ID, first.ID)
	}

	for _, ranges := range []string{"1-3", "2-4"} {
		other := base
		other.ShardRanges = ranges
		job, existing, err := mgr.CreateJob(other)
		if err != nil {
			t.Fatal(err)
		}
		if existing || job.ID == first.ID {
			t.Errorf("shardRanges %q must not dedup against %q; existing=%v ids %s/%s",
				ranges, base.ShardRanges, existing, job.ID, first.ID)
		}
	}

	spelling := DownloadRequest{
		Repo: "dedup/shard-spelling", Filters: []string{".safetensors"},
		ExactMatch: true, ShardRanges: "1-3",
	}
	spanJob, existing, err := mgr.CreateJob(spelling)
	if err != nil {
		t.Fatal(err)
	}
	if existing {
		t.Fatal("first spelling request should create a new job")
	}
	sameCoverage := spelling
	sameCoverage.ShardRanges = "1,2,3"
	expanded, existing, err := mgr.CreateJob(sameCoverage)
	if err != nil {
		t.Fatal(err)
	}
	if existing || expanded.ID == spanJob.ID {
		t.Errorf("spelling pin: %q must stay distinct from %q; existing=%v ids %s/%s",
			sameCoverage.ShardRanges, spelling.ShardRanges, existing, expanded.ID, spanJob.ID)
	}
	duplicated := spelling
	duplicated.ShardRanges = "1-3,1-3"
	repeat, existing, err := mgr.CreateJob(duplicated)
	if err != nil {
		t.Fatal(err)
	}
	if existing || repeat.ID == spanJob.ID {
		t.Errorf("multiplicity pin: %q must stay distinct from %q; existing=%v ids %s/%s",
			duplicated.ShardRanges, spelling.ShardRanges, existing, repeat.ID, spanJob.ID)
	}
}

// TestCreateJobDedupAppendFilterSubdirOrderSemantic is the negative Step-0
// pin: with appendFilterSubdir the planner attributes each file to its
// winning filter (longest match, ties to the first listed filter), so filter
// list order can name different destination subdirectories and stays part of
// the request's identity: an identical order still dedups, a swap does not.
func TestCreateJobDedupAppendFilterSubdirOrderSemantic(t *testing.T) {
	mgr, cleanup := newRouteTestManager(t, Config{CacheDir: t.TempDir()})
	defer cleanup()

	base := DownloadRequest{
		Repo: "dedup/append-subdir-order", AppendFilterSubdir: true,
		Filters: []string{"q4_k_m", "q5_k_m"}, // equal length: either can win a tie
	}
	first, existing, err := mgr.CreateJob(base)
	if err != nil {
		t.Fatal(err)
	}
	if existing {
		t.Fatal("first request should create a new job")
	}
	same, existing, err := mgr.CreateJob(base)
	if err != nil {
		t.Fatal(err)
	}
	if !existing || same.ID != first.ID {
		t.Errorf("identical appendFilterSubdir request should dedup; existing=%v ids %s/%s", existing, same.ID, first.ID)
	}

	swapped := base
	swapped.Filters = []string{"q5_k_m", "q4_k_m"}
	other, existing, err := mgr.CreateJob(swapped)
	if err != nil {
		t.Fatal(err)
	}
	if existing || other.ID == first.ID {
		t.Errorf("appendFilterSubdir filter-order swap must not dedup; existing=%v ids %s/%s", existing, other.ID, first.ID)
	}
}

// TestCreateJobShardRangeWhitespaceInert pins whitespace around shardRanges
// segments and at the expression's edges as inert for identity: the shard
// parser trims outer and per-segment whitespace before parsing spans, so a
// whitespace-only difference describes the same request and must join the
// same queued/running job where a byte comparison queued two. The
// distinct-span control (`1-3` vs `2-4`) is already pinned by
// TestCreateJobShardRangeSegmentOrderIdentity.
func TestCreateJobShardRangeWhitespaceInert(t *testing.T) {
	mgr, cleanup := newRouteTestManager(t, Config{CacheDir: t.TempDir()})
	defer cleanup()

	base := DownloadRequest{
		Repo: "dedup/shard-whitespace", Filters: []string{".safetensors"},
		ExactMatch: true, ShardRanges: "1-3,5",
	}
	first, existing, err := mgr.CreateJob(base)
	if err != nil {
		t.Fatal(err)
	}
	if existing {
		t.Fatal("first request should create a new job")
	}

	for _, ranges := range []string{"1-3, 5", " 1-3,5 "} {
		padded := base
		padded.ShardRanges = ranges
		same, existing, err := mgr.CreateJob(padded)
		if err != nil {
			t.Fatal(err)
		}
		if !existing || same.ID != first.ID {
			t.Errorf("whitespace-only shardRanges difference %q should dedup against %q; existing=%v ids %s/%s",
				ranges, base.ShardRanges, existing, same.ID, first.ID)
		}
	}
}
