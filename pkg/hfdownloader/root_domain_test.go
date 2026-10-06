// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"os"
	"path/filepath"
	"testing"
)

func TestManagedRootIdentityUsesCapturedLexicalBase(t *testing.T) {
	base := t.TempDir()
	missing := filepath.Join(base, "models")
	set := NewManagedRootSet(base, []ManagedRootSpec{{Path: "models", Roles: ManagedRootBrowse | ManagedRootLocal}})
	rootID := ManagedRootID("models", base)
	if path, err := set.Resolve(rootID, "owner", "repo"); err != nil || path != filepath.Join(missing, "owner", "repo") {
		t.Fatalf("resolve = %q, %v", path, err)
	}
	for _, id := range [][2]string{{"..", "outside"}, {"owner", ".."}, {"owner", "bad\\name"}, {"owner", "nested/name"}} {
		if _, err := set.Resolve(rootID, id[0], id[1]); err == nil {
			t.Errorf("resolve accepted unsafe repository path %q/%q", id[0], id[1])
		}
	}
	for _, id := range [][2]string{{"owner", "My Model"}, {"ümlaut", "模型"}} {
		want := filepath.Join(missing, id[0], id[1])
		if got, err := set.Resolve(rootID, id[0], id[1]); err != nil || got != want {
			t.Errorf("resolve valid observed ID %q/%q = %q, %v; want %q", id[0], id[1], got, err, want)
		}
	}
	if err := os.MkdirAll(filepath.Join(missing, "owner", "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	owner, err := set.OwnerForPath(filepath.Join("models", "owner", "repo"))
	if err != nil || owner.ID != rootID {
		t.Fatalf("owner after root creation = %#v, %v", owner, err)
	}

	caseIDs := []string{
		ManagedRootID(filepath.Join(base, "Models"), base),
		ManagedRootID(filepath.Join(base, "models"), base),
	}
	if caseIDs[0] == caseIDs[1] {
		t.Fatal("case-distinct lexical roots shared an ID")
	}
}

func TestManagedRootOwnerWalkUsesFreshPhysicalNestedFacts(t *testing.T) {
	base := t.TempDir()
	store := filepath.Join(base, "store")
	parentRepo := filepath.Join(store, "outer", "repo")
	child := filepath.Join(store, "nested")
	childRepo := filepath.Join(child, "inner", "repo")
	for _, path := range []string{parentRepo, childRepo} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(parentRepo, "weights.safetensors"), []byte("outer"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(childRepo, "weights.gguf"), []byte("inner"), 0o644); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "store-link")
	if err := os.Symlink(store, alias); err != nil {
		t.Skipf("directory symlink unavailable: %v", err)
	}
	parentID := ManagedRootID(alias, base)
	childID := ManagedRootID(child, base)
	set := NewManagedRootSet(base, []ManagedRootSpec{
		{Path: alias, Roles: ManagedRootBrowse | ManagedRootProtected | ManagedRootLocal},
		{Path: child, Roles: ManagedRootBrowse | ManagedRootProtected | ManagedRootLocal},
	})
	owner, err := set.OwnerForPath(filepath.Join(alias, "nested", "inner", "repo", "weights.gguf"))
	if err != nil || owner.ID != childID {
		t.Fatalf("owner through alias = %#v, %v; want %s", owner, err, childID)
	}
	nested, err := set.NestedProtectedRoots(parentID)
	if err != nil || len(nested) != 1 || nested[0].ID != childID {
		t.Fatalf("nested roots = %#v, %v", nested, err)
	}
	var parentFiles, childFiles int
	if err := set.WalkOwned(parentID, parentRepo, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			parentFiles++
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := set.WalkOwned(parentID, filepath.Join(alias, "nested", "inner", "repo"), func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			parentFiles++
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := set.WalkOwned(childID, childRepo, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			childFiles++
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if parentFiles != 1 || childFiles != 1 {
		t.Fatalf("owned walk counts parent=%d child=%d, want 1 each", parentFiles, childFiles)
	}
}

func TestManagedRootNestedProtectedFindsInverseAliasAndMissingDescendant(t *testing.T) {
	base := t.TempDir()
	effect := filepath.Join(base, "hub", "models--owner--model")
	if err := os.MkdirAll(filepath.Join(effect, "application-data"), 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "configured-alias")
	if err := os.Symlink(filepath.Join(effect, "application-data"), alias); err != nil {
		t.Skipf("directory symlink unavailable: %v", err)
	}
	rootID := ManagedRootID(filepath.Join(base, "hub"), base)
	set := NewManagedRootSet(base, []ManagedRootSpec{
		{Path: filepath.Join(base, "hub"), Roles: ManagedRootHub | ManagedRootProtected},
		{Path: filepath.Join(alias, "new-child"), Roles: ManagedRootProtected},
	})
	if nested, err := set.NestedProtectedRoots(rootID); err != nil || len(nested) != 1 {
		t.Fatalf("inverse alias descendant: nested=%#v err=%v", nested, err)
	}
	target := filepath.Join(effect, "nested", "repo")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestManagedRootNestedProtectedKeepsMissingReservationUnderRootAlias(t *testing.T) {
	base := t.TempDir()
	hub := filepath.Join(base, "hub")
	if err := os.MkdirAll(filepath.Join(hub, "models--owner--model"), 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "hub-alias")
	if err := os.Symlink(hub, alias); err != nil {
		t.Skipf("directory symlink unavailable: %v", err)
	}
	rootID := ManagedRootID(hub, base)
	reservedID := ManagedRootID(filepath.Join(alias, "not-yet-created"), base)
	set := NewManagedRootSet(base, []ManagedRootSpec{
		{Path: hub, Roles: ManagedRootHub | ManagedRootProtected},
		{Path: filepath.Join(alias, "not-yet-created"), Roles: ManagedRootProtected},
	})
	nested, err := set.NestedProtectedRoots(rootID)
	if err != nil || len(nested) != 1 || nested[0].ID != reservedID {
		t.Fatalf("missing reservation under root alias = %#v, %v; want %s", nested, err, reservedID)
	}
}

func TestManagedRootGroupsMergeRestrictionsOnlyForProvenAliases(t *testing.T) {
	base := t.TempDir()
	cache := filepath.Join(base, "cache")
	alias := filepath.Join(base, "cache-link")
	if err := os.Mkdir(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(cache, alias); err != nil {
		t.Skipf("directory symlink unavailable: %v", err)
	}
	if same, err := SameFilePaths(cache, alias); err != nil || !same {
		t.Fatalf("filesystem alias equality = %v, %v", same, err)
	}
	aliasID := ManagedRootID(alias, base)
	set := NewManagedRootSet(base, []ManagedRootSpec{
		{Path: alias, Roles: ManagedRootBrowse | ManagedRootLocal},
		{Path: cache, Roles: ManagedRootBrowse | ManagedRootLocal | ManagedRootProtected, Restrictions: ManagedRootSkipSpecial},
	})
	groups, err := set.BrowseRootGroups()
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].Root.Restrictions&ManagedRootSkipSpecial == 0 || len(groups[0].Members) != 2 {
		t.Fatalf("physical alias root roles/restrictions = %#v", groups)
	}
	if allowed, err := set.AllowsOwner(aliasID, "models"); err != nil || allowed {
		t.Fatalf("proven alias did not merge SkipSpecial: allowed=%v err=%v", allowed, err)
	}
	missing := NewManagedRootSet(base, []ManagedRootSpec{
		{Path: filepath.Join(base, "missing-a"), Roles: ManagedRootBrowse | ManagedRootLocal},
		{Path: filepath.Join(base, "missing-b"), Roles: ManagedRootBrowse | ManagedRootLocal | ManagedRootProtected, Restrictions: ManagedRootSkipSpecial},
	})
	missingGroups, err := missing.BrowseRootGroups()
	if err != nil {
		t.Fatal(err)
	}
	if len(missingGroups) != 2 {
		t.Fatalf("unobserved missing roots were collapsed: %#v", missingGroups)
	}
	if _, err := SameFilePaths(filepath.Join(base, "missing-a"), filepath.Join(base, "missing-b")); err == nil {
		t.Fatal("missing paths were treated as known physically distinct objects")
	}
}

func TestNamespaceObservationRetainsDistinctLocalSlotsAndAliases(t *testing.T) {
	base := t.TempDir()
	first := filepath.Join(base, "first")
	alias := filepath.Join(base, "alias")
	second := filepath.Join(base, "second")
	firstRepo := filepath.Join(first, "owner", "model")
	secondRepo := filepath.Join(second, "owner", "model")
	for _, path := range []string{firstRepo, secondRepo} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(first, alias); err != nil {
		t.Skipf("directory symlink unavailable: %v", err)
	}
	set := NewManagedRootSet(base, []ManagedRootSpec{
		{Path: first, Roles: ManagedRootBrowse | ManagedRootProtected | ManagedRootLocal},
		{Path: alias, Roles: ManagedRootBrowse | ManagedRootProtected | ManagedRootLocal},
		{Path: second, Roles: ManagedRootBrowse | ManagedRootProtected | ManagedRootLocal},
	})
	all, err := set.ObserveNamespaceMemberships()
	if err != nil {
		t.Fatal(err)
	}
	memberships := NamespaceMembershipsFor(all, "owner/model", RepoTypeModel)
	if len(memberships) != 3 {
		t.Fatalf("memberships = %#v; want all three logical root slots", memberships)
	}
	if memberships[0].Root.ID == memberships[1].Root.ID || memberships[0].Root.ID == memberships[2].Root.ID || memberships[1].Root.ID == memberships[2].Root.ID {
		t.Fatalf("distinct configured root identities were collapsed: %#v", memberships)
	}
	if !os.SameFile(memberships[0].Directory, memberships[1].Directory) || os.SameFile(memberships[0].Directory, memberships[2].Directory) {
		t.Fatalf("directory-object facts did not identify alias vs independent slot: %#v", memberships)
	}
}

func TestRepoPhysicalCopiesMergesCrossRoleAliasesButRetainsRootRestrictions(t *testing.T) {
	base := t.TempDir()
	hub := filepath.Join(base, "hub")
	hubRepo := filepath.Join(hub, "models--owner--model")
	localRoot := filepath.Join(base, "local")
	localRepo := filepath.Join(localRoot, "owner", "model")
	if err := os.MkdirAll(hubRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(localRepo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(hubRepo, localRepo); err != nil {
		t.Skipf("directory symlink unavailable: %v", err)
	}
	set := NewManagedRootSet(base, []ManagedRootSpec{
		{Path: hub, Roles: ManagedRootHub | ManagedRootProtected},
		{Path: localRoot, Roles: ManagedRootBrowse | ManagedRootLocal | ManagedRootProtected, Restrictions: ManagedRootSkipSpecial},
	})
	memberships, err := set.ObserveNamespaceMemberships()
	if err != nil {
		t.Fatal(err)
	}
	matches := NamespaceMembershipsFor(memberships, "owner/model", RepoTypeModel)
	if len(matches) != 2 || matches[0].Root.ID == matches[1].Root.ID || !os.SameFile(matches[0].Directory, matches[1].Directory) {
		t.Fatalf("cross-role logical memberships and same-object facts were not retained: %#v", matches)
	}
}

func TestRepoPhysicalCopiesTreatFriendlyFoldersAsProjectionOnly(t *testing.T) {
	base := t.TempDir()
	hub := filepath.Join(base, "hub")
	models := filepath.Join(base, "models")
	hubRepo := filepath.Join(hub, "models--owner--model")
	projection := filepath.Join(models, "owner", "model")
	for _, path := range []string{hubRepo, projection} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	set := NewManagedRootSet(base, []ManagedRootSpec{
		{Path: hub, Roles: ManagedRootHub | ManagedRootProtected},
		{Path: models, Roles: ManagedRootBrowse | ManagedRootProtected | ManagedRootModelProjection},
	})
	copies, err := set.RepoPhysicalCopies("owner/model", RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if len(copies.Copies) != 1 || copies.Copies[0].Kind != PhysicalCopyHub || copies.Copies[0].Path != hubRepo || len(copies.Projections) != 1 || copies.Projections[0].Path != projection {
		t.Fatalf("Hub and friendly projection observations = %#v", copies)
	}
	if err := os.RemoveAll(projection); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(projection), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(hubRepo, projection); err != nil {
		t.Skipf("directory symlink unavailable: %v", err)
	}
	copies, err = set.RepoPhysicalCopies("owner/model", RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	projectionID := ManagedRootID(models, base)
	if len(copies.Copies) != 1 || len(copies.Projections) != 1 || copies.Copies[0].Kind != PhysicalCopyHub || copies.Projections[0].RootIDs[0] != projectionID {
		t.Fatalf("same-object Hub/projection memberships were not retained separately: %#v", copies)
	}

	if err := os.RemoveAll(hubRepo); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(projection); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(projection, 0o755); err != nil {
		t.Fatal(err)
	}
	orphanCopies, err := set.RepoPhysicalCopies("owner/model", RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if len(orphanCopies.Copies) != 0 || len(orphanCopies.Projections) != 1 {
		t.Fatalf("orphan projection was not retained as a separate observed slot: %#v", orphanCopies)
	}
}

func TestNamespaceMembershipRetainsMixedProjectionAndLocalRoles(t *testing.T) {
	base := t.TempDir()
	models := filepath.Join(base, "models")
	repo := filepath.Join(models, "owner", "model")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "weights.gguf"), []byte("weights"), 0o644); err != nil {
		t.Fatal(err)
	}
	set := NewManagedRootSet(base, []ManagedRootSpec{
		{Path: models, Roles: ManagedRootBrowse | ManagedRootProtected | ManagedRootModelProjection},
		{Path: models, Roles: ManagedRootBrowse | ManagedRootProtected | ManagedRootLocal},
	})
	memberships, err := set.ObserveNamespaceMemberships()
	if err != nil {
		t.Fatal(err)
	}
	matches := NamespaceMembershipsFor(memberships, "owner/model", RepoTypeModel)
	if len(matches) != 2 {
		t.Fatalf("mixed namespace roles did not preserve both logical memberships: %#v", matches)
	}
	if matches[0].Root.ID != matches[1].Root.ID || matches[0].Kind == matches[1].Kind {
		t.Fatalf("logical membership identities/roles were conflated: %#v", matches)
	}
	for _, membership := range matches {
		if membership.Kind == PhysicalCopyLocal && membership.TypeKnown {
			t.Fatalf("Local membership inferred repository type: %#v", membership)
		}
	}
}
