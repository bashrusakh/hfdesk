// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package hfdownloader

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const ownershipCaseFixtureCommit = "1234567890abcdef1234567890abcdef12345678"

// assertDirectoryUnreadable pins the real unprivileged permission fixture:
// directory read denied, search (path traversal) still working.
func assertDirectoryUnreadable(t *testing.T, dir string) {
	t.Helper()
	if handle, err := os.Open(dir); err == nil {
		handle.Close()
		t.Fatal("directory is unexpectedly readable; the no-read case is not effective")
	} else if !os.IsPermission(err) {
		t.Fatalf("directory open error = %v, want permission denied", err)
	}
	if _, err := os.ReadDir(dir); !os.IsPermission(err) {
		t.Fatalf("directory readdir error = %v, want permission denied", err)
	}
}

// TestDownloadWithoutPayloadDirectoryRead is the H/blobs 0300 regression from
// the native permission check: cache download must reach HTTP and publish
// exact bytes without READ permission on payload directories (owner
// write+search only), exactly like baseline 339f34c8 did.
func TestDownloadWithoutPayloadDirectoryRead(t *testing.T) {
	for _, tc := range []struct {
		name     string
		restrict string // "" = normal-read control, "hub"/"blobs" = mode 0300 parent
	}{
		{name: "normal-read", restrict: ""},
		{name: "hub-no-read", restrict: "hub"},
		{name: "blobs-no-read", restrict: "blobs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runOwnershipCaseDownloadFixture(t, tc.restrict)
		})
	}
}

