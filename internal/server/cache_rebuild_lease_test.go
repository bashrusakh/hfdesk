// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
)

// TestCacheRebuildFailsFastBehindRunningDownload pins the wait of the cache
// rebuild: a running download holds the cache mutation lease for its whole
// transfer, so the rebuild must end its wait with a clear error instead of
// staying blocked behind that transfer.
func TestCacheRebuildFailsFastBehindRunningDownload(t *testing.T) {
	previous := cacheRebuildLeaseBudget
	cacheRebuildLeaseBudget = 250 * time.Millisecond
	defer func() { cacheRebuildLeaseBudget = previous }()

	storage := t.TempDir()
	s := newTestServerWithConfig(t, Config{CacheDir: storage})

	// The ordinary rebuild, where nothing else holds the lease, is
	// unaffected.
	if w := cacheRequest(t, s, "POST", "/api/cache/rebuild", `{}`); w.Code != http.StatusOK {
		t.Fatalf("ordinary rebuild status=%d body=%s", w.Code, w.Body.String())
	}

	// A running transfer holds the cache mutation lease.
	release, err := hfdownloader.NewHFCache(storage, 0).AcquireRebuildCoordinationLease(true)
	if err != nil {
		t.Fatalf("hold the cache mutation lease: %v", err)
	}
	defer release()

	started := time.Now()
	responses := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		responses <- cacheRequest(t, s, "POST", "/api/cache/rebuild", `{}`)
	}()
	var w *httptest.ResponseRecorder
	select {
	case w = <-responses:
	case <-time.After(15 * time.Second):
		t.Fatal("the rebuild request stayed blocked behind the running download")
	}
	elapsed := time.Since(started)
	if w.Code != http.StatusConflict {
		t.Fatalf("rebuild behind a running download status=%d body=%s; want conflict", w.Code, w.Body.String())
	}
	if elapsed > 10*time.Second {
		t.Fatalf("rebuild waited %s behind a running download; want a prompt failure", elapsed)
	}
}

// TestCacheDeleteFailsFastBehindRunningDownload pins the wait of the cache
// deletion: a running download holds the cache mutation lease for its whole
// transfer, so the deletion must end its wait with the same clear conflict
// answer the rebuild gives instead of staying blocked behind that transfer.
func TestCacheDeleteFailsFastBehindRunningDownload(t *testing.T) {
	previous := cacheDeleteLeaseBudget
	cacheDeleteLeaseBudget = 250 * time.Millisecond
	defer func() { cacheDeleteLeaseBudget = previous }()

	storage := t.TempDir()
	s := newTestServerWithConfig(t, Config{CacheDir: storage})
	cache := hfdownloader.NewHFCache(storage, 0)
	repo, err := cache.Repo("owner/model", hfdownloader.RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	// The ordinary deletion, where nothing else holds the lease, is
	// unaffected.
	if w := cacheRequest(t, s, "DELETE", "/api/cache/owner/model", ""); w.Code != http.StatusOK {
		t.Fatalf("ordinary delete status=%d body=%s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(repo.Path()); !os.IsNotExist(err) {
		t.Fatalf("ordinary delete kept the repository: %v", err)
	}
	// The contended case needs the repository again.
	if err := repo.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	// A running transfer holds the cache mutation lease.
	release, err := cache.AcquireRebuildCoordinationLease(true)
	if err != nil {
		t.Fatalf("hold the cache mutation lease: %v", err)
	}
	defer release()

	started := time.Now()
	responses := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		responses <- cacheRequest(t, s, "DELETE", "/api/cache/owner/model", "")
	}()
	var w *httptest.ResponseRecorder
	select {
	case w = <-responses:
	case <-time.After(15 * time.Second):
		t.Fatal("the delete request stayed blocked behind the running download")
	}
	elapsed := time.Since(started)
	if w.Code != http.StatusConflict {
		t.Fatalf("delete behind a running download status=%d body=%s; want conflict", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Cache is busy") {
		t.Fatalf("delete behind a running download body=%s; want the busy answer", w.Body.String())
	}
	if elapsed > 10*time.Second {
		t.Fatalf("delete waited %s behind a running download; want a prompt failure", elapsed)
	}
	// The refused deletion changed nothing.
	if _, err := os.Stat(repo.Path()); err != nil {
		t.Fatalf("the refused deletion removed the repository: %v", err)
	}
}
