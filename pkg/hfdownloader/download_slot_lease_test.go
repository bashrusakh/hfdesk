// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Ownership waiters release their transfer slot and reacquire it before
// transfer, in cache and flat mode and for both in-process and OS-lock waits.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type slotLeaseFile struct {
	rel  string
	body []byte
	// omitSize exercises multipart-to-single handoff after unknown-size HEAD.
	omitSize bool
}

type slotLeaseHub struct {
	srv *httptest.Server

	payloads map[string]*atomic.Int64
	entered  map[string]chan struct{}
	enter    map[string]*sync.Once
	omitSize map[string]bool

	// gateFirst holds the first payload response until releaseFirst is
	// closed (or the request context ends).
	gateFirst    bool
	firstEntered chan struct{}
	firstOnce    sync.Once
	releaseFirst chan struct{}
	releaseOnce  sync.Once

	active    atomic.Int64
	maxActive atomic.Int64
}

func newSlotLeaseHub(t *testing.T, files []slotLeaseFile, gateFirst bool) *slotLeaseHub {
	t.Helper()
	h := &slotLeaseHub{
		payloads:     map[string]*atomic.Int64{},
		entered:      map[string]chan struct{}{},
		enter:        map[string]*sync.Once{},
		omitSize:     map[string]bool{},
		gateFirst:    gateFirst,
		firstEntered: make(chan struct{}),
		releaseFirst: make(chan struct{}),
	}
	var nodes []string
	for _, f := range files {
		h.payloads[f.rel] = &atomic.Int64{}
		h.entered[f.rel] = make(chan struct{})
		h.enter[f.rel] = &sync.Once{}
		h.omitSize[f.rel] = f.omitSize
		sha := singleTestSHA(f.body)
		if f.omitSize {
			nodes = append(nodes, fmt.Sprintf(
				`{"type":"file","path":%q,"lfs":{"oid":%q,"sha256":%q}}`,
				f.rel, sha, sha))
			continue
		}
		nodes = append(nodes, fmt.Sprintf(
			`{"type":"file","path":%q,"size":%d,"lfs":{"oid":%q,"size":%d,"sha256":%q}}`,
			f.rel, len(f.body), sha, len(f.body), sha))
	}
	tree := "[" + strings.Join(nodes, ",") + "]"
	byName := map[string][]byte{}
	for _, f := range files {
		byName[f.rel] = f.body
	}

	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/revision/main"):
			_, _ = w.Write([]byte(`{"sha":"commit123"}`))
		case strings.Contains(r.URL.Path, "/tree/"):
			_, _ = w.Write([]byte(tree))
		case strings.Contains(r.URL.Path, "/resolve/"):
			rel := filepath.Base(r.URL.Path)
			body, ok := byName[rel]
			if !ok {
				http.NotFound(w, r)
				return
			}
			if r.Method == http.MethodHead {
				if h.omitSize[rel] {
					// Keep range support while leaving size unresolved.
					w.Header().Set("Accept-Ranges", "bytes")
					return
				}
				http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
				return
			}
			if r.Method != http.MethodGet {
				http.NotFound(w, r)
				return
			}
			h.payloads[rel].Add(1)
			cur := h.active.Add(1)
			defer h.active.Add(-1)
			for {
				max := h.maxActive.Load()
				if cur <= max || h.maxActive.CompareAndSwap(max, cur) {
					break
				}
			}
			h.enter[rel].Do(func() { close(h.entered[rel]) })
			if h.gateFirst {
				h.firstOnce.Do(func() { close(h.firstEntered) })
				select {
				case <-h.releaseFirst:
				case <-r.Context().Done():
				}
			}
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(func() {
		h.release()
		h.srv.Close()
	})
	return h
}

func (h *slotLeaseHub) release() {
	h.releaseOnce.Do(func() { close(h.releaseFirst) })
}

func (h *slotLeaseHub) payloadCount(rel string) int64 {
	return h.payloads[rel].Load()
}

func slotLeaseFlatDst(t *testing.T, out, rel string) string {
	t.Helper()
	base, err := DestinationBase(out, "owner/model")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(base, filepath.FromSlash(rel))
}

