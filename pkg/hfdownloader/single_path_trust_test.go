// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Covers single-request resume proof, response framing, and Verify="none".

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// postProofCancelContext observes the proof-to-rename boundary via the closed
// partial handle, avoiding ctx.Err call counts that vary across Go versions.
type postProofCancelContext struct {
	context.Context
	boundary func() bool
	cancel   context.CancelFunc
	armed    bool
	observed atomic.Bool
}

func (c *postProofCancelContext) Err() error {
	if !c.observed.Load() && c.boundary() {
		c.observed.Store(true)
		if c.armed {
			c.cancel()
			if err := c.Context.Err(); err != nil {
				return err
			}
			return context.Canceled
		}
	}
	return c.Context.Err()
}

// closedFileBoundary detects this regular-file handle's close through Stat failure, without sleeps.
func closedFileBoundary(f *os.File) func() bool {
	return func() bool {
		_, err := f.Stat()
		return err != nil
	}
}

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
		var requests atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			w.Write(full)
		}))
		t.Cleanup(srv.Close)
		it := PlanItem{RelativePath: "complete.bin", URL: srv.URL + "/complete.bin", Size: size, LFS: true, SHA256: sha}
		// A matching pinned identity plus clean framing reaches content proof without
		// HTTP; corrupt same-size bytes must then fail that proof.
		seedSinglePartial(t, dst, part, Job{Repo: "o/r"}, it, completeSingleIdentity())
		return it, dst, srv, &requests
	}

	t.Run("corrupt complete part is rejected in size mode", func(t *testing.T) {
		corrupt := append([]byte(nil), full...)
		corrupt[size/2] ^= 0xFF
		it, dst, srv, requests := newDownload(t, corrupt)

		hashPassCount.Store(0)
		verified, err := downloadSingle(context.Background(), srv.Client(), "", Job{Repo: "o/r"},
			Settings{Verify: "size", Retries: 0, StallTimeout: "0"}, it, dst, singleTestIdentity,
			func(ProgressEvent) {})

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
			Settings{Verify: "size", Retries: 0, StallTimeout: "0"}, it, dst, singleTestIdentity,
			func(ProgressEvent) {})

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
			Settings{Verify: "sha256", Retries: 0, StallTimeout: "0"}, it, dst, singleTestIdentity,
			func(ProgressEvent) {})

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
			Settings{Verify: "none", Retries: 0, StallTimeout: "0"}, it, dst, singleTestIdentity,
			func(ProgressEvent) {})

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

// A moved-commit partial is rejected by identity; same-identity corrupt bytes
// are rejected by content proof.
func TestDownloadSingle_MovedCommitPartialIsContentProven(t *testing.T) {
	const size = 4_096
	const prefix = 2_048
	full := rangedTestPayload(size)
	sha := singleTestSHA(full)

	serveRange := func(t *testing.T, requested *string) *httptest.Server {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			*requested = r.Header.Get("Range")
			if *requested == "" {
				w.Header().Set("Content-Length", strconv.Itoa(len(full)))
				_, _ = w.Write(full)
				return
			}
			var start, end int64
			if _, err := fmt.Sscanf(*requested, "bytes=%d-%d", &start, &end); err != nil {
				http.Error(w, "bad range", http.StatusBadRequest)
				return
			}
			content := full[start : end+1]
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
			w.Header().Set("Content-Length", strconv.Itoa(len(content)))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(content)
		}))
		t.Cleanup(srv.Close)
		return srv
	}

	t.Run("a record of another revision is not inherited", func(t *testing.T) {
		dir := t.TempDir()
		dst := filepath.Join(dir, "moved.bin")
		job := Job{Repo: "o/r"}
		it := PlanItem{RelativePath: "moved.bin", Size: size, LFS: true, SHA256: sha}
		earlier := singleOpts{Commit: "5555555555555555555555555555555555555555", CommitPinned: true}
		seedSinglePartial(t, dst, bytes.Repeat([]byte{0x5A}, prefix), job, it, earlier)

		var requested string
		srv := serveRange(t, &requested)
		it.URL = srv.URL + "/moved.bin"

		current := singleOpts{Commit: "6666666666666666666666666666666666666666", CommitPinned: true}
		hashPassCount.Store(0)
		if _, err := downloadSingle(context.Background(), srv.Client(), "", job,
			Settings{Verify: "size", Retries: 0, StallTimeout: "0"}, it, dst, current,
			func(ProgressEvent) {}); err != nil {
			t.Fatalf("downloadSingle: %v", err)
		}
		if requested != "" {
			t.Errorf("another revision's bytes were resumed: request %q, want a full request", requested)
		}
		if got := hashPassCount.Load(); got != 0 {
			t.Errorf("inherited-bytes proof performed %d full-file passes; want 0 (nothing was inherited)", got)
		}
		assertExactFile(t, dst, full)
	})

	t.Run("a matching record with foreign bytes is content proven", func(t *testing.T) {
		dir := t.TempDir()
		dst := filepath.Join(dir, "moved.bin")
		job := Job{Repo: "o/r"}
		it := PlanItem{RelativePath: "moved.bin", Size: size, LFS: true, SHA256: sha}
		// Same length as the valid prefix, different revision's bytes, and a
		// record that names THIS item.
		seedSinglePartial(t, dst, bytes.Repeat([]byte{0x5A}, prefix), job, it, singleTestIdentity)

		var requested string
		srv := serveRange(t, &requested)
		it.URL = srv.URL + "/moved.bin"

		hashPassCount.Store(0)
		verified, err := downloadSingle(context.Background(), srv.Client(), "", job,
			Settings{Verify: "size", Retries: 0, StallTimeout: "0"}, it, dst, singleTestIdentity,
			func(ProgressEvent) {})

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
	})
}

