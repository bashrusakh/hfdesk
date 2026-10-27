// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Exercises same-destination exclusion across single/multipart writers and
// cleanup through Download. A gated local server and contention hook establish
// overlap deterministically.

import (
	"bytes"
	"context"
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

// flatModeGatedHub holds payload GETs and signals transfer start. HEAD is left
// ungated because multipart routing needs it to resolve size. resolveRev
// selects whether fixtures use a pinned commit.
type flatModeGatedHub struct {
	srv        *httptest.Server
	entered    chan struct{}
	gate       chan struct{}
	enterOnce  sync.Once
	gateOnce   sync.Once
	payloads   atomic.Int64
	resolveRev bool
}

func newFlatModeGatedHub(t *testing.T, full []byte, rel string, lfs bool) *flatModeGatedHub {
	t.Helper()
	return newFlatModeGatedHubWithRevision(t, full, rel, lfs, true)
}

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
		// Unblock handlers before closing the test server.
		h.open()
		h.srv.Close()
	})
	return h
}

func (h *flatModeGatedHub) open() {
	h.gateOnce.Do(func() { close(h.gate) })
}

func (h *flatModeGatedHub) awaitPayloadEntered(t *testing.T) {
	t.Helper()
	select {
	case <-h.entered:
	case <-time.After(60 * time.Second):
		t.Fatal("no writer entered the payload transfer")
	}
}

// Tests both single/multipart contention directions. The multipart-owner case
// uses an unpinned revision so resume routing does not change the route under test.
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

func testFlatModeRoutesExcludeEachOther(t *testing.T, ownerThreshold, writerThreshold string, resolveRev bool) {
	t.Helper()
	const total = 4_096
	full := rangedTestPayload(total)
	hub := newFlatModeGatedHubWithRevision(t, full, "model.bin", true, resolveRev)
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

	ownerResult := make(chan flatModeOutcome, 1)
	go func() {
		ownerResult <- runFlatModeDownload(context.Background(), flatModeSettings(out, hub.srv.URL, ownerThreshold))
	}()

	hub.awaitPayloadEntered(t)

	contended := installBlobDomainContentionProbe(t)
	writerResult := make(chan flatModeOutcome, 1)
	go func() {
		writerResult <- runFlatModeDownload(context.Background(), flatModeSettings(out, hub.srv.URL, writerThreshold))
	}()
	awaitContention(t, contended, "the destination's ownership domain")
	select {
	case writer := <-writerResult:
		t.Fatalf("the second writer finished while the owner still held the destination (err=%v)", writer.err)
	default:
	}
	if got := hub.payloads.Load(); got != 1 {
		t.Errorf("payload requests while the owner holds the domain = %d, want only the owner's one", got)
	}
	if _, serr := os.Stat(dst); !os.IsNotExist(serr) {
		t.Errorf("the destination exists while the owner still holds the domain, stat err=%v", serr)
	}

	// Flat mode has no blob target to re-check after releasing the owner.
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

	var writer flatModeOutcome
	select {
	case writer = <-writerResult:
	case <-time.After(60 * time.Second):
		t.Fatal("the waiting writer did not finish after the owner released the destination")
	}
	if writer.err != nil {
		t.Fatalf("the waiting writer failed: %v", writer.err)
	}
	assertExactFile(t, dst, full)

	mu.Lock()
	got := append([]string(nil), acquired...)
	mu.Unlock()
	if len(got) != 2 {
		t.Errorf("ownership-domain acquisitions = %v, want both writers to use the one destination domain", got)
	}
	for _, key := range got {
		if !strings.HasPrefix(key, flatDomainKeyPrefix) {
			t.Errorf("ownership-domain key = %q, want a flat destination domain", key)
		}
	}
}

// Single-request writers of one flat destination serialize; the waiter
// transfers only after the first owner releases.
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

	// Observe actual domain contention before checking the waiting writer.
	contended := installBlobDomainContentionProbe(t)
	writerResult := make(chan flatModeOutcome, 1)
	go func() {
		writerResult <- runFlatModeDownload(context.Background(), flatModeSettings(out, hub.srv.URL, "1MB"))
	}()
	awaitContention(t, contended, "the flat destination's ownership domain")

	if got := hub.payloads.Load(); got != 1 {
		t.Errorf("payload requests while the owner holds the domain = %d, want only the owner's one", got)
	}
	if _, serr := os.Stat(dst); !os.IsNotExist(serr) {
		t.Errorf("the destination exists while the owner still holds the domain, stat err=%v", serr)
	}

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

// Unknown-size handoff retains the domain acquired before HEAD; reacquiring it
// would self-contend, while dropping it would transfer unowned.
func TestDownloadFlatModeUnknownSizeHandoffUsesOneDomain(t *testing.T) {
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
			node := fmt.Sprintf(`{"type":"file","path":"model.bin","lfs":{"oid":%q,"sha256":%q}}`, sha, sha)
			_, _ = w.Write([]byte("[" + node + "]"))
		case strings.Contains(r.URL.Path, "/resolve/") && r.Method == http.MethodHead:
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

// Cleanup preserves a live flat-destination owner's partial and removes it
// once that domain is free.
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