func slotLeaseFlatSettings(out, endpoint string) Settings {
	return Settings{
		OutputDir:          out,
		Endpoint:           endpoint,
		Concurrency:        1,
		MaxActiveDownloads: 1,
		MultipartThreshold: "1MB",
		Verify:             "size",
		Retries:            0,
		StallTimeout:       "0",
	}
}

func runSlotLeaseDownload(ctx context.Context, cfg Settings, done chan string) <-chan error {
	result := make(chan error, 1)
	go func() {
		result <- Download(ctx, Job{Repo: "owner/model", Revision: "main"}, cfg, func(ev ProgressEvent) {
			if ev.Event != "file_done" || strings.Contains(ev.Message, "skip") {
				return
			}
			select {
			case done <- ev.Path:
			default:
			}
		})
	}()
	return result
}

func awaitSlotFileDone(t *testing.T, done <-chan string, rel string) {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		select {
		case got := <-done:
			if got == rel {
				return
			}
		case <-deadline:
			t.Fatalf("%s did not complete while the other item waited for its held ownership domain: with MaxActiveDownloads=1 the limit-1 slot was held by the waiting item", rel)
		}
	}
}

func TestDownloadSlotLeaseFlatMode(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	payloadA := bytes.Repeat([]byte("a"), 4_096)
	payloadB := bytes.Repeat([]byte("b"), 4_096)
	hub := newSlotLeaseHub(t, []slotLeaseFile{
		{rel: "a.bin", body: payloadA},
		{rel: "b.bin", body: payloadB},
	}, false)
	out := t.TempDir()
	dstA := slotLeaseFlatDst(t, out, "a.bin")
	dstB := slotLeaseFlatDst(t, out, "b.bin")

	// Hold both domain layers for a.bin.
	flat := newFlatDestinationDomain(dstA)
	owner, err := blobDomainAcquire(context.Background(), flat.key, flat.lockSrc)
	if err != nil {
		t.Fatal(err)
	}
	ownerReleased := false
	releaseOwner := func() {
		if ownerReleased {
			return
		}
		ownerReleased = true
		owner.release()
	}
	defer releaseOwner()

	done := make(chan string, 8)
	result := runSlotLeaseDownload(context.Background(), slotLeaseFlatSettings(out, hub.srv.URL), done)

	awaitSlotFileDone(t, done, "b.bin")
	assertExactFile(t, dstB, payloadB)

	if _, serr := os.Stat(dstA); !os.IsNotExist(serr) {
		t.Fatalf("a.bin exists while its destination domain is held by another writer, stat err=%v", serr)
	}
	if got := hub.payloadCount("a.bin"); got != 0 {
		t.Fatalf("a.bin issued %d payload requests while its destination domain was held, want 0", got)
	}

	releaseOwner()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Download: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("Download did not finish after the destination domain was released")
	}
	assertExactFile(t, dstA, payloadA)
	assertExactFile(t, dstB, payloadB)
	if got := hub.payloadCount("a.bin"); got != 1 {
		t.Errorf("a.bin payload requests = %d, want exactly 1", got)
	}
}

func TestDownloadSlotLeaseCrossProcessWait(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	payloadA := bytes.Repeat([]byte("a"), 4_096)
	payloadB := bytes.Repeat([]byte("b"), 4_096)
	hub := newSlotLeaseHub(t, []slotLeaseFile{
		{rel: "a.bin", body: payloadA},
		{rel: "b.bin", body: payloadB},
	}, false)
	out := t.TempDir()
	dstA := slotLeaseFlatDst(t, out, "a.bin")
	dstB := slotLeaseFlatDst(t, out, "b.bin")

	// Hold only the OS lock to exercise the foreign-process wait path.
	owner, err := acquireStagingOwnershipLock(flatDomainLockSrc(dstA))
	if err != nil {
		t.Fatal(err)
	}
	ownerReleased := false
	releaseOwner := func() {
		if ownerReleased {
			return
		}
		ownerReleased = true
		owner.release()
	}
	defer releaseOwner()

	done := make(chan string, 8)
	result := runSlotLeaseDownload(context.Background(), slotLeaseFlatSettings(out, hub.srv.URL), done)
	awaitSlotFileDone(t, done, "b.bin")
	assertExactFile(t, dstB, payloadB)
	if got := hub.payloadCount("a.bin"); got != 0 {
		t.Fatalf("a.bin issued %d payload requests while its OS ownership lock was held, want 0", got)
	}

	releaseOwner()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Download: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("Download did not finish after the OS ownership lock was released")
	}
	assertExactFile(t, dstA, payloadA)
	if got := hub.payloadCount("a.bin"); got != 1 {
		t.Errorf("a.bin payload requests = %d, want exactly 1", got)
	}
}

