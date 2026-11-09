// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Flat-mode cross-machine exclusion requires that every participant resolving
// one shared OwnershipDir derives the same realm identity, the same
// path-derived resource keys and the same lock filenames. Two divergences must
// be modelled separately: host-local stat identity (dev:ino), which the
// dev:ino alias dimension deliberately keeps host-local, and the mount-prefix
// spelling of the whole realm+item topology, which the realm-relative
// path-derived keys must cancel out. These tests model both — divergent stat
// identity per participant, and the topology mounted under a different prefix
// per participant — and assert the path-derived identity converges while the
// alias dimension (objectKey/entryKey/item-entry) stays host-local on purpose.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// mountSpellingForTest rewrites path into the spelling of the named mount
// prefix on the same volume, modelling one host's view of a shared topology.
func mountSpellingForTest(tag, path string) string {
	volume := filepath.VolumeName(path)
	return volume + string(filepath.Separator) + tag + strings.TrimPrefix(path, volume)
}

// TestFlatRealmSharedIdentityConvergesAcrossDivergentStat drives two realm
// resolvers over ONE shared OwnershipDir while the injected stat identity
// diverges per participant. Both must derive the same realm ID, the same
// path-derived item key and lock filename, and a real OS lock held by one must
// be refused by the other. The dev:ino-derived item-entry key is the same-host
// alias dimension and is expected to diverge here (that divergence proves the
// stat seam is actually applied).
func TestFlatRealmSharedIdentityConvergesAcrossDivergentStat(t *testing.T) {
	if !ownershipOSLockAvailable {
		t.Skip("this platform has no cross-process ownership primitive")
	}
	base := t.TempDir()
	shared := filepath.Join(base, "shared-ownership")
	out := filepath.Join(base, "out")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	item := filepath.Join(out, "repo", "model.bin")

	// host is only read and written from this test goroutine.
	host := "A"
	ownershipStatIdentity = func(path string) (string, error) {
		return "host-" + host + ":" + path, nil
	}
	t.Cleanup(func() { ownershipStatIdentity = nil })

	host = "A"
	realmA, err := newOwnershipRealm(shared)
	if err != nil {
		t.Fatalf("participant A: %v", err)
	}
	host = "B"
	realmB, err := newOwnershipRealm(shared)
	if err != nil {
		t.Fatalf("participant B: %v", err)
	}

	if len(realmA.id) != 32 {
		t.Fatalf("realm ID %q is not an opaque 32-hex value", realmA.id)
	}
	if realmA.id != realmB.id {
		t.Fatalf("participants of one shared OwnershipDir derived different realm IDs under divergent stat identity: %q != %q", realmA.id, realmB.id)
	}

	// Path-derived item keys and lock filenames must converge.
	host = "A"
	resolvedA, err := realmA.resolveOwnershipPath(item, false)
	if err != nil {
		t.Fatal(err)
	}
	host = "B"
	resolvedB, err := realmB.resolveOwnershipPath(item, false)
	if err != nil {
		t.Fatal(err)
	}
	if resolvedA.itemKeys[0] != resolvedB.itemKeys[0] {
		t.Errorf("path-derived item key diverged: %q != %q", resolvedA.itemKeys[0], resolvedB.itemKeys[0])
	}
	if realmA.lockPath(resolvedA.itemKeys[0]) != realmB.lockPath(resolvedB.itemKeys[0]) {
		t.Errorf("path-derived item lock filename diverged under divergent stat identity")
	}
	// The dev:ino alias dimension is same-host-only and does not converge here;
	// if it did, the stat seam would not be applied and this test proves nothing.
	if resolvedA.itemKeys[1] == resolvedB.itemKeys[1] {
		t.Errorf("dev:ino-derived item-entry key unexpectedly converged; the stat seam is not applied")
	}

	// A real OS lock held by one participant on the converged key must be
	// refused by the other: exclusion now follows the shared identity, not the
	// host-local stat identity.
	lease, err := realmA.acquire(context.Background(),
		[]ownershipLockRequest{{key: resolvedA.itemKeys[0], mode: ownershipExclusive}}, 0)
	if err != nil {
		t.Fatalf("participant A exclusive item lock: %v", err)
	}
	defer lease.Release()
	if _, err := realmB.acquire(context.Background(),
		[]ownershipLockRequest{{key: resolvedB.itemKeys[0], mode: ownershipExclusive}}, 0); !errors.Is(err, errOwnershipBusy) {
		t.Fatalf("participant B lock over the converged key = %v, want busy", err)
	}
}

