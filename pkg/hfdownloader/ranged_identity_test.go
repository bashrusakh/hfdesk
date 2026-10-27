// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Covers pinned and unpinned ranged resume identities, including the positive
// case that preserves a matching prefix and requests only its missing suffix.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// staleRangedPayload is the content an earlier run of a moved branch staged.
// It differs from the payload the server serves now, so inherited bytes are
// observable in the published file.
func staleRangedPayload(size int) []byte {
	b := make([]byte, size)
	for i := range b {
		b[i] = 0xA5
	}
	return b
}

// requestTouchesStart reports a bytes=0- request. In these prefix fixtures,
// restarting from zero requests it; resuming the committed prefix does not.
func requestTouchesStart(ranges []string) bool {
	for _, rh := range ranges {
		if strings.HasPrefix(rh, "bytes=0-") {
			return true
		}
	}
	return false
}

// writeRecordWithoutPinnedFlag removes commit_pinned to test missing-field state.
func writeRecordWithoutPinnedFlag(t *testing.T, metaPath string) {
	t.Helper()
	raw, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["commit_pinned"]; !ok {
		t.Fatalf("the record does not carry a pinned flag: %s", raw)
	}
	delete(doc, "commit_pinned")
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metaPath, out, 0o644); err != nil {
		t.Fatal(err)
	}
}

// Unpinned resume state must restart from zero. With no known digest in
// size mode, stale inherited bytes would otherwise escape content proof.
func TestRangedResumeRejectsAnUnpinnedIdentity(t *testing.T) {
	const total = 8000
	fresh := rangedTestPayload(total)
	stale := staleRangedPayload(total)

	cases := []struct {
		name string
		// record rewrites the seeded record on disk.
		record     func(t *testing.T, metaPath string)
		planPinned bool
	}{
		{
			name: "the record is not pinned",
			record: func(t *testing.T, metaPath string) {
				meta, err := readStagingMeta(metaPath)
				if err != nil {
					t.Fatal(err)
				}
				meta.CommitPinned = false
				if err := writeStagingMeta(metaPath, meta); err != nil {
					t.Fatal(err)
				}
			},
			planPinned: true,
		},
		{
			name:       "the record carries no pinned flag",
			record:     writeRecordWithoutPinnedFlag,
			planPinned: true,
		},
		{
			name:       "the current plan is not pinned",
			record:     func(t *testing.T, metaPath string) {},
			planPinned: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			dst := filepath.Join(dir, "unpinned.bin")
			staging := dst + ".incomplete"
			// Seed a committed stale prefix, distinct from the server's current bytes.
			ranges := []RangeProgress{
				{Start: 0, End: total/2 - 1, Committed: total / 2, Done: true},
				{Start: total / 2, End: total - 1},
			}
			seedRangedState(t, staging, "o/r", "unpinned.bin", total, ranges, stale)
			tc.record(t, staging+".meta")

			srv, gotRanges := rangedTestServer(t, fresh)
			it := PlanItem{RelativePath: "unpinned.bin", URL: srv.URL + "/unpinned.bin", Size: total, AcceptRanges: true}
			cfg := Settings{Concurrency: 2, Retries: 0, StallTimeout: "0", Verify: "size"}

			if _, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst,
				withTestDomain(t, dst, multipartOpts{Commit: rangedSeedCommit, CommitPinned: tc.planPinned}), func(ProgressEvent) {}); err != nil {
				t.Fatalf("downloadMultipart: %v", err)
			}

			assertExactFile(t, dst, fresh)
			if !requestTouchesStart(*gotRanges) {
				t.Errorf("the stale committed range was inherited; requests=%v", *gotRanges)
			}
		})
	}
}

