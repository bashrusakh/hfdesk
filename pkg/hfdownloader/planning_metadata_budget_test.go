// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
	oldBudget := metadataFetchTimeout
	metadataFetchTimeout = testBudget
	defer func() { metadataFetchTimeout = oldBudget }()

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

	// Deliberately asymmetric bounds: the asserted bound carries runner
	// starvation headroom, while the watchdog fires well beyond it, where an
	// unbounded read and a severely starved runner are honestly
	// indistinguishable.
	const (
		slack         = 10 * time.Second // asserted-bound headroom (starvation tolerance)
		watchdogSlack = 30 * time.Second // watchdog fires well beyond the asserted bound
	)
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
	case <-time.After(testBudget + watchdogSlack):
		cancel()
		t.Fatalf("planning metadata fetch did not complete within the watchdog (cap %v + slack %v): unbounded JSON body read or a starved runner", testBudget, watchdogSlack)
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

	// Deliberately asymmetric bounds: the asserted bound carries runner
	// starvation headroom, while the watchdog fires well beyond it, where an
	// unbounded confirmation and a severely starved runner are honestly
	// indistinguishable.
	const (
		slack         = 10 * time.Second // asserted-bound headroom (starvation tolerance)
		watchdogSlack = 30 * time.Second // watchdog fires well beyond the asserted bound
	)
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
	case <-time.After(addressingProbeBudget + watchdogSlack):
		cancel()
		t.Fatalf("fallback confirmation did not complete within the watchdog (budget %v + slack %v): unbounded confirmation or a starved runner", addressingProbeBudget, watchdogSlack)
	}
}