// TestFlatRealmSharedIdentityConvergesAcrossDivergentMountPrefix moves the
// whole shared realm+item topology to a different prefix spelling per
// participant (the /mnt/nas vs /vol/nas defect) while stat identity also
// diverges. Both must derive identical path-derived item and ancestor-directory
// keys and identical lock filenames, and a real exclusive OS lock held by one
// must be refused by the other. The dev:ino item-entry key must still diverge:
// that dimension stays host-local on purpose and its divergence proves the
// stat seam is applied.
func TestFlatRealmSharedIdentityConvergesAcrossDivergentMountPrefix(t *testing.T) {
	if !ownershipOSLockAvailable {
		t.Skip("this platform has no cross-process ownership primitive")
	}
	base := t.TempDir()
	shared := filepath.Join(base, "shared-ownership")
	out := filepath.Join(base, "out")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	item := filepath.Join(out, "repo", "model.bin")
	sibling := filepath.Join(out, "repo", "other.bin")

	// host is only read and written from this test goroutine.
	host := "A"
	ownershipStatIdentity = func(path string) (string, error) {
		return "host-" + host + ":" + path, nil
	}
	ownershipPathSpelling = func(path string) string {
		return mountSpellingForTest("mount-"+host, path)
	}
	t.Cleanup(func() {
		ownershipStatIdentity = nil
		ownershipPathSpelling = nil
	})

	host = "A"
	realmA, err := newOwnershipRealm(shared)
	if err != nil {
		t.Fatalf("participant A: %v", err)
	}
	host = "B"
	realmB, err := newOwnershipRealm(shared)
	if err != nil {
		t.Fatalf("participant B: %v", err)
	}
	if realmA.id != realmB.id || len(realmA.id) != 32 {
		t.Fatalf("participants of one shared OwnershipDir derived different realm IDs: %q != %q", realmA.id, realmB.id)
	}

	host = "A"
	resolvedA, err := realmA.resolveOwnershipPath(item, false)
	if err != nil {
		t.Fatal(err)
	}
	siblingA, err := realmA.resolveOwnershipPath(sibling, false)
	if err != nil {
		t.Fatal(err)
	}
	host = "B"
	resolvedB, err := realmB.resolveOwnershipPath(item, false)
	if err != nil {
		t.Fatal(err)
	}

	// The path-derived item key and its lock filename are prefix-movement
	// invariant: one shared topology, two mount spellings, one identity.
	if resolvedA.itemKeys[0] != resolvedB.itemKeys[0] {
		t.Errorf("path-derived item key diverged across mount-prefix spellings: %q != %q", resolvedA.itemKeys[0], resolvedB.itemKeys[0])
	}
	if realmA.lockPath(resolvedA.itemKeys[0]) != realmB.lockPath(resolvedB.itemKeys[0]) {
		t.Errorf("path-derived item lock filename diverged across mount-prefix spellings")
	}
	// Ancestor directory path keys (the shared-ancestor dimension) converge
	// element-wise, not only the leaf item key.
	if len(resolvedA.directories) != len(resolvedB.directories) {
		t.Fatalf("directory chain lengths differ: %d != %d", len(resolvedA.directories), len(resolvedB.directories))
	}
	for i := range resolvedA.directories {
		if resolvedA.directories[i].pathKey != resolvedB.directories[i].pathKey {
			t.Errorf("ancestor directory path key %d diverged across mount-prefix spellings: %q != %q",
				i, resolvedA.directories[i].pathKey, resolvedB.directories[i].pathKey)
		}
	}
	// Injectivity spot-check: a sibling item stays a distinct resource.
	if resolvedA.itemKeys[0] == siblingA.itemKeys[0] {
		t.Errorf("sibling items share one path-derived key")
	}
	// The alias dimension stays host-local: if it converged, the stat seam
	// would not be applied and the convergence above would prove nothing.
	if resolvedA.itemKeys[1] == resolvedB.itemKeys[1] {
		t.Errorf("dev:ino-derived item-entry key unexpectedly converged; the stat seam is not applied")
	}

	// A real OS lock held by one participant on the converged key must be
	// refused by the other.
	lease, err := realmA.acquire(context.Background(),
		[]ownershipLockRequest{{key: resolvedA.itemKeys[0], mode: ownershipExclusive}}, 0)
	if err != nil {
		t.Fatalf("participant A exclusive item lock: %v", err)
	}
	defer lease.Release()
	if _, err := realmB.acquire(context.Background(),
		[]ownershipLockRequest{{key: resolvedB.itemKeys[0], mode: ownershipExclusive}}, 0); !errors.Is(err, errOwnershipBusy) {
		t.Fatalf("participant B lock over the converged key = %v, want busy", err)
	}
}