func TestDownloadSlotLeaseCacheMode(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	payloadA := bytes.Repeat([]byte("a"), 4_096)
	payloadB := bytes.Repeat([]byte("b"), 4_096)
	shaA := singleTestSHA(payloadA)
	shaB := singleTestSHA(payloadB)

	cacheDir := t.TempDir()
	repo := ownershipContractRepo(t, cacheDir)
	ownerDst := mustDomainDst(t, repo, shaA)
	key, keyed := cacheDstDomainKey(ownerDst)
	if !keyed {
		t.Fatalf("the cache destination %s has no ownership-domain key", ownerDst)
	}
	owner, err := blobDomainAcquire(context.Background(), key, ownerDst)
	if err != nil {
		t.Fatal(err)
	}
	ownerReleased := false
	releaseOwner := func() {
		if ownerReleased {
			return
		}
		ownerReleased = true
		owner.release()
	}
	defer releaseOwner()

	hub := newSlotLeaseHub(t, []slotLeaseFile{
		{rel: "a.bin", body: payloadA},
		{rel: "b.bin", body: payloadB},
	}, false)
	done := make(chan string, 8)
	result := runSlotLeaseDownload(context.Background(), ownershipContractSettings(cacheDir, hub.srv.URL, "1MB"), done)

	awaitSlotFileDone(t, done, "b.bin")
	assertExactFile(t, repo.BlobPath(shaB), payloadB)
	assertOwnershipContractLinks(t, cacheDir, "b.bin")
	if _, serr := os.Stat(repo.BlobPath(shaA)); !os.IsNotExist(serr) {
		t.Fatalf("blob %s exists while its ownership domain is held by another writer, stat err=%v", shaA, serr)
	}
	assertOwnershipContractLinksMissing(t, cacheDir, "a.bin")
	if got := hub.payloadCount("a.bin"); got != 0 {
		t.Fatalf("a.bin issued %d payload requests while its blob domain was held, want 0", got)
	}

	releaseOwner()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Download: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("Download did not finish after the blob ownership domain was released")
	}
	assertExactFile(t, repo.BlobPath(shaA), payloadA)
	assertOwnershipContractLinks(t, cacheDir, "a.bin")
	if got := hub.payloadCount("a.bin"); got != 1 {
		t.Errorf("a.bin payload requests = %d, want exactly 1", got)
	}
}

func assertOwnershipContractLinksMissing(t *testing.T, cacheDir, rel string) {
	t.Helper()
	repoDir := filepath.Join(cacheDir, "hub", "models--owner--model")
	snap := filepath.Join(repoDir, "snapshots", "commit123", filepath.FromSlash(rel))
	if _, err := os.Lstat(snap); !os.IsNotExist(err) {
		t.Errorf("the snapshot entry %s exists while the item waited, err=%v", snap, err)
	}
}

// An unknown-size item yields its slot during the domain wait, then keeps
// the acquired domain through the multipart-to-single handoff.
func TestDownloadSlotLeaseFlatModeUnknownSizeHandoff(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	payloadA := bytes.Repeat([]byte("a"), 4_096)
	payloadB := bytes.Repeat([]byte("b"), 4_096)
	hub := newSlotLeaseHub(t, []slotLeaseFile{
		{rel: "a.bin", body: payloadA, omitSize: true},
		{rel: "b.bin", body: payloadB},
	}, false)
	out := t.TempDir()
	dstA := slotLeaseFlatDst(t, out, "a.bin")
	dstB := slotLeaseFlatDst(t, out, "b.bin")

	flat := newFlatDestinationDomain(dstA)
	owner, err := blobDomainAcquire(context.Background(), flat.key, flat.lockSrc)
	if err != nil {
		t.Fatal(err)
	}
	ownerReleased := false
	releaseOwner := func() {
		if ownerReleased {
			return
		}
		ownerReleased = true
		owner.release()
	}
	defer releaseOwner()

	cfg := slotLeaseFlatSettings(out, hub.srv.URL)
	cfg.MultipartThreshold = "0"
	done := make(chan string, 8)
	result := runSlotLeaseDownload(context.Background(), cfg, done)

	awaitSlotFileDone(t, done, "b.bin")
	assertExactFile(t, dstB, payloadB)
	if got := hub.payloadCount("a.bin"); got != 0 {
		t.Fatalf("a.bin issued %d payload requests while its destination domain was held, want 0", got)
	}

	releaseOwner()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Download: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("Download did not finish after the destination domain was released")
	}
	assertExactFile(t, dstA, payloadA)
	if got := hub.payloadCount("a.bin"); got != 1 {
		t.Errorf("a.bin payload requests = %d, want exactly 1", got)
	}
}

