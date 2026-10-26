// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Cross-route migration identity (issue #140 follow-up).
//
// A leftover single-request `<dst>.part` can be imported into the multipart
// staging layout only as the object it belongs to. A `.part` WITH an identity
// sidecar is the current-version single-request staging name: its bytes are
// inherited only when the recorded identity is the current plan's (repo,
// commit, path, size, PINNED on both sides). A changed revision under the same
// staging name and the same size must not contribute its prefix to the new
// object's suffix - for a digest-less item no content proof can catch that
// mixture. A `.part` WITHOUT a sidecar keeps the historical pre-#140 prefix
// migration.
//
// Clean framing crosses the route boundary too: when the matching sidecar does
// not record a cleanly-framed completion, a fully-sized `.part` is not a
// completed layout. The migration reserves its final byte, so the ranged
// worker re-requests [size-1, size) exactly like a single-path restart.

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
	"testing"
	"time"
)

const (
	crossRouteCommitA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	crossRouteCommitB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// crossRoutePlanHub serves a plan that resolves to crossRouteCommitB for one
// digest-less LFS file, model.bin, and serves payload for it. The LFS record
// intentionally carries no digest: the plan permits an empty SHA-256, which is
// exactly the class where no content proof can catch a cross-revision mixture.
func crossRoutePlanHub(t *testing.T, payload []byte) (*httptest.Server, func() []string) {
	t.Helper()
	n := len(payload)
	node := fmt.Sprintf(`{"type":"file","path":"model.bin","size":%d,"lfs":{"size":%d}}`, n, n)
	var (
		mu     sync.Mutex
		ranges []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/models/o/r/revision/main":
			_, _ = w.Write([]byte(`{"sha":"` + crossRouteCommitB + `"}`))
		case strings.HasPrefix(r.URL.Path, "/api/models/o/r/tree/"):
			_, _ = w.Write([]byte("[" + node + "]"))
		case r.URL.Path == "/o/r/resolve/"+crossRouteCommitB+"/model.bin":
			if r.Method == http.MethodGet {
				mu.Lock()
				ranges = append(ranges, r.Header.Get("Range"))
				mu.Unlock()
			}
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(payload))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), ranges...)
	}
}

// crossRoutePayloads returns two same-sized payloads whose first half differs,
// so a migrated prefix plus the other object's suffix is a visible mixture.
func crossRoutePayloads(size int) (a, b []byte) {
	b = rangedTestPayload(size)
	a = append([]byte(nil), b...)
	for i := 0; i < size/2; i++ {
		a[i] ^= 0xFF
	}
	return a, b
}

func crossRouteRun(t *testing.T, out string, srv *httptest.Server) {
	t.Helper()
	cfg := Settings{
		OutputDir:          out,
		Endpoint:           srv.URL,
		Concurrency:        1,
		MaxActiveDownloads: 1,
		MultipartThreshold: "0",
		Verify:             "size",
		Retries:            0,
		StallTimeout:       "0",
	}
	if err := Download(context.Background(), Job{Repo: "o/r", Revision: "main"}, cfg, func(ProgressEvent) {}); err != nil {
		t.Fatalf("Download: %v", err)
	}
}

// TestCrossRouteMigrationRejectsChangedRevision is the primary regression: the
// multipart route must not import a current-version `.part` staged for a
// different resolved commit, even when the size and path match and the item has
// no digest. Without the identity check the published file is A's prefix plus
// B's suffix.
func TestCrossRouteMigrationRejectsChangedRevision(t *testing.T) {
	const size = 64
	const half = size / 2
	aPayload, bPayload := crossRoutePayloads(size)
	out := t.TempDir()
	dst := legacyOutputDst(t, out)

	// A same-sized partial from revision A, with its matching pinned sidecar
	// (commit A). The plan under test resolves to commit B.
	seedSinglePartial(t, dst, aPayload[:half], Job{Repo: "o/r"},
		PlanItem{RelativePath: "model.bin", Size: size},
		singleOpts{Commit: crossRouteCommitA, CommitPinned: true})
	if _, err := os.Stat(singlePartIdentityPath(dst)); err != nil {
		t.Fatalf("fixture precondition: missing sidecar: %v", err)
	}

	srv, ranges := crossRoutePlanHub(t, bPayload)
	crossRouteRun(t, out, srv)

	assertExactFile(t, dst, bPayload)
	if bytes.Equal(bPayload, aPayload) {
		t.Fatal("fixture payloads must differ")
	}
	published, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read published file: %v", err)
	}
	if bytes.HasPrefix(published, aPayload[:half]) {
		t.Errorf("published file carries revision A's migrated prefix: %q", published)
	}
	// A mismatched identity starts fresh: the single range request covers the
	// whole file from byte 0, not the missing suffix of an imported prefix.
	if got := ranges(); len(got) != 1 || got[0] != fmt.Sprintf("bytes=0-%d", size-1) {
		t.Errorf("payload GET ranges = %v, want one whole-file request (no migration)", got)
	}
	// A successful publish treats the stale single-path artifacts as obsolete.
	for _, p := range []string{dst + ".part", singlePartIdentityPath(dst)} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s survived a successful publish (stat err=%v)", filepath.Base(p), err)
		}
	}
}

