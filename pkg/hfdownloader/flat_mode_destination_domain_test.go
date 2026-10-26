// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Regression coverage for the flat/output-mode DESTINATION domain.
//
// A flat/output-mode destination owns ONE ownership domain, and both routes
// and the cleanup resolve that ONE identity:
//
//   - the domain's lifetime lock is the persistent sibling of the MULTIPART
//     staging name, <destination>.incomplete.lock - the very lock the
//     multipart route's staging claim already takes;
//   - the single-request route holds that domain while it transfers.
//
// One lock file therefore excludes every pair of writers of the same flat
// destination: single-vs-single, single-vs-multipart and
// multipart-vs-multipart. Before the flat destination owned a domain, the
// single-request route took no lock at all, so two writers of the same flat
// destination interleaved over the same payload, and a cleanup could delete
// another writer's LIVE partial.
//
// The cases below drive the real Download path, so they exercise the product
// routing and not just the raw lock primitive. The synchronization is
// deterministic and never a sleep: the live owner is held INSIDE the
// transfer by a gated local server (the first payload request signals the
// test), and the production contention hooks prove that the second writer
// really met the contended lock.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// flatModeGatedHub serves one file and holds every PAYLOAD response until
// the test opens the gate.
//
// The first payload request closes entered, so the test knows that a writer
// is inside the transfer - and therefore holds the destination's ownership
// domain - without sleeping. The HEAD requests are NOT gated: the multipart
// route resolves the size with a HEAD before it claims the staging, and a
// gated HEAD would stall the very writer the test is waiting for.
//
// resolveRevision says whether the endpoint resolves the requested revision
// to an immutable commit. When it does not, the planner falls back to the
// requested revision, which pins nothing: the staging record of such a run
// is NOT resumable, so a second writer does not switch to the multipart
// route to consume it.
type flatModeGatedHub struct {
	srv        *httptest.Server
	entered    chan struct{}
	gate       chan struct{}
	enterOnce  sync.Once
	gateOnce   sync.Once
	payloads   atomic.Int64
	resolveRev bool
}

// newFlatModeGatedHub starts the gated local server for one file.
func newFlatModeGatedHub(t *testing.T, full []byte, rel string, lfs bool) *flatModeGatedHub {
	t.Helper()
	return newFlatModeGatedHubWithRevision(t, full, rel, lfs, true)
}

