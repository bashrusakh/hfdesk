// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Known LFS digests are proved before publication. No-digest HEAD-ETag
// fallback is sha256-only, remains post-publication, and is outside these cases.

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// prePublishHub declares the digest of declared and serves served; callers
// use equal-size payloads to isolate content proof from length validation.
func prePublishHub(t *testing.T, declared, served []byte) *httptest.Server {
	t.Helper()
	sha := singleTestSHA(declared)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/revision/main"):
			_, _ = w.Write([]byte(`{"sha":"commit123"}`))
		case strings.Contains(r.URL.Path, "/tree/"):
			_, _ = w.Write([]byte(fmt.Sprintf(
				`[{"type":"file","path":"model.bin","size":%d,"lfs":{"oid":"%s","size":%d,"sha256":"%s"}}]`,
				len(served), sha, len(served), sha)))
		case strings.Contains(r.URL.Path, "/resolve/"):
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(served))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func corruptSameSize(full []byte) []byte {
	bad := append([]byte(nil), full...)
	bad[len(bad)/2] ^= 0xFF
	return bad
}

// prePublishCase selects the route, verify mode and destination of one
// pre-publication proof row; check applies that row's distinct oracle.
type prePublishCase struct {
	name string
	// size sets the payload; below-threshold sizes force the single-request
	// route and larger ones the multipart route.
	size int
	// verify is the Settings.Verify mode under test.
	verify string
	// threshold forces the route: a high threshold keeps the single-request
	// path, a low one the multipart path.
	threshold string
	// concurrency matches the route's transfer shape.
	concurrency int
	// corrupt serves same-size corrupt bytes instead of the declared content.
	corrupt bool
	// cache selects the content-addressed cache destination over local output.
	cache bool
	check func(t *testing.T, res prePublishResult)
}

// prePublishResult carries one row's Download outcome to its oracle.
type prePublishResult struct {
	err  error
	good []byte
	sha  string
	// dst is the local-mode destination file; empty in cache mode.
	dst string
	// blobsDir is the cache-mode blobs directory; empty in output mode.
	blobsDir string
}