// TestCrossRouteMigrationKeepsMatchingIdentity is the positive control: the
// same path at the SAME resolved commit still migrates its prefix, so a fix
// that refused every sidecar-bearing `.part` would fail here (the whole file
// would be re-requested) and the published bytes are exact.
func TestCrossRouteMigrationKeepsMatchingIdentity(t *testing.T) {
	const size = 64
	const half = size / 2
	_, bPayload := crossRoutePayloads(size)
	out := t.TempDir()
	dst := legacyOutputDst(t, out)

	seedSinglePartial(t, dst, bPayload[:half], Job{Repo: "o/r"},
		PlanItem{RelativePath: "model.bin", Size: size},
		singleOpts{Commit: crossRouteCommitB, CommitPinned: true})

	srv, ranges := crossRoutePlanHub(t, bPayload)
	crossRouteRun(t, out, srv)

	assertExactFile(t, dst, bPayload)
	want := fmt.Sprintf("bytes=%d-%d", half, size-1)
	if got := ranges(); len(got) != 1 || got[0] != want {
		t.Errorf("payload GET ranges = %v, want only the suffix %q (matching prefix reused)", got, want)
	}
}

// TestCrossRouteMigrationOfUnframedCompletePartialRevalidatesFinalByte
// crosses the clean-framing rule with the route boundary: a matching sidecar
// without FramingComplete does not make a fully-sized `.part` a completed
// layout. Its final (torn) byte is reserved and re-requested, so the corrupt
// last byte is replaced by the served one instead of being published.
func TestCrossRouteMigrationOfUnframedCompletePartialRevalidatesFinalByte(t *testing.T) {
	const size = 64
	_, bPayload := crossRoutePayloads(size)
	torn := append([]byte(nil), bPayload...)
	torn[size-1] ^= 0xFF

	out := t.TempDir()
	dst := legacyOutputDst(t, out)
	seedSinglePartial(t, dst, torn, Job{Repo: "o/r"},
		PlanItem{RelativePath: "model.bin", Size: size},
		singleOpts{Commit: crossRouteCommitB, CommitPinned: true})

	srv, ranges := crossRoutePlanHub(t, bPayload)
	crossRouteRun(t, out, srv)

	assertExactFile(t, dst, bPayload)
	want := fmt.Sprintf("bytes=%d-%d", size-1, size-1)
	if got := ranges(); len(got) != 1 || got[0] != want {
		t.Errorf("payload GET ranges = %v, want the revalidated final byte %q", got, want)
	}
}

// migrationOpenStaging returns the freshly preallocated staging file the
// multipart caller has open when it invokes the migration.
func migrationOpenStaging(t *testing.T, dir string, size int64) (*os.File, string) {
	t.Helper()
	staging := filepath.Join(dir, "model.bin.incomplete")
	out, err := os.OpenFile(staging, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = out.Close() })
	if err := out.Truncate(size); err != nil {
		t.Fatal(err)
	}
	return out, staging
}

