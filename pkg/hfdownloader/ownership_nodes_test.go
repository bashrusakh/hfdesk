// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCacheOwnedAnchorAndNodeIdentityAcrossHubViews(t *testing.T) {
	base := t.TempDir()
	hub := filepath.Join(base, "shared-hub")
	if err := os.MkdirAll(hub, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "hub-view")
	if err := os.Symlink(hub, alias); err != nil {
		t.Skipf("symlink alias unavailable: %v", err)
	}

	cacheA := NewHFCacheResolved(filepath.Join(base, "profile-a"), hub)
	cacheB := NewHFCacheResolved(filepath.Join(base, "profile-b"), alias)
	realmA, err := cacheA.ownershipRealmForData()
	if err != nil {
		t.Fatal(err)
	}
	realmB, err := cacheB.ownershipRealmForData()
	if err != nil {
		t.Fatal(err)
	}
	if realmA.id != realmB.id {
		t.Fatalf("Hub views have different anchor IDs: %q != %q", realmA.id, realmB.id)
	}
	if len(realmA.id) != 32 || strings.ContainsAny(realmA.id, string(filepath.Separator)) {
		t.Fatalf("anchor ID is not opaque and path-independent: %q", realmA.id)
	}

	pathA := filepath.Join(hub, "prefix", "lease", "file.gguf")
	pathB := filepath.Join(alias, "prefix", "lease", "file.gguf")
	lease, err := realmA.acquireWriter(context.Background(), []string{pathA}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	fenceA, err := realmA.fenceProof(pathA)
	if err != nil {
		t.Fatal(err)
	}
	fenceB, err := realmB.fenceProof(pathB)
	if err != nil {
		t.Fatal(err)
	}
	if fenceA != fenceB {
		t.Fatalf("Hub aliases have different v4 fences: %q != %q", fenceA, fenceB)
	}
	if !lease.holdsCoverage(realmB, pathB) {
		t.Fatal("lease does not cover the same path through the Hub alias")
	}
	if lease.holdsCoverage(realmB, filepath.Join(alias, "prefix", "lease", "other.gguf")) {
		t.Fatal("lease claims coverage for a different item")
	}

	controlRoot := filepath.Join(hub, ".hfdesk-control", "v1", "root")
	for _, path := range []string{
		filepath.Join(controlRoot, "metadata", "bootstrap.lock"),
		filepath.Join(controlRoot, "metadata", "anchor-id"),
		filepath.Join(controlRoot, "metadata", "profile"),
		filepath.Join(controlRoot, "children", "prefix", "metadata", "node-id"),
		filepath.Join(controlRoot, "children", "prefix", "children", "lease", "metadata", "children.lock"),
		filepath.Join(controlRoot, "children", "prefix", "children", "lease", "children", "file.gguf", "metadata", "entry.lock"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("cache-owned control entry %q: %v", path, err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(hub, "prefix", "lease"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("ownership metadata occupied payload components: %v", entries)
	}
}

func TestCacheOwnedAnchorsFollowHubAndFriendlyRootsIndependently(t *testing.T) {
	base := t.TempDir()
	friendly := filepath.Join(base, "friendly")
	hubA := filepath.Join(base, "hub-a")
	hubB := filepath.Join(base, "hub-b")
	for _, root := range []string{friendly, hubA, hubB} {
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cacheA := NewHFCacheResolved(friendly, hubA)
	cacheB := NewHFCacheResolved(friendly, hubB)
	hubRealmA, err := cacheA.ownershipRealmForData()
	if err != nil {
		t.Fatal(err)
	}
	hubRealmB, err := cacheB.ownershipRealmForData()
	if err != nil {
		t.Fatal(err)
	}
	friendlyRealmA, err := cacheA.ownershipRealmForFriendly()
	if err != nil {
		t.Fatal(err)
	}
	friendlyRealmB, err := cacheB.ownershipRealmForFriendly()
	if err != nil {
		t.Fatal(err)
	}
	if hubRealmA.id == hubRealmB.id || hubRealmA.lockDir == hubRealmB.lockDir {
		t.Fatal("different Hub roots selected one cache-owned namespace")
	}
	if friendlyRealmA.id != friendlyRealmB.id || friendlyRealmA.lockDir != friendlyRealmB.lockDir {
		t.Fatal("the same friendly root did not converge on one control namespace")
	}

	repoA, err := cacheA.Repo("owner/model", RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	repoB, err := cacheB.Repo("owner/model", RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := repoA.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if err := repoB.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	const sha = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	hubFenceA, err := hubRealmA.fenceProof(repoA.BlobPath(sha))
	if err != nil {
		t.Fatal(err)
	}
	hubFenceB, err := hubRealmB.fenceProof(repoB.BlobPath(sha))
	if err != nil {
		t.Fatal(err)
	}
	if hubFenceA == hubFenceB {
		t.Fatal("identical blob names under different Hub roots shared an item fence")
	}

	for _, realm := range []*ownershipRealm{friendlyRealmA, friendlyRealmB} {
		if err := realm.ensureDirectories(context.Background(), []string{repoA.FriendlyPath()}, blobOwnershipWaitBudget); err != nil {
			t.Fatal(err)
		}
	}
	friendlyPath := filepath.Join(repoA.FriendlyPath(), "hfd.yaml")
	friendlyFenceA, err := friendlyRealmA.fenceProof(friendlyPath)
	if err != nil {
		t.Fatal(err)
	}
	friendlyFenceB, err := friendlyRealmB.fenceProof(friendlyPath)
	if err != nil {
		t.Fatal(err)
	}
	if friendlyFenceA != friendlyFenceB {
		t.Fatal("identical friendly paths under one R root did not share an item fence")
	}
}

func TestCacheOwnershipDirCannotSelectAlternateHubAnchor(t *testing.T) {
	base := t.TempDir()
	r := filepath.Join(base, "friendly")
	h := filepath.Join(base, "hub")
	stale := filepath.Join(base, "old-local-owner")
	for _, root := range []string{r, h} {
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	explicit := NewHFCacheResolvedWithOwnershipDir(r, h, stale)
	if _, err := explicit.ownershipRealmForData(); !errors.Is(err, errCacheOwnershipDirNotSupported) {
		t.Fatalf("explicit cache ownership override error = %v", err)
	}
	if _, err := explicit.ownershipRealmForFriendly(); !errors.Is(err, errCacheOwnershipDirNotSupported) {
		t.Fatalf("explicit friendly ownership override error = %v", err)
	}
	if _, err := (Settings{CacheDir: r, HubDir: h, OwnershipDir: stale}).BuildHFCache(); !errors.Is(err, errCacheOwnershipDirNotSupported) {
		t.Fatalf("cache settings accepted an alternate OwnershipDir: %v", err)
	}
	for _, path := range []string{filepath.Join(stale, "hub"), filepath.Join(stale, "friendly")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("explicit cache setting created an alternate control root %s: %v", path, err)
		}
	}

	cache := NewHFCacheResolved(r, h)
	realm, err := cache.ownershipRealmForData()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := filepath.Clean(realm.root), filepath.Join(h, ".hfdesk-control"); got != want {
		t.Fatalf("cache anchor root = %q, want Hub-derived root %q", got, want)
	}
	if got, want := filepath.Clean(cache.OwnershipDir()), filepath.Join(h, ".hfdesk-control"); got != want {
		t.Fatalf("effective cache ownership directory = %q, want %q", got, want)
	}
}

func TestCacheOwnedMissingParentPromotionAndFileDirectoryReuse(t *testing.T) {
	hub := filepath.Join(t.TempDir(), "hub")
	cache := NewHFCacheResolved(filepath.Dir(hub), hub)
	realm, err := cache.ownershipRealmForData()
	if err != nil {
		t.Fatal(err)
	}

	missing := filepath.Join(hub, "fresh", "nested", "file.gguf")
	lease, err := realm.acquireWriter(context.Background(), []string{missing}, 0)
	if err != nil {
		t.Fatalf("promote missing parent nodes: %v", err)
	}
	if !lease.holdsCoverage(realm, missing) {
		t.Fatal("promoted lease does not cover its requested item")
	}
	lease.Release()
	profilePath := filepath.Join(hub, ".hfdesk-control", "v1", "root", "children", "fresh", "children", "nested", "metadata", "profile")
	if _, err := readOwnershipCaseProfile(profilePath); err != nil {
		t.Fatalf("missing-parent case profile was not durably initialized: %v", err)
	}

	flip := filepath.Join(hub, "flip")
	if err := os.WriteFile(flip, []byte("file generation"), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := realm.acquireWriter(context.Background(), []string{flip}, 0)
	if err != nil {
		t.Fatal(err)
	}
	first.Release()
	controlFlip := filepath.Join(hub, ".hfdesk-control", "v1", "root", "children", "flip")
	firstID, err := readOwnershipNodeID(filepath.Join(controlFlip, "metadata", "node-id"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(flip); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(flip, "lease"), 0o755); err != nil {
		t.Fatal(err)
	}
	second, err := realm.acquireWriter(context.Background(), []string{filepath.Join(flip, "lease", "file.gguf")}, 0)
	if err != nil {
		t.Fatalf("reuse node after file-to-directory transition: %v", err)
	}
	defer second.Release()
	secondID, err := readOwnershipNodeID(filepath.Join(controlFlip, "metadata", "node-id"))
	if err != nil {
		t.Fatal(err)
	}
	if firstID != secondID {
		t.Fatalf("file-to-directory transition rotated node ID: %q != %q", firstID, secondID)
	}

	parentMeta := filepath.Join(hub, ".hfdesk-control", "v1", "root", "metadata")
	bootstrap, err := ownershipLockOpen(filepath.Join(parentMeta, "child-bootstrap.lock"), ownershipExclusive)
	if err != nil {
		t.Fatal(err)
	}
	recoveryMeta := filepath.Join(hub, ".hfdesk-control", "v1", "root", "children", "recovered", "metadata")
	if err := os.MkdirAll(recoveryMeta, 0o755); err != nil {
		bootstrap.release()
		t.Fatal(err)
	}
	tempID := filepath.Join(recoveryMeta, "node-id.tmp-crashed-writer")
	if err := os.WriteFile(tempID, []byte("partial"), 0o600); err != nil {
		bootstrap.release()
		t.Fatal(err)
	}
	recoveredID, err := loadOrCreateOwnershipNodeID(recoveryMeta, "node-id")
	bootstrap.release()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tempID); !os.IsNotExist(err) {
		t.Fatalf("recognized crash temp survived recovery: %v", err)
	}
	if recoveredID == "" {
		t.Fatal("crash recovery did not publish a node ID")
	}
}

func TestConcurrentCacheRootBootstrapWaitsAndConverges(t *testing.T) {
	hub := filepath.Join(t.TempDir(), "hub")
	bootstrapPath, held := holdCacheRootBootstrap(t, hub)
	defer held.release()

	attempted := observeCacheRootBootstrap(t, bootstrapPath)

	type result struct {
		realm *ownershipRealm
		err   error
	}
	results := make(chan result, 2)
	for i := 0; i < cap(results); i++ {
		cache := NewHFCacheResolved(filepath.Join(t.TempDir(), "friendly"), hub)
		go func() {
			realm, err := cache.ownershipRealmForData()
			results <- result{realm: realm, err: err}
		}()
	}
	for i := 0; i < cap(results); i++ {
		select {
		case <-attempted:
		case <-time.After(time.Second):
			t.Fatal("initializer did not contend on the shared root bootstrap lock")
		}
	}
	select {
	case got := <-results:
		t.Fatalf("initializer returned while bootstrap was held: realm=%v err=%v", got.realm, got.err)
	default:
	}
	held.release()

	var anchorID string
	for i := 0; i < cap(results); i++ {
		select {
		case got := <-results:
			if got.err != nil {
				t.Fatalf("initialize after bootstrap release: %v", got.err)
			}
			if anchorID == "" {
				anchorID = got.realm.id
			} else if got.realm.id != anchorID {
				t.Fatalf("concurrent initializers diverged: %q != %q", got.realm.id, anchorID)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("initializer did not finish after bootstrap release")
		}
	}
}

func TestCacheRootBootstrapCancellationAndBudgetExpiryAreRetryable(t *testing.T) {
	t.Run("context cancellation", func(t *testing.T) {
		hub := filepath.Join(t.TempDir(), "hub")
		bootstrapPath, held := holdCacheRootBootstrap(t, hub)
		attempted := observeCacheRootBootstrap(t, bootstrapPath)
		cache := NewHFCacheResolved(filepath.Dir(hub), hub)
		ctx, cancel := context.WithCancel(context.Background())
		type result struct {
			realm *ownershipRealm
			err   error
		}
		finished := make(chan result, 1)
		go func() {
			realm, err := cache.ownershipRealmForDataWait(ctx, time.Second)
			finished <- result{realm: realm, err: err}
		}()
		waitForBootstrapAttempt(t, attempted)
		cancel()
		select {
		case got := <-finished:
			if !errors.Is(got.err, context.Canceled) {
				t.Fatalf("bootstrap wait error = %v, want context canceled", got.err)
			}
		case <-time.After(time.Second):
			t.Fatal("bootstrap wait ignored context cancellation")
		}
		held.release()
		if _, err := cache.ownershipRealmForData(); err != nil {
			t.Fatalf("cache instance remained poisoned after cancellation: %v", err)
		}
	})

	t.Run("bounded expiry", func(t *testing.T) {
		hub := filepath.Join(t.TempDir(), "hub")
		bootstrapPath, held := holdCacheRootBootstrap(t, hub)
		attempted := observeCacheRootBootstrap(t, bootstrapPath)
		cache := NewHFCacheResolved(filepath.Dir(hub), hub)
		_, err := cache.ownershipRealmForDataWait(context.Background(), 80*time.Millisecond)
		if !errors.Is(err, errOwnershipBusy) && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("bootstrap budget error = %v, want busy or deadline exceeded", err)
		}
		select {
		case <-attempted:
		default:
			t.Fatal("initializer did not attempt the held bootstrap lock")
		}
		anchorID := filepath.Join(hub, ".hfdesk-control", "v1", "root", "metadata", "anchor-id")
		if _, statErr := os.Stat(anchorID); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("timed-out initializer published an anchor ID: %v", statErr)
		}
		held.release()
		if _, err := cache.ownershipRealmForData(); err != nil {
			t.Fatalf("cache instance remained poisoned after budget expiry: %v", err)
		}
	})
}

func TestDownloadCancellationDuringCacheBootstrap(t *testing.T) {
	cacheRoot := filepath.Join(t.TempDir(), "cache")
	hub := filepath.Join(cacheRoot, "hub")
	bootstrapPath, held := holdCacheRootBootstrap(t, hub)
	defer held.release()
	attempted := observeCacheRootBootstrap(t, bootstrapPath)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Download(ctx, Job{Repo: "owner/model", Revision: "main"}, Settings{CacheDir: cacheRoot, HubDir: hub}, nil)
	}()
	waitForBootstrapAttempt(t, attempted)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Download bootstrap wait error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Download ignored cancellation while waiting for cache bootstrap")
	}
}

func holdCacheRootBootstrap(t *testing.T, hub string) (string, ownershipLockHandle) {
	t.Helper()
	metaDir := filepath.Join(hub, ".hfdesk-control", "v1", "root", "metadata")
	if err := os.MkdirAll(metaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(metaDir, "bootstrap.lock")
	handle, err := ownershipLockOpen(path, ownershipExclusive)
	if err != nil {
		t.Fatal(err)
	}
	return path, handle
}

func observeCacheRootBootstrap(t *testing.T, bootstrapPath string) <-chan struct{} {
	t.Helper()
	oldOpen := ownershipLockOpen
	attempted := make(chan struct{}, 8)
	ownershipLockOpen = func(path string, mode ownershipLockMode) (ownershipLockHandle, error) {
		if filepath.Clean(path) == filepath.Clean(bootstrapPath) {
			select {
			case attempted <- struct{}{}:
			default:
			}
		}
		return oldOpen(path, mode)
	}
	t.Cleanup(func() { ownershipLockOpen = oldOpen })
	return attempted
}

func waitForBootstrapAttempt(t *testing.T, attempted <-chan struct{}) {
	t.Helper()
	select {
	case <-attempted:
	case <-time.After(time.Second):
		t.Fatal("initializer did not attempt the shared root bootstrap lock")
	}
}
