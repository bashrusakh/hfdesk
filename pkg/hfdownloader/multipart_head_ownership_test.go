// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// A multipart HEAD failure must not remove the single-route `.part`/sidecar:
// the destination domain excludes writers but does not transfer artifact
// ownership between routes. Tests cover stale and live single-route partials
// through the real Download path; the contention hook establishes ordering.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// crossRouteHeadHub serves one LFS file whose HEAD is a permanent 404, while
// payload GETs are served from full. When gated, every payload response is
// held until the test opens the gate, and the first one closes entered.
type crossRouteHeadHub struct {
	srv       *httptest.Server
	entered   chan struct{}
	gate      chan struct{}
	enterOnce sync.Once
	gateOnce  sync.Once
	headCalls atomic.Int64
	gets      atomic.Int64
}

func newCrossRouteHeadHub(t *testing.T, full []byte, rel string, gated bool) *crossRouteHeadHub {
	t.Helper()
	h := &crossRouteHeadHub{entered: make(chan struct{}), gate: make(chan struct{})}
	sha := singleTestSHA(full)
	node := fmt.Sprintf(`{"type":"file","path":%q,"size":%d,"lfs":{"oid":%q,"size":%d,"sha256":%q}}`,
		rel, len(full), sha, len(full), sha)
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/revision/main"):
			_, _ = w.Write([]byte(`{"sha":"commit123"}`))
		case strings.Contains(r.URL.Path, "/tree/"):
			_, _ = w.Write([]byte("[" + node + "]"))
		case strings.Contains(r.URL.Path, "/resolve/") || strings.Contains(r.URL.Path, "/raw/"):
			if r.Method == http.MethodHead {
				h.headCalls.Add(1)
				http.NotFound(w, r)
				return
			}
			h.gets.Add(1)
			if gated {
				h.enterOnce.Do(func() { close(h.entered) })
				select {
				case <-h.gate:
				case <-r.Context().Done():
					return
				}
			}
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(full))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(func() {
		// Unblock handlers before closing the server.
		h.open()
		h.srv.Close()
	})
	return h
}

// open releases the gate. It is safe to call more than once.
func (h *crossRouteHeadHub) open() {
	h.gateOnce.Do(func() { close(h.gate) })
}

