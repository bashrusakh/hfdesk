// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Flat routes and cleanup share one persistent .incomplete.lock; hooks establish contention through Download.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func flatModeSettings(out, endpoint, threshold string) Settings {
	return Settings{
		OutputDir:          out,
		Endpoint:           endpoint,
		Concurrency:        1,
		MaxActiveDownloads: 1,
		MultipartThreshold: threshold,
		Verify:             "size",
		Retries:            0,
		StallTimeout:       "0",
	}
}

type flatModeOutcome struct {
	err   error
	skips []string
	done  []string
}

func runFlatModeDownload(ctx context.Context, cfg Settings) flatModeOutcome {
	out := flatModeOutcome{}
	out.err = Download(ctx, Job{Repo: "o/r", Revision: "main"}, cfg, func(ev ProgressEvent) {
		switch {
		case ev.Event == "file_done" && strings.Contains(ev.Message, "skip"):
			out.skips = append(out.skips, ev.Message)
		case ev.Event == "file_done":
			out.done = append(out.done, ev.Path)
		}
	})
	return out
}

func dirEntryNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

func TestDownloadFlatModeMultipartRouteKeepsPersistentOwnershipLock(t *testing.T) {
	const total = 4_096
	full := rangedTestPayload(total)
	srv, getRanges := recordingHub(t, full, "model.bin", true)
	out := t.TempDir()
	dst := legacyOutputDst(t, out)

	res := runFlatModeDownload(context.Background(), flatModeSettings(out, srv.URL, "0"))
	if res.err != nil {
		t.Fatalf("Download: %v", res.err)
	}
	if len(res.skips) != 0 {
		t.Fatalf("skips = %v, want none: the file was not present before the download", res.skips)
	}
	assertExactFile(t, dst, full)
	if got := getRanges(); len(got) == 0 {
		t.Fatal("the multipart route issued no ranged GET: the route under test did not run")
	}

	lockPath := dst + ".incomplete.lock"
	fi, err := os.Stat(lockPath)
	if err != nil {
		t.Fatalf("the persistent ownership lock %s is missing: %v", filepath.Base(lockPath), err)
	}
	if fi.Size() != 0 {
		t.Errorf("the ownership lock %s is %d bytes, want a zero-byte artifact", filepath.Base(lockPath), fi.Size())
	}
	got := dirEntryNames(t, filepath.Dir(dst))
	want := []string{filepath.Base(dst), filepath.Base(lockPath)}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the destination directory holds %v, want exactly %v", got, want)
	}
}

// A second multipart writer fails while the destination is held and neither
// creates nor overwrites it; absent and pre-existing targets are covered.
func TestDownloadFlatModeMultipartRouteExcludesSecondWriter(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	for _, tc := range []struct {
		name        string
		preexisting []byte
	}{
		{name: "destination absent"},
		{name: "destination present", preexisting: []byte("pre-existing destination bytes")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testFlatModeMultipartRouteExcludesSecondWriter(t, tc.preexisting)
		})
	}
}

