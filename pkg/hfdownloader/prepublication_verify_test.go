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

// Both sha256 and etag must reject same-size known-digest corruption before
// the single-path rename, leaving neither a destination nor cached payload.
func TestDownloadSingle_KnownDigestProvenBeforePublish(t *testing.T) {
	const size = 4_096
	good := rangedTestPayload(size)
	bad := corruptSameSize(good)
	sha := singleTestSHA(good)

	for _, mode := range []string{"sha256", "etag"} {
		t.Run(mode+"/output mismatch is not published", func(t *testing.T) {
			srv := prePublishHub(t, good, bad)
			out := t.TempDir()
			base, err := DestinationBase(out, "owner/model")
			if err != nil {
				t.Fatal(err)
			}
			cfg := Settings{
				OutputDir:          out,
				Endpoint:           srv.URL,
				Concurrency:        1,
				MaxActiveDownloads: 1,
				MultipartThreshold: "1MB", // force the single-request path
				Verify:             mode,
				Retries:            0,
				StallTimeout:       "0",
			}
			hashPassCount.Store(0)
			err = Download(context.Background(), Job{Repo: "owner/model", Revision: "main"}, cfg, func(ProgressEvent) {})
			if err == nil || !strings.Contains(err.Error(), "sha256 verify failed") {
				t.Fatalf("Download error = %v, want a sha256 verification failure", err)
			}
			if got := hashPassCount.Load(); got != 1 {
				t.Errorf("known-digest proof performed %d full-file passes; want exactly 1", got)
			}
			dst := filepath.Join(base, "model.bin")
			if _, serr := os.Stat(dst); !os.IsNotExist(serr) {
				t.Errorf("corrupt bytes were published at %s (stat err=%v)", dst, serr)
			}
			if _, serr := os.Stat(dst + ".part"); !os.IsNotExist(serr) {
				t.Errorf("unproven .part survived the verification failure, stat err=%v", serr)
			}
		})

		t.Run(mode+"/output match publishes once", func(t *testing.T) {
			srv := prePublishHub(t, good, good)
			out := t.TempDir()
			base, err := DestinationBase(out, "owner/model")
			if err != nil {
				t.Fatal(err)
			}
			cfg := Settings{
				OutputDir:          out,
				Endpoint:           srv.URL,
				Concurrency:        1,
				MaxActiveDownloads: 1,
				MultipartThreshold: "1MB",
				Verify:             mode,
				Retries:            0,
				StallTimeout:       "0",
			}
			hashPassCount.Store(0)
			if err := Download(context.Background(), Job{Repo: "owner/model", Revision: "main"}, cfg, func(ProgressEvent) {}); err != nil {
				t.Fatalf("Download: %v", err)
			}
			if got := hashPassCount.Load(); got != 1 {
				t.Errorf("known-digest verification performed %d full-file passes; want exactly 1", got)
			}
			assertExactFile(t, filepath.Join(base, "model.bin"), good)
		})
	}

	t.Run("sha256/cache mismatch leaves no tmp or blob", func(t *testing.T) {
		srv := prePublishHub(t, good, bad)
		cacheDir := t.TempDir()
		cfg := Settings{
			CacheDir:           cacheDir,
			Endpoint:           srv.URL,
			Concurrency:        1,
			MaxActiveDownloads: 1,
			MultipartThreshold: "1MB",
			Verify:             "sha256",
			Retries:            0,
			StallTimeout:       "0",
		}
		hashPassCount.Store(0)
		err := Download(context.Background(), Job{Repo: "owner/model", Revision: "main"}, cfg, func(ProgressEvent) {})
		if err == nil || !strings.Contains(err.Error(), "sha256 verify failed") {
			t.Fatalf("Download error = %v, want a sha256 verification failure", err)
		}
		blobsDir := filepath.Join(cacheDir, "hub", "models--owner--model", "blobs")
		for _, name := range []string{sha, "tmp-" + sha, "tmp-" + sha + ".part"} {
			if _, serr := os.Stat(filepath.Join(blobsDir, name)); !os.IsNotExist(serr) {
				t.Errorf("%s survived the pre-publication verification failure, stat err=%v", name, serr)
			}
		}
	})
}

// ETag mode must prove a known digest before publishing the ranged payload.
// A mismatch must leave neither the content-addressed blob nor a tmp-<sha> restage.
func TestDownloadMultipart_ETagKnownDigestProvenBeforePublish(t *testing.T) {
	const size = 64 * 1024 // above the 1KB threshold → multipart
	good := rangedTestPayload(size)
	bad := corruptSameSize(good) // one wrong byte inside a non-first range
	sha := singleTestSHA(good)

	cfgFor := func(srv *httptest.Server, cacheDir string) Settings {
		return Settings{
			CacheDir:           cacheDir,
			Endpoint:           srv.URL,
			Concurrency:        4,
			MaxActiveDownloads: 1,
			MultipartThreshold: "1KB",
			Verify:             "etag",
			Retries:            0,
			StallTimeout:       "0",
		}
	}

	t.Run("mismatch leaves no blob and discards staging", func(t *testing.T) {
		srv := prePublishHub(t, good, bad)
		cacheDir := t.TempDir()
		hashPassCount.Store(0)
		err := Download(context.Background(), Job{Repo: "owner/model", Revision: "main"}, cfgFor(srv, cacheDir), func(ProgressEvent) {})
		if err == nil || !strings.Contains(err.Error(), "sha256 verify failed") {
			t.Fatalf("Download error = %v, want a sha256 verification failure", err)
		}
		if got := hashPassCount.Load(); got != 1 {
			t.Errorf("etag known-digest proof performed %d full-file passes; want exactly 1", got)
		}
		blobsDir := filepath.Join(cacheDir, "hub", "models--owner--model", "blobs")
		for _, name := range []string{sha, "tmp-" + sha} {
			if _, serr := os.Stat(filepath.Join(blobsDir, name)); !os.IsNotExist(serr) {
				t.Errorf("%s was published despite the digest mismatch, stat err=%v", name, serr)
			}
		}
		for _, name := range []string{sha + ".incomplete", sha + ".incomplete.meta"} {
			if _, serr := os.Stat(filepath.Join(blobsDir, name)); !os.IsNotExist(serr) {
				t.Errorf("untrustworthy staging %s survived the mismatch, stat err=%v", name, serr)
			}
		}
	})

	t.Run("match publishes once", func(t *testing.T) {
		srv := prePublishHub(t, good, good)
		cacheDir := t.TempDir()
		hashPassCount.Store(0)
		if err := Download(context.Background(), Job{Repo: "owner/model", Revision: "main"}, cfgFor(srv, cacheDir), func(ProgressEvent) {}); err != nil {
			t.Fatalf("Download: %v", err)
		}
		if got := hashPassCount.Load(); got != 1 {
			t.Errorf("etag known-digest verification performed %d full-file passes; want exactly 1", got)
		}
		blob := filepath.Join(cacheDir, "hub", "models--owner--model", "blobs", sha)
		assertExactFile(t, blob, good)
		if _, serr := os.Stat(filepath.Join(filepath.Dir(blob), "tmp-"+sha)); !os.IsNotExist(serr) {
			t.Errorf("a tmp-<sha> restage file exists, stat err=%v", serr)
		}
	})
}
