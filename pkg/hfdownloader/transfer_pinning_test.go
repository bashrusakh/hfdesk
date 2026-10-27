// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// The transfer is pinned to the RESOLVED commit (issue #140 follow-up).
//
// The planner resolves the requested revision to a commit, and the resolved
// commit is the identity the resume records and the published snapshot use.
// The transfer itself must be built from that SAME identity: the tree walk
// that discovers the files AND every per-file URL. Otherwise the plan can
// name commit A while the HTTP GET (and every retried Range) serves whatever
// the moved branch B contains, and B's bytes are published under A's digest.
//
// The regressions below cover:
//
//	(a) a pinned plan fetches and publishes the RESOLVED commit's bytes, and
//	    both request families (tree walk and file URL) carry the commit;
//	(b) Verify=sha256 publishes the resolved commit's bytes and DETECTS a
//	    server that serves the wrong bytes even for the committed URL;
//	(c) an UNPINNED identity (the endpoint resolves no commit) requires ONE
//	    full-file content proof before publication, and a mismatch publishes
//	    nothing;
//	(d) a PINNED identity in Verify=size still performs ZERO full-file hash
//	    passes on the normal path.
//
// The fixtures are deterministic: a local httptest server, no network.

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
	"strings"
	"sync"
	"testing"
	"time"
)

// pinTestCommitA is the resolved commit the pinned fixtures serve.
const pinTestCommitA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// movedBranchHub models a branch that moved between the revision lookup and
// the transfer.
//
// The revision endpoint resolves the requested revision "main" to commit A.
// The tree at commit A declares the digest of bytes A. The payload endpoint
// serves bytes A for the committed URL and bytes B - the same length,
// different content - for the requested revision, so a transfer that is not
// pinned to the commit publishes B's bytes under A's digest.
//
// An endpoint that resolves NO commit (unresolved) answers the revision
// request with 404, so the planner falls back to the requested revision: the
// tree walk and the payload URL then both name "main".
type movedBranchHub struct {
	// commitA is the resolved commit, or "" when the endpoint resolves none.
	commitA string
	// bytesA is the content the committed tree declares and serves.
	bytesA []byte
	// bytesB is the same-length content the requested revision serves.
	bytesB []byte
	// serveBAtCommit makes the committed URL serve bytesB too: the server
	// lies about the commit, and only the content proof can catch it.
	serveBAtCommit bool

	mu       sync.Mutex
	requests []string
}

func (h *movedBranchHub) shaA() string {
	sum := sha256.Sum256(h.bytesA)
	return hex.EncodeToString(sum[:])
}

func (h *movedBranchHub) record(path string) {
	h.mu.Lock()
	h.requests = append(h.requests, path)
	h.mu.Unlock()
}

func (h *movedBranchHub) recorded() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.requests...)
}