// TestCrossRouteMigrationPreservesMismatchedPartial pins the decision-time
// half of the identity rule: a mismatched sidecar-bearing `.part` is neither
// imported nor deleted; only the later successful publish treats it as
// obsolete.
func TestCrossRouteMigrationPreservesMismatchedPartial(t *testing.T) {
	const size = 64
	const prefix = 20
	aPayload, _ := crossRoutePayloads(size)
	dir := t.TempDir()
	dst := filepath.Join(dir, "model.bin")
	job := Job{Repo: "o/r"}
	it := PlanItem{RelativePath: "model.bin", Size: size}
	if err := os.WriteFile(dst+".part", aPayload[:prefix], 0o644); err != nil {
		t.Fatal(err)
	}
	seedSingleIdentity(t, dst, job, it, singleOpts{Commit: crossRouteCommitA, CommitPinned: true})

	out, _ := migrationOpenStaging(t, dir, size)
	ranges, ok, err := migrateLegacySinglePart(context.Background(), dst, out, size, 1,
		it, job, crossRouteCommitB, true)
	if !errors.Is(err, errNoLegacyState) || ok || ranges != nil {
		t.Fatalf("migrateLegacySinglePart = (%v, %v, %v), want errNoLegacyState with no migration", ranges, ok, err)
	}
	part, err := os.ReadFile(dst + ".part")
	if err != nil {
		t.Fatalf("mismatched partial was removed: %v", err)
	}
	if !bytes.Equal(part, aPayload[:prefix]) {
		t.Errorf("mismatched partial bytes changed: %q", part)
	}
	record, err := readSinglePartIdentity(singlePartIdentityPath(dst))
	if err != nil || record == nil || record.Commit != crossRouteCommitA {
		t.Fatalf("mismatched sidecar was not preserved: record=%+v err=%v", record, err)
	}
}

// TestCrossRouteMigrationRefusesUnreadableSidecar covers the mandated
// non-not-exist read failure: unprovable ownership imports nothing and
// preserves the partial.
func TestCrossRouteMigrationRefusesUnreadableSidecar(t *testing.T) {
	const size = 64
	aPayload, _ := crossRoutePayloads(size)
	dir := t.TempDir()
	dst := filepath.Join(dir, "model.bin")
	job := Job{Repo: "o/r"}
	it := PlanItem{RelativePath: "model.bin", Size: size}
	if err := os.WriteFile(dst+".part", aPayload[:10], 0o644); err != nil {
		t.Fatal(err)
	}
	// A directory at the sidecar path makes os.ReadFile fail with a real
	// error rather than not-exist.
	if err := os.MkdirAll(singlePartIdentityPath(dst), 0o755); err != nil {
		t.Fatal(err)
	}

	out, _ := migrationOpenStaging(t, dir, size)
	ranges, ok, err := migrateLegacySinglePart(context.Background(), dst, out, size, 1,
		it, job, crossRouteCommitB, true)
	if !errors.Is(err, errNoLegacyState) || ok || ranges != nil {
		t.Fatalf("migrateLegacySinglePart = (%v, %v, %v), want errNoLegacyState with no migration", ranges, ok, err)
	}
	if _, err := os.Stat(dst + ".part"); err != nil {
		t.Errorf("partial was removed after an unreadable sidecar: %v", err)
	}
	if fi, err := os.Stat(singlePartIdentityPath(dst)); err != nil || !fi.IsDir() {
		t.Errorf("sidecar path was not preserved as an unreadable artifact: fi=%v err=%v", fi, err)
	}
}

// TestCrossRouteMigrationKeepsSidecarlessLegacyPartial pins the other half of
// the distinction: a `.part` WITHOUT a sidecar is the historical pre-#140
// shape and still migrates as a sequential prefix.
func TestCrossRouteMigrationKeepsSidecarlessLegacyPartial(t *testing.T) {
	const size = 64
	const prefix = 20
	_, bPayload := crossRoutePayloads(size)
	dir := t.TempDir()
	dst := filepath.Join(dir, "model.bin")
	job := Job{Repo: "o/r"}
	it := PlanItem{RelativePath: "model.bin", Size: size}
	if err := os.WriteFile(dst+".part", bPayload[:prefix], 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(singlePartIdentityPath(dst)); !os.IsNotExist(err) {
		t.Fatalf("fixture precondition: sidecar must be absent (stat err=%v)", err)
	}

	out, staging := migrationOpenStaging(t, dir, size)
	ranges, ok, err := migrateLegacySinglePart(context.Background(), dst, out, size, 1,
		it, job, crossRouteCommitB, true)
	if err != nil || !ok {
		t.Fatalf("migrateLegacySinglePart = (%v, %v, %v), want a legacy migration", ranges, ok, err)
	}
	if got := committedTotal(ranges); got != prefix {
		t.Errorf("migrated committed bytes = %d, want %d", got, prefix)
	}
	staged, err := os.ReadFile(staging)
	if err != nil {
		t.Fatalf("read staging: %v", err)
	}
	if !bytes.Equal(staged[:prefix], bPayload[:prefix]) {
		t.Errorf("migrated prefix = %q, want %q", staged[:prefix], bPayload[:prefix])
	}
	// The caller removes the legacy source only after the migrated layout is
	// durable; the migration itself preserves it.
	if _, err := os.Stat(dst + ".part"); err != nil {
		t.Errorf("legacy partial was removed by the migration: %v", err)
	}
}
