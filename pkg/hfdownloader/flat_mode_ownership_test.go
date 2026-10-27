// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Regression coverage for the flat/output-mode exclusion contract.
//
// A flat/output (legacy) destination owns ONE ownership domain, and both
// routes and the cleanup resolve that ONE identity. The domain's lifetime
// lock is the persistent sibling of the MULTIPART staging name:
// <destination>.incomplete.lock - the zero-byte artifact the multipart route
// already leaves next to the user's model file and never unlinks. The
// MULTIPART route takes that lock through its staging claim; the
// SINGLE-REQUEST route holds the same domain while it transfers. One lock
// file therefore excludes every pair of writers of the same flat
// destination: single-vs-single, single-vs-multipart and
// multipart-vs-multipart.
//
// Before the flat destination owned a domain, the single-request route took
// no lock at all, so two writers of the same flat destination interleaved
// over the same payload and could publish corrupt bytes.
//
// The cases below drive the real Download path, so they exercise the product
// routing and not just the raw lock primitive. The synchronization is
// deterministic: the live owner holds the OS ownership lock through the
// package's own lock primitive (the lock belongs to the OPEN FILE
// DESCRIPTION, not to the process, so a second open in this process conflicts
// exactly like another process's writer does - the model the shipped
// ownership contract cases already use), and the production contention hook
// proves that the second writer really hit the contended lock instead of
// inferring it from a sleep.

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

// flatModeSettings returns the Download settings of the flat/output-mode
// cases: the legacy output directory and no cache directory (so the
// downloader uses the flat layout), one connection, no retries, and the
// threshold that selects the route under test.
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

// flatModeOutcome is what one flat-mode Download reported.
type flatModeOutcome struct {
	err   error
	skips []string
	done  []string
}

// runFlatModeDownload runs one flat-mode Download and collects its outcome.
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

// dirEntryNames returns the sorted names of the entries in dir.
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

// TestDownloadFlatModeMultipartRouteKeepsPersistentOwnershipLock pins the
// artifact shape of the flat/output-mode MULTIPART route.
//
// A successful flat-mode multipart download leaves the destination file AND
// the persistent zero-byte <destination>.incomplete.lock next to it: the
// staging payload became the destination and its record is removed by the
// successful publish, but the ownership lock is never unlinked. That lock is
// the artifact that excludes a concurrent second writer of the same flat
// destination, so its absence would silently re-open the race the route is
// supposed to close.
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
	// The multipart route really ran: it transferred the payload in ranges.
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
	// The lock is the ONLY artifact the route leaves next to the destination.
	got := dirEntryNames(t, filepath.Dir(dst))
	want := []string{filepath.Base(dst), filepath.Base(lockPath)}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the destination directory holds %v, want exactly %v", got, want)
	}
}