func TestDownloadSlotLeaseCancellation(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	for _, tc := range []struct {
		name string
		hold func(t *testing.T, dst string) (release func())
	}{
		{
			name: "in-process domain slot",
			hold: func(t *testing.T, dst string) func() {
				flat := newFlatDestinationDomain(dst)
				dom, err := blobDomainAcquire(context.Background(), flat.key, flat.lockSrc)
				if err != nil {
					t.Fatal(err)
				}
				return dom.release
			},
		},
		{
			name: "cross-process OS lock",
			hold: func(t *testing.T, dst string) func() {
				lock, err := acquireStagingOwnershipLock(flatDomainLockSrc(dst))
				if err != nil {
					t.Fatal(err)
				}
				return lock.release
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := bytes.Repeat([]byte("c"), 4_096)
			hub := newSlotLeaseHub(t, []slotLeaseFile{{rel: "a.bin", body: payload}}, false)
			out := t.TempDir()
			dst := slotLeaseFlatDst(t, out, "a.bin")

			releaseOwner := tc.hold(t, dst)
			released := false
			release := func() {
				if released {
					return
				}
				released = true
				releaseOwner()
			}
			defer release()

			// Install the package hook before launching the item.
			contended := installBlobDomainContentionProbe(t)
			if tc.name == "cross-process OS lock" {
				contended = installDomainContentionProbe(t)
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan string, 4)
			result := runSlotLeaseDownload(ctx, slotLeaseFlatSettings(out, hub.srv.URL), done)
			awaitContention(t, contended, "the held destination domain")

			cancel()
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("Download after cancellation = %v, want a context cancellation", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("Download of a waiting item did not return promptly after cancellation")
			}
			if got := hub.payloadCount("a.bin"); got != 0 {
				t.Fatalf("the cancelled item issued %d payload requests, want 0", got)
			}
			if _, serr := os.Stat(dst); !os.IsNotExist(serr) {
				t.Fatalf("the cancelled item created the destination, stat err=%v", serr)
			}

			release()
			done2 := make(chan string, 4)
			result2 := runSlotLeaseDownload(context.Background(), slotLeaseFlatSettings(out, hub.srv.URL), done2)
			awaitSlotFileDone(t, done2, "a.bin")
			select {
			case err := <-result2:
				if err != nil {
					t.Fatalf("the ordinary download after the cancellation failed: %v", err)
				}
			case <-time.After(60 * time.Second):
				t.Fatal("the ordinary download after the cancellation did not finish")
			}
			assertExactFile(t, dst, payload)
		})
	}
}

