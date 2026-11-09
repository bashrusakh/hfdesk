package hfdownloader

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestAddressingPreflightBudgetBoundsFullyCachedWalk pins the preflight's
// total budget on the path that performs only cache-side probes: a large
// fully-cached repository must stop walking at the deadline and return the
// pinned plan unchanged instead of probing every item with no bound. The
// probe counter discriminates — each hook call is one walk iteration, so a
// count below the item total proves the deadline stopped the walk.
func TestAddressingPreflightBudgetBoundsFullyCachedWalk(t *testing.T) {
	const itemCount = 800
	content := "fully-cached-body"
	files := make(map[string]string, itemCount)
	for i := 0; i < itemCount; i++ {
		files[fmt.Sprintf("weights/part-%04d.safetensors", i)] = content
	}
	f := newPinningFixture(t, files, files, nil, nil, "", 0, "")
	cfg := Settings{CacheDir: t.TempDir(), Endpoint: f.server.URL, Verify: "size"}
	job := Job{Repo: "owner/model", Revision: "main"}

	cache, err := cfg.BuildHFCache()
	if err != nil {
		t.Fatal(err)
	}
	repoDir, err := cache.Repo(job.Repo, RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := repoDir.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	sha := digestText(content)
	if err := os.WriteFile(repoDir.BlobPath(sha), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	var probes atomic.Int32
	restore := addressingCacheProbeHook
	addressingCacheProbeHook = func(context.Context) {
		probes.Add(1)
		// Stretch each cache-side probe so only a deadline-bounded walk can
		// stop before every item has been probed.
		time.Sleep(5 * time.Millisecond)
	}
	t.Cleanup(func() { addressingCacheProbeHook = restore })

	plan, err := PlanRepo(context.Background(), job, cfg)
	if err != nil {
		t.Fatalf("fully-cached plan: %v", err)
	}
	got := probes.Load()
	if got == 0 {
		t.Fatal("preflight walk never probed the cache; fixture did not exercise the path")
	}
	// Floor: the budget-bounded walk performs ~600 probes (5ms each under
	// the 3s budget); even a heavily loaded CI run stays far above this,
	// while an over-aggressive early break (e.g. after one iteration) fails
	// it. Floor and ceiling together prove "bounded at ~the budget", not
	// merely "stopped early".
	const minProbeFloor = 100
	if int(got) < minProbeFloor {
		t.Fatalf("preflight walk ran only %d cache-side probes for %d cached items; the walk stopped far below any plausible budget-bounded count", got, itemCount)
	}
	if int(got) >= itemCount {
		t.Fatalf("preflight walk ran %d cache-side probes for %d cached items; the %v budget did not bound it", got, itemCount, addressingProbeBudget)
	}
	if !plan.CommitPinned || plan.Commit != pinningFixtureCommit || len(plan.Items) != itemCount {
		t.Fatalf("budget expiry changed the pinned plan: pinned=%v commit=%q items=%d", plan.CommitPinned, plan.Commit, len(plan.Items))
	}
	for _, item := range plan.Items {
		if !strings.Contains(item.URL, "/"+pinningFixtureCommit+"/") {
			t.Fatalf("walk returned a non-pinned URL after budget expiry: %q", item.URL)
		}
	}
	for _, request := range f.requests() {
		if strings.Contains(request.path, "/tree/main") {
			t.Fatalf("fully-cached walk attempted a branch fallback: %+v", request)
		}
	}
}

// TestRevisionRecheckSharesAddressingProbeBudget pins the second budget
// escape: the branch-confirmation metadata lookup runs under the same
// preflight budget, so a metadata API that stalls the JSON body cannot delay
// planning past it, and an expired budget keeps the pinned plan (inconclusive,
// no branch-mode fallback).
func TestRevisionRecheckSharesAddressingProbeBudget(t *testing.T) {
	const content = "one-file-body"
	var revisionCalls, branchTrees atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/revision/"):
			if revisionCalls.Add(1) == 1 {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(RepoInfo{SHA: pinningFixtureCommit})
				return
			}
			// The re-confirmation answers headers but then stalls the JSON
			// body, so only the request context can bound the body read.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			select {
			case <-r.Context().Done():
			case <-release:
			case <-time.After(15 * time.Second):
			}
		case strings.Contains(r.URL.Path, "/tree/"):
			if !strings.HasSuffix(r.URL.Path, "/tree/"+pinningFixtureCommit) {
				branchTrees.Add(1)
				http.NotFound(w, r)
				return
			}
			nodes := []hfNode{{Type: "file", Path: "config.json", Size: int64(len(content)), Sha256: digestText(content)}}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(nodes)
		case strings.Contains(r.URL.Path, "/raw/") || strings.Contains(r.URL.Path, "/resolve/"):
			marker := "/raw/"
			if !strings.Contains(r.URL.Path, marker) {
				marker = "/resolve/"
			}
			rest := r.URL.Path[strings.Index(r.URL.Path, marker)+len(marker):]
			parts := strings.SplitN(rest, "/", 2)
			if len(parts) == 2 && parts[0] == pinningFixtureCommit {
				// Pinned addressing is confirmed missing for this file.
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write([]byte(content))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	cfg := Settings{OutputDir: t.TempDir(), Endpoint: srv.URL, Verify: "size"}
	start := time.Now()
	plan, err := PlanRepo(context.Background(), Job{Repo: "owner/model", Revision: "main"}, cfg)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("planning with a stalling revision re-check: %v", err)
	}
	if elapsed > addressingProbeBudget+4*time.Second {
		t.Errorf("planning took %v; the %v budget did not bound the revision re-check", elapsed, addressingProbeBudget)
	}
	if plan == nil || !plan.CommitPinned || plan.Commit != pinningFixtureCommit || len(plan.Items) != 1 || !strings.Contains(plan.Items[0].URL, "/"+pinningFixtureCommit+"/") {
		t.Fatalf("budget expiry changed the pinned plan: %+v", plan)
	}
	if got := revisionCalls.Load(); got != 2 {
		t.Fatalf("revision requests = %d, want initial resolution plus one bounded re-check", got)
	}
	if got := branchTrees.Load(); got != 0 {
		t.Fatalf("revision re-check expiry attempted %d branch-tree fallback rebuilds", got)
	}
}

// TestCacheSideProbeClampedToAddressingProbeBudget pins the cache-side
// probe's context clamp: a probe that hangs (standing in for the ownership
// init gate's minutes-long wait) is cut at the preflight deadline, so the
// walk stops within budget+slack with the pinned plan unchanged instead of
// outliving the budget by the gate's own timeout.
func TestCacheSideProbeClampedToAddressingProbeBudget(t *testing.T) {
	const itemCount = 4
	content := "clamped-probe-body"
	files := make(map[string]string, itemCount)
	for i := 0; i < itemCount; i++ {
		files[fmt.Sprintf("weights/part-%04d.safetensors", i)] = content
	}
	f := newPinningFixture(t, files, files, nil, nil, "", 0, "")
	cfg := Settings{CacheDir: t.TempDir(), Endpoint: f.server.URL, Verify: "size"}
	job := Job{Repo: "owner/model", Revision: "main"}

	cache, err := cfg.BuildHFCache()
	if err != nil {
		t.Fatal(err)
	}
	repoDir, err := cache.Repo(job.Repo, RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := repoDir.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	sha := digestText(content)
	if err := os.WriteFile(repoDir.BlobPath(sha), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	var probes atomic.Int32
	restore := addressingCacheProbeHook
	addressingCacheProbeHook = func(ctx context.Context) {
		probes.Add(1)
		// Hang like the ownership init gate can: only the probe's own
		// clamped context may cut this wait at the preflight deadline.
		select {
		case <-ctx.Done():
		case <-time.After(15 * time.Second):
		}
	}
	t.Cleanup(func() { addressingCacheProbeHook = restore })

	start := time.Now()
	plan, err := PlanRepo(context.Background(), job, cfg)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("planning with a hanging cache-side probe: %v", err)
	}
	if elapsed > addressingProbeBudget+4*time.Second {
		t.Errorf("planning took %v; the hanging cache-side probe outlived the %v budget", elapsed, addressingProbeBudget)
	}
	if got := probes.Load(); got != 1 {
		t.Fatalf("cache-side probes = %d; a probe cut at the deadline must end the walk at the next loop-head check, not continue probing", got)
	}
	if !plan.CommitPinned || plan.Commit != pinningFixtureCommit || len(plan.Items) != itemCount {
		t.Fatalf("clamped probe changed the pinned plan: pinned=%v commit=%q items=%d", plan.CommitPinned, plan.Commit, len(plan.Items))
	}
	for _, item := range plan.Items {
		if !strings.Contains(item.URL, "/"+pinningFixtureCommit+"/") {
			t.Fatalf("walk returned a non-pinned URL after the probe clamp: %q", item.URL)
		}
	}
	for _, request := range f.requests() {
		if strings.Contains(request.path, "/tree/main") {
			t.Fatalf("clamped probe triggered a branch fallback: %+v", request)
		}
	}
}
