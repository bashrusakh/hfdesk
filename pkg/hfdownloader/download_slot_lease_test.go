// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Regression coverage for the MaxActiveDownloads slot lease (issue #140 /
// PR #151).
//
// An item that waits for another writer's ownership domain is not
// transferring, so it must NOT hold one of the job's MaxActiveDownloads
// slots: with a low limit one waiting item used to keep every other file of
// the job from starting, so the job looked stalled while its concurrency
// budget was spent on a wait. The slot is still required for a TRANSFER: a
// contended item takes its slot back before the acquisition returns a domain
// it will transfer under.
//
// The cases drive the real Download path, so they cover the three item-path
// acquisitions (the cache destination's domain, the flat destination's domain
// and the flat-mode hand-off inside downloadMultipart) and the two waits the
// domain acquisition can perform (the in-process slot and the cross-process
// lifetime OS lock). The synchronization is deterministic: the test holds the
// destination's domain itself, and the payload/`file_done` signals of the
// OTHER file prove that the job proceeded. No sleep decides the outcome.

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

// slotLeaseFile is one file of the slot-lease test hub.
type slotLeaseFile struct {
	rel  string
	body []byte
	// omitSize makes the tree and the HEAD response report no size, which
	// routes a multipart item through the unknown-size hand-off to the
	// single-request path.
	omitSize bool
}

// slotLeaseHub serves a small repository and records the per-file payload
// requests. It can hold the FIRST payload response open, which is what the
// concurrency case needs: while that response is held, a second transfer may
// only start if the MaxActiveDownloads limiter let it.
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

// newSlotLeaseHub starts the two-file hub in tree order (the order of files).
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
			// No Size and no LFS.Size: the plan's item has an unknown size.
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
					// Range support is advertised but the size stays
					// unknown, so the multipart route hands the item over
					// to the single-request path.
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
			// active counts payload requests that are inside the handler.
			// The first one may be held below, so a second transfer would
			// be visible as a concurrent handler entry.
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

// release opens the first-payload gate. It is safe to call more than once.
func (h *slotLeaseHub) release() {
	h.releaseOnce.Do(func() { close(h.releaseFirst) })
}

// payloadCount returns how many payload GETs the file received.
func (h *slotLeaseHub) payloadCount(rel string) int64 {
	return h.payloads[rel].Load()
}

// slotLeaseFlatDst returns the flat/output-mode destination of one file.
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

// slotLeaseFlatSettings returns the flat-mode settings of the slot cases:
// one transfer slot, one connection, and the single-request route for the
// small payloads below.
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

// runSlotLeaseDownload runs one Download and reports every completed path on
// the done channel.
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

// awaitSlotFileDone waits for the file_done event of one path.
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

// TestDownloadSlotLeaseFlatMode covers the flat/output-mode destination
// domain: while the first item waits for the domain the test holds, the
// second file of the SAME limit-1 job must transfer and complete.
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

	// The live owner of a.bin's flat destination domain: the in-process slot
	// AND the lifetime OS lock, exactly what a second writer of that
	// destination would meet.
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

	// b.bin must transfer and complete while a.bin waits.
	awaitSlotFileDone(t, done, "b.bin")
	assertExactFile(t, dstB, payloadB)

	// a.bin must not have transferred or appeared while the test holds its
	// domain.
	if _, serr := os.Stat(dstA); !os.IsNotExist(serr) {
		t.Fatalf("a.bin exists while its destination domain is held by another writer, stat err=%v", serr)
	}
	if got := hub.payloadCount("a.bin"); got != 0 {
		t.Fatalf("a.bin issued %d payload requests while its destination domain was held, want 0", got)
	}

	// The owner releases the domain: the waiting item transfers exactly once
	// and the job completes.
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

// TestDownloadSlotLeaseCrossProcessWait covers the second wait of the domain
// acquisition: the in-process slot is free, so the item passes it and waits
// for the lifetime OS lock held by the test. The item must release its
// MaxActiveDownloads slot for that wait too.
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

	// Only the OS lock is held (no in-process domain entry): this is the
	// cross-process branch of acquireBlobDomainWithin.
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

// TestDownloadSlotLeaseCacheMode covers the cache destination's domain: while
// the first item waits for the blob key's domain, the second item of the
// SAME limit-1 job must transfer, publish its blob and create its cache
// links.
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

// assertOwnershipContractLinksMissing checks that an item's own cache entries
// do not exist.
func assertOwnershipContractLinksMissing(t *testing.T, cacheDir, rel string) {
	t.Helper()
	repoDir := filepath.Join(cacheDir, "hub", "models--owner--model")
	snap := filepath.Join(repoDir, "snapshots", "commit123", filepath.FromSlash(rel))
	if _, err := os.Lstat(snap); !os.IsNotExist(err) {
		t.Errorf("the snapshot entry %s exists while the item waited, err=%v", snap, err)
	}
}

// TestDownloadSlotLeaseFlatModeUnknownSizeHandoff covers the THIRD item-path
// acquisition: the flat-mode hand-off inside downloadMultipart, where an item
// whose size stays unknown after HEAD takes the destination's domain before it
// hands over to the single-request path. The wait must yield the slot there
// too, so the other file of the limit-1 job still transfers.
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
	// A threshold of 0 selects the multipart route, which is where the
	// unknown-size hand-off lives.
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

// TestDownloadSlotLeaseCancellation covers cancellation of a waiting item:
// Download returns promptly, the waiting item transfers nothing, and the
// destination can be downloaded normally afterwards (the lease did not leave
// the item stuck or the slot poisoned).
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

			// Install the contention probe BEFORE the item starts: it proves
			// the item really was waiting for the domain when the test
			// cancels.
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

			// The destination is free afterwards: an ordinary download with
			// the same settings still completes, so the waiting item did not
			// leave the slot/lease state broken.
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

// TestDownloadSlotLeaseBoundsConcurrentTransfers is the regression guard: the
// lease must still bound concurrent TRANSFERS. With MaxActiveDownloads=1 the
// first payload response is held open, so a second transfer could only start
// if the limiter let it.
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

			// The first transfer is inside the handler. With one slot no
			// second payload request may start while it is held.
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

// slotNoticeRecorder records the lease signals of a domain acquisition.
type slotNoticeRecorder struct {
	mu     sync.Mutex
	begins int
	ends   int
}

func (r *slotNoticeRecorder) beginWait() {
	r.mu.Lock()
	r.begins++
	r.mu.Unlock()
}

func (r *slotNoticeRecorder) endWait() bool {
	r.mu.Lock()
	r.ends++
	r.mu.Unlock()
	return true
}

// TestDownloadSlotLeaseFailedDomainAcquisitionDoesNotRetakeTheSlot pins the
// failure path of the lease: when the acquisition gives up (the ownership
// budget expired while a proven live owner still holds the lock), no domain
// reaches the caller and the item is about to report the actionable error, so
// the slot must stay returned. Re-taking it would make the failure wait for
// some other transfer to free a slot before the error could be reported.
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