// Invalid 206 intervals, totals or body lengths must fail without publication
// and preserve the resumable partial.
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
			it := PlanItem{RelativePath: "ranged.bin", Size: size}
			seedSinglePartial(t, dst, full[:prefix], Job{Repo: "o/r"}, it, singleTestIdentity)
			var requested atomic.Value
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requested.Store(r.Header.Get("Range"))
				tc.handler(w, r)
			}))
			defer srv.Close()

			it.URL = srv.URL + "/ranged.bin"
			_, err := downloadSingle(context.Background(), srv.Client(), "", Job{Repo: "o/r"},
				Settings{Verify: "none", Retries: 0, StallTimeout: "0"}, it, dst, singleTestIdentity,
				func(ProgressEvent) {})

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
	it := PlanItem{RelativePath: "overflow-restart.bin", Size: size}
	seedSinglePartial(t, dst, full[:prefix], Job{Repo: "o/r"}, it, singleTestIdentity)
	var requests atomic.Int32
	var lastRange atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		lastRange.Store(r.Header.Get("Range"))
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
	it.URL = srv.URL
	cfg := Settings{Verify: "size", Retries: 0, StallTimeout: "0"}
	if _, err := downloadSingle(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, singleTestIdentity, func(ProgressEvent) {}); err == nil {
		t.Fatal("oversized response unexpectedly succeeded")
	}
	fi, err := os.Stat(dst + ".part")
	if err != nil || fi.Size() != prefix {
		t.Fatalf("invalid suffix was retained: stat err=%v, want committed prefix %d", err, prefix)
	}
	if _, err := downloadSingle(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, singleTestIdentity, func(ProgressEvent) {}); err != nil {
		t.Fatalf("valid restart failed: %v", err)
	}
	assertExactFile(t, dst, full)
	if got := requests.Load(); got != 2 {
		t.Errorf("GET count=%d, want first rejection plus suffix refetch", got)
	}
	if got, _ := lastRange.Load().(string); got != fmt.Sprintf("bytes=%d-%d", prefix, size-1) {
		t.Errorf("restart range=%q, want the missing suffix", got)
	}
}