// A multipart HEAD failure owns no single-route .part or sidecar and must
// leave an earlier run's pair untouched.
func TestFlatModeMultipartHeadFailFastPreservesSingleRoutePartial(t *testing.T) {
	const total = 4_096
	full := rangedTestPayload(total)
	out := t.TempDir()
	dst := legacyOutputDst(t, out)

	// The single-request route's pair, exactly as a writer leaves it between
	// opening the partial and its first write: an empty `.part` with a
	// matching, pinned identity sidecar.
	job := Job{Repo: "o/r"}
	it := PlanItem{RelativePath: "model.bin", Size: total}
	if err := os.WriteFile(dst+".part", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	seedSingleIdentity(t, dst, job, it, singleOpts{Commit: "commit123", CommitPinned: true})
	sidecarBefore, err := os.ReadFile(singlePartIdentityPath(dst))
	if err != nil {
		t.Fatalf("fixture precondition: single-request identity sidecar: %v", err)
	}

	hub := newCrossRouteHeadHub(t, full, "model.bin", false)
	res := runFlatModeDownload(context.Background(), flatModeSettings(out, hub.srv.URL, "0"))
	if res.err == nil {
		t.Fatalf("the permanent HEAD failure did not fail the item (skips=%v done=%v)", res.skips, res.done)
	}
	if !errors.Is(res.err, ErrNotFound) {
		t.Errorf("Download error = %v, want ErrNotFound", res.err)
	}
	if hub.headCalls.Load() == 0 {
		t.Fatal("no HEAD was issued: the multipart route under test did not run")
	}
	if got := hub.gets.Load(); got != 0 {
		t.Errorf("payload GETs = %d, want 0: the fail-fast must happen before any transfer", got)
	}

	fi, serr := os.Stat(dst + ".part")
	if serr != nil {
		t.Fatalf("the fail-fast removed the single-request partial: %v", serr)
	}
	if fi.Size() != 0 {
		t.Errorf("partial size = %d bytes, want the seeded empty partial", fi.Size())
	}
	sidecarAfter, serr := os.ReadFile(singlePartIdentityPath(dst))
	if serr != nil {
		t.Fatalf("the fail-fast removed the single-request identity sidecar: %v", serr)
	}
	if !bytes.Equal(sidecarBefore, sidecarAfter) {
		t.Errorf("the identity sidecar changed across the fail-fast:\nbefore=%s\nafter=%s", sidecarBefore, sidecarAfter)
	}
}

// A live single writer excludes the multipart writer before HEAD. Its empty
// partial and sidecar must survive until it publishes; the waiter then runs
// HEAD and fails without disturbing the completed destination.
func TestFlatModeMultipartHeadFailFastDoesNotUnlinkLiveSingleWriter(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	const total = 4_096
	full := rangedTestPayload(total)
	out := t.TempDir()
	dst := legacyOutputDst(t, out)
	hub := newCrossRouteHeadHub(t, full, "model.bin", true)
	shortBlobOwnershipWait(t, 250*time.Millisecond)

	// Hold the single route inside its payload GET.
	writer1 := make(chan flatModeOutcome, 1)
	go func() {
		writer1 <- runFlatModeDownload(context.Background(), flatModeSettings(out, hub.srv.URL, "1MB"))
	}()
	select {
	case <-hub.entered:
	case res := <-writer1:
		t.Fatalf("writer 1 finished before its payload transfer started (err=%v)", res.err)
	case <-time.After(60 * time.Second):
		t.Fatal("writer 1 never started its payload transfer")
	}

	// The gated single writer holds the domain and has an empty partial with
	// a persisted identity sidecar.
	sidecarBefore, err := os.ReadFile(singlePartIdentityPath(dst))
	if err != nil {
		t.Fatalf("fixture precondition: writer 1 has no identity sidecar: %v", err)
	}
	partBefore, serr := os.Stat(dst + ".part")
	if serr != nil {
		t.Fatalf("fixture precondition: writer 1 has no partial: %v", serr)
	}
	if partBefore.Size() != 0 {
		t.Fatalf("fixture precondition: writer 1's partial is %d bytes, want the empty staging file", partBefore.Size())
	}

	// The multipart waiter must not reach HEAD while the single writer owns dst.
	contended := installBlobDomainContentionProbe(t)
	writer2 := make(chan flatModeOutcome, 1)
	go func() {
		writer2 <- runFlatModeDownload(context.Background(), flatModeSettings(out, hub.srv.URL, "0"))
	}()
	awaitContention(t, contended, "the live single-route writer's destination domain")
	select {
	case res := <-writer2:
		t.Fatalf("the second writer finished while the live owner still held the destination (err=%v)", res.err)
	default:
	}
	if got := hub.headCalls.Load(); got != 0 {
		t.Errorf("HEAD calls = %d, want 0: the waiting writer must not run its HEAD", got)
	}
	if got := hub.gets.Load(); got != 1 {
		t.Errorf("payload GETs = %d, want only writer 1's transfer while it holds the destination", got)
	}

	partAfter, serr := os.Stat(dst + ".part")
	if serr != nil {
		t.Fatalf("the waiting writer unlinked the live single-route writer's partial: %v", serr)
	}
	if partAfter.Size() != 0 {
		t.Errorf("live partial size = %d bytes, want the untouched empty file", partAfter.Size())
	}
	sidecarAfter, serr := os.ReadFile(singlePartIdentityPath(dst))
	if serr != nil {
		t.Fatalf("the waiting writer unlinked the live single-route writer's identity sidecar: %v", serr)
	}
	if !bytes.Equal(sidecarBefore, sidecarAfter) {
		t.Errorf("the live writer's identity sidecar changed:\nbefore=%s\nafter=%s", sidecarBefore, sidecarAfter)
	}

	// Successful publication after release also checks that the original partial
	// survived the waiting multipart writer.
	hub.open()
	var published flatModeOutcome
	select {
	case published = <-writer1:
	case <-time.After(60 * time.Second):
		t.Fatal("writer 1 did not finish after the gate opened")
	}
	if published.err != nil {
		t.Fatalf("writer 1 failed after the multipart writer waited: %v", published.err)
	}
	assertExactFile(t, dst, full)

	// Writer 2 now owns the destination and runs its permanent-404 HEAD; it
	// must fail without transferring and without disturbing the published
	// file.
	var second flatModeOutcome
	select {
	case second = <-writer2:
	case <-time.After(60 * time.Second):
		t.Fatal("writer 2 did not finish after writer 1 released the destination")
	}
	if !errors.Is(second.err, ErrNotFound) {
		t.Errorf("multipart writer error = %v, want ErrNotFound", second.err)
	}
	if got := hub.gets.Load(); got != 1 {
		t.Errorf("payload GETs = %d, want exactly writer 1's one transfer", got)
	}
	assertExactFile(t, dst, full)
}
