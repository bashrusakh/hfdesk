// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// B1 regression coverage: the single-request path is under the same
// validation/trust model as the ranged path.
//
//   - a complete inherited .part is published only after a content proof when a
//     digest is known and Verify != "none" (a corrupt same-size part must never
//     reach the blob);
//   - a moved-commit / wrong-prefix .part is caught by the same proof even
//     though its length matches;
//   - every 206 is validated for the exact requested Content-Range, expected
//     total and exact body length, and the bounded writer rejects an overlong
//     body; none of those may publish;
//   - Verify="none" keeps its documented no-hash semantics.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func singleTestSHA(full []byte) string {
	sum := sha256.Sum256(full)
	return hex.EncodeToString(sum[:])
}

func TestDownloadSingle_CompletePartialRequiresContentProof(t *testing.T) {
	const size = 4_000
	full := rangedTestPayload(size)
	sha := singleTestSHA(full)

	newDownload := func(t *testing.T, part []byte) (PlanItem, string, *httptest.Server, *atomic.Int32) {
		t.Helper()
		dir := t.TempDir()
		dst := filepath.Join(dir, "complete.bin")
		if err := os.WriteFile(dst+".part", part, 0o644); err != nil {
			t.Fatal(err)
		}
		var requests atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			w.Write(full)
		}))
		t.Cleanup(srv.Close)
		it := PlanItem{RelativePath: "complete.bin", URL: srv.URL + "/complete.bin", Size: size, LFS: true, SHA256: sha}
		return it, dst, srv, &requests
	}

	t.Run("corrupt complete part is rejected in size mode", func(t *testing.T) {
		corrupt := append([]byte(nil), full...)
		corrupt[size/2] ^= 0xFF
		it, dst, srv, requests := newDownload(t, corrupt)

		hashPassCount.Store(0)
		verified, err := downloadSingle(context.Background(), srv.Client(), "", Job{Repo: "o/r"},
			Settings{Verify: "size", Retries: 0, StallTimeout: "0"}, it, dst, func(ProgressEvent) {})
		if err == nil || !strings.Contains(err.Error(), "sha256 verify failed") {
			t.Fatalf("corrupt complete .part error = %v, want a sha256 verification failure", err)
		}
		if verified {
			t.Error("a failed proof must not report contentVerified")
		}
		if _, serr := os.Stat(dst); !os.IsNotExist(serr) {
			t.Errorf("corrupt complete .part was published, stat err=%v", serr)
		}
		if _, serr := os.Stat(dst + ".part"); !os.IsNotExist(serr) {
			t.Errorf("unproven .part was not discarded, stat err=%v", serr)
		}
		if got := hashPassCount.Load(); got != 1 {
			t.Errorf("inherited-bytes proof performed %d full-file passes; want exactly 1", got)
		}
		if got := requests.Load(); got != 0 {
			t.Errorf("complete .part issued %d requests; want 0", got)
		}
	})

	t.Run("valid complete part is proven once and published", func(t *testing.T) {
		it, dst, srv, requests := newDownload(t, full)

		hashPassCount.Store(0)
		verified, err := downloadSingle(context.Background(), srv.Client(), "", Job{Repo: "o/r"},
			Settings{Verify: "size", Retries: 0, StallTimeout: "0"}, it, dst, func(ProgressEvent) {})
		if err != nil {
			t.Fatalf("downloadSingle: %v", err)
		}
		if !verified {
			t.Error("the inherited-bytes proof was not reported as completed")
		}
		if got := hashPassCount.Load(); got != 1 {
			t.Errorf("inherited-bytes proof performed %d full-file passes; want exactly 1", got)
		}
		if got := requests.Load(); got != 0 {
			t.Errorf("complete .part issued %d requests; want 0", got)
		}
		assertExactFile(t, dst, full)
	})

	t.Run("strict mode proves inherited bytes exactly once", func(t *testing.T) {
		it, dst, srv, _ := newDownload(t, full)

		hashPassCount.Store(0)
		verified, err := downloadSingle(context.Background(), srv.Client(), "", Job{Repo: "o/r"},
			Settings{Verify: "sha256", Retries: 0, StallTimeout: "0"}, it, dst, func(ProgressEvent) {})
		if err != nil {
			t.Fatalf("downloadSingle: %v", err)
		}
		if !verified {
			t.Error("strict mode did not report the inherited-bytes proof, so the caller would repeat it")
		}
		if got := hashPassCount.Load(); got != 1 {
			t.Errorf("strict inherited-bytes proof performed %d full-file passes; want exactly 1", got)
		}
		assertExactFile(t, dst, full)
	})

	t.Run("verify none inherits the no-hash semantics", func(t *testing.T) {
		corrupt := append([]byte(nil), full...)
		corrupt[0] ^= 0xFF
		it, dst, srv, requests := newDownload(t, corrupt)

		hashPassCount.Store(0)
		verified, err := downloadSingle(context.Background(), srv.Client(), "", Job{Repo: "o/r"},
			Settings{Verify: "none", Retries: 0, StallTimeout: "0"}, it, dst, func(ProgressEvent) {})
		if err != nil {
			t.Fatalf("Verify=none downloadSingle: %v", err)
		}
		if verified {
			t.Error("Verify=none must not perform or report a content proof")
		}
		if got := hashPassCount.Load(); got != 0 {
			t.Errorf("Verify=none performed %d hash passes; want 0", got)
		}
		if got := requests.Load(); got != 0 {
			t.Errorf("complete .part issued %d requests; want 0", got)
		}
		assertExactFile(t, dst, corrupt)
	})
}