func TestDownloadSingle_FinalBytesRequireFramingEOF(t *testing.T) {
	const size, prefix = 8192, 4096
	full := rangedTestPayload(size)
	for _, lateOverflow := range []bool{false, true} {
		name := "exact bytes without timely EOF"
		if lateOverflow {
			name = "overflow delayed beyond stall deadline"
		}
		t.Run(name, func(t *testing.T) {
			dst := filepath.Join(t.TempDir(), "framing.bin")
			it := PlanItem{RelativePath: "framing.bin", Size: size}
			seedSinglePartial(t, dst, full[:prefix], Job{Repo: "o/r"}, it, singleTestIdentity)
			var gets atomic.Int32
			var secondRange atomic.Value
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var start, end int
				if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
					http.Error(w, "range required", http.StatusBadRequest)
					return
				}
				request := gets.Add(1)
				if request == 2 {
					secondRange.Store(r.Header.Get("Range"))
				}
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write(full[start : end+1])
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				if request == 1 {
					// No Content-Length: the HTTP body is chunked and remains
					// unterminated beyond the stall watchdog.
					time.Sleep(200 * time.Millisecond)
					if lateOverflow {
						_, _ = w.Write([]byte("late overflow"))
					}
				}
			}))
			defer srv.Close()
			it.URL = srv.URL
			cfg := Settings{Verify: "none", Retries: 0, StallTimeout: "40ms"}
			if _, err := downloadSingle(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, singleTestIdentity, func(ProgressEvent) {}); err == nil {
				t.Fatal("unterminated/overflowing response published after writing all requested bytes")
			}
			if _, err := os.Stat(dst); !os.IsNotExist(err) {
				t.Fatalf("destination was published before framing EOF: %v", err)
			}
			part := dst + ".part"
			if got, err := os.ReadFile(part); err != nil || !bytes.Equal(got, full[:size-1]) {
				t.Fatalf("failed attempt must preserve the valid prefix and reserve the final byte: len=%d err=%v", len(got), err)
			}
			if _, err := downloadSingle(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, singleTestIdentity, func(ProgressEvent) {}); err != nil {
				t.Fatalf("healthy final-byte restart: %v", err)
			}
			assertExactFile(t, dst, full)
			if got, _ := secondRange.Load().(string); got != fmt.Sprintf("bytes=%d-%d", size-1, size-1) {
				t.Fatalf("restart range=%q, want only the unframed final byte", got)
			}
			if got := gets.Load(); got != 2 {
				t.Errorf("GET count=%d, want failed framing plus clean final-byte retry", got)
			}
		})
	}
}

func TestDownloadSingle_CancelAtFinalizeBeforeRename(t *testing.T) {
	const size = 4096
	full := rangedTestPayload(size)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(size))
		_, _ = w.Write(full)
	}))
	defer srv.Close()

	for _, cleanup := range []bool{false, true} {
		name := "pause preserves partial"
		if cleanup {
			name = "explicit cancel cleans partial"
		}
		t.Run(name, func(t *testing.T) {
			dst := filepath.Join(t.TempDir(), "cancel-finalize.bin")
			old := []byte("previous destination")
			if err := os.WriteFile(dst, old, 0o644); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cfg := Settings{Verify: "none", Retries: 0, StallTimeout: "0", CleanupPartialsOnCancel: func() bool { return cleanup }}
			it := PlanItem{RelativePath: "cancel-finalize.bin", URL: srv.URL, Size: size}
			_, err := downloadSingle(ctx, srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, singleOpts{Domain: acquireTestDomain(t, dst)}, func(ev ProgressEvent) {
				if ev.Event == "file_finalizing" {
					cancel()
				}
			})

			if !errors.Is(err, context.Canceled) {
				t.Fatalf("download error=%v, want canceled", err)
			}
			if got, readErr := os.ReadFile(dst); readErr != nil || !bytes.Equal(got, old) {
				t.Fatalf("pre-existing destination changed on cancel: %q, %v", got, readErr)
			}
			part := dst + ".part"
			gotPart, partErr := os.ReadFile(part)
			if cleanup {
				if !os.IsNotExist(partErr) {
					t.Errorf("explicit cancel retained partial: %v", partErr)
				}
			} else if partErr != nil || !bytes.Equal(gotPart, full) {
				t.Errorf("pause failed to preserve complete partial: len=%d err=%v", len(gotPart), partErr)
			}
		})
	}
}

