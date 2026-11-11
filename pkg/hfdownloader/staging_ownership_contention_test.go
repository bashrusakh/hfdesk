// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// A shared external-scope probe can conflict with an exclusive claim. That
// contention is not proof of a live writer: retry or fail rather than report a
// successful skip. Tests exercise this through Download.

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// startExternalScopeProbeHolder takes the current external realm's shared
// writer probe over the given paths; a shared probe is never a writer.

func startExternalScopeProbeHolder(t *testing.T, paths ...string) *ownershipLease {
	t.Helper()
	ownerDir, err := resolveOwnershipDir("")
	if err != nil {
		t.Fatal(err)
	}
	realm, err := newOwnershipRealm(ownerDir)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := realm.probeWriterLease(context.Background(), paths)
	if err != nil {
		t.Fatalf("acquire shared external scope probe: %v", err)
	}
	return lease
}

type contentionDownloadFixture struct {
	srv       *httptest.Server
	out       string
	dst       string
	staging   string
	metaPath  string
	sha       string
	full      []byte
	total     int
	getRanges func() []string
}

// newContentionDownloadFixture seeds a valid resumable prefix.
func newContentionDownloadFixture(t *testing.T) *contentionDownloadFixture {
	t.Helper()
	const total = 4_096
	full := rangedTestPayload(total)
	sha := singleTestSHA(full)
	srv, getRanges := recordingHub(t, full, "model.bin", true)
	out := t.TempDir()
	dst := legacyOutputDst(t, out)
	staging := dst + ".incomplete"
	metaPath := staging + ".meta"
	seedRangedState(t, staging, "o/r", "model.bin", total,
		[]RangeProgress{{Start: 0, End: total - 1, Committed: total / 2}}, full)
	meta, err := readStagingMeta(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	meta.Commit = "commit123"
	meta.SHA256 = sha
	meta.LockFenced = true
	if err := writeStagingMeta(metaPath, meta); err != nil {
		t.Fatal(err)
	}
	return &contentionDownloadFixture{
		srv: srv, out: out, dst: dst, staging: staging, metaPath: metaPath,
		sha: sha, full: full, total: total, getRanges: getRanges,
	}
}

func (f *contentionDownloadFixture) settings() Settings {
	return Settings{
		OutputDir:          f.out,
		Endpoint:           f.srv.URL,
		Concurrency:        1,
		MaxActiveDownloads: 1,
		MultipartThreshold: "1MB",
		Verify:             "size",
		Retries:            0,
		StallTimeout:       "0",
	}
}

type downloadOutcome struct {
	skips  []string
	err    error
	events []ProgressEvent
}

func (f *contentionDownloadFixture) runDownload(t *testing.T, ctx context.Context) downloadOutcome {
	t.Helper()
	out := downloadOutcome{}
	err := Download(ctx, Job{Repo: "o/r", Revision: "main"}, f.settings(), func(ev ProgressEvent) {
		out.events = append(out.events, ev)
		if ev.Event == "file_done" && strings.Contains(ev.Message, "skip") {
			out.skips = append(out.skips, ev.Message)
		}
	})
	out.err = err
	return out
}

// A shared status probe may block exclusive acquisition without being a
// writer; the claim must retry and publish. The hook signals the exact wait.
func TestDownload_StatusProbeInFlightDoesNotSkipTheFile(t *testing.T) {
	f := newContentionDownloadFixture(t)

	probe := startExternalScopeProbeHolder(t, f.dst)
	if err := probe.Validate(); err != nil {
		t.Fatal("the probe did not take the lock file")
	}
	defer probe.Release()

	// The acquisition hook signals contention while the test still holds the probe.
	contended := make(chan struct{})
	var once sync.Once
	restore := blobDomainContentionHook
	blobDomainContentionHook = func(string) { once.Do(func() { close(contended) }) }
	defer func() { blobDomainContentionHook = restore }()

	result := make(chan downloadOutcome, 1)
	go func() { result <- f.runDownload(t, context.Background()) }()

	select {
	case <-contended:
	case out := <-result:
		// Early completion is acceptable only as non-skipping success.
		if len(out.skips) > 0 {
			t.Fatalf("the job skipped the file while a probe held the lock: %v", out.skips)
		}
		if out.err != nil {
			t.Fatalf("Download: %v", out.err)
		}
		return
	case <-time.After(30 * time.Second):
		t.Fatal("the claim never observed the contended ownership lock")
	}

	probe.Release()

	var out downloadOutcome
	select {
	case out = <-result:
	case <-time.After(30 * time.Second):
		t.Fatal("Download did not finish after the probe released the lock")
	}

	if len(out.skips) > 0 {
		t.Fatalf("the job recorded a successful skip while a probe held the lock: %v", out.skips)
	}
	if out.err != nil {
		t.Fatalf("Download: %v", out.err)
	}
	assertExactFile(t, f.dst, f.full)
	if got := f.getRanges(); len(got) != 1 || got[0] != fmt.Sprintf("bytes=%d-%d", f.total/2, f.total-1) {
		t.Errorf("ranged GETs = %v, want exactly the uncommitted suffix", got)
	}
}

// Unresolved probe contention must fail instead of skipping a missing file.
func TestDownload_UnresolvableContentionFailsInsteadOfSkipping(t *testing.T) {
	f := newContentionDownloadFixture(t)

	probe := startExternalScopeProbeHolder(t, f.dst)
	defer probe.Release()

	// Shrink the acquisition window so the test does not wait the production
	// bound; the behavior under test is the classification, not the duration.
	shortBlobOwnershipWait(t, 300*time.Millisecond)

	// Prove the contention really happened while the probe held the lock.
	contended := make(chan struct{})
	var once sync.Once
	restoreHook := blobDomainContentionHook
	blobDomainContentionHook = func(string) { once.Do(func() { close(contended) }) }
	defer func() { blobDomainContentionHook = restoreHook }()

	out := f.runDownload(t, context.Background())

	select {
	case <-contended:
	default:
		t.Fatal("the claim never observed the contended ownership lock")
	}
	if len(out.skips) > 0 {
		t.Fatalf("the job recorded a successful skip although no live writer was proven: %v", out.skips)
	}
	if out.err == nil {
		t.Fatal("Download succeeded although the ownership lock never became free")
	}
	if !errors.Is(out.err, errStagingOwnershipUncertain) {
		t.Errorf("Download error = %v, want an uncertain-scope failure while a shared probe is held", out.err)
	}
	if _, err := os.Stat(f.dst); err == nil {
		t.Error("the destination exists although the download failed")
	}
	if _, err := os.Stat(f.staging); err != nil {
		t.Errorf("the resumable staging file was removed by the failed claim: %v", err)
	}
}

// Cancellation interrupts a contended ownership wait without a skip.
func TestDownload_CancellationWhileWaitingForOwnershipIsPrompt(t *testing.T) {
	f := newContentionDownloadFixture(t)

	probe := startExternalScopeProbeHolder(t, f.dst)
	defer probe.Release()

	// Cancel only after destination-domain acquisition reports lock contention.
	contended := make(chan struct{})
	var once sync.Once
	restoreHook := blobDomainContentionHook
	blobDomainContentionHook = func(string) { once.Do(func() { close(contended) }) }
	defer func() { blobDomainContentionHook = restoreHook }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan downloadOutcome, 1)
	go func() { result <- f.runDownload(t, ctx) }()

	select {
	case <-contended:
	case out := <-result:
		if len(out.skips) > 0 {
			t.Fatalf("the job skipped the file although it was cancelled: %v", out.skips)
		}
		if out.err == nil {
			t.Fatal("Download succeeded although it was cancelled")
		}
		return
	case <-time.After(30 * time.Second):
		t.Fatal("the claim never observed the contended ownership lock")
	}

	cancel()

	var out downloadOutcome
	select {
	case out = <-result:
	case <-time.After(10 * time.Second):
		t.Fatal("Download did not stop after its context was cancelled")
	}
	if len(out.skips) > 0 {
		t.Fatalf("the job recorded a successful skip although it was cancelled: %v", out.skips)
	}
	if out.err == nil {
		t.Fatal("Download succeeded although it was cancelled")
	}
	if !errors.Is(out.err, context.Canceled) {
		t.Errorf("Download error = %v, want context.Canceled", out.err)
	}
}