func TestDownloadSlotLeaseBoundsConcurrentTransfers(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	for _, tc := range []struct {
		name        string
		installSeam bool
	}{
		{name: "production domain"},
		{name: "blobDomainAcquire seam returns a wait-free domain", installSeam: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payloadA := bytes.Repeat([]byte("a"), 4_096)
			payloadB := bytes.Repeat([]byte("b"), 4_096)
			hub := newSlotLeaseHub(t, []slotLeaseFile{
				{rel: "a.bin", body: payloadA},
				{rel: "b.bin", body: payloadB},
			}, true)
			out := t.TempDir()
			dstA := slotLeaseFlatDst(t, out, "a.bin")
			dstB := slotLeaseFlatDst(t, out, "b.bin")

			if tc.installSeam {
				oldAcquire := blobDomainAcquire
				blobDomainAcquire = func(context.Context, string, string) (*blobDomain, error) {
					return &blobDomain{}, nil
				}
				t.Cleanup(func() { blobDomainAcquire = oldAcquire })
			}

			done := make(chan string, 8)
			result := runSlotLeaseDownload(context.Background(), slotLeaseFlatSettings(out, hub.srv.URL), done)

			select {
			case <-hub.firstEntered:
			case <-time.After(30 * time.Second):
				t.Fatal("no payload transfer started")
			}
			select {
			case <-hub.entered["b.bin"]:
				t.Fatal("b.bin started a transfer while the first transfer was still in flight with MaxActiveDownloads=1")
			case <-time.After(500 * time.Millisecond):
				// Bounded negative observation: no second transfer started.
			}
			if got := hub.payloadCount("b.bin"); got != 0 {
				t.Fatalf("b.bin started %d payload requests while the first transfer was in flight, want 0", got)
			}

			hub.release()
			select {
			case err := <-result:
				if err != nil {
					t.Fatalf("Download: %v", err)
				}
			case <-time.After(60 * time.Second):
				t.Fatal("Download did not finish")
			}
			if got := hub.maxActive.Load(); got != 1 {
				t.Errorf("maximum concurrent payload transfers = %d, want 1", got)
			}
			assertExactFile(t, dstA, payloadA)
			assertExactFile(t, dstB, payloadB)
		})
	}
}

type slotNoticeRecorder struct {
	mu     sync.Mutex
	begins int
	ends   int
	// began closes on the first beginWait; nil disables this synchronization signal.
	began chan struct{}
}

func (r *slotNoticeRecorder) beginWait() {
	r.mu.Lock()
	r.begins++
	if r.begins == 1 && r.began != nil {
		close(r.began)
	}
	r.mu.Unlock()
}

func (r *slotNoticeRecorder) endWait() bool {
	r.mu.Lock()
	r.ends++
	r.mu.Unlock()
	return true
}

func (r *slotNoticeRecorder) counts() (begins, ends int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.begins, r.ends
}

// Failed acquisition keeps the slot returned so error reporting cannot wait
// for an unrelated transfer.
func TestDownloadSlotLeaseFailedDomainAcquisitionDoesNotRetakeTheSlot(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	dst := filepath.Join(t.TempDir(), "tmp-blob")
	owner, err := acquireStagingOwnershipLock(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.release()

	notice := &slotNoticeRecorder{}
	ctx := withDownloadSlotWaitNotice(context.Background(), notice)
	domain, err := acquireBlobDomainWithin(ctx, "failed-acquisition-key", dst, 0, 100*time.Millisecond, true)
	if !errors.Is(err, errStagingOwnershipBusy) {
		t.Fatalf("acquisition error = %v, want errStagingOwnershipBusy", err)
	}
	if domain != nil {
		t.Fatalf("acquisition returned a domain (%v) together with an error", domain)
	}
	notice.mu.Lock()
	begins, ends := notice.begins, notice.ends
	notice.mu.Unlock()
	if begins != 1 {
		t.Errorf("beginWait calls = %d, want 1: the contended acquisition must return the slot", begins)
	}
	if ends != 0 {
		t.Errorf("endWait calls = %d, want 0: a failed acquisition has nothing to transfer and must not retake the slot", ends)
	}
}

func TestDownloadSlotLeaseFreePathDoesNotReleaseOrRetake(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	dst := filepath.Join(t.TempDir(), "tmp-blob")
	key := flatDomainKey(dst)
	notice := &slotNoticeRecorder{}
	ctx := withDownloadSlotWaitNotice(context.Background(), notice)

	domain, err := acquireBlobDomainWithin(ctx, key, dst, 0, 100*time.Millisecond, true)
	if err != nil {
		t.Fatalf("uncontended domain acquisition: %v", err)
	}
	if domain == nil {
		t.Fatal("uncontended domain acquisition returned no domain")
	}
	domain.release()

	notice.mu.Lock()
	begins, ends := notice.begins, notice.ends
	notice.mu.Unlock()
	if begins != 0 {
		t.Errorf("beginWait calls = %d, want 0: the free path must not return the slot", begins)
	}
	if ends != 0 {
		t.Errorf("endWait calls = %d, want 0: the free path must not retake the slot", ends)
	}
}