// A matching pinned record must retain its prefix and fetch only the suffix.
func TestRangedResumeKeepsAPinnedMatchingIdentity(t *testing.T) {
	const total = 8000
	fresh := rangedTestPayload(total)

	dir := t.TempDir()
	dst := filepath.Join(dir, "pinned.bin")
	staging := dst + ".incomplete"
	ranges := []RangeProgress{
		{Start: 0, End: total/2 - 1, Committed: total / 2, Done: true},
		{Start: total / 2, End: total - 1},
	}
	seedRangedState(t, staging, "o/r", "pinned.bin", total, ranges, fresh)

	srv, gotRanges := rangedTestServer(t, fresh)
	it := PlanItem{RelativePath: "pinned.bin", URL: srv.URL + "/pinned.bin", Size: total, AcceptRanges: true}
	cfg := Settings{Concurrency: 2, Retries: 0, StallTimeout: "0", Verify: "size"}

	if _, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst,
		withTestDomain(t, dst, multipartOpts{Commit: rangedSeedCommit, CommitPinned: true}), func(ProgressEvent) {}); err != nil {
		t.Fatalf("downloadMultipart: %v", err)
	}

	assertExactFile(t, dst, fresh)
	want := fmt.Sprintf("bytes=%d-%d", total/2, total-1)
	if len(*gotRanges) != 1 || (*gotRanges)[0] != want {
		t.Errorf("ranged requests = %v, want exactly the missing suffix %q", *gotRanges, want)
	}
}

// Download must reject stale ranges when commit resolution falls back to a
// mutable revision. This LFS fixture has no digest to catch mixed bytes later.
func TestDownloadUnresolvedCommitDoesNotInheritStagedBytes(t *testing.T) {
	const total = 8000
	fresh := rangedTestPayload(total)
	stale := staleRangedPayload(total)

	var (
		mu        sync.Mutex
		gotRanges []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/revision/main"):
			// The revision cannot be resolved: the plan falls back to the
			// requested revision, which pins nothing.
			http.NotFound(w, r)
		case strings.Contains(r.URL.Path, "/tree/"):
			// An LFS file WITHOUT a digest: the planner permits an empty
			// SHA-256, and without one no content proof is possible.
			_, _ = w.Write([]byte(fmt.Sprintf(
				`[{"type":"file","path":"model.bin","size":%d,"lfs":{"size":%d}}]`, total, total)))
		case strings.Contains(r.URL.Path, "/resolve/"):
			if r.Method == http.MethodGet {
				mu.Lock()
				gotRanges = append(gotRanges, r.Header.Get("Range"))
				mu.Unlock()
			}
			w.Header().Set("Content-Length", strconv.Itoa(len(fresh)))
			w.Header().Set("Accept-Ranges", "bytes")
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(fresh))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	out := t.TempDir()
	dst := legacyOutputDst(t, out)
	staging := dst + ".incomplete"
	ranges := []RangeProgress{
		{Start: 0, End: total/2 - 1, Committed: total / 2, Done: true},
		{Start: total / 2, End: total - 1},
	}
	seedRangedState(t, staging, "o/r", "model.bin", total, ranges, stale)
	// The earlier run could not resolve the commit either, so its record
	// names the requested revision and is NOT pinned.
	meta, err := readStagingMeta(staging + ".meta")
	if err != nil {
		t.Fatal(err)
	}
	meta.Commit = "main"
	meta.CommitPinned = false
	if err := writeStagingMeta(staging+".meta", meta); err != nil {
		t.Fatal(err)
	}

	cfg := Settings{
		OutputDir:          out,
		Endpoint:           srv.URL,
		Concurrency:        2,
		MaxActiveDownloads: 1,
		MultipartThreshold: "1",
		Verify:             "size",
		Retries:            0,
		StallTimeout:       "0",
	}
	if err := Download(context.Background(), Job{Repo: "o/r", Revision: "main"}, cfg, func(ProgressEvent) {}); err != nil {
		t.Fatalf("Download: %v", err)
	}

	assertExactFile(t, dst, fresh)
	mu.Lock()
	ranges2 := append([]string(nil), gotRanges...)
	mu.Unlock()
	if !requestTouchesStart(ranges2) {
		t.Errorf("the stale committed range was inherited; requests=%v", ranges2)
	}
}
