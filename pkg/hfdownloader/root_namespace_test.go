// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"os"
	"path/filepath"
	"testing"
)

func TestObserveNamespaceMembershipsRetainsAllSlotsAndOwnedRegions(t *testing.T) {
	base := t.TempDir()
	hub := filepath.Join(base, "hub")
	local := filepath.Join(base, "local")
	foreign := filepath.Join(hub, "models--bob--modelB")
	region := filepath.Join(foreign, "nested", "blobs")
	localSlot := filepath.Join(local, "bob", "modelB")
	for _, path := range []string{region, localSlot} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	set := NewManagedRootSet(base, []ManagedRootSpec{
		{Path: hub, Roles: ManagedRootHub | ManagedRootProtected},
		{Path: local, Roles: ManagedRootLocal | ManagedRootProtected},
	})
	all, err := set.ObserveNamespaceMemberships()
	if err != nil {
		t.Fatal(err)
	}
	matches := NamespaceMembershipsFor(all, "bob/modelB", RepoTypeModel)
	if len(matches) != 2 {
		t.Fatalf("memberships = %#v; want Hub and unknown-type Local slots", matches)
	}
	var hubFound, localFound, regionFound bool
	for _, membership := range matches {
		if membership.Kind == PhysicalCopyHub && membership.TypeKnown && membership.RepoType == RepoTypeModel {
			hubFound = true
			for _, observed := range membership.Regions {
				if observed.Path == region && os.SameFile(observed.Info, mustStat(t, region)) {
					regionFound = true
				}
			}
		}
		if membership.Kind == PhysicalCopyLocal && !membership.TypeKnown && membership.RepoType == "" {
			localFound = true
		}
	}
	if !hubFound || !localFound || !regionFound {
		t.Fatalf("incomplete namespace facts: Hub=%v Local=%v region=%v matches=%#v", hubFound, localFound, regionFound, matches)
	}
}