// TestStalledTreePageBodyFailsPlanningWithinFetchBudget pins the per-request
// cap on the tree-page body read during the INITIAL pinned walk: headers and
// a partial JSON page arrive, then the body stalls — the exact phase the
// transport's ResponseHeaderTimeout cannot bound and the phase that
// previously hung a deadline-less planning job. Planning must fail within
// cap+slack with the actionable timeout error, never as caller cancellation,
// and the expiry must never enable the branch fallback: the pinned rule is
// that only an authoritative tree 404 reaches the confirmation.
func TestStalledTreePageBodyFailsPlanningWithinFetchBudget(t *testing.T) {
	var mu sync.Mutex
	var seenPaths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seenPaths = append(seenPaths, r.URL.Path)
		mu.Unlock()
		switch {
		case strings.Contains(r.URL.Path, "/revision/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"sha":"cccccccccccccccccccccccccccccccccccccccc"}`))
		case strings.Contains(r.URL.Path, "/tree/"):
			// Headers and a partial JSON body arrive, then the body stalls.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`[{"type":"file","path":"config.json"`))
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			<-r.Context().Done()
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	const testBudget = 500 * time.Millisecond
	oldBudget := metadataFetchTimeout
	metadataFetchTimeout = testBudget
	defer func() { metadataFetchTimeout = oldBudget }()

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

	// Deliberately asymmetric bounds: the asserted bound carries runner
	// starvation headroom, while the watchdog fires well beyond it, where an
	// unbounded read and a severely starved runner are honestly
	// indistinguishable.
	const (
		slack         = 10 * time.Second // asserted-bound headroom (starvation tolerance)
		watchdogSlack = 30 * time.Second // watchdog fires well beyond the asserted bound
	)
	select {
	case res := <-done:
		if res.plan != nil || res.err == nil {
			t.Fatalf("stalled tree page plan=%v err=%v, want a planning timeout failure", res.plan, res.err)
		}
		if elapsed := time.Since(start); elapsed > testBudget+slack {
			t.Errorf("planning returned after %v, want within cap+slack %v", elapsed, testBudget+slack)
		}
		wantPrefix := "tree page API timed out after " + testBudget.String()
		if !strings.Contains(res.err.Error(), wantPrefix) ||
			!strings.Contains(res.err.Error(), `for "owner/model"`) ||
			!strings.Contains(res.err.Error(), "check the endpoint") {
			t.Errorf("timeout error = %q, want an actionable message naming the budget, repo and remedy (%q)", res.err, wantPrefix)
		}
		// An internal per-request expiry is not caller cancellation; a
		// wrapped context error would misclassify this failure as a
		// cancelled job in Download's terminal log and job reporting.
		if errors.Is(res.err, context.DeadlineExceeded) || errors.Is(res.err, context.Canceled) {
			t.Errorf("timeout error %v must not classify as caller cancellation", res.err)
		}
		// Uncertainty never enables the branch fallback: exactly the one
		// pinned tree page, no retry/rescan of it, and no branch-revision
		// tree or file request may follow the expiry.
		mu.Lock()
		paths := append([]string(nil), seenPaths...)
		mu.Unlock()
		var treeRequests int
		for _, p := range paths {
			if strings.Contains(p, "/tree/") {
				treeRequests++
			}
			if strings.Contains(p, "/tree/main") ||
				strings.Contains(p, "/raw/main/") ||
				strings.Contains(p, "/resolve/main/") {
				t.Errorf("tree-page timeout enabled the branch fallback: %s", p)
			}
		}
		if treeRequests != 1 {
			t.Errorf("tree requests = %d (%v), want exactly the single pinned page", treeRequests, paths)
		}
	case <-time.After(testBudget + watchdogSlack):
		cancel()
		t.Fatalf("tree-page body read did not complete within the watchdog (cap %v + slack %v): unbounded JSON page decode or a starved runner", testBudget, watchdogSlack)
	}
}

// TestStalledFallbackRebuildTreePageFailsWithinFetchBudgetWithoutRescan pins
// the same per-request cap on the fallback-rebuild walk (the second walkTree
// execution): after the pinned tree 404 and a successful revision
// confirmation, the rebuild walk's stalled page body must fail planning
// within cap+slack with the actionable timeout error — never classified as
// cancellation, never producing a plan — and the expiry must not trigger any
// further fallback rescan or file request.
func TestStalledFallbackRebuildTreePageFailsWithinFetchBudgetWithoutRescan(t *testing.T) {
	files := map[string]string{"config.json": "branch-content"}
	f := newPinningFixture(t, files, files, nil, nil, "", 0, "")
	f.missingPinnedTree = true
	f.stallBranchTree = true

	const testBudget = 500 * time.Millisecond
	oldBudget := metadataFetchTimeout
	metadataFetchTimeout = testBudget
	defer func() { metadataFetchTimeout = oldBudget }()

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

	// Deliberately asymmetric bounds: the asserted bound carries runner
	// starvation headroom, while the watchdog fires well beyond it, where an
	// unbounded read and a severely starved runner are honestly
	// indistinguishable.
	const (
		slack         = 10 * time.Second // asserted-bound headroom (starvation tolerance)
		watchdogSlack = 30 * time.Second // watchdog fires well beyond the asserted bound
	)
	select {
	case res := <-done:
		if res.plan != nil || res.err == nil {
			t.Fatalf("stalled rebuild walk plan=%v err=%v, want a planning timeout failure", res.plan, res.err)
		}
		if elapsed := time.Since(start); elapsed > testBudget+slack {
			t.Errorf("planning returned after %v, want within cap+slack %v", elapsed, testBudget+slack)
		}
		wantPrefix := "tree page API timed out after " + testBudget.String()
		if !strings.Contains(res.err.Error(), wantPrefix) ||
			!strings.Contains(res.err.Error(), `for "owner/model"`) ||
			!strings.Contains(res.err.Error(), "check the endpoint") {
			t.Errorf("timeout error = %q, want an actionable message naming the budget, repo and remedy (%q)", res.err, wantPrefix)
		}
		if errors.Is(res.err, context.DeadlineExceeded) || errors.Is(res.err, context.Canceled) {
			t.Errorf("timeout error %v must not classify as caller cancellation", res.err)
		}
		var commitTree, branchTree, transfers int
		for _, request := range f.requests() {
			switch {
			case strings.Contains(request.path, "/tree/") && strings.HasSuffix(request.path, "/tree/"+pinningFixtureCommit):
				commitTree++
			case strings.Contains(request.path, "/tree/") && strings.HasSuffix(request.path, "/tree/main"):
				branchTree++
			case strings.Contains(request.path, "/raw/") || strings.Contains(request.path, "/resolve/"):
				transfers++
			}
		}
		if commitTree != 1 || branchTree != 1 {
			t.Errorf("tree requests = commit %d branch %d, want one initial 404 and one stalled rebuild page (no rescan on expiry)", commitTree, branchTree)
		}
		if transfers != 0 {
			t.Errorf("rebuild-walk expiry produced %d file requests, want none", transfers)
		}
		f.mu.Lock()
		revisionCalls := f.revisionCalls
		f.mu.Unlock()
		if revisionCalls != 2 {
			t.Errorf("revision calls = %d, want one initial pin and one confirmation, no lookup after expiry", revisionCalls)
		}
	case <-time.After(testBudget + watchdogSlack):
		cancel()
		t.Fatalf("fallback-rebuild tree page did not complete within the watchdog (cap %v + slack %v): unbounded rebuild-walk JSON body read or a starved runner", testBudget, watchdogSlack)
	}
}

// TestStalledTreePageHeadersFailPlanningWithinFetchBudget pins the other
// half of the per-request deadline: a page request whose response HEADERS
// stall past the cap must terminate at the cap with the same actionable
// timeout — one attempt, no retry loop — instead of burning the transport's
// response-header window once per retryable attempt.
func TestStalledTreePageHeadersFailPlanningWithinFetchBudget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/revision/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"sha":"cccccccccccccccccccccccccccccccccccccccc"}`))
		case strings.Contains(r.URL.Path, "/tree/"):
			// Stall before any response header is written.
			<-r.Context().Done()
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	const testBudget = 500 * time.Millisecond
	oldBudget := metadataFetchTimeout
	metadataFetchTimeout = testBudget
	defer func() { metadataFetchTimeout = oldBudget }()

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

	// Deliberately asymmetric bounds: the asserted bound carries runner
	// starvation headroom, while the watchdog fires well beyond it, where a
	// deadline that never fires and a severely starved runner are honestly
	// indistinguishable.
	const (
		slack         = 10 * time.Second // asserted-bound headroom (starvation tolerance)
		watchdogSlack = 30 * time.Second // watchdog fires well beyond the asserted bound
	)
	select {
	case res := <-done:
		if res.plan != nil || res.err == nil {
			t.Fatalf("header-stalled tree page plan=%v err=%v, want a planning timeout failure", res.plan, res.err)
		}
		if elapsed := time.Since(start); elapsed > testBudget+slack {
			t.Errorf("planning returned after %v, want within cap+slack %v", elapsed, testBudget+slack)
		}
		wantPrefix := "tree page API timed out after " + testBudget.String()
		if !strings.Contains(res.err.Error(), wantPrefix) ||
			!strings.Contains(res.err.Error(), "check the endpoint") {
			t.Errorf("timeout error = %q, want an actionable message naming the budget and remedy (%q)", res.err, wantPrefix)
		}
		if errors.Is(res.err, context.DeadlineExceeded) || errors.Is(res.err, context.Canceled) {
			t.Errorf("timeout error %v must not classify as caller cancellation", res.err)
		}
	case <-time.After(testBudget + watchdogSlack):
		cancel()
		t.Fatalf("header-stalled tree page did not complete within the watchdog (cap %v + slack %v): the page request deadline never fired, or the runner is starved", testBudget, watchdogSlack)
	}
}