// TestFlatRealmKeysCancelRealPrefixMovement models two hosts mounting one
// shared topology at different REAL prefixes (no seam): two prefix copies
// with identical relative structure and one shared persisted realm ID must
// derive identical path-derived keys and identical lock filenames. Real
// cross-host lock exclusion over one physical shared OwnershipDir is covered
// by the divergent-spelling test above; this test pins that the keys
// themselves depend only on the realm-relative topology.
func TestFlatRealmKeysCancelRealPrefixMovement(t *testing.T) {
	if !ownershipOSLockAvailable {
		t.Skip("this platform has no cross-process ownership primitive")
	}
	base := t.TempDir()
	const sharedRealmID = "0123456789abcdef0123456789abcdef"
	derive := func(prefix string) (*ownershipRealm, string, string) {
		t.Helper()
		realmDir := filepath.Join(base, prefix, "hfdesk-ownership")
		if err := os.MkdirAll(realmDir, 0o755); err != nil {
			t.Fatal(err)
		}
		// Both mounts read the one persisted realm identity of the shared
		// OwnershipDir, as two hosts of the declared configuration do.
		if err := os.WriteFile(filepath.Join(realmDir, ownershipRealmIDFile), []byte(sharedRealmID), 0o600); err != nil {
			t.Fatal(err)
		}
		itemDir := filepath.Join(base, prefix, "models", "repo")
		if err := os.MkdirAll(itemDir, 0o755); err != nil {
			t.Fatal(err)
		}
		realm, err := newOwnershipRealm(realmDir)
		if err != nil {
			t.Fatal(err)
		}
		return realm, filepath.Join(itemDir, "model.bin"), filepath.Join(itemDir, "other.bin")
	}
	realmA, itemA, siblingA := derive("mount-a")
	realmB, itemB, _ := derive("mount-b")
	if realmA.id != sharedRealmID || realmB.id != sharedRealmID {
		t.Fatalf("prefix copies did not read the shared realm ID: %q, %q", realmA.id, realmB.id)
	}

	resolvedA, err := realmA.resolveOwnershipPath(itemA, false)
	if err != nil {
		t.Fatal(err)
	}
	resolvedB, err := realmB.resolveOwnershipPath(itemB, false)
	if err != nil {
		t.Fatal(err)
	}
	if resolvedA.itemKeys[0] != resolvedB.itemKeys[0] {
		t.Errorf("path-derived item key embedded the mount prefix: %q != %q", resolvedA.itemKeys[0], resolvedB.itemKeys[0])
	}
	if filepath.Base(realmA.lockPath(resolvedA.itemKeys[0])) != filepath.Base(realmB.lockPath(resolvedB.itemKeys[0])) {
		t.Errorf("path-derived lock filename embedded the mount prefix")
	}
	if len(resolvedA.directories) != len(resolvedB.directories) {
		t.Fatalf("directory chain lengths differ: %d != %d", len(resolvedA.directories), len(resolvedB.directories))
	}
	for i := range resolvedA.directories {
		if resolvedA.directories[i].pathKey != resolvedB.directories[i].pathKey {
			t.Errorf("ancestor directory path key %d embedded the mount prefix: %q != %q",
				i, resolvedA.directories[i].pathKey, resolvedB.directories[i].pathKey)
		}
	}
	siblingResolved, err := realmA.resolveOwnershipPath(siblingA, false)
	if err != nil {
		t.Fatal(err)
	}
	if resolvedA.itemKeys[0] == siblingResolved.itemKeys[0] {
		t.Errorf("sibling items share one path-derived key")
	}
}