func (h *movedBranchHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.record(r.URL.Path)
	switch {
	case strings.HasSuffix(r.URL.Path, "/revision/main"):
		if h.commitA == "" {
			// The endpoint does not resolve a commit.
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"sha":"` + h.commitA + `"}`))
	case strings.Contains(r.URL.Path, "/tree/"):
		// The tree of the resolved commit (or of the requested revision for
		// an unresolved endpoint). Both declare bytes A's digest.
		_, _ = fmt.Fprintf(w,
			`[{"type":"file","path":"model.bin","size":%d,"lfs":{"oid":"%s","size":%d,"sha256":"%s"}}]`,
			len(h.bytesA), h.shaA(), len(h.bytesA), h.shaA())
	case strings.Contains(r.URL.Path, "/resolve/"):
		// Serve the bytes of the identity the URL names.
		payload := h.bytesA
		if h.serveBAtCommit || !strings.Contains(r.URL.Path, "/resolve/"+h.commitA+"/") {
			payload = h.bytesB
		}
		// ServeContent honours Range and emits exact 206 responses.
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(payload))
	default:
		http.NotFound(w, r)
	}
}

// newMovedBranchHub starts a moved-branch fixture. bytesA and bytesB must
// have the same length: only the content, never the size, tells them apart.
func newMovedBranchHub(t *testing.T, commitA string, bytesA, bytesB []byte, serveBAtCommit bool) (*movedBranchHub, *httptest.Server) {
	t.Helper()
	if len(bytesA) != len(bytesB) {
		t.Fatalf("the fixture needs same-size payloads, got %d and %d", len(bytesA), len(bytesB))
	}
	hub := &movedBranchHub{commitA: commitA, bytesA: bytesA, bytesB: bytesB, serveBAtCommit: serveBAtCommit}
	srv := httptest.NewServer(hub)
	t.Cleanup(srv.Close)
	return hub, srv
}

// pinTestPayloads returns two same-size payloads with different content.
func pinTestPayloads(size int) ([]byte, []byte) {
	a := make([]byte, size)
	for i := range a {
		a[i] = byte(i % 251)
	}
	b := make([]byte, size)
	for i := range b {
		b[i] = byte((i*7 + 13) % 251)
	}
	return a, b
}

// TestTransferIsPinnedToResolvedCommit is regression (a) and (e).
//
// The plan resolves "main" to commit A, and the branch behind "main" has
// moved: the requested revision serves DIFFERENT bytes of the same size. The
// download must fetch the RESOLVED commit's bytes and publish exactly those,
// and both request families - the tree walk and the per-file URL - must name
// the same identity.
func TestTransferIsPinnedToResolvedCommit(t *testing.T) {
	const size = 2 << 20
	bytesA, bytesB := pinTestPayloads(size)

	for _, route := range []string{"ranged", "single"} {
		t.Run(route, func(t *testing.T) {
			hub, srv := newMovedBranchHub(t, pinTestCommitA, bytesA, bytesB, false)
			out := t.TempDir()
			base, err := DestinationBase(out, "owner/model")
			if err != nil {
				t.Fatal(err)
			}
			threshold := "1MB"
			if route == "single" {
				threshold = "1GiB"
			}
			cfg := Settings{
				OutputDir:          out,
				Endpoint:           srv.URL,
				Concurrency:        2,
				MaxActiveDownloads: 1,
				MultipartThreshold: threshold,
				Verify:             "size",
				Retries:            0,
				StallTimeout:       "0",
			}
			hashPassCount.Store(0)
			if err := Download(context.Background(), Job{Repo: "owner/model", Revision: "main"}, cfg, func(ProgressEvent) {}); err != nil {
				t.Fatalf("Download: %v", err)
			}
			// The published bytes are the RESOLVED commit's bytes, never the
			// moved branch's.
			assertExactFile(t, filepath.Join(base, "model.bin"), bytesA)

			// Both request families carry the resolved commit.
			var sawTree, sawResolve bool
			for _, path := range hub.recorded() {
				if strings.Contains(path, "/tree/") {
					if !strings.Contains(path, "/tree/"+pinTestCommitA+"/") && !strings.HasSuffix(path, "/tree/"+pinTestCommitA) {
						t.Errorf("the tree walk did not use the resolved commit: %s", path)
					}
					sawTree = true
				}
				if strings.Contains(path, "/resolve/") {
					if !strings.Contains(path, "/resolve/"+pinTestCommitA+"/") {
						t.Errorf("the file URL did not use the resolved commit: %s", path)
					}
					sawResolve = true
				}
			}
			if !sawTree || !sawResolve {
				t.Fatalf("requests = %v, want a tree walk and a file request", hub.recorded())
			}
		})
	}
}

// TestTransferPinnedCommitDetectsWrongBytesUnderSHA256 is regression (b).
//
// With Verify=sha256 the committed bytes are published. A server that serves
// the WRONG bytes even for the committed URL is caught by the content proof,
// and nothing is published.
func TestTransferPinnedCommitDetectsWrongBytesUnderSHA256(t *testing.T) {
	const size = 2 << 20
	bytesA, bytesB := pinTestPayloads(size)

	t.Run("the committed bytes are published", func(t *testing.T) {
		_, srv := newMovedBranchHub(t, pinTestCommitA, bytesA, bytesB, false)
		out := t.TempDir()
		base, err := DestinationBase(out, "owner/model")
		if err != nil {
			t.Fatal(err)
		}
		cfg := Settings{
			OutputDir:          out,
			Endpoint:           srv.URL,
			Concurrency:        2,
			MaxActiveDownloads: 1,
			MultipartThreshold: "1MB",
			Verify:             "sha256",
			Retries:            0,
			StallTimeout:       "0",
		}
		if err := Download(context.Background(), Job{Repo: "owner/model", Revision: "main"}, cfg, func(ProgressEvent) {}); err != nil {
			t.Fatalf("Download: %v", err)
		}
		assertExactFile(t, filepath.Join(base, "model.bin"), bytesA)
	})

	t.Run("wrong bytes under the committed URL are detected", func(t *testing.T) {
		_, srv := newMovedBranchHub(t, pinTestCommitA, bytesA, bytesB, true)
		out := t.TempDir()
		base, err := DestinationBase(out, "owner/model")
		if err != nil {
			t.Fatal(err)
		}
		cfg := Settings{
			OutputDir:          out,
			Endpoint:           srv.URL,
			Concurrency:        2,
			MaxActiveDownloads: 1,
			MultipartThreshold: "1MB",
			Verify:             "sha256",
			Retries:            0,
			StallTimeout:       "0",
		}
		err = Download(context.Background(), Job{Repo: "owner/model", Revision: "main"}, cfg, func(ProgressEvent) {})
		if err == nil || !strings.Contains(err.Error(), "sha256 verify failed") {
			t.Fatalf("Download error = %v, want a sha256 verification failure", err)
		}
		if _, serr := os.Stat(filepath.Join(base, "model.bin")); !os.IsNotExist(serr) {
			t.Errorf("the wrong bytes were published, stat err=%v", serr)
		}
	})
}

// TestUnpinnedIdentityRequiresContentProofBeforePublish is regression (c).
//
// An endpoint that resolves no commit pins nothing, so the requested revision
// is used exactly as before. The integrity hole that leaves open is closed by
// the downloader: with a known digest and Verify != "none", ONE full-file
// content proof runs before publication, and a mismatch publishes nothing.
func TestUnpinnedIdentityRequiresContentProofBeforePublish(t *testing.T) {
	const size = 2 << 20
	bytesA, bytesB := pinTestPayloads(size)

	for _, route := range []string{"ranged", "single"} {
		t.Run(route, func(t *testing.T) {
			// The endpoint resolves NO commit: the tree declares bytes A's
			// digest, the payload endpoint serves bytes B.
			_, srv := newMovedBranchHub(t, "", bytesA, bytesB, false)
			out := t.TempDir()
			base, err := DestinationBase(out, "owner/model")
			if err != nil {
				t.Fatal(err)
			}
			threshold := "1MB"
			if route == "single" {
				threshold = "1GiB"
			}
			cfg := Settings{
				OutputDir:          out,
				Endpoint:           srv.URL,
				Concurrency:        2,
				MaxActiveDownloads: 1,
				MultipartThreshold: threshold,
				Verify:             "size",
				Retries:            0,
				StallTimeout:       "0",
			}
			hashPassCount.Store(0)
			err = Download(context.Background(), Job{Repo: "owner/model", Revision: "main"}, cfg, func(ProgressEvent) {})
			if err == nil || !strings.Contains(err.Error(), "sha256 verify failed") {
				t.Fatalf("Download error = %v, want a sha256 verification failure", err)
			}
			if got := hashPassCount.Load(); got != 1 {
				t.Errorf("the unpinned identity performed %d full-file passes, want exactly 1", got)
			}
			// Nothing is published: the unproven bytes stay off the
			// destination.
			if _, serr := os.Stat(filepath.Join(base, "model.bin")); !os.IsNotExist(serr) {
				t.Errorf("the unproven bytes were published, stat err=%v", serr)
			}
		})
	}
}

// TestPinnedIdentitySizeModePerformsNoFullFileHash is regression (d).
//
// The normal size-mode rule ("no reread") still holds for a PINNED identity:
// the transfer performs ZERO full-file hash passes.
func TestPinnedIdentitySizeModePerformsNoFullFileHash(t *testing.T) {
	const size = 2 << 20
	bytesA, bytesB := pinTestPayloads(size)

	for _, route := range []string{"ranged", "single"} {
		t.Run(route, func(t *testing.T) {
			_, srv := newMovedBranchHub(t, pinTestCommitA, bytesA, bytesB, false)
			out := t.TempDir()
			base, err := DestinationBase(out, "owner/model")
			if err != nil {
				t.Fatal(err)
			}
			threshold := "1MB"
			if route == "single" {
				threshold = "1GiB"
			}
			cfg := Settings{
				OutputDir:          out,
				Endpoint:           srv.URL,
				Concurrency:        2,
				MaxActiveDownloads: 1,
				MultipartThreshold: threshold,
				Verify:             "size",
				Retries:            0,
				StallTimeout:       "0",
			}
			hashPassCount.Store(0)
			if err := Download(context.Background(), Job{Repo: "owner/model", Revision: "main"}, cfg, func(ProgressEvent) {}); err != nil {
				t.Fatalf("Download: %v", err)
			}
			if got := hashPassCount.Load(); got != 0 {
				t.Errorf("the pinned size-mode transfer performed %d full-file passes, want 0", got)
			}
			assertExactFile(t, filepath.Join(base, "model.bin"), bytesA)
		})
	}
}