// TestDownloadSingle_MovedCommitPartialIsContentProven covers the single-path
// moved-commit case: a .part prefix from another revision (or any wrong bytes)
// has the right length, so only the content proof can reject it.
func TestDownloadSingle_MovedCommitPartialIsContentProven(t *testing.T) {
	const size = 4_096
	const prefix = 2_048
	full := rangedTestPayload(size)
	sha := singleTestSHA(full)

	dir := t.TempDir()
	dst := filepath.Join(dir, "moved.bin")
	// Same length as the valid prefix, different revision's bytes.
	if err := os.WriteFile(dst+".part", bytes.Repeat([]byte{0x5A}, prefix), 0o644); err != nil {
		t.Fatal(err)
	}

	var requested string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested = r.Header.Get("Range")
		var start, end int64
		if _, err := fmt.Sscanf(requested, "bytes=%d-%d", &start, &end); err != nil {
			http.Error(w, "bad range", http.StatusBadRequest)
			return
		}
		content := full[start : end+1]
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(content)
	}))
	defer srv.Close()

	it := PlanItem{RelativePath: "moved.bin", URL: srv.URL + "/moved.bin", Size: size, LFS: true, SHA256: sha}
	hashPassCount.Store(0)
	verified, err := downloadSingle(context.Background(), srv.Client(), "", Job{Repo: "o/r"},
		Settings{Verify: "size", Retries: 0, StallTimeout: "0"}, it, dst, func(ProgressEvent) {})
	if err == nil || !strings.Contains(err.Error(), "sha256 verify failed") {
		t.Fatalf("moved-commit partial error = %v, want a sha256 verification failure (published=%v)", err, verified)
	}
	if want := fmt.Sprintf("bytes=%d-%d", prefix, size-1); requested != want {
		t.Errorf("resume requested %q, want the exact missing suffix %q", requested, want)
	}
	if _, serr := os.Stat(dst); !os.IsNotExist(serr) {
		t.Errorf("wrong-prefix bytes were published, stat err=%v", serr)
	}
	if _, serr := os.Stat(dst + ".part"); !os.IsNotExist(serr) {
		t.Errorf("unproven partial was not discarded, stat err=%v", serr)
	}
	if got := hashPassCount.Load(); got != 1 {
		t.Errorf("moved-commit proof performed %d full-file passes; want exactly 1", got)
	}
}

