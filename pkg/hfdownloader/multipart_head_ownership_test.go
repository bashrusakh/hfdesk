// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Cross-route artifact ownership at the MULTIPART HEAD fail-fast.
//
// downloadMultipart stages at <staging>.incomplete and creates no `.part` of
// its own, and in flat/output mode the destination's ownership domain is taken
// at the Download level only for the route that does not take it internally.
// A multipart item therefore performs its HEAD holding nothing, so a permanent
// (401/403/404) HEAD response must not touch the single-request route's
// `<dst>.part` and `<dst>.part.meta`: those artifacts belong to another route
// (possibly a LIVE writer of it) or to an earlier run. Unlinking them would
// remove a live writer's open partial, whose later publication then fails with
// ENOENT, and would let one route delete another route's resume state.
//
// The cases below drive the real Download path. The first pre-creates the pair
// and asserts it survives the fail-fast byte-identically. The second keeps a
// live single-route writer blocked INSIDE its payload GET (so its `.part` is
// still empty and its sidecar already written), fails a second, multipart
// writer of the same destination on HEAD, and then releases the first writer:
// it must publish its own bytes, which is only possible when its staging inode
// was never unlinked.

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

// newCrossRouteHeadHub starts the local server for one LFS file.
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
		// The gate is opened before the server is closed, so a still-blocked
		// handler cannot keep the server from shutting down.
		h.open()
		h.srv.Close()
	})
	return h
}

// open releases the gate. It is safe to call more than once.
func (h *crossRouteHeadHub) open() {
	h.gateOnce.Do(func() { close(h.gate) })
}

// TestFlatModeMultipartHeadFailFastPreservesSingleRoutePartial pins the
// decision rule: a multipart HEAD failure creates no `.part`, and any
// `.part`/sidecar present belongs to the single-request route or to an earlier
// run and is left byte-identically untouched.
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

	// The multipart route created no `.part`: the pair belongs to the
	// single-request route and must survive untouched.
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

// TestFlatModeMultipartHeadFailFastDoesNotUnlinkLiveSingleWriter reproduces the
// live-writer failure: a single-request writer holds the flat destination's
// domain with an empty `.part` inside a blocked payload transfer while a second
// (multipart) writer of the SAME destination fails its HEAD. The fail-fast must
// not unlink the live writer's partial or sidecar, and that writer must publish
// successfully once it is released.
func TestFlatModeMultipartHeadFailFastDoesNotUnlinkLiveSingleWriter(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	const total = 4_096
	full := rangedTestPayload(total)
	out := t.TempDir()
	dst := legacyOutputDst(t, out)
	hub := newCrossRouteHeadHub(t, full, "model.bin", true)

	// Writer 1: the single-request route (a threshold above the item size),
	// held inside its payload GET.
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

	// Precondition: writer 1 is inside the transfer - it holds the flat
	// destination's domain - and its partial is the empty file the multipart
	// fail-fast used to delete.
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

	// Writer 2: the multipart route of the SAME flat destination. Its HEAD is
	// a permanent 404, so it fails fast while writer 1 owns the destination.
	res := runFlatModeDownload(context.Background(), flatModeSettings(out, hub.srv.URL, "0"))
	if res.err == nil {
		t.Fatalf("the permanent HEAD failure did not fail the item (skips=%v done=%v)", res.skips, res.done)
	}
	if !errors.Is(res.err, ErrNotFound) {
		t.Errorf("multipart writer error = %v, want ErrNotFound", res.err)
	}
	if hub.headCalls.Load() == 0 {
		t.Fatal("no HEAD was issued: the multipart writer under test did not run")
	}

	// The live writer's artifacts were not unlinked, and are byte-identical.
	partAfter, serr := os.Stat(dst + ".part")
	if serr != nil {
		t.Fatalf("the multipart fail-fast unlinked the live single-route writer's partial: %v", serr)
	}
	if partAfter.Size() != 0 {
		t.Errorf("live partial size = %d bytes, want the untouched empty file", partAfter.Size())
	}
	sidecarAfter, serr := os.ReadFile(singlePartIdentityPath(dst))
	if serr != nil {
		t.Fatalf("the multipart fail-fast unlinked the live single-route writer's identity sidecar: %v", serr)
	}
	if !bytes.Equal(sidecarBefore, sidecarAfter) {
		t.Errorf("the live writer's identity sidecar changed:\nbefore=%s\nafter=%s", sidecarBefore, sidecarAfter)
	}

	// Release writer 1: it publishes its own bytes. Its publication renames
	// the very `.part` the fail-fast used to unlink, so this is the
	// end-to-end proof that its staging inode survived.
	hub.open()
	var published flatModeOutcome
	select {
	case published = <-writer1:
	case <-time.After(60 * time.Second):
		t.Fatal("writer 1 did not finish after the gate opened")
	}
	if published.err != nil {
		t.Fatalf("writer 1 failed after the multipart HEAD failure: %v", published.err)
	}
	assertExactFile(t, dst, full)
	if got := hub.gets.Load(); got != 1 {
		t.Errorf("payload GETs = %d, want exactly writer 1's one transfer", got)
	}
}