// At the post-proof/pre-rename boundary, pause preserves the partial, explicit
// cancel removes it, and neither publishes. Use the observable closed-handle boundary.
func TestFinalizeSinglePartial_PostProofCancelHonorsCleanupPolicy(t *testing.T) {
	const size = 32 * 1024
	full := rangedTestPayload(size)
	sha := singleTestSHA(full)

	for _, cleanup := range []bool{false, true} {
		name := "pause preserves partial"
		if cleanup {
			name = "explicit cancel cleans partial"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			configure := Settings{Verify: "sha256", CleanupPartialsOnCancel: func() bool { return cleanup }}
			it := PlanItem{RelativePath: "post-hash-policy.bin", Size: size, SHA256: sha}
			emit := func(ProgressEvent) {}

			// The single path's partial is always dst+".part"; the cleanup
			// policy removes that exact name.
			dst := filepath.Join(dir, "post-hash-policy.bin")
			tmp := dst + ".part"
			if err := os.WriteFile(tmp, full, 0o644); err != nil {
				t.Fatal(err)
			}
			out, err := os.OpenFile(tmp, os.O_RDWR, 0o644)
			if err != nil {
				t.Fatal(err)
			}
			baseCtx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx := &postProofCancelContext{
				Context:  baseCtx,
				boundary: closedFileBoundary(out),
				cancel:   cancel,
				armed:    true,
			}
			hashPassCount.Store(0)
			finOpts := pinnedSingleIdentity()
			finOpts.Domain = acquireTestDomain(t, dst)
			verified, err := finalizeSinglePartial(ctx, out, tmp, dst, it, configure, false, finOpts, emit)
			if !errors.Is(err, context.Canceled) || verified {
				t.Fatalf("post-proof cancellation = (%v,%v), want canceled before publication", verified, err)
			}
			if !ctx.observed.Load() {
				t.Fatal("cancellation did not reach the post-proof boundary")
			}
			if hashPassCount.Load() != 1 {
				t.Errorf("prepublication proof passes=%d, want one", hashPassCount.Load())
			}
			if _, err := os.Stat(dst); !os.IsNotExist(err) {
				t.Fatalf("destination published after post-proof cancellation: %v", err)
			}
			got, partErr := os.ReadFile(tmp)
			if cleanup {
				if !os.IsNotExist(partErr) {
					t.Errorf("explicit cancel retained the proven partial: %v", partErr)
				}
			} else if partErr != nil || !bytes.Equal(got, full) {
				t.Errorf("pause failed to preserve the proven partial: len=%d err=%v", len(got), partErr)
			}
		})
	}
}

// Proof cancellation must close this writable partial before cleanup: Windows
// cannot unlink it while that handle is open. Check the ordering on every platform.
func TestDownloadSingle_CancelDuringProofRemovesPartial(t *testing.T) {
	const size = 64 * 1024
	full := rangedTestPayload(size)
	sha := singleTestSHA(full)
	dst := filepath.Join(t.TempDir(), "cancel-proof.bin")
	job := Job{Repo: "o/r"}
	it := PlanItem{RelativePath: "cancel-proof.bin", Size: size, LFS: true, SHA256: sha}
	// An exactly-sized partial with a matching pinned record that also proves
	// clean framing reaches the content proof without any HTTP request.
	seedSinglePartial(t, dst, full, job, it, completeSingleIdentity())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var payload *os.File
	var proofStarted, payloadClosedAtCleanup bool
	cleanupCalls := 0
	cfg := Settings{
		Verify:       "sha256",
		Retries:      0,
		StallTimeout: "0",
		CleanupPartialsOnCancel: func() bool {
			cleanupCalls++
			if payload != nil {
				_, serr := payload.Stat()
				payloadClosedAtCleanup = serr != nil
			}
			return true
		},
	}
	singleProofStartHook = func(out *os.File) {
		payload = out
		proofStarted = true
		cancel()
	}
	defer func() { singleProofStartHook = nil }()

	proofOpts := singleTestIdentity
	proofOpts.Domain = acquireTestDomain(t, dst)
	verified, err := downloadSingle(ctx, http.DefaultClient, "", job, cfg, it, dst, proofOpts,
		func(ProgressEvent) {})

	if !proofStarted {
		t.Fatal("the content proof never started; the fixture did not reach proveKnownDigest")
	}
	if !errors.Is(err, context.Canceled) || verified {
		t.Fatalf("cancel during the content proof = (%v,%v), want canceled without publication", verified, err)
	}
	if cleanupCalls != 1 {
		t.Fatalf("explicit-cancel cleanup ran %d times, want exactly the proof-cancel branch's one", cleanupCalls)
	}
	if payload == nil {
		t.Fatal("the proof seam did not receive the partial's payload handle")
	}
	if !payloadClosedAtCleanup {
		t.Error("the payload handle was still open when the cancel cleanup ran; " +
			"Windows cannot unlink the .part there, so the cancelled payload and its sidecar would survive")
	}
	if _, serr := os.Stat(dst); !os.IsNotExist(serr) {
		t.Errorf("destination exists after a cancelled proof, stat err=%v", serr)
	}
	for _, path := range []string{dst + ".part", dst + ".part.meta"} {
		if _, serr := os.Stat(path); !os.IsNotExist(serr) {
			t.Errorf("explicit cancel during the content proof left %s behind (stat err=%v)", filepath.Base(path), serr)
		}
	}
}
