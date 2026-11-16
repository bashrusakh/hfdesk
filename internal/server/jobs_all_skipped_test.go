// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
)

// transferCountingHub fronts a fixture hub with a cumulative content-GET
// counter so each run can prove whether it fetched file content: file bytes
// only ever flow over content GETs, so zero content GETs also prove no
// payload transfer. Metadata (revision/tree) requests, HEADs, and the
// downloader's 1-byte addressing probe (its own header, a 206 marker — not
// file content) count in neither.
func transferCountingHub(t *testing.T, inner *httptest.Server) (string, func() int64) {
	t.Helper()
	var got atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.Header.Get(addressingProbeHeader) != "1" &&
			!strings.Contains(r.URL.Path, "/revision/") && !strings.Contains(r.URL.Path, "/tree/") {
			got.Add(1)
		}
		inner.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(proxy.Close)
	return proxy.URL, func() int64 { return got.Load() }
}

// twoShardHub serves a two-shard LFS tree pinned to one revision — the
// multi-file twin of cacheDownloadFixture, with the addressing-probe and
// ETag response shapes the production downloader expects — so a rerun can
// skip both shards by content-addressed proof.
func twoShardHub(t *testing.T) *httptest.Server {
	t.Helper()
	files := map[string][]byte{
		"model-00001-of-00002.safetensors": []byte("shard-one bytes"),
		"model-00002-of-00002.safetensors": []byte("shard-two bytes"),
	}
	nodes := []map[string]any{}
	for p, b := range files {
		nodes = append(nodes, map[string]any{"type": "file", "path": p, "size": len(b), "lfs": map[string]any{"oid": fmt.Sprintf("%x", sha256.Sum256(b)), "size": len(b)}})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/revision/"):
			_, _ = w.Write([]byte(`{"sha":"commit123"}`))
		case strings.Contains(r.URL.Path, "/tree/"):
			_ = json.NewEncoder(w).Encode(nodes)
		case strings.Contains(r.URL.Path, "/resolve/"):
			body, ok := files[r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]]
			if !ok {
				http.NotFound(w, r)
				return
			}
			if r.Header.Get(addressingProbeHeader) == "1" {
				writeAddressingProbeResponse(w, len(body), body[:1])
				return
			}
			w.Header().Set("Content-Length", fmt.Sprint(len(body)))
			w.Header().Set("ETag", fmt.Sprintf("%x", sha256.Sum256(body)))
			_, _ = w.Write(body) // net/http discards the body on HEAD
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestFullySkippedJobEndsCompleted pins that a run whose requested files are
// ALL already present ends completed with the same finished shape as a run
// that transferred everything (the job contract's success rule lives in
// pkg/hfdownloader/doc.go), across four destination/selection shapes.
func TestFullySkippedJobEndsCompleted(t *testing.T) {
	isolateCacheState(t)
	for _, tc := range []struct {
		name    string
		shards  bool
		flat    bool
		preSeed bool
	}{
		{name: "HF-cache single-file rerun"},
		{name: "HF-cache 2-shard rerun", shards: true},
		{name: "flat rerun", flat: true},
		{name: "flat pre-seeded first run", flat: true, preSeed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var inner *httptest.Server
			var payload []byte
			if tc.shards {
				inner = twoShardHub(t)
			} else {
				inner, payload, _, _ = cacheDownloadFixture(t, "")
			}
			endpoint, contentGETs := transferCountingHub(t, inner)

			cfg := Config{MaxActive: 1, Concurrency: 1, Verify: "sha256", Endpoint: endpoint}
			if tc.flat {
				root := t.TempDir()
				cfg.LocalDir, cfg.CacheDir = root, t.TempDir()
				destBase, err := hfdownloader.DestinationBase(root, "owner/model")
				assertNoError(t, err, "flat destination base")
				if tc.preSeed {
					assertNoError(t, os.MkdirAll(destBase, 0o755), "seed destination dir")
					assertNoError(t, os.WriteFile(filepath.Join(destBase, "weights.safetensors"), payload, 0o644), "seed destination file")
				}
			} else {
				cfg.CacheDir = t.TempDir()
				t.Setenv("HF_HUB_CACHE", filepath.Join(t.TempDir(), "hub"))
			}

			mgr := newLifecycleTestManager(t, cfg)
			run := func() *Job {
				job, _, err := mgr.CreateJob(DownloadRequest{Repo: "owner/model"})
				assertNoError(t, err, "create job")
				terminal := waitJobStatus(t, mgr, job.ID, JobStatusCompleted, 10*time.Second)
				mgr.WaitAll(10 * time.Second)
				if terminal.Status != JobStatusCompleted {
					t.Fatalf("run status = %q (err %q), want completed", terminal.Status, terminal.Error)
				}
				return terminal
			}
			assertFinished := func(job *Job) {
				t.Helper()
				p := job.Progress
				if p.TotalFiles == 0 || p.CompletedFiles != p.TotalFiles || p.DownloadedBytes != p.TotalBytes {
					t.Fatalf("fully-skipped job progress = %+v, want all files complete with DownloadedBytes == TotalBytes", p)
				}
				for _, f := range job.Files {
					if f.Status != "complete" {
						t.Fatalf("fully-skipped job file %s status = %q, want complete", f.Path, f.Status)
					}
				}
			}

			got0 := contentGETs()
			first := run()
			got1 := contentGETs()
			if tc.preSeed {
				// The FIRST run is the skipping run: zero content GETs prove
				// no payload transfer — the pre-seeded bytes are authoritative.
				if got1 != got0 {
					t.Fatalf("pre-seeded first run issued %d content GET(s), want none", got1-got0)
				}
				assertFinished(first)
				return
			}
			// The seed run must fetch real content, so the rerun's skip is a
			// real skip of previously downloaded content, not a no-op run.
			if got1 == got0 {
				t.Fatal("seed run issued no content GET: the skip would not be real")
			}
			second := run()
			got2 := contentGETs()
			if got2 != got1 {
				t.Fatalf("fully-skipped rerun issued %d content GET(s) to the hub fixture, want none", got2-got1)
			}
			assertFinished(second)
		})
	}
}