// A known digest is proved BEFORE publication on every route and verify mode:
// a same-size mismatch leaves neither a destination nor a blob nor restageable
// staging state, and a matching payload publishes exactly once after exactly
// one full-file proof pass.
func TestDownloadKnownDigestProvenBeforePublish(t *testing.T) {
	const (
		singleSize    = 4_096
		multipartSize = 64 * 1024 // above the 1KB threshold → multipart
	)

	// Both single-route verify modes share one oracle each: the original mode
	// loop asserted the same thing for sha256 and etag.
	outputMismatch := func(t *testing.T, res prePublishResult) {
		if res.err == nil || !strings.Contains(res.err.Error(), "sha256 verify failed") {
			t.Fatalf("Download error = %v, want a sha256 verification failure", res.err)
		}
		if got := hashPassCount.Load(); got != 1 {
			t.Errorf("known-digest proof performed %d full-file passes; want exactly 1", got)
		}
		if _, serr := os.Stat(res.dst); !os.IsNotExist(serr) {
			t.Errorf("corrupt bytes were published at %s (stat err=%v)", res.dst, serr)
		}
		if _, serr := os.Stat(res.dst + ".part"); !os.IsNotExist(serr) {
			t.Errorf("unproven .part survived the verification failure, stat err=%v", serr)
		}
	}
	outputMatch := func(t *testing.T, res prePublishResult) {
		if res.err != nil {
			t.Fatalf("Download: %v", res.err)
		}
		if got := hashPassCount.Load(); got != 1 {
			t.Errorf("known-digest verification performed %d full-file passes; want exactly 1", got)
		}
		assertExactFile(t, res.dst, res.good)
	}

	cases := []prePublishCase{
		{
			name:        "single/sha256/output mismatch is not published",
			size:        singleSize,
			verify:      "sha256",
			threshold:   "1MB", // force the single-request path
			concurrency: 1,
			corrupt:     true,
			check:       outputMismatch,
		},
		{
			name:        "single/sha256/output match publishes once",
			size:        singleSize,
			verify:      "sha256",
			threshold:   "1MB",
			concurrency: 1,
			check:       outputMatch,
		},
		{
			name:        "single/sha256/cache mismatch leaves no tmp or blob",
			size:        singleSize,
			verify:      "sha256",
			threshold:   "1MB",
			concurrency: 1,
			corrupt:     true,
			cache:       true,
			check: func(t *testing.T, res prePublishResult) {
				if res.err == nil || !strings.Contains(res.err.Error(), "sha256 verify failed") {
					t.Fatalf("Download error = %v, want a sha256 verification failure", res.err)
				}
				for _, name := range []string{res.sha, "tmp-" + res.sha, "tmp-" + res.sha + ".part"} {
					if _, serr := os.Stat(filepath.Join(res.blobsDir, name)); !os.IsNotExist(serr) {
						t.Errorf("%s survived the pre-publication verification failure, stat err=%v", name, serr)
					}
				}
			},
		},
		{
			name:        "single/etag/output mismatch is not published",
			size:        singleSize,
			verify:      "etag",
			threshold:   "1MB",
			concurrency: 1,
			corrupt:     true,
			check:       outputMismatch,
		},
		{
			name:        "single/etag/output match publishes once",
			size:        singleSize,
			verify:      "etag",
			threshold:   "1MB",
			concurrency: 1,
			check:       outputMatch,
		},
		{
			name:        "multipart/etag/mismatch leaves no blob and discards staging",
			size:        multipartSize,
			verify:      "etag",
			threshold:   "1KB",
			concurrency: 4,
			corrupt:     true, // one wrong byte inside a non-first range
			cache:       true,
			check: func(t *testing.T, res prePublishResult) {
				if res.err == nil || !strings.Contains(res.err.Error(), "sha256 verify failed") {
					t.Fatalf("Download error = %v, want a sha256 verification failure", res.err)
				}
				if got := hashPassCount.Load(); got != 1 {
					t.Errorf("etag known-digest proof performed %d full-file passes; want exactly 1", got)
				}
				for _, name := range []string{res.sha, "tmp-" + res.sha} {
					if _, serr := os.Stat(filepath.Join(res.blobsDir, name)); !os.IsNotExist(serr) {
						t.Errorf("%s was published despite the digest mismatch, stat err=%v", name, serr)
					}
				}
				for _, name := range []string{res.sha + ".incomplete", res.sha + ".incomplete.meta"} {
					if _, serr := os.Stat(filepath.Join(res.blobsDir, name)); !os.IsNotExist(serr) {
						t.Errorf("untrustworthy staging %s survived the mismatch, stat err=%v", name, serr)
					}
				}
			},
		},
		{
			name:        "multipart/etag/match publishes once",
			size:        multipartSize,
			verify:      "etag",
			threshold:   "1KB",
			concurrency: 4,
			cache:       true,
			check: func(t *testing.T, res prePublishResult) {
				if res.err != nil {
					t.Fatalf("Download: %v", res.err)
				}
				if got := hashPassCount.Load(); got != 1 {
					t.Errorf("etag known-digest verification performed %d full-file passes; want exactly 1", got)
				}
				assertExactFile(t, filepath.Join(res.blobsDir, res.sha), res.good)
				if _, serr := os.Stat(filepath.Join(res.blobsDir, "tmp-"+res.sha)); !os.IsNotExist(serr) {
					t.Errorf("a tmp-<sha> restage file exists, stat err=%v", serr)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			good := rangedTestPayload(tc.size)
			served := good
			if tc.corrupt {
				served = corruptSameSize(good)
			}
			srv := prePublishHub(t, good, served)
			cfg := Settings{
				Endpoint:           srv.URL,
				Concurrency:        tc.concurrency,
				MaxActiveDownloads: 1,
				MultipartThreshold: tc.threshold,
				Verify:             tc.verify,
				Retries:            0,
				StallTimeout:       "0",
			}
			res := prePublishResult{good: good, sha: singleTestSHA(good)}
			if tc.cache {
				cacheDir := t.TempDir()
				cfg.CacheDir = cacheDir
				res.blobsDir = filepath.Join(cacheDir, "hub", "models--owner--model", "blobs")
			} else {
				out := t.TempDir()
				base, err := DestinationBase(out, "owner/model")
				if err != nil {
					t.Fatal(err)
				}
				cfg.OutputDir = out
				res.dst = filepath.Join(base, "model.bin")
			}

			hashPassCount.Store(0)
			res.err = Download(context.Background(), Job{Repo: "owner/model", Revision: "main"}, cfg, func(ProgressEvent) {})
			tc.check(t, res)
		})
	}
}