// TestDownloadSingle_RejectsInvalidRangedResponses pins the strict 206
// contract: an exact Content-Range for the requested interval, the expected
// total, and exactly the requested body length. A wrong range, a short body and
// an oversized body must all fail without publishing, leaving resumable bytes.
func TestDownloadSingle_RejectsInvalidRangedResponses(t *testing.T) {
	const size = 4_000
	const prefix = 1_200
	full := rangedTestPayload(size)

	cases := []struct {
		name    string
		handler func(w http.ResponseWriter, r *http.Request)
		wantErr string // empty means any error
	}{
		{
			name: "wrong range",
			handler: func(w http.ResponseWriter, r *http.Request) {
				// Valid syntax, wrong start: the client asked for [prefix, size).
				w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", size-1, size))
				w.Header().Set("Content-Length", strconv.Itoa(size))
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write(full)
			},
			wantErr: "does not match requested bytes",
		},
		{
			name: "short body",
			handler: func(w http.ResponseWriter, r *http.Request) {
				// Exact range headers, but stop after half the suffix: no
				// Content-Length so the truncation is clean at the HTTP layer.
				want := size - prefix
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", prefix, size-1, size))
				w.WriteHeader(http.StatusPartialContent)
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				_, _ = w.Write(full[prefix : prefix+want/2])
			},
			wantErr: "ranged body short",
		},
		{
			name: "oversized body",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", prefix, size-1, size))
				w.WriteHeader(http.StatusPartialContent)
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				_, _ = w.Write(full[prefix:])
				_, _ = w.Write(bytes.Repeat([]byte{0xEE}, 512))
			},
			wantErr: "exceeds assigned range",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			dst := filepath.Join(dir, "ranged.bin")
			if err := os.WriteFile(dst+".part", full[:prefix], 0o644); err != nil {
				t.Fatal(err)
			}
			var requested atomic.Value
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requested.Store(r.Header.Get("Range"))
				tc.handler(w, r)
			}))
			defer srv.Close()

			it := PlanItem{RelativePath: "ranged.bin", URL: srv.URL + "/ranged.bin", Size: size}
			_, err := downloadSingle(context.Background(), srv.Client(), "", Job{Repo: "o/r"},
				Settings{Verify: "none", Retries: 0, StallTimeout: "0"}, it, dst, func(ProgressEvent) {})
			if err == nil {
				t.Fatal("invalid ranged response was accepted and published")
			}
			if tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want %q", err, tc.wantErr)
			}
			if got, _ := requested.Load().(string); got != fmt.Sprintf("bytes=%d-%d", prefix, size-1) {
				t.Errorf("requested %q, want the exact resume range", got)
			}
			if _, serr := os.Stat(dst); !os.IsNotExist(serr) {
				t.Errorf("invalid ranged response published the destination, stat err=%v", serr)
			}
			if _, serr := os.Stat(dst + ".part"); serr != nil {
				t.Errorf("resumable partial was removed after a protocol failure: %v", serr)
			}
		})
	}
}

func TestDownloadSingle_OversizedSuffixIsNotTrustedOnRestart(t *testing.T) {
	const size, prefix = 4096, 2048
	full := rangedTestPayload(size)
	dst := filepath.Join(t.TempDir(), "overflow-restart.bin")
	if err := os.WriteFile(dst+".part", full[:prefix], 0o644); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", prefix, size-1, size))
		w.WriteHeader(http.StatusPartialContent)
		if requests.Load() == 1 {
			_, _ = w.Write(full[prefix:])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			_, _ = w.Write([]byte("extra"))
			return
		}
		_, _ = w.Write(full[prefix:])
	}))
	defer srv.Close()
	it := PlanItem{RelativePath: "overflow-restart.bin", URL: srv.URL, Size: size}
	cfg := Settings{Verify: "size", Retries: 0, StallTimeout: "0"}
	if _, err := downloadSingle(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, func(ProgressEvent) {}); err == nil {
		t.Fatal("oversized response unexpectedly succeeded")
	}
	fi, err := os.Stat(dst + ".part")
	if err != nil || fi.Size() != prefix {
		t.Fatalf("invalid suffix was retained: stat err=%v, want committed prefix %d", err, prefix)
	}
	if _, err := downloadSingle(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, func(ProgressEvent) {}); err != nil {
		t.Fatalf("valid restart failed: %v", err)
	}
	assertExactFile(t, dst, full)
	if got := requests.Load(); got != 2 {
		t.Errorf("GET count=%d, want first rejection plus suffix refetch", got)
	}
}
