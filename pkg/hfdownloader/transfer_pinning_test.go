// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Covers commit pinning for tree and file requests, content proof for unpinned
// revisions, and the normal Verify=size no-reread path using local servers.

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

const pinTestCommitA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// movedBranchHub serves different same-size bytes at resolved and moving
// revision URLs. With no commit, revision lookup returns 404 and tree/file
// requests fall back to main, allowing the unpinned-proof path to be tested.
type movedBranchHub struct {
	// commitA is the resolved commit, or "" when the endpoint resolves none.
	commitA string
	// bytesA supplies the declared digest and normal committed content.
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
		payload := h.bytesA
		if h.serveBAtCommit || !strings.Contains(r.URL.Path, "/resolve/"+h.commitA+"/") {
			payload = h.bytesB
		}
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

// Tree and file requests must use the resolved commit even when main serves
// different same-size bytes; publication must contain the committed bytes.
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
			assertExactFile(t, filepath.Join(base, "model.bin"), bytesA)
			if got := hashPassCount.Load(); got != 0 {
				t.Errorf("the pinned size-mode transfer performed %d full-file passes, want 0", got)
			}

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

// SHA256 verification must reject wrong bytes even at the resolved commit URL,
// while publishing the correct committed bytes.
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

// An unresolved revision with a known digest and Verify != none requires one
// pre-publication content proof on both routes; mismatch publishes nothing.
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
			if _, serr := os.Stat(filepath.Join(base, "model.bin")); !os.IsNotExist(serr) {
				t.Errorf("the unproven bytes were published, stat err=%v", serr)
			}
		})
	}
}