// TestDownloadFlatModeMultipartRouteExcludesSecondWriter pins the exclusion
// itself: a second writer of the same flat destination on the multipart route
// fails with the actionable live-owner error while a PROVEN live owner holds
// the destination's domain, and it neither creates, overwrites nor corrupts
// the destination.
//
// Both destination states are covered, because they are different failure
// modes: the excluded writer must not CREATE the destination it was not
// allowed to publish, and it must not OVERWRITE a destination that already
// holds other bytes.
func TestDownloadFlatModeMultipartRouteExcludesSecondWriter(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	for _, tc := range []struct {
		name string
		// preexisting is the content the destination holds before the
		// excluded writer runs. An empty value means the destination does
		// not exist yet.
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

// testFlatModeMultipartRouteExcludesSecondWriter is the body of the exclusion
// case, once per destination state.
func testFlatModeMultipartRouteExcludesSecondWriter(t *testing.T, preexisting []byte) {
	t.Helper()
	const total = 4_096
	full := rangedTestPayload(total)
	sha := singleTestSHA(full)
	srv, getRanges := recordingHub(t, full, "model.bin", true)
	out := t.TempDir()
	dst := legacyOutputDst(t, out)

	if len(preexisting) > 0 {
		// A destination that holds other bytes is not skipped by the
		// pre-download check (its size differs), so the item really reaches
		// the multipart route instead of being declined.
		if err := os.WriteFile(dst, preexisting, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// The live owner holds the destination's one lifetime OS ownership lock
	// (the multipart staging name's persistent sibling) for the whole
	// interval. The excluded writer waits out its bounded budget and fails
	// with the actionable live-owner error.
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
		// The writer really hit the contended destination domain.
	case res := <-result:
		// The download already finished. The exclusion must be proven, not
		// inferred, so the contention is checked before the outcome is
		// accepted.
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

	// The item fails with the actionable live-owner error.
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
	// A live owner means the file is NOT present: a skip or a file_done
	// would report a success the output directory does not have.
	if len(res.skips) != 0 {
		t.Errorf("skips = %v, want none: the file is not present while the live owner holds the lock", res.skips)
	}
	if len(res.done) != 0 {
		t.Errorf("file_done events = %v, want none: the file is not present", res.done)
	}
	// The excluded writer transferred nothing and staged nothing.
	if got := getRanges(); len(got) != 0 {
		t.Errorf("the excluded writer issued %d payload GETs (%v), want 0", len(got), got)
	}
	if _, serr := os.Stat(dst + ".incomplete"); !os.IsNotExist(serr) {
		t.Errorf("the excluded writer created the staging payload, stat err=%v", serr)
	}
	if _, serr := os.Stat(dst + ".incomplete.meta"); !os.IsNotExist(serr) {
		t.Errorf("the excluded writer created the staging record, stat err=%v", serr)
	}
	// The destination is untouched: not created, and not overwritten with
	// the payload.
	if len(preexisting) == 0 {
		if _, serr := os.Stat(dst); !os.IsNotExist(serr) {
			t.Errorf("the excluded writer created the destination, stat err=%v", serr)
		}
		return
	}
	assertExactFile(t, dst, preexisting)
}

// TestDownloadFlatModeSingleRouteTakesTheDestinationDomain pins the identity
// the single-request route uses in flat/output mode.
//
// A flat destination owns ONE domain, and the domain's lifetime lock is the
// persistent sibling of the MULTIPART staging name:
// <destination>.incomplete.lock - the item-level destination-domain lock every
// route acquires exactly once before the HEAD/claim; the multipart claim takes
// no lock of its own. The single-request route holds that same domain while
// it transfers, so the two routes are mutually excluded, and the cleanup
// resolves the same lock.
//
// The artifact shape is pinned too: the route leaves the destination and the
// zero-byte <destination>.incomplete.lock next to it. That lock file is NOT a
// new artifact name - the multipart route already leaves exactly that file -
// so the flat namespace gains no second file next to the user's model file.
func TestDownloadFlatModeSingleRouteTakesTheDestinationDomain(t *testing.T) {
	const total = 4_096
	full := rangedTestPayload(total)
	srv, getRanges := recordingHub(t, full, "model.bin", true)
	out := t.TempDir()
	dst := legacyOutputDst(t, out)

	// Record every ownership-lock acquisition of the run. The seam wraps the
	// production acquisition, so the behavior under test is unchanged.
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
	// The single-request route really ran: one full GET, no ranged GET.
	if got := getRanges(); len(got) != 1 || got[0] != "" {
		t.Errorf("payload GETs = %v, want exactly one full (non-ranged) GET on the single path", got)
	}

	mu.Lock()
	got := append([]string(nil), acquired...)
	mu.Unlock()
	// The route holds the destination's domain, whose lock source is the
	// MULTIPART staging name - the same lock the multipart claim takes.
	want := []string{dst + ".incomplete"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ownership-lock acquisitions = %v, want exactly %v: the single-request route must hold the destination's domain", got, want)
	}

	// The persistent lock file is the one the multipart route already leaves:
	// no NEW artifact name appears next to the user's model file.
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