func testFlatModeMultipartRouteExcludesSecondWriter(t *testing.T, preexisting []byte) {
	t.Helper()
	const total = 4_096
	full := rangedTestPayload(total)
	sha := singleTestSHA(full)
	srv, getRanges := recordingHub(t, full, "model.bin", true)
	out := t.TempDir()
	dst := legacyOutputDst(t, out)

	if len(preexisting) > 0 {
		// Force the pre-download check to reach the transfer path.
		if err := os.WriteFile(dst, preexisting, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	shortBlobOwnershipWait(t, 250*time.Millisecond)
	owner, err := acquireStagingOwnershipLock(dst + ".incomplete")
	if err != nil {
		t.Fatal(err)
	}
	defer owner.release()

	// The probe is installed before the writer starts: the hook is a
	// package-level variable.
	contended := installDomainContentionProbe(t)
	result := make(chan flatModeOutcome, 1)
	go func() { result <- runFlatModeDownload(context.Background(), flatModeSettings(out, srv.URL, "0")) }()

	select {
	case <-contended:
	case res := <-result:
		// Require contention evidence even if the result won the select race.
		select {
		case <-contended:
		default:
			t.Fatalf("the download finished without observing the contended ownership lock (err=%v)", res.err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the writer never observed the contended destination domain")
	}

	var res flatModeOutcome
	select {
	case res = <-result:
	case <-time.After(60 * time.Second):
		t.Fatal("the download did not finish")
	}

	if res.err == nil {
		t.Fatalf("the second writer reported success although a live owner holds the staging lock (skips=%v, done=%v)",
			res.skips, res.done)
	}
	if !errors.Is(res.err, errBlobOwnedByLiveWriter) {
		t.Errorf("Download error = %v, want the actionable live-owner error", res.err)
	}
	if !strings.Contains(res.err.Error(), sha) {
		t.Errorf("Download error = %v, want the blob key %s in the message", res.err, sha)
	}
	if !strings.Contains(res.err.Error(), "is being downloaded by another process") ||
		!strings.Contains(res.err.Error(), "try again later") {
		t.Errorf("Download error = %v, want the actionable live-owner message", res.err)
	}
	if len(res.skips) != 0 {
		t.Errorf("skips = %v, want none: the file is not present while the live owner holds the lock", res.skips)
	}
	if len(res.done) != 0 {
		t.Errorf("file_done events = %v, want none: the file is not present", res.done)
	}
	if got := getRanges(); len(got) != 0 {
		t.Errorf("the excluded writer issued %d payload GETs (%v), want 0", len(got), got)
	}
	if _, serr := os.Stat(dst + ".incomplete"); !os.IsNotExist(serr) {
		t.Errorf("the excluded writer created the staging payload, stat err=%v", serr)
	}
	if _, serr := os.Stat(dst + ".incomplete.meta"); !os.IsNotExist(serr) {
		t.Errorf("the excluded writer created the staging record, stat err=%v", serr)
	}
	if len(preexisting) == 0 {
		if _, serr := os.Stat(dst); !os.IsNotExist(serr) {
			t.Errorf("the excluded writer created the destination, stat err=%v", serr)
		}
		return
	}
	assertExactFile(t, dst, preexisting)
}

// Confirms the single route uses the same persistent .incomplete.lock as the
// multipart route and cleanup.
func TestDownloadFlatModeSingleRouteTakesTheDestinationDomain(t *testing.T) {
	const total = 4_096
	full := rangedTestPayload(total)
	srv, getRanges := recordingHub(t, full, "model.bin", true)
	out := t.TempDir()
	dst := legacyOutputDst(t, out)

	// Observe the production acquisition without changing its behavior.
	var (
		mu       sync.Mutex
		acquired []string
	)
	oldAcquire := stagingOwnershipAcquire
	stagingOwnershipAcquire = func(staging string) (*stagingOwnershipLock, error) {
		mu.Lock()
		acquired = append(acquired, staging)
		mu.Unlock()
		return oldAcquire(staging)
	}
	defer func() { stagingOwnershipAcquire = oldAcquire }()

	res := runFlatModeDownload(context.Background(), flatModeSettings(out, srv.URL, "1MB"))
	if res.err != nil {
		t.Fatalf("Download: %v", res.err)
	}
	assertExactFile(t, dst, full)
	if got := getRanges(); len(got) != 1 || got[0] != "" {
		t.Errorf("payload GETs = %v, want exactly one full (non-ranged) GET on the single path", got)
	}

	mu.Lock()
	got := append([]string(nil), acquired...)
	mu.Unlock()
	want := []string{dst + ".incomplete"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ownership-lock acquisitions = %v, want exactly %v: the single-request route must hold the destination's domain", got, want)
	}

	lockPath := dst + ".incomplete.lock"
	fi, err := os.Stat(lockPath)
	if err != nil {
		t.Fatalf("the persistent ownership lock %s is missing: %v", filepath.Base(lockPath), err)
	}
	if fi.Size() != 0 {
		t.Errorf("the ownership lock %s is %d bytes, want a zero-byte artifact", filepath.Base(lockPath), fi.Size())
	}
	names := dirEntryNames(t, filepath.Dir(dst))
	wantNames := []string{filepath.Base(dst), filepath.Base(lockPath)}
	if !reflect.DeepEqual(names, wantNames) {
		t.Errorf("the destination directory holds %v, want exactly %v", names, wantNames)
	}
}
