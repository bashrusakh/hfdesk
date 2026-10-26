// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// The fail-fast branch of the single-request path must not destroy the
// resume identity (issue #140 follow-up).
//
// A permanent 401/403/404 fails the item immediately. The branch removes the
// zero-byte partial the attempt created - and the identity sidecar that
// describes the partial. Removing the sidecar while a NON-EMPTY partial
// survives destroys the only proof that the bytes on disk belong to this
// object: the next run cannot inherit them and discards them, so a permanent
// (but eventually fixable) error loses the resumable progress.
//
// The regression below pins the rule: the sidecar goes only with the partial
// it describes, and a later run with the same identity resumes from the
// stored offset.

import (
	"bytes"
	"context"
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

// TestSinglePathFailFastKeepsInheritedPartialAndIdentity pins the fail-fast
// rule of the single-request path.
//
// An inherited partial (staged by an earlier run, with the identity sidecar
// that proves it belongs to this object) survives a permanent 403 byte for
// byte, and so does its sidecar. A later run with the same identity resumes
// from the stored offset instead of re-downloading the whole object.
func TestSinglePathFailFastKeepsInheritedPartialAndIdentity(t *testing.T) {
	body := []byte("the complete object content of the item")
	const prefixLen = 7
	prefix := body[:prefixLen]

	dir := t.TempDir()
	dst := filepath.Join(dir, "model.bin")
	job := Job{Repo: "o/r"}
	it := PlanItem{RelativePath: "model.bin", Size: int64(len(body))}
	opts := singleTestIdentity

	// The partial and its sidecar are staged by an earlier run.
	seedSinglePartial(t, dst, prefix, job, it, opts)
	sidecarBefore, err := os.ReadFile(singlePartIdentityPath(dst))
	if err != nil {
		t.Fatalf("read the staged sidecar: %v", err)
	}

	var (
		mu        sync.Mutex
		ranges    []string
		forbidden atomic.Bool
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ranges = append(ranges, r.Header.Get("Range"))
		mu.Unlock()
		if forbidden.Load() {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
	}))
	t.Cleanup(srv.Close)
	it.URL = srv.URL + "/model.bin"

	cfg := Settings{Retries: 0, StallTimeout: "0"}

	// The permanent error fails the item with the actionable message.
	forbidden.Store(true)
	_, err = downloadSingle(context.Background(), srv.Client(), "", job, cfg, it, dst, opts, func(ProgressEvent) {})
	if err == nil {
		t.Fatal("the permanent 403 did not fail the item")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("item error = %v, want the actionable permanent-status message", err)
	}

	// The partial survives byte for byte.
	got, rerr := os.ReadFile(dst + ".part")
	if rerr != nil {
		t.Fatalf("the fail-fast error removed the staged partial: %v", rerr)
	}
	if !bytes.Equal(got, prefix) {
		t.Errorf("staged partial = %q, want the inherited prefix %q", got, prefix)
	}

	// The identity sidecar survives byte for byte: it is the only proof that
	// the surviving bytes belong to this object.
	sidecarAfter, rerr := os.ReadFile(singlePartIdentityPath(dst))
	if rerr != nil {
		t.Fatalf("the fail-fast error removed the identity sidecar: %v", rerr)
	}
	if !bytes.Equal(sidecarAfter, sidecarBefore) {
		t.Errorf("identity sidecar = %q, want the staged record %q", sidecarAfter, sidecarBefore)
	}

	// A later run with the same identity resumes from the stored offset.
	forbidden.Store(false)
	mu.Lock()
	ranges = nil
	mu.Unlock()
	if _, err := downloadSingle(context.Background(), srv.Client(), "", job, cfg, it, dst, opts, func(ProgressEvent) {}); err != nil {
		t.Fatalf("the resumed download failed: %v", err)
	}
	mu.Lock()
	gotRanges := append([]string(nil), ranges...)
	mu.Unlock()
	if len(gotRanges) != 1 {
		t.Fatalf("requests = %v, want exactly one", gotRanges)
	}
	want := fmt.Sprintf("bytes=%d-%d", prefixLen, len(body)-1)
	if gotRanges[0] != want {
		t.Errorf("resume Range = %q, want %q", gotRanges[0], want)
	}
	assertExactFile(t, dst, body)
}

// TestSinglePathFailFastRemovesTheEmptyPartialAndItsSidecar is the other
// side of the same rule: a partial that is EMPTY carries no resumable
// progress, so it and its sidecar are removed together and no identity record
// outlives the partial it describes.
func TestSinglePathFailFastRemovesTheEmptyPartialAndItsSidecar(t *testing.T) {
	body := []byte("the complete object content of the item")

	dir := t.TempDir()
	dst := filepath.Join(dir, "model.bin")
	job := Job{Repo: "o/r"}
	it := PlanItem{RelativePath: "model.bin", Size: int64(len(body))}
	opts := singleTestIdentity

	// An empty partial with the sidecar that describes it.
	seedSinglePartial(t, dst, nil, job, it, opts)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	it.URL = srv.URL + "/model.bin"

	cfg := Settings{Retries: 0, StallTimeout: "0"}
	if _, err := downloadSingle(context.Background(), srv.Client(), "", job, cfg, it, dst, opts, func(ProgressEvent) {}); err == nil {
		t.Fatal("the permanent 403 did not fail the item")
	}
	if _, serr := os.Stat(dst + ".part"); !os.IsNotExist(serr) {
		t.Errorf("the empty partial survived, stat err=%v", serr)
	}
	if _, serr := os.Stat(singlePartIdentityPath(dst)); !os.IsNotExist(serr) {
		t.Errorf("the identity sidecar outlived its partial, stat err=%v", serr)
	}
}