// newFlatModeGatedHubWithRevision starts the gated local server and decides
// whether the endpoint resolves the requested revision.
func newFlatModeGatedHubWithRevision(t *testing.T, full []byte, rel string, lfs, resolveRev bool) *flatModeGatedHub {
	t.Helper()
	h := &flatModeGatedHub{
		entered:    make(chan struct{}),
		gate:       make(chan struct{}),
		resolveRev: resolveRev,
	}
	sha := singleTestSHA(full)
	node := fmt.Sprintf(`{"type":"file","path":%q,"size":%d,"sha256":%q}`, rel, len(full), sha)
	if lfs {
		node = fmt.Sprintf(`{"type":"file","path":%q,"size":%d,"lfs":{"oid":%q,"size":%d,"sha256":%q}}`,
			rel, len(full), sha, len(full), sha)
	}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/revision/main"):
			if !h.resolveRev {
				// The endpoint does not resolve the revision: the planner
				// falls back to the requested revision, which pins nothing.
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte(`{"sha":"commit123"}`))
		case strings.Contains(r.URL.Path, "/tree/"):
			_, _ = w.Write([]byte("[" + node + "]"))
		case strings.Contains(r.URL.Path, "/resolve/") || strings.Contains(r.URL.Path, "/raw/"):
			if r.Method == http.MethodGet {
				h.payloads.Add(1)
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
func (h *flatModeGatedHub) open() {
	h.gateOnce.Do(func() { close(h.gate) })
}

// awaitPayloadEntered blocks until a writer is inside the transfer.
func (h *flatModeGatedHub) awaitPayloadEntered(t *testing.T) {
	t.Helper()
	select {
	case <-h.entered:
	case <-time.After(60 * time.Second):
		t.Fatal("no writer entered the payload transfer")
	}
}

// TestDownloadFlatModeRoutesExcludeEachOther pins the exclusion between the
// two routes of ONE flat destination.
//
// Before the flat destination owned a domain, the single-request route took
// no lock at all, so a single-route writer and a multipart-route writer of
// the same destination interleaved over the same payload and could publish
// corrupt bytes.
//
// Both directions are covered, because they fail differently: the
// single-request writer WAITS for the domain and then fails with the
// actionable live-owner error, while the multipart writer's claim finds the
// contended lock at once.
//
// The multipart-owner direction uses an endpoint that does NOT resolve the
// requested revision. A pinned, resumable staging record would make the
// second writer switch to the multipart route to consume that state (the
// pre-existing resume rule), and the case under test is the writer that
// stays on the single-request route and meets the destination's domain.
func TestDownloadFlatModeRoutesExcludeEachOther(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	t.Run("single-request owner excludes the multipart writer", func(t *testing.T) {
		testFlatModeRoutesExcludeEachOther(t, "1MB", "0", true)
	})
	t.Run("multipart owner excludes the single-request writer", func(t *testing.T) {
		testFlatModeRoutesExcludeEachOther(t, "0", "1MB", false)
	})
}

// testFlatModeRoutesExcludeEachOther is the body of the cross-route
// exclusion case: the owner runs on ownerThreshold and holds the
// destination's domain inside the transfer, the writer runs on
// writerThreshold and must be excluded.
func testFlatModeRoutesExcludeEachOther(t *testing.T, ownerThreshold, writerThreshold string, resolveRev bool) {
	t.Helper()
	const total = 4_096
	full := rangedTestPayload(total)
	sha := singleTestSHA(full)
	hub := newFlatModeGatedHubWithRevision(t, full, "model.bin", true, resolveRev)
	out := t.TempDir()
	dst := legacyOutputDst(t, out)

	// The excluded writer must not wait the production budget: the behavior
	// under test is the exclusion, not the duration.
	shortBlobOwnershipWait(t, 250*time.Millisecond)

	// Record every ownership-domain acquisition of the run. In flat mode the
	// MULTIPART route does not take the domain (its staging claim takes the
	// same lock file), so the acquisition count says which of the two
	// writers went through the domain and which one was excluded by the
	// claim.
	var (
		mu       sync.Mutex
		acquired []string
	)
	oldAcquire := blobDomainAcquire
	blobDomainAcquire = func(ctx context.Context, key, dst string) (*blobDomain, error) {
		mu.Lock()
		acquired = append(acquired, key)
		mu.Unlock()
		return oldAcquire(ctx, key, dst)
	}
	defer func() { blobDomainAcquire = oldAcquire }()

	ownerResult := make(chan flatModeOutcome, 1)
	go func() {
		ownerResult <- runFlatModeDownload(context.Background(), flatModeSettings(out, hub.srv.URL, ownerThreshold))
	}()

	// The owner is inside the transfer, so it holds the destination's domain.
	hub.awaitPayloadEntered(t)

	// The probe is installed before the writer starts: the hook is a
	// package-level variable.
	contended := installDomainContentionProbe(t)
	writerResult := make(chan flatModeOutcome, 1)
	go func() {
		writerResult <- runFlatModeDownload(context.Background(), flatModeSettings(out, hub.srv.URL, writerThreshold))
	}()

	var writer flatModeOutcome
	select {
	case <-contended:
		// The writer really met the contended lock.
	case writer = <-writerResult:
		// The writer already finished. The exclusion must be proven, not
		// inferred, so the contention is checked before the outcome is
		// accepted.
		select {
		case <-contended:
		default:
			t.Fatalf("the writer finished without observing the contended ownership lock (err=%v)", writer.err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("neither the contention nor the writer's completion was observed")
	}

	select {
	case writer = <-writerResult:
	case <-time.After(60 * time.Second):
		t.Fatal("the excluded writer did not finish")
	}

	// The item fails with the actionable live-owner error.
	if writer.err == nil {
		t.Fatalf("the excluded writer reported success although a live owner holds the destination (skips=%v, done=%v)",
			writer.skips, writer.done)
	}
	if !errors.Is(writer.err, errBlobOwnedByLiveWriter) {
		t.Errorf("the excluded writer's error = %v, want the actionable live-owner error", writer.err)
	}
	if !strings.Contains(writer.err.Error(), sha) {
		t.Errorf("the excluded writer's error = %v, want the blob key %s in the message", writer.err, sha)
	}
	if !strings.Contains(writer.err.Error(), "is being downloaded by another process") ||
		!strings.Contains(writer.err.Error(), "try again later") {
		t.Errorf("the excluded writer's error = %v, want the actionable live-owner message", writer.err)
	}
	// A live owner means the file is NOT present: a skip or a file_done
	// would report a success the output directory does not have.
	if len(writer.skips) != 0 {
		t.Errorf("skips = %v, want none: the file is not present while the live owner holds the destination", writer.skips)
	}
	if len(writer.done) != 0 {
		t.Errorf("file_done events = %v, want none: the file is not present", writer.done)
	}
	// The excluded writer transferred nothing: the destination did not exist
	// when it was excluded.
	if _, serr := os.Stat(dst); !os.IsNotExist(serr) {
		t.Errorf("the excluded writer created the destination, stat err=%v", serr)
	}

	// The owner publishes the exact bytes.
	hub.open()
	var owner flatModeOutcome
	select {
	case owner = <-ownerResult:
	case <-time.After(60 * time.Second):
		t.Fatal("the owner did not finish")
	}
	if owner.err != nil {
		t.Fatalf("the owner failed: %v", owner.err)
	}
	assertExactFile(t, dst, full)
	// Only the owner transferred the payload.
	if got := hub.payloads.Load(); got != 1 {
		t.Errorf("payload requests = %d, want exactly the owner's one: the excluded writer transferred bytes", got)
	}
	// Exactly ONE of the two writers went through the destination's domain.
	// In flat mode the multipart route does not take it - its staging claim
	// takes the same lock file - so the count names the single-request
	// writer of the pair.
	mu.Lock()
	got := append([]string(nil), acquired...)
	mu.Unlock()
	if len(got) != 1 {
		t.Errorf("ownership-domain acquisitions = %v, want exactly one: the multipart route must not take the destination's domain", got)
	}
	for _, key := range got {
		if !strings.HasPrefix(key, flatDomainKeyPrefix) {
			t.Errorf("ownership-domain key = %q, want a flat destination domain", key)
		}
	}
}

// TestDownloadFlatModeSingleRouteWritersAreExcluded pins that two
// single-request writers of ONE flat destination are mutually excluded.
//
// The second writer waits for the destination's domain instead of writing
// over the first writer's live partial. A flat destination has no published
// content-addressed target to re-check, so the waiting writer obtains the
// file itself afterwards - but never WHILE the owner holds the destination.
func TestDownloadFlatModeSingleRouteWritersAreExcluded(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	const total = 4_096
	full := rangedTestPayload(total)
	hub := newFlatModeGatedHub(t, full, "model.bin", true)
	out := t.TempDir()
	dst := legacyOutputDst(t, out)

	ownerResult := make(chan flatModeOutcome, 1)
	go func() {
		ownerResult <- runFlatModeDownload(context.Background(), flatModeSettings(out, hub.srv.URL, "1MB"))
	}()
	hub.awaitPayloadEntered(t)

	// The in-process domain contention hook proves the second writer waited
	// for the destination's domain instead of proceeding.
	contended := installBlobDomainContentionProbe(t)
	writerResult := make(chan flatModeOutcome, 1)
	go func() {
		writerResult <- runFlatModeDownload(context.Background(), flatModeSettings(out, hub.srv.URL, "1MB"))
	}()
	awaitContention(t, contended, "the flat destination's ownership domain")

	// While the owner holds the domain, the second writer transferred
	// nothing and the destination does not exist.
	if got := hub.payloads.Load(); got != 1 {
		t.Errorf("payload requests while the owner holds the domain = %d, want only the owner's one", got)
	}
	if _, serr := os.Stat(dst); !os.IsNotExist(serr) {
		t.Errorf("the destination exists while the owner still holds the domain, stat err=%v", serr)
	}

	// The owner publishes the exact bytes and releases the domain; the
	// waiting writer then obtains the domain and the file itself.
	hub.open()
	var owner flatModeOutcome
	select {
	case owner = <-ownerResult:
	case <-time.After(60 * time.Second):
		t.Fatal("the owner did not finish")
	}
	if owner.err != nil {
		t.Fatalf("the owner failed: %v", owner.err)
	}
	var writer flatModeOutcome
	select {
	case writer = <-writerResult:
	case <-time.After(60 * time.Second):
		t.Fatal("the second writer did not finish")
	}
	if writer.err != nil {
		t.Fatalf("the second writer failed: %v", writer.err)
	}
	assertExactFile(t, dst, full)
}

// TestDownloadFlatModeUnknownSizeHandoffTakesTheDestinationDomain pins that
// the multipart route's hand-off to the single-request path holds the
// destination's domain.
//
// The hand-off happens when the size stays unknown after the HEAD. The
// multipart route's claim - the OTHER holder of the destination's lock - is
// never reached there, so the hand-off must take the domain itself: without
// it the transfer would run with no ownership lock at all, and two writers of
// the same flat destination would race over the same payload.
func TestDownloadFlatModeUnknownSizeHandoffTakesTheDestinationDomain(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	const total = 4_096
	full := rangedTestPayload(total)
	sha := singleTestSHA(full)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/revision/main"):
			_, _ = w.Write([]byte(`{"sha":"commit-unknown-size"}`))
		case strings.Contains(r.URL.Path, "/tree/"):
			// Both tree Size and LFS.Size are omitted: the plan's item has
			// an unknown size.
			node := fmt.Sprintf(`{"type":"file","path":"model.bin","lfs":{"oid":%q,"sha256":%q}}`, sha, sha)
			_, _ = w.Write([]byte("[" + node + "]"))
		case strings.Contains(r.URL.Path, "/resolve/") && r.Method == http.MethodHead:
			// The HEAD advertises range support but omits Content-Length, so
			// the size stays unknown and the multipart route hands the item
			// over to the single-request path.
			w.Header().Set("Accept-Ranges", "bytes")
		case strings.Contains(r.URL.Path, "/resolve/") && r.Method == http.MethodGet:
			w.Header().Set("Content-Length", strconv.Itoa(len(full)))
			_, _ = w.Write(full)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	out := t.TempDir()
	dst := legacyOutputDst(t, out)

	var (
		mu       sync.Mutex
		acquired []string
	)
	oldAcquire := blobDomainAcquire
	blobDomainAcquire = func(ctx context.Context, key, dst string) (*blobDomain, error) {
		mu.Lock()
		acquired = append(acquired, key)
		mu.Unlock()
		return oldAcquire(ctx, key, dst)
	}
	defer func() { blobDomainAcquire = oldAcquire }()

	// The threshold selects the multipart route, and the hand-off inside it
	// is the only place the domain can be taken.
	res := runFlatModeDownload(context.Background(), flatModeSettings(out, srv.URL, "0"))
	if res.err != nil {
		t.Fatalf("Download: %v", res.err)
	}
	mu.Lock()
	got := append([]string(nil), acquired...)
	mu.Unlock()
	if len(got) != 1 || !strings.HasPrefix(got[0], flatDomainKeyPrefix) {
		t.Errorf("ownership-domain acquisitions = %v, want exactly one flat destination domain", got)
	}
	assertExactFile(t, dst, full)
}

// TestCleanupJobPartFilesFlatModeRespectsTheDestinationDomain pins that a
// cleanup of a flat destination removes its artifacts only while that
// destination's domain is held.
//
// A cleanup that removed them without the domain could delete the LIVE
// partial of another writer (a cancelled job whose destination meanwhile
// became owned by another job), and that writer would keep writing to an
// unlinked inode and fail its later publication with ENOENT.
//
// The second half pins the other side of the same contract: the job that
// owns the destination can still clean up its OWN artifacts, because its
// writer released the domain when it returned.
func TestCleanupJobPartFilesFlatModeRespectsTheDestinationDomain(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	out := t.TempDir()
	dst := legacyOutputDst(t, out)

	artifacts := map[string]string{
		dst + ".part":            "the live writer's single-path partial",
		dst + ".part.meta":       "the live writer's identity sidecar",
		dst + ".incomplete":      "the live writer's ranged staging",
		dst + ".incomplete.meta": `{"lock_fenced":true}`,
	}
	for path, body := range artifacts {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// A live writer holds the destination's domain: the persistent sibling
	// lock of the multipart staging name.
	owner, err := acquireStagingOwnershipLock(flatDomainLockSrc(dst))
	if err != nil {
		t.Fatal(err)
	}

	if err := CleanupJobPartFiles(Settings{OutputDir: out}, []string{dst}); err != nil {
		t.Fatalf("CleanupJobPartFiles: %v", err)
	}
	for path, body := range artifacts {
		got, rerr := os.ReadFile(path)
		if rerr != nil {
			t.Fatalf("the live owner's artifact %s disappeared: %v", filepath.Base(path), rerr)
		}
		if string(got) != body {
			t.Errorf("the live owner's artifact %s was rewritten: %q", filepath.Base(path), got)
		}
	}

	// The owner released the destination: the job's own artifacts are
	// removed.
	owner.release()
	if err := CleanupJobPartFiles(Settings{OutputDir: out}, []string{dst}); err != nil {
		t.Fatalf("CleanupJobPartFiles: %v", err)
	}
	for path := range artifacts {
		if _, serr := os.Stat(path); !os.IsNotExist(serr) {
			t.Errorf("the destination's own artifact %s survived the cleanup, stat err=%v", filepath.Base(path), serr)
		}
	}
}