// TestFlatRealmSplitTopologyKeepsWorkingSameHost covers the documented
// residual of the realm-relative derivation: a split topology — OwnershipDir
// and the coordinated items rooted at unrelated locations — keeps working on
// one host (keys are computed, two participants over the same topology with
// the same spelling exclude each other). Cross-host convergence is NOT
// promised for this shape; doc.go requires identical rel-token input there —
// the same structure below the lowest common ancestor and the same descending
// component spellings. The divergence probe pins that boundary: moving ONLY
// the items' prefix while the realm's spelling stays fixed must change the
// derived keys.
func TestFlatRealmSplitTopologyKeepsWorkingSameHost(t *testing.T) {
	if !ownershipOSLockAvailable {
		t.Skip("this platform has no cross-process ownership primitive")
	}
	realmBase := t.TempDir()
	itemsBase := t.TempDir()
	shared := filepath.Join(realmBase, "ownership-root")
	item := filepath.Join(itemsBase, "out", "repo", "model.bin")
	if err := os.MkdirAll(filepath.Dir(item), 0o755); err != nil {
		t.Fatal(err)
	}

	realmA, err := newOwnershipRealm(shared)
	if err != nil {
		t.Fatalf("participant A: %v", err)
	}
	realmB, err := newOwnershipRealm(shared)
	if err != nil {
		t.Fatalf("participant B: %v", err)
	}

	// Same host, same spelling: keys compute and exclusion holds.
	resolvedA, err := realmA.resolveOwnershipPath(item, false)
	if err != nil {
		t.Fatalf("split-topology key derivation failed: %v", err)
	}
	resolvedB, err := realmB.resolveOwnershipPath(item, false)
	if err != nil {
		t.Fatalf("split-topology key derivation failed: %v", err)
	}
	if resolvedA.itemKeys[0] != resolvedB.itemKeys[0] {
		t.Fatalf("same-host split topology derived different keys: %q != %q", resolvedA.itemKeys[0], resolvedB.itemKeys[0])
	}
	lease, err := realmA.acquire(context.Background(),
		[]ownershipLockRequest{{key: resolvedA.itemKeys[0], mode: ownershipExclusive}}, 0)
	if err != nil {
		t.Fatalf("participant A exclusive item lock: %v", err)
	}
	defer lease.Release()
	if _, err := realmB.acquire(context.Background(),
		[]ownershipLockRequest{{key: resolvedB.itemKeys[0], mode: ownershipExclusive}}, 0); !errors.Is(err, errOwnershipBusy) {
		t.Fatalf("participant B lock over the same-host key = %v, want busy", err)
	}

	// Documented residual: with the realm's spelling fixed and only the items
	// moved to another prefix, the split topology cannot cancel the prefixes
	// out — no cross-host convergence is claimed there (doc.go requires
	// identical spellings). This also proves the derivation consumes the
	// spelling at all: a spelling-blind derivation yields equal keys here.
	t.Cleanup(func() { ownershipPathSpelling = nil })
	ownershipPathSpelling = func(path string) string {
		if strings.HasPrefix(path, itemsBase) {
			return mountSpellingForTest("moved-items", path)
		}
		return path
	}
	movedB, err := realmB.resolveOwnershipPath(item, false)
	if err != nil {
		t.Fatalf("split-topology key derivation failed: %v", err)
	}
	if movedB.itemKeys[0] == resolvedA.itemKeys[0] {
		t.Errorf("split topology converged under an item-only prefix movement; the documented residual (identical spellings required) is not pinned")
	}
}

// TestFlatRealmFirstUseConvergesOnOnePersistedID races many first-time users of
// a fresh realm and asserts they all converge on one persisted ID. A crash
// creation temp left behind must not corrupt the result.
func TestFlatRealmFirstUseConvergesOnOnePersistedID(t *testing.T) {
	if !ownershipOSLockAvailable {
		t.Skip("this platform has no cross-process ownership primitive")
	}
	shared := filepath.Join(t.TempDir(), "fresh-ownership")
	if err := os.MkdirAll(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	crashTemp := filepath.Join(shared, ownershipRealmIDFile+".tmp-crashed")
	if err := os.WriteFile(crashTemp, []byte("partial-id"), 0o600); err != nil {
		t.Fatal(err)
	}

	const participants = 12
	ids := make([]string, participants)
	errs := make([]error, participants)
	var wg sync.WaitGroup
	for i := 0; i < participants; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			realm, err := newOwnershipRealm(shared)
			if err != nil {
				errs[i] = err
				return
			}
			ids[i] = realm.id
		}(i)
	}
	wg.Wait()

	for i := 0; i < participants; i++ {
		if errs[i] != nil {
			t.Fatalf("participant %d: %v", i, errs[i])
		}
		if len(ids[i]) != 32 {
			t.Fatalf("participant %d derived ID %q, want 32-hex", i, ids[i])
		}
		if ids[i] != ids[0] {
			t.Fatalf("simultaneous first-use creation diverged: %q != %q", ids[i], ids[0])
		}
	}
	stored, err := readOwnershipNodeID(filepath.Join(shared, ownershipRealmIDFile))
	if err != nil {
		t.Fatalf("read persisted realm ID: %v", err)
	}
	if stored != ids[0] {
		t.Fatalf("persisted realm ID %q != derived %q", stored, ids[0])
	}
	if _, err := os.Stat(crashTemp); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("crash creation temp survived first use: %v", err)
	}
}
