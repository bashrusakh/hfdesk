// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package smartdl

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// stallHandler serves normal responses for everything except the stalled
// paths: a stalled path sends response headers and a partial body and then
// blocks until the request context ends — the exact phase the transport's
// response-header window cannot bound and the phase a per-fetch cap exists
// to end. It also counts requests that reach a stalled path, so a test can
// pin that the stalled fetch was actually attempted.
func newStalledBodyFixture(t *testing.T, normal http.HandlerFunc, stalledPath func(string) bool) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var stalledHits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !stalledPath(r.URL.Path) {
			normal(w, r)
			return
		}
		stalledHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"model_`)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	return srv, &stalledHits
}

// analyzeNormalEndpoints serves the non-stalled endpoints of an analyze
// flow: the file tree, a quick refs response, and anything else with a
// plain success or the fail-open 404 (auth-check).
func analyzeNormalEndpoints(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.Contains(r.URL.Path, "/datasets/"):
		// Single-namespace fixture: the dataset probe must miss so the
		// analyze flow reaches the stalled fetch instead of ErrBothExist.
		http.NotFound(w, r)
	case strings.Contains(r.URL.Path, "/tree/"):
		_, _ = io.WriteString(w, `[{"type":"file","path":"config.json","size":100},{"type":"file","path":"model.safetensors","size":100}]`)
	case strings.HasSuffix(r.URL.Path, "/refs"):
		_, _ = io.WriteString(w, `{"branches":[{"name":"main","targetCommit":"abc"}]}`)
	case strings.Contains(r.URL.Path, "/raw/"):
		_, _ = io.WriteString(w, `{"model_type":"llama"}`)
	default:
		http.NotFound(w, r)
	}
}

// TestAnalyze_StalledMetadataBodyFailsWithinFetchBudget pins the per-fetch
// metadata cap on the analyze flow's required config/metadata reads: a
// server that sends response headers and a partial body and then stalls must
// fail the analysis within cap+slack with the actionable budget error —
// never a silently partial result and never classified as caller
// cancellation. The caller-deadline sub-case pins the opposite direction:
// when the caller's own deadline wins, the failure stays the caller's
// context error and is never re-shaped into a budget expiry.
func TestAnalyze_StalledMetadataBodyFailsWithinFetchBudget(t *testing.T) {
	srv, _ := newStalledBodyFixture(t, analyzeNormalEndpoints, func(path string) bool {
		return strings.Contains(path, "/raw/")
	})

	t.Run("budget expiry is an analysis failure", func(t *testing.T) {
		const testBudget = 500 * time.Millisecond
		oldBudget := metadataFetchTimeout
		metadataFetchTimeout = testBudget
		defer func() { metadataFetchTimeout = oldBudget }()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		type result struct {
			info *RepoInfo
			err  error
		}
		done := make(chan result, 1)
		start := time.Now()
		go func() {
			info, err := NewAnalyzer(AnalyzerOptions{Endpoint: srv.URL, HTTPClient: srv.Client()}).Analyze(ctx, "owner/model", false)
			done <- result{info, err}
		}()

		// Deliberately asymmetric bounds: the asserted bound carries runner
		// starvation headroom, while the watchdog fires well beyond it, where
		// an unbounded read and a severely starved runner are honestly
		// indistinguishable.
		const (
			slack         = 10 * time.Second // asserted-bound headroom (starvation tolerance)
			watchdogSlack = 30 * time.Second // watchdog fires well beyond the asserted bound
		)
		select {
		case res := <-done:
			if res.info != nil || res.err == nil {
				t.Fatalf("stalled metadata body info=%v err=%v, want an analysis timeout failure and no partial result", res.info, res.err)
			}
			if elapsed := time.Since(start); elapsed > testBudget+slack {
				t.Errorf("analysis returned after %v, want within cap+slack %v", elapsed, testBudget+slack)
			}
			for _, want := range []string{
				`metadata config.json`,
				`file content fetch for "config.json"`,
				"timed out after 500ms",
				"check the endpoint, proxy, or network and retry",
			} {
				if !strings.Contains(res.err.Error(), want) {
					t.Errorf("timeout error = %q, want it to contain %q", res.err, want)
				}
			}
			// The budget expiry stays a FAILURE for error classifiers and
			// must never read as caller cancellation.
			if errors.Is(res.err, context.DeadlineExceeded) || errors.Is(res.err, context.Canceled) {
				t.Errorf("timeout error %v must not classify as caller cancellation", res.err)
			}
		case <-time.After(testBudget + watchdogSlack):
			cancel()
			t.Fatalf("analysis did not complete within the watchdog (cap %v + slack %v): unbounded config body read or a starved runner", testBudget, watchdogSlack)
		}
	})

	t.Run("caller deadline stays caller cancellation", func(t *testing.T) {
		// The default 2-minute cap is untouched here: a server that stalls
		// before any response header runs into the caller's own 300ms
		// deadline first, and that failure must surface as the caller's
		// context error, never re-shaped into a budget expiry.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		}))
		t.Cleanup(srv.Close)

		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		_, err := NewAnalyzer(AnalyzerOptions{Endpoint: srv.URL, HTTPClient: srv.Client()}).
			fetchFile(ctx, "owner/model", false, "main", "config.json", false)
		if err == nil {
			t.Fatal("caller-deadline fetch succeeded, want the caller's context error")
		}
		if strings.Contains(err.Error(), "timed out after") {
			t.Errorf("caller-deadline error %v must not be reported as a budget expiry", err)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("caller-deadline error %v must classify as context.DeadlineExceeded", err)
		}
	})
}

