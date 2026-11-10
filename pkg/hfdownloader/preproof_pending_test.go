// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// A pending known-digest proof must survive a crash, failed cleanup, or a
// Verify-mode change.

import (
	"bytes"
	"context"
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
)

func proofPendingRaw(t *testing.T, metaPath string) bool {
	t.Helper()
	raw, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("read staging record %s: %v", filepath.Base(metaPath), err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("parse staging record %s: %v", filepath.Base(metaPath), err)
	}
	v, ok := fields["proof_pending"]
	return ok && string(v) == "true"
}

func setProofPendingRaw(t *testing.T, metaPath string, pending bool) {
	t.Helper()
	raw, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("read staging record %s: %v", filepath.Base(metaPath), err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("parse staging record %s: %v", filepath.Base(metaPath), err)
	}
	if pending {
		fields["proof_pending"] = json.RawMessage("true")
	} else {
		delete(fields, "proof_pending")
	}
	data, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metaPath, data, 0o644); err != nil {
		t.Fatalf("write staging record %s: %v", filepath.Base(metaPath), err)
	}
}

// proofRecoveryHub serves one LFS file and counts ranged GETs to distinguish
// re-download from publication of surviving staging bytes.
func proofRecoveryHub(t *testing.T, content []byte) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	sha := singleTestSHA(content)
	var gets atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/revision/main"):
			_, _ = w.Write([]byte(`{"sha":"commit123"}`))
		case strings.Contains(r.URL.Path, "/tree/"):
			_, _ = w.Write([]byte(fmt.Sprintf(
				`[{"type":"file","path":"model.bin","size":%d,"lfs":{"oid":"%s","size":%d,"sha256":"%s"}}]`,
				len(content), sha, len(content), sha)))
		case strings.Contains(r.URL.Path, "/resolve/"):
			if r.Method == http.MethodGet && r.Header.Get("Range") != "" {
				gets.Add(1)
			}
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(content))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &gets
}

// If mismatch cleanup cannot acquire the metadata gate, a later size-mode run
// must re-prove or discard the surviving all-Done state, never publish it unchecked.
func TestStagingProofPendingSurvivesCleanupFailure(t *testing.T) {
	const total = 64 * 1024
	good := rangedTestPayload(total)
	bad := corruptSameSize(good)
	sha := singleTestSHA(good)

	srv := prePublishHub(t, good, bad)
	cacheDir := t.TempDir()
	blobsDir := filepath.Join(cacheDir, "hub", "models--owner--model", "blobs")
	staging := filepath.Join(blobsDir, sha+".incomplete")
	metaPath := staging + ".meta"

	cfg := Settings{
		CacheDir:           cacheDir,
		Endpoint:           srv.URL,
		Concurrency:        4,
		MaxActiveDownloads: 1,
		MultipartThreshold: "1KB",
		Verify:             "etag",
		Retries:            0,
		StallTimeout:       "0",
	}

	var gate *stagingStateLock
	err := Download(context.Background(), Job{Repo: "owner/model", Revision: "main"}, cfg, func(ev ProgressEvent) {
		if ev.Event != "file_finalizing" || gate != nil {
			return
		}
		gate = &stagingStateLock{path: stagingLockPath(metaPath)}
		if lockErr := gate.acquire(context.Background(), time.Second); lockErr != nil {
			t.Errorf("hold metadata gate during finalization: %v", lockErr)
			gate = nil
		}
	})
	if gate == nil {
		t.Fatal("finalization did not hold the metadata gate")
	}
	gate.release()
	if err == nil || !strings.Contains(err.Error(), "sha256 verify failed") {
		t.Fatalf("Download error = %v, want a sha256 verification failure", err)
	}
	if _, serr := os.Stat(metaPath); serr != nil {
		t.Fatalf("record should survive blocked owned cleanup: %v", serr)
	}
	if !proofPendingRaw(t, metaPath) {
		t.Error("durable ProofPending flag did not survive blocked cleanup")
	}
	if _, serr := os.Stat(staging); serr != nil {
		t.Fatalf("staging payload should survive blocked cleanup: %v", serr)
	}
	if _, serr := os.Stat(filepath.Join(blobsDir, sha)); !os.IsNotExist(serr) {
		t.Errorf("a corrupt blob was published despite the mismatch, stat err=%v", serr)
	}

	// With the gate released, a later Verify=size run must not publish surviving
	// corrupt bytes with zero GETs. The correct source permits safe retry.
	srv2, payloadGets := proofRecoveryHub(t, good)
	sizeCfg := cfg
	sizeCfg.Endpoint = srv2.URL
	sizeCfg.Verify = "size"
	sizeErr := Download(context.Background(), Job{Repo: "owner/model", Revision: "main"}, sizeCfg, func(ProgressEvent) {})
	blobPath := filepath.Join(blobsDir, sha)
	blobExists := false
	if _, serr := os.Stat(blobPath); serr == nil {
		blobExists = true
	}
	if payloadGets.Load() == 0 {
		if sizeErr == nil || blobExists {
			t.Fatalf("the surviving record was laundered in size mode with zero GETs: published=%v err=%v", blobExists, sizeErr)
		}
	} else {
		if sizeErr != nil {
			t.Fatalf("size re-download failed: %v", sizeErr)
		}
		assertExactFile(t, blobPath, good)
	}
	for _, p := range []string{staging, metaPath} {
		if _, serr := os.Stat(p); !os.IsNotExist(serr) {
			t.Errorf("%s survived the size-mode recovery, stat err=%v", filepath.Base(p), serr)
		}
	}
}

