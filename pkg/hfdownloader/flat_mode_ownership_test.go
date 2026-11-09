// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Flat routes use the external realm without creating lock-shaped payload siblings.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func flatModeSettings(out, endpoint, threshold string) Settings {
	return Settings{
		OutputDir:          out,
		OwnershipDir:       filepath.Join(filepath.Dir(out), "hfdesk-ownership"),
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

func TestDownloadFlatModeMultipartRouteUsesExternalOwnershipRealm(t *testing.T) {
	const total = 4_096
	full := rangedTestPayload(total)
	srv, getRanges := recordingHub(t, full, "model.bin", true)
	out := t.TempDir()
	dst := legacyOutputDst(t, out)

	cfg := flatModeSettings(out, srv.URL, "0")
	res := runFlatModeDownload(context.Background(), cfg)
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

	if _, err := os.Stat(dst + ".incomplete.lock"); !os.IsNotExist(err) {
		t.Errorf("an in-tree lock-shaped artifact was created, stat err=%v", err)
	}
	got := dirEntryNames(t, filepath.Dir(dst))
	want := []string{filepath.Base(dst)}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the destination directory holds %v, want exactly %v", got, want)
	}
	assertExternalRealmLockFiles(t, cfg.OwnershipDir)
}

func TestFlatDownloadKeepsLockShapedOrdinaryFilenameIndependent(t *testing.T) {
	body := []byte("ordinary file named like the former ownership lock")
	other := []byte("the destination whose suffix resembles an old lock")
	hub := newSlotLeaseHub(t, []slotLeaseFile{
		{rel: "foo", body: other},
		{rel: "foo.incomplete.lock", body: body},
	}, false)
	out := t.TempDir()
	cfg := slotLeaseFlatSettings(out, hub.srv.URL)
	cfg.OwnershipDir = filepath.Join(filepath.Dir(out), "hfdesk-ownership")
	if err := Download(context.Background(), Job{Repo: "owner/model", Revision: "main"}, cfg, nil); err != nil {
		t.Fatalf("Download: %v", err)
	}
	base, err := DestinationBase(out, "owner/model")
	if err != nil {
		t.Fatal(err)
	}
	assertExactFile(t, filepath.Join(base, "foo"), other)
	assertExactFile(t, filepath.Join(base, "foo.incomplete.lock"), body)
	if got, want := dirEntryNames(t, base), []string{"foo", "foo.incomplete.lock"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("output entries = %v, want only the two requested files %v", got, want)
	}
	assertExternalRealmLockFiles(t, cfg.OwnershipDir)
}

// A second multipart writer fails while the destination is held and neither
// creates nor overwrites it; absent and pre-existing targets are covered.
func TestDownloadFlatModeMultipartRouteExcludesSecondWriter(t *testing.T) {
	if !ownershipOSLockAvailable {
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

	cfg := flatModeSettings(out, srv.URL, "0")
	shortBlobOwnershipWait(t, 250*time.Millisecond)
	realm, err := newOwnershipRealm(cfg.OwnershipDir)
	if err != nil {
		t.Fatal(err)
	}
	flat := newFlatDestinationDomain(dst)
	owner, err := realm.acquireWriter(context.Background(), []string{dst, flat.lockSrc, dst + ".part", dst + ".incomplete.meta"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Release()

	// The probe is installed before the writer starts: the hook is a
	// package-level variable.
	contended := installBlobDomainContentionProbe(t)
	result := make(chan flatModeOutcome, 1)
	go func() { result <- runFlatModeDownload(context.Background(), cfg) }()

	var res flatModeOutcome
	gotResult := false
	select {
	case <-contended:
	case res = <-result:
		gotResult = true
		// Require contention evidence even if the result won the select race.
		select {
		case <-contended:
		default:
			t.Fatalf("the download finished without observing the contended ownership domain (err=%v)", res.err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the writer never observed the contended destination domain")
	}

	if !gotResult {
		select {
		case res = <-result:
		case <-time.After(60 * time.Second):
			t.Fatal("the download did not finish")
		}
	}

	if res.err == nil {
		t.Fatalf("the second writer reported success although a live owner holds the destination scope (skips=%v, done=%v)",
			res.skips, res.done)
	}
	if !errors.Is(res.err, errStagingOwnershipBusy) {
		t.Errorf("Download error = %v, want the exclusive-scope-held error", res.err)
	}
	if !strings.Contains(res.err.Error(), sha) {
		t.Errorf("Download error = %v, want the blob key %s in the message", res.err, sha)
	}
	if !strings.Contains(res.err.Error(), "exclusively held") || !strings.Contains(res.err.Error(), "try again later") {
		t.Errorf("Download error = %v, want the actionable scope-held message", res.err)
	}
	if len(res.skips) != 0 {
		t.Errorf("skips = %v, want none: the file is not present while the live owner holds the scope", res.skips)
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

// Confirms the single route uses external ownership without an in-tree fence.
func TestDownloadFlatModeSingleRouteUsesExternalOwnership(t *testing.T) {
	const total = 4_096
	full := rangedTestPayload(total)
	srv, getRanges := recordingHub(t, full, "model.bin", true)
	out := t.TempDir()
	dst := legacyOutputDst(t, out)

	cfg := flatModeSettings(out, srv.URL, "1MB")
	res := runFlatModeDownload(context.Background(), cfg)
	if res.err != nil {
		t.Fatalf("Download: %v", res.err)
	}
	assertExactFile(t, dst, full)
	if got := getRanges(); len(got) != 1 || got[0] != "" {
		t.Errorf("payload GETs = %v, want exactly one full (non-ranged) GET on the single path", got)
	}

	if _, err := os.Stat(dst + ".incomplete.lock"); !os.IsNotExist(err) {
		t.Errorf("an in-tree lock-shaped artifact was created, stat err=%v", err)
	}
	names := dirEntryNames(t, filepath.Dir(dst))
	wantNames := []string{filepath.Base(dst)}
	if !reflect.DeepEqual(names, wantNames) {
		t.Errorf("the destination directory holds %v, want exactly %v", names, wantNames)
	}
	assertExternalRealmLockFiles(t, cfg.OwnershipDir)
}

func assertExternalRealmLockFiles(t *testing.T, ownershipDir string) {
	t.Helper()
	realm, err := newOwnershipRealm(ownershipDir)
	if err != nil {
		t.Fatalf("create ownership realm: %v", err)
	}
	entries, err := os.ReadDir(realm.lockDir)
	if err != nil {
		t.Fatalf("read external lock directory: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("the external ownership realm contains no item locks")
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".lock") {
			t.Errorf("unexpected external ownership entry %q", entry.Name())
		}
		info, err := entry.Info()
		if err != nil {
			t.Errorf("stat external lock %s: %v", entry.Name(), err)
			continue
		}
		if info.Size() != 0 {
			t.Errorf("external lock %s is %d bytes, want zero", entry.Name(), info.Size())
		}
	}
}