// TestAnalyze_StalledRefsBodyDegradesWithinFetchBudget pins the cap on an
// OPTIONAL analyze fetch: refs failure is non-fatal for the analysis, so a
// stalled refs body must end at the cap and degrade exactly like any other
// refs failure — the analysis completes without refs within cap+slack
// instead of hanging on the unbounded body read. The fixture's stalled-path
// counter pins that the refs fetch was actually attempted, so a regression
// that drops the call cannot pass this test vacuously.
func TestAnalyze_StalledRefsBodyDegradesWithinFetchBudget(t *testing.T) {
	srv, stalledHits := newStalledBodyFixture(t, analyzeNormalEndpoints, func(path string) bool {
		return strings.HasSuffix(path, "/refs")
	})

	const testBudget = 500 * time.Millisecond
	oldBudget := metadataFetchTimeout
	metadataFetchTimeout = testBudget
	defer func() { metadataFetchTimeout = oldBudget }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		info *RepoInfo
		err  error
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		info, err := NewAnalyzer(AnalyzerOptions{Endpoint: srv.URL, HTTPClient: srv.Client()}).Analyze(ctx, "owner/model", false)
		done <- result{info, err}
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
		if res.err != nil {
			t.Fatalf("optional refs failure became fatal: %v", res.err)
		}
		if res.info == nil {
			t.Fatal("analysis returned no result after an optional refs failure")
		}
		if len(res.info.Refs) != 0 {
			t.Errorf("refs = %v, want the established empty degradation for a failed refs fetch", res.info.Refs)
		}
		if got := stalledHits.Load(); got != 1 {
			t.Errorf("stalled refs requests = %d, want exactly 1: the refs fetch must actually be attempted for this degradation to discriminate", got)
		}
		if elapsed := time.Since(start); elapsed > testBudget+slack {
			t.Errorf("analysis returned after %v, want within cap+slack %v", elapsed, testBudget+slack)
		}
		if res.info.Metadata["config.json"] == nil {
			t.Errorf("analysis metadata lost while degrading an optional refs failure: %+v", res.info.Metadata)
		}
	case <-time.After(testBudget + watchdogSlack):
		cancel()
		t.Fatalf("analysis did not complete within the watchdog (cap %v + slack %v): unbounded refs body read or a starved runner", testBudget, watchdogSlack)
	}
}