// An all-Done ProofPending record must receive one proof in size mode:
// corrupt bytes fail without publication; matching bytes publish.
func TestStagingProofPendingForcesReproofInSizeMode(t *testing.T) {
	const total = 64 * 1024
	good := rangedTestPayload(total)
	sha := singleTestSHA(good)
	ranges := []RangeProgress{{Start: 0, End: total - 1, Committed: total, Done: true}}

	cases := []struct {
		name      string
		payload   []byte
		wantError bool
	}{
		{name: "corrupt payload", payload: corruptSameSize(good), wantError: true},
		{name: "matching payload", payload: good, wantError: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			staging := filepath.Join(dir, "proof.bin.incomplete")
			pub := filepath.Join(dir, "proof.bin")
			seedRangedState(t, staging, "o/r", "proof.bin", total, ranges, tc.payload)
			meta, err := readStagingMeta(staging + ".meta")
			if err != nil {
				t.Fatal(err)
			}
			meta.SHA256 = sha
			if err := writeStagingMeta(staging+".meta", meta); err != nil {
				t.Fatal(err)
			}
			setProofPendingRaw(t, staging+".meta", true)

			srv, gotRanges := rangedTestServer(t, good)
			it := PlanItem{RelativePath: "proof.bin", URL: srv.URL + "/proof.bin", Size: total, AcceptRanges: true, LFS: true, SHA256: sha}
			cfg := Settings{Verify: "size", Concurrency: 1, Retries: 0, StallTimeout: "0"}

			hashPassCount.Store(0)
			verified, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, pub,
				withTestDomain(t, pub, multipartOpts{Staging: staging, Publish: pub, Commit: rangedSeedCommit, CommitPinned: true}), func(ProgressEvent) {})
			if len(*gotRanges) != 0 {
				t.Errorf("all-Done record issued %v; the proof alone should run", *gotRanges)
			}
			if got := hashPassCount.Load(); got != 1 {
				t.Errorf("size run with ProofPending performed %d full-file passes; want exactly 1", got)
			}
			if tc.wantError {
				if err == nil || !strings.Contains(err.Error(), "sha256 verify failed") {
					t.Fatalf("corrupt ProofPending payload error = %v, want a sha256 verification failure", err)
				}
				if verified {
					t.Error("a failed forced proof reported contentVerified")
				}
				if _, serr := os.Stat(pub); !os.IsNotExist(serr) {
					t.Errorf("corrupt ProofPending payload was published, stat err=%v", serr)
				}
				return
			}
			if err != nil {
				t.Fatalf("matching ProofPending payload failed: %v", err)
			}
			if !verified {
				t.Error("the forced size proof did not report contentVerified")
			}
			assertExactFile(t, pub, good)
			for _, p := range []string{staging, staging + ".meta"} {
				if _, serr := os.Stat(p); !os.IsNotExist(serr) {
					t.Errorf("%s survived a successful publish, stat err=%v", filepath.Base(p), serr)
				}
			}
		})
	}
}

