// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"
	"net/http/httptest"
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