func runOwnershipCaseDownloadFixture(t *testing.T, restrict string) {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "cache")
	hub := filepath.Join(root, "hub")
	repoPath := filepath.Join(hub, "models--owner--model")
	blobs := filepath.Join(repoPath, "blobs")
	for _, dir := range []string{
		blobs,
		filepath.Join(repoPath, "snapshots"),
		filepath.Join(repoPath, "refs"),
		filepath.Join(hub, ".hfdesk-control", "v1"),
		filepath.Join(root, ".hfdesk-control", "v1"),
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	parent := hub
	if restrict == "blobs" {
		parent = blobs
	}
	payload := bytes.Repeat([]byte("HFDesk-permission-fixture\n"), 256)
	sum := sha256.Sum256(payload)
	sha := hex.EncodeToString(sum[:])
	source := filepath.Join(parent, "named-readable-source.bin")
	if err := os.WriteFile(source, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	mode := os.FileMode(0o700)
	if restrict != "" {
		mode = 0o300
	}
	if err := os.Chmod(parent, mode); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })

	// The fixture must really enforce the regression preconditions: no
	// directory read, named reads and creation allowed, control space writable.
	if restrict != "" {
		assertDirectoryUnreadable(t, parent)
	}
	if got, err := os.ReadFile(source); err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("named payload read failed: %v", err)
	}
	namedCreate := filepath.Join(parent, "named-create-check")
	if err := os.WriteFile(namedCreate, []byte("create works"), 0o600); err != nil {
		t.Fatalf("named creation denied: %v", err)
	}
	_ = os.Remove(namedCreate)
	for _, control := range []string{
		filepath.Join(hub, ".hfdesk-control", "v1"),
		filepath.Join(root, ".hfdesk-control", "v1"),
	} {
		probe := filepath.Join(control, "writable-control-check")
		if err := os.WriteFile(probe, []byte("writable"), 0o600); err != nil {
			t.Fatalf("control space %s not writable: %v", control, err)
		}
		_ = os.Remove(probe)
	}

	var mu sync.Mutex
	var requests []string
	fileDone := 0
	var plan *Plan
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.RequestURI())
		mu.Unlock()
		w.Header().Set("X-Repo-Commit", ownershipCaseFixtureCommit)
		switch r.URL.Path {
		case "/api/models/owner/model/revision/main":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"sha":%q}`, ownershipCaseFixtureCommit)
		case "/api/models/owner/model/tree/" + ownershipCaseFixtureCommit:
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `[{"type":"file","path":"model.bin","size":%d,"lfs":{"oid":%q,"sha256":%q,"size":%d}}]`,
				len(payload), sha, sha, len(payload))
		case "/owner/model/resolve/" + ownershipCaseFixtureCommit + "/model.bin",
			"/owner/model/raw/" + ownershipCaseFixtureCommit + "/model.bin":
			w.Header().Set("ETag", `"`+sha+`"`)
			http.ServeContent(w, r, "model.bin", time.Time{}, bytes.NewReader(payload))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	settings := Settings{
		CacheDir:           root,
		HubDir:             hub,
		Endpoint:           server.URL,
		Concurrency:        1,
		MaxActiveDownloads: 1,
		MultipartThreshold: "1MiB",
		Verify:             "sha256",
		Retries:            0,
		StallTimeout:       "0",
	}
	settings.OnPlanComplete = func(p Plan) {
		mu.Lock()
		defer mu.Unlock()
		q := p
		plan = &q
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	downloadErr := Download(ctx, Job{Repo: "owner/model", Revision: "main"}, settings, func(e ProgressEvent) {
		mu.Lock()
		defer mu.Unlock()
		if e.Event == "file_done" {
			fileDone++
		}
	})
	mu.Lock()
	reached := len(requests)
	mu.Unlock()
	if downloadErr != nil {
		t.Fatalf("Download without directory read failed after %d HTTP requests: %v", reached, downloadErr)
	}
	if reached == 0 {
		t.Fatal("Download never reached the HTTP fixture")
	}
	if fileDone != 1 {
		t.Fatalf("file_done events = %d, want 1", fileDone)
	}
	if plan == nil || !plan.CommitPinned || !plan.CommitResolved ||
		plan.Commit != ownershipCaseFixtureCommit || len(plan.Items) != 1 ||
		!plan.Items[0].LFS || plan.Items[0].SHA256 != sha {
		t.Fatalf("plan was not resolved and pinned to one LFS item: %+v", plan)
	}
	for key, path := range map[string]string{
		"blob":     filepath.Join(blobs, sha),
		"snapshot": filepath.Join(repoPath, "snapshots", ownershipCaseFixtureCommit, "model.bin"),
		"friendly": filepath.Join(root, "models", "owner", "model", "model.bin"),
	} {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, payload) {
			t.Errorf("%s path %q not readable with exact bytes: %v", key, path, err)
		}
	}
	for _, suffix := range []string{".part", ".part.meta", ".incomplete", ".incomplete.meta"} {
		if _, err := os.Stat(filepath.Join(blobs, sha+suffix)); !os.IsNotExist(err) {
			t.Errorf("staging artifact %s remains after download: %v", suffix, err)
		}
	}
	if got, err := os.ReadFile(source); err != nil || !bytes.Equal(got, payload) {
		t.Errorf("named source changed: %v", err)
	}

	// No case-probe residue may remain anywhere under the payload trees.
	for _, dir := range []string{hub, blobs} {
		_ = os.Chmod(dir, 0o700)
	}
	if err := filepath.WalkDir(hub, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasPrefix(d.Name(), ownershipCaseProbePrefix) {
			t.Errorf("case-probe residue left in payload: %s", path)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk payload after download: %v", err)
	}
}

// TestOwnershipDirectoryCaseProfilePermissionMatrix pins the permission
// envelope of the Linux provider: native flag query for readable directories
// (normal path, no probe), behavioral probe for write+search-only
// directories, fail-closed for search-only, and preserved error classes.
func TestOwnershipDirectoryCaseProfilePermissionMatrix(t *testing.T) {
	const flagProvider = "linux-fs-casefold-v1"
	const probeProvider = "linux-case-probe-v1"
	for _, populated := range []bool{false, true} {
		name := "empty"
		if populated {
			name = "populated"
		}
		t.Run("no-read "+name+" directory probes without residue", func(t *testing.T) {
			dir := t.TempDir()
			wantEntries := 0
			if populated {
				if err := os.WriteFile(filepath.Join(dir, "named-entry"), []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
				wantEntries = 1
			}
			readable, err := ownershipDirectoryCaseProfile(dir, dir)
			if err != nil {
				t.Fatal(err)
			}
			if readable.Provider != flagProvider {
				t.Fatalf("readable directory provider = %q, want native flag query", readable.Provider)
			}
			if err := os.Chmod(dir, 0o300); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
			assertDirectoryUnreadable(t, dir)
			probed, err := ownershipDirectoryCaseProfile(dir, dir)
			if err != nil {
				t.Fatalf("profile without directory read: %v", err)
			}
			if probed.Provider != probeProvider {
				t.Fatalf("no-read provider = %q, want behavioral probe", probed.Provider)
			}
			if probed.Sensitive != readable.Sensitive {
				t.Fatalf("probe verdict sensitive=%v disagrees with flag query sensitive=%v", probed.Sensitive, readable.Sensitive)
			}
			if err := os.Chmod(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != wantEntries {
				t.Fatalf("probe left residue: %d entries, want %d", len(entries), wantEntries)
			}
		})
	}
	t.Run("search-only directory fails closed", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Chmod(dir, 0o100); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		if _, err := ownershipDirectoryCaseProfile(dir, dir); !errors.Is(err, errOwnershipCaseProfileUnavailable) {
			t.Fatalf("search-only profile error = %v, want errOwnershipCaseProfileUnavailable", err)
		}
	})
}

// TestClassifyOwnershipCaseCollation covers the collation decision branches,
// including the case-folded verdict that a case-sensitive-only host cannot
// produce through real syscalls.
func TestClassifyOwnershipCaseCollation(t *testing.T) {
	exists := os.ErrExist
	boom := errors.New("boom")
	for _, tc := range []struct {
		name  string
		obs   ownershipCaseProbeObservation
		want  bool
		isErr bool
	}{
		{"both spellings coexist", ownershipCaseProbeObservation{firstBound: true, variantBound: true}, true, false},
		{"probe entry vanished after create", ownershipCaseProbeObservation{variantBound: true}, false, true},
		{"probe entry vanished at collision", ownershipCaseProbeObservation{variantErr: exists}, false, true},
		{"probe entry identity lost at collision", ownershipCaseProbeObservation{variantErr: exists, variantExists: true}, false, true},
		{"variant identity lost after create", ownershipCaseProbeObservation{firstBound: true}, false, true},
		{"variant aliases probe entry when folded", ownershipCaseProbeObservation{firstBound: true, variantErr: exists, variantExists: true, sameEntry: true}, false, false},
		{"distinct pre-existing variant entry with bound probe entry", ownershipCaseProbeObservation{firstBound: true, variantErr: exists, variantExists: true}, true, false},
		{"variant vanished at collision", ownershipCaseProbeObservation{firstBound: true, variantErr: exists}, false, true},
		{"creation failed", ownershipCaseProbeObservation{firstBound: true, variantErr: boom}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := classifyOwnershipCaseCollation(tc.obs)
			if (err != nil) != tc.isErr {
				t.Fatalf("error = %v, want error: %v", err, tc.isErr)
			}
			if err == nil && got != tc.want {
				t.Fatalf("sensitive = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestOwnershipCaseProfileProbeConcurrentClients exercises concurrent
// unprivileged clients probing one shared cache directory: verdicts must
// agree and no probe entry may survive.
func TestOwnershipCaseProfileProbeConcurrentClients(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	const workers = 4
	const rounds = 4
	var wg sync.WaitGroup
	var mu sync.Mutex
	var first ownershipCaseProfile
	probes := 0
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				profile, err := ownershipDirectoryCaseProfile(dir, dir)
				mu.Lock()
				if err != nil {
					t.Errorf("concurrent probe failed: %v", err)
				} else {
					if probes == 0 {
						first = profile
					} else if profile.Sensitive != first.Sensitive || profile.Provider != first.Provider {
						t.Errorf("concurrent probe verdicts diverged: %+v vs %+v", profile, first)
					}
					probes++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if probes != workers*rounds {
		t.Fatalf("successful probes = %d, want %d", probes, workers*rounds)
	}
	if first.Provider != "linux-case-probe-v1" {
		t.Fatalf("concurrent probe provider = %q, want behavioral probe", first.Provider)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("concurrent probes left residue: %v", entries)
	}
}