// Fail publication after proof so the cleared durable flag remains inspectable;
// a later size-mode resume must publish without another hash pass.
func TestStagingProofSuccessClearsPendingDurably(t *testing.T) {
	const total = 64 * 1024
	good := rangedTestPayload(total)
	sha := singleTestSHA(good)

	dir := t.TempDir()
	staging := filepath.Join(dir, "clear.bin.incomplete")
	pub := filepath.Join(dir, "clear.bin")
	ranges := []RangeProgress{{Start: 0, End: total - 1, Committed: total, Done: true}}
	seedRangedState(t, staging, "o/r", "clear.bin", total, ranges, good)
	meta, err := readStagingMeta(staging + ".meta")
	if err != nil {
		t.Fatal(err)
	}
	meta.SHA256 = sha
	if err := writeStagingMeta(staging+".meta", meta); err != nil {
		t.Fatal(err)
	}
	setProofPendingRaw(t, staging+".meta", true)

	// Force the post-proof publish rename to fail: a non-empty directory
	// cannot be replaced by a file rename on the supported platforms.
	if err := os.MkdirAll(filepath.Join(pub, "occupied"), 0o755); err != nil {
		t.Fatal(err)
	}

	srv, _ := rangedTestServer(t, good)
	it := PlanItem{RelativePath: "clear.bin", URL: srv.URL + "/clear.bin", Size: total, AcceptRanges: true, LFS: true, SHA256: sha}
	cfg := Settings{Verify: "size", Concurrency: 1, Retries: 0, StallTimeout: "0"}

	hashPassCount.Store(0)
	verified, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, pub,
		withTestDomain(t, pub, multipartOpts{Staging: staging, Publish: pub, Commit: rangedSeedCommit, CommitPinned: true}), func(ProgressEvent) {})
	if err == nil {
		t.Fatal("publish over a non-empty directory unexpectedly succeeded")
	}
	if !verified {
		t.Error("the forced proof succeeded but contentVerified was false")
	}
	if got := hashPassCount.Load(); got != 1 {
		t.Errorf("forced proof performed %d full-file passes; want exactly 1", got)
	}
	if _, serr := os.Stat(staging + ".meta"); serr != nil {
		t.Fatalf("record must survive the failed publish: %v", serr)
	}
	if proofPendingRaw(t, staging+".meta") {
		t.Error("ProofPending was not cleared durably after a successful proof")
	}

	// A subsequent size run trusts the proven record without another hash pass.
	if err := os.RemoveAll(pub); err != nil {
		t.Fatal(err)
	}
	hashPassCount.Store(0)
	verified, err = downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, pub,
		withTestDomain(t, pub, multipartOpts{Staging: staging, Publish: pub, Commit: rangedSeedCommit, CommitPinned: true}), func(ProgressEvent) {})
	if err != nil {
		t.Fatalf("size resume of a proven record: %v", err)
	}
	if verified {
		t.Error("a proven record must not be re-proved in size mode")
	}
	if got := hashPassCount.Load(); got != 0 {
		t.Errorf("size resume of a proven record performed %d full-file passes; want 0", got)
	}
	assertExactFile(t, pub, good)
	for _, p := range []string{staging, staging + ".meta"} {
		if _, serr := os.Stat(p); !os.IsNotExist(serr) {
			t.Errorf("%s survived the successful publish, stat err=%v", filepath.Base(p), serr)
		}
	}
}
