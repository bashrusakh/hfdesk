// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestStalledRepoInfoBodyFailsPlanningWithinFetchBudget pins the per-fetch
// TOTAL cap on the required initial metadata fetch: a server that sends
// response headers but then stalls the JSON body must fail planning with the
// actionable timeout error within cap+slack instead of hanging forever (the
// body read was previously bounded only by a deadline-less parent context).
// The internal cap expiry is a planning FAILURE, not a caller cancellation:
// Download distinguishes the two in its terminal log and job reporting.
func TestStalledRepoInfoBodyFailsPlanningWithinFetchBudget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/revision/") {
			http.NotFound(w, r)
			return
		}
		// Headers and a partial JSON body arrive, then the body stalls: the
		// exact phase the transport's ResponseHeaderTimeout cannot bound.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"sha":"`))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	const testBudget = 500 * time.Millisecond
	oldBudget := repoInfoFetchTimeout
	repoInfoFetchTimeout = testBudget
	defer func() { repoInfoFetchTimeout = oldBudget }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		plan *Plan
		err  error
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		plan, err := PlanRepo(ctx, Job{Repo: "owner/model", Revision: "main"}, Settings{Endpoint: srv.URL})
		done <- result{plan, err}
	}()

	const slack = 3 * time.Second
	select {
	case res := <-done:
		if res.plan != nil || res.err == nil {
			t.Fatalf("stalled metadata fetch plan=%v err=%v, want a planning timeout failure", res.plan, res.err)
		}
		if elapsed := time.Since(start); elapsed > testBudget+slack {
			t.Errorf("planning returned after %v, want within cap+slack %v", elapsed, testBudget+slack)
		}
		if !strings.Contains(res.err.Error(), "repo info API timed out after") ||
			!strings.Contains(res.err.Error(), "check the endpoint") {
			t.Errorf("timeout error = %q, want an actionable message naming the elapsed budget and remedy", res.err)
		}
		// An internal budget expiry is not caller cancellation; a wrapped
		// context error would misclassify this failure as a cancelled job.
		if errors.Is(res.err, context.DeadlineExceeded) || errors.Is(res.err, context.Canceled) {
			t.Errorf("timeout error %v must not classify as caller cancellation", res.err)
		}
	case <-time.After(testBudget + slack):
		cancel()
		t.Fatalf("planning metadata fetch outlived its per-fetch cap (%v + %v): the JSON body read is unbounded", testBudget, slack)
	}
}

// TestHangingFallbackConfirmationNeverEnablesBranchFallback pins the
// tree-walk-404 fallback confirmation (the addressing-probe budget family):
// when the fresh revision lookup stalls, the confirmation must expire
// inconclusively within the family budget and NEVER enable the branch
// fallback — uncertainty never upgrades to branch-only addressing (see
// TestRevisionAuthOrServerFailureNeverEnablesBranchFallback).
func TestHangingFallbackConfirmationNeverEnablesBranchFallback(t *testing.T) {
	files := map[string]string{"config.json": "branch-content"}
	f := newPinningFixture(t, files, files, nil, nil, "", 0, "")
	// Pinned commit tree 404s, so the fallback condition at the tree-walk
	// error reaches the confirmation lookup; that lookup (revision call 2,
	// after the initial pin) then stalls until its budget expires.
	f.missingPinnedTree = true
	f.hangRevisionAfter = 2

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		plan *Plan
		err  error
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		plan, err := PlanRepo(ctx, Job{Repo: "owner/model", Revision: "main"}, Settings{Endpoint: f.server.URL})
		done <- result{plan, err}
	}()

	const slack = 5 * time.Second
	select {
	case res := <-done:
		if res.plan != nil || res.err == nil {
			t.Fatalf("hanging confirmation plan=%v err=%v, want the inconclusive tree-walk failure", res.plan, res.err)
		}
		// The ORIGINAL tree-walk 404 must surface unchanged: an inconclusive
		// confirmation contributes no error of its own, so planning returns
		// the fixture's documented tree-walk error value (the identity
		// TestPlanRepo_TerminalTreeFailsFast pins), not a confirmation error.
		if res.err.Error() != "tree API failed: 404 Not Found" {
			t.Errorf("error = %q, want the original tree-walk 404 %q, not a confirmation error", res.err, "tree API failed: 404 Not Found")
		}
		if elapsed := time.Since(start); elapsed > addressingProbeBudget+slack {
			t.Errorf("planning returned after %v, want within family budget+slack %v", elapsed, addressingProbeBudget+slack)
		}
		for _, request := range f.requests() {
			if strings.Contains(request.path, "/tree/main") ||
				strings.Contains(request.path, "/raw/main/") ||
				strings.Contains(request.path, "/resolve/main/") {
				t.Errorf("expiring confirmation enabled the branch fallback: %s %s", request.method, request.path)
			}
		}
		f.mu.Lock()
		revisionCalls := f.revisionCalls
		f.mu.Unlock()
		if revisionCalls != 2 {
			t.Errorf("revision calls = %d, want one initial pin and one confirmation", revisionCalls)
		}
	case <-time.After(addressingProbeBudget + slack):
		cancel()
		t.Fatalf("fallback confirmation outlived the addressing-probe budget (%v + %v): an unbounded confirmation can hang planning", addressingProbeBudget, slack)
	}
}