func TestObserveNamespaceMembershipsUsesRepoIDReadValidation(t *testing.T) {
	base := t.TempDir()
	local := filepath.Join(base, "local")
	for _, id := range []string{"owner/My Model", "ümlaut/模型"} {
		if err := os.MkdirAll(filepath.Join(local, filepath.FromSlash(id)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	set := NewManagedRootSet(base, []ManagedRootSpec{{Path: local, Roles: ManagedRootLocal | ManagedRootProtected}})
	all, err := set.ObserveNamespaceMemberships()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"owner/My Model", "ümlaut/模型"} {
		if got := len(NamespaceMembershipsFor(all, id, RepoTypeModel)); got != 1 {
			t.Errorf("read namespace membership for %q = %d; want 1", id, got)
		}
	}
	for _, name := range []string{"..", "bad\\name", "nested/name"} {
		if _, _, _, ok := parseHubRepoDirName("models--owner--" + name); ok {
			t.Errorf("Hub parser accepted unsafe/non-component repo %q", name)
		}
	}
	if _, _, _, ok := parseHubRepoDirName("models--owner--My Model"); !ok {
		t.Fatal("Hub parser rejected a valid repo ID component")
	}
	if _, _, typ, ok := parseHubRepoDirName("datasets--ümlaut--模型"); !ok || typ != RepoTypeDataset {
		t.Fatal("Hub dataset parser rejected a valid Unicode repo ID")
	}
}

func TestHubInternalNamespaceDoesNotBecomeLocalCopy(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "shared")
	repo := filepath.Join(root, "models--owner--model")
	if err := os.MkdirAll(filepath.Join(repo, "blobs"), 0o755); err != nil {
		t.Fatal(err)
	}
	set := NewManagedRootSet(base, []ManagedRootSpec{{Path: root, Roles: ManagedRootHub | ManagedRootLocal | ManagedRootProtected}})
	all, err := set.ObserveNamespaceMemberships()
	if err != nil {
		t.Fatal(err)
	}
	matches := NamespaceMembershipsFor(all, "owner/model", RepoTypeModel)
	if len(matches) != 1 || matches[0].Kind != PhysicalCopyHub || !matches[0].TypeKnown {
		t.Fatalf("Hub namespace produced unexpected logical copies: %#v", matches)
	}
}

func TestManagedNamespaceRetainsRolesAndPrunesNestedOwnedRegion(t *testing.T) {
	base := t.TempDir()
	dual := filepath.Join(base, "dual")
	dualRepo := filepath.Join(dual, "owner", "same")
	if err := os.MkdirAll(dualRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	dualSet := NewManagedRootSet(base, []ManagedRootSpec{{Path: dual, Roles: ManagedRootLocal | ManagedRootModelProjection | ManagedRootProtected}})
	all, err := dualSet.ObserveNamespaceMemberships()
	if err != nil {
		t.Fatal(err)
	}
	matched := NamespaceMembershipsFor(all, "owner/same", RepoTypeModel)
	var localRole, projectionRole bool
	for _, membership := range matched {
		localRole = localRole || membership.Kind == PhysicalCopyLocal && !membership.TypeKnown
		projectionRole = projectionRole || membership.Kind == 0 && membership.TypeKnown && membership.RepoType == RepoTypeModel
	}
	if len(matched) != 2 || !localRole || !projectionRole || matched[0].Root.ID != matched[1].Root.ID {
		t.Fatalf("logical Local and projection roles were not preserved: %#v", matched)
	}

	parent := filepath.Join(base, "parent")
	parentRepo := filepath.Join(parent, "owner", "nested")
	nestedRoot := filepath.Join(parentRepo, "route")
	for _, path := range []string{parentRepo, nestedRoot} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(parentRepo, "own.safetensors"), []byte("own"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nestedRoot, "child.gguf"), []byte("child"), 0o644); err != nil {
		t.Fatal(err)
	}
	set := NewManagedRootSet(base, []ManagedRootSpec{
		{Path: parent, Roles: ManagedRootLocal | ManagedRootProtected},
		{Path: nestedRoot, Roles: ManagedRootLocal | ManagedRootProtected},
	})
	all, err = set.ObserveNamespaceMemberships()
	if err != nil {
		t.Fatal(err)
	}
	parentSlot := NamespaceMembershipsFor(all, "owner/nested", RepoTypeModel)
	if len(parentSlot) < 1 {
		t.Fatalf("parent membership missing: %#v", all)
	}
	var parentOwn, nestedLeak bool
	var nestedBoundary bool
	for _, entry := range parentSlot[0].Entries {
		parentOwn = parentOwn || entry.Info.Name() == "own.safetensors"
		nestedLeak = nestedLeak || entry.Info.Name() == "child.gguf"
	}
	for _, boundary := range parentSlot[0].Boundaries {
		nestedBoundary = nestedBoundary || boundary.Path == nestedRoot && boundary.Pruned && boundary.OwnerRootID != ""
	}
	if !parentOwn || nestedLeak || !nestedBoundary {
		t.Fatalf("owned region facts mismatch: own=%v nested-leak=%v nested-boundary=%v entries=%#v boundaries=%#v", parentOwn, nestedLeak, nestedBoundary, parentSlot[0].Entries, parentSlot[0].Boundaries)
	}
}

func TestManagedNamespaceDoesNotEraseForeignRepoIDAtAlias(t *testing.T) {
	base := t.TempDir()
	firstHub := filepath.Join(base, "hub-a")
	secondHub := filepath.Join(base, "hub-b")
	foreign := filepath.Join(secondHub, "models--bob--modelB")
	alias := filepath.Join(firstHub, "models--alice--modelA")
	if err := os.MkdirAll(filepath.Join(foreign, "blobs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(firstHub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(foreign, alias); err != nil {
		t.Skipf("directory symlink unavailable: %v", err)
	}
	set := NewManagedRootSet(base, []ManagedRootSpec{
		{Path: firstHub, Roles: ManagedRootHub | ManagedRootProtected},
		{Path: secondHub, Roles: ManagedRootHub | ManagedRootProtected},
	})
	all, err := set.ObserveNamespaceMemberships()
	if err != nil {
		t.Fatal(err)
	}
	alice := NamespaceMembershipsFor(all, "alice/modelA", RepoTypeModel)
	bob := NamespaceMembershipsFor(all, "bob/modelB", RepoTypeModel)
	if len(alice) != 1 || len(bob) != 1 || alice[0].RepoID == bob[0].RepoID ||
		!os.SameFile(alice[0].Directory, bob[0].Directory) || alice[0].Root.ID == bob[0].Root.ID {
		t.Fatalf("logical slot IDs were lost at equal directory objects: Alice=%#v Bob=%#v", alice, bob)
	}
	if !alice[0].TypeKnown || !bob[0].TypeKnown || len(bob[0].Regions) < 2 ||
		filepath.Clean(bob[0].Regions[1].Path) != filepath.Clean(filepath.Join(foreign, "blobs")) {
		t.Fatalf("typed Hub identity or Bob's owned region was not retained: Alice=%#v Bob=%#v", alice[0], bob[0])
	}
}

func TestManagedNamespaceIncompleteOwnedReadIsUnknown(t *testing.T) {
	base := t.TempDir()
	local := filepath.Join(base, "local")
	repo := filepath.Join(local, "owner", "model")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	info := mustStat(t, repo)
	set := NewManagedRootSet(base, []ManagedRootSpec{{Path: local, Roles: ManagedRootLocal | ManagedRootProtected}})
	set.observe = &observedNamespace{dirs: map[string]observedDirectory{
		repo: {info: info, err: os.ErrPermission},
	}}
	if memberships, err := set.ObserveNamespaceMemberships(); err == nil || memberships != nil {
		t.Fatalf("incomplete owned-region read was treated as complete: memberships=%#v err=%v", memberships, err)
	}
}
