// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

type observedEntry struct {
	name string
	info os.FileInfo
	err  error
}

func (e observedEntry) Name() string               { return e.name }
func (e observedEntry) IsDir() bool                { return e.info.IsDir() }
func (e observedEntry) Type() os.FileMode          { return e.info.Mode().Type() }
func (e observedEntry) Info() (os.FileInfo, error) { return e.info, e.err }

type observedDirectory struct {
	info    os.FileInfo
	entries []os.DirEntry
	batches [][]os.DirEntry
	err     error
}

type observedNamespace struct {
	dirs map[string]observedDirectory
	stat map[string]os.FileInfo
}

func (n *observedNamespace) openDir(path string) (effectDirectory, error) {
	path = filepath.Clean(path)
	dir, ok := n.dirs[path]
	if !ok {
		return nil, os.ErrNotExist
	}
	return &observedHandle{observedDirectory: dir}, nil
}
func (n *observedNamespace) statDir(path string) (os.FileInfo, error) {
	path = filepath.Clean(path)
	if info, ok := n.stat[path]; ok {
		return info, nil
	}
	return nil, os.ErrNotExist
}

type observedHandle struct {
	observedDirectory
	read  bool
	batch int
}

func (h *observedHandle) stat() (os.FileInfo, error) { return h.info, nil }
func (h *observedHandle) readDir(int) ([]os.DirEntry, error) {
	if len(h.batches) > 0 {
		if h.batch < len(h.batches) {
			entries := h.batches[h.batch]
			h.batch++
			return entries, nil
		}
		return nil, io.EOF
	}
	if h.read {
		if h.err != nil {
			return nil, h.err
		}
		return nil, io.EOF
	}
	h.read = true
	return h.entries, h.err
}

func TestManagedRootEffectProofContinuesAfterShortSuccessfulBatch(t *testing.T) {
	base := t.TempDir()
	hub := filepath.Join(base, "hub")
	effect := filepath.Join(hub, "models--owner--model")
	first, firstInfo := modelObservedDir(t, filepath.Join(effect, "first"))
	protectedPath := filepath.Join(effect, "later")
	_, protectedInfo := modelObservedDir(t, protectedPath)
	set := NewManagedRootSet(base, []ManagedRootSpec{
		{Path: hub, Roles: ManagedRootHub | ManagedRootProtected},
		{Path: filepath.Join(base, "elsewhere", "reservation"), Roles: ManagedRootProtected},
	})
	set.observe = &observedNamespace{
		dirs: map[string]observedDirectory{
			effect: {info: mustStat(t, effect), batches: [][]os.DirEntry{
				{observedEntry{name: "first", info: firstInfo}},
				{observedEntry{name: "later", info: protectedInfo}},
			}},
			filepath.Join(effect, "first"): first,
			protectedPath:                  {info: protectedInfo},
		},
		stat: map[string]os.FileInfo{filepath.Join(base, "elsewhere", "reservation"): protectedInfo},
	}
	if err := set.WholeCopyAllowed(ManagedRootID(hub, base), effect); err == nil {
		t.Fatal("short successful batch hid a later protected effect")
	}
}

func TestManagedRootEffectProofRequiresEOFForCompletion(t *testing.T) {
	base := t.TempDir()
	effect := filepath.Join(base, "effect")
	root, _ := modelObservedDir(t, effect)
	set := NewManagedRootSet(base, nil)
	set.observe = &observedNamespace{dirs: map[string]observedDirectory{
		effect: {info: root.info, batches: [][]os.DirEntry{{}}},
	}}
	if _, err := set.observeEffectDirectories(effect); err == nil {
		t.Fatal("empty successful batch was mistaken for explicit exhaustion")
	}
}

func TestManagedRootEffectProofAcceptsShortBatchFollowedByEOF(t *testing.T) {
	base := t.TempDir()
	effect := filepath.Join(base, "effect")
	child, childInfo := modelObservedDir(t, filepath.Join(effect, "child"))
	child.err = io.EOF
	root, _ := modelObservedDir(t, effect)
	set := NewManagedRootSet(base, nil)
	set.observe = &observedNamespace{dirs: map[string]observedDirectory{
		effect:                         {info: root.info, batches: [][]os.DirEntry{{observedEntry{name: "child", info: childInfo}}}},
		filepath.Join(effect, "child"): child,
	}}
	if _, err := set.observeEffectDirectories(effect); err != nil {
		t.Fatalf("short successful batch followed by EOF: %v", err)
	}
}

func (*observedHandle) close() error { return nil }

func modelObservedDir(t *testing.T, path string, children ...os.DirEntry) (observedDirectory, os.FileInfo) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return observedDirectory{info: info, entries: children}, info
}

func TestManagedRootEffectProofFindsProtectedObjectInOtherNamespace(t *testing.T) {
	base := t.TempDir()
	hub := filepath.Join(base, "hub")
	effect := filepath.Join(hub, "models--owner--model")
	inside := filepath.Join(effect, "application-data")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	protectedAlias := filepath.Join(base, "unrelated", "protected")
	rootDir, _ := modelObservedDir(t, effect)
	childInfo, err := os.Stat(inside)
	if err != nil {
		t.Fatal(err)
	}
	set := NewManagedRootSet(base, []ManagedRootSpec{
		{Path: hub, Roles: ManagedRootHub | ManagedRootProtected},
		{Path: protectedAlias, Roles: ManagedRootProtected},
	})
	set.observe = &observedNamespace{
		dirs: map[string]observedDirectory{
			effect: {info: rootDir.info, entries: []os.DirEntry{observedEntry{name: "application-data", info: childInfo}}},
			inside: {info: childInfo},
		},
		stat: map[string]os.FileInfo{protectedAlias: childInfo},
	}
	forward, _, err := containmentDistance(effect, protectedAlias)
	if err != nil {
		t.Fatal(err)
	}
	reverse, _, err := containmentDistance(protectedAlias, effect)
	if err != nil {
		t.Fatal(err)
	}
	if forward || reverse {
		t.Fatalf("fixture does not preserve the old namespace-ancestor false negative: forward=%v reverse=%v", forward, reverse)
	}
	if err := set.WholeCopyAllowed(ManagedRootID(hub, base), effect); err == nil {
		t.Fatal("effect proof missed a protected directory object exposed under an unrelated namespace parent")
	}
}

func TestManagedRootEffectProofRefusesIncompleteAndCycleObservations(t *testing.T) {
	for _, tc := range []struct {
		name string
		make func(t *testing.T, effect string) *observedNamespace
	}{
		{
			name: "partial read",
			make: func(t *testing.T, effect string) *observedNamespace {
				root, _ := modelObservedDir(t, effect)
				return &observedNamespace{dirs: map[string]observedDirectory{effect: {info: root.info, err: io.ErrUnexpectedEOF}}}
			},
		},
		{
			name: "unreadable entry identity",
			make: func(t *testing.T, effect string) *observedNamespace {
				root, _ := modelObservedDir(t, effect)
				return &observedNamespace{dirs: map[string]observedDirectory{
					effect: {info: root.info, entries: []os.DirEntry{observedEntry{name: "unknown", err: os.ErrPermission}}},
				}}
			},
		},
		{
			name: "active object cycle",
			make: func(t *testing.T, effect string) *observedNamespace {
				root, info := modelObservedDir(t, effect)
				child := filepath.Join(effect, "loop")
				return &observedNamespace{dirs: map[string]observedDirectory{
					effect: {info: root.info, entries: []os.DirEntry{observedEntry{name: "loop", info: info}}},
					child:  {info: info},
				}}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			hub := filepath.Join(base, "hub")
			effect := filepath.Join(hub, "models--owner--model")
			if err := os.MkdirAll(effect, 0o755); err != nil {
				t.Fatal(err)
			}
			set := NewManagedRootSet(base, []ManagedRootSpec{{Path: hub, Roles: ManagedRootHub | ManagedRootProtected}})
			set.observe = tc.make(t, effect)
			if err := set.WholeCopyAllowed(ManagedRootID(hub, base), effect); err == nil {
				t.Fatal("incomplete effect observation authorized removal")
			}
		})
	}
}

func TestManagedRootEffectProofKeepsDistinctViewsOfRepeatedObjects(t *testing.T) {
	base := t.TempDir()
	hub := filepath.Join(base, "hub")
	effect := filepath.Join(hub, "models--owner--model")
	if err := os.MkdirAll(effect, 0o755); err != nil {
		t.Fatal(err)
	}
	viewSource := filepath.Join(base, "view-source")
	viewDir, viewInfo := modelObservedDir(t, viewSource)
	_, protectedInfo := modelObservedDir(t, filepath.Join(base, "protected-source"))
	firstView, secondView := filepath.Join(effect, "first-view"), filepath.Join(effect, "second-view")
	protectedView := filepath.Join(secondView, "protected-child")
	set := NewManagedRootSet(base, []ManagedRootSpec{
		{Path: hub, Roles: ManagedRootHub | ManagedRootProtected},
		{Path: filepath.Join(base, "external-protected"), Roles: ManagedRootProtected},
	})
	set.observe = &observedNamespace{
		dirs: map[string]observedDirectory{
			effect:        {info: mustStat(t, effect), entries: []os.DirEntry{observedEntry{name: "first-view", info: viewInfo}, observedEntry{name: "second-view", info: viewInfo}}},
			firstView:     viewDir,
			secondView:    {info: viewInfo, entries: []os.DirEntry{observedEntry{name: "protected-child", info: protectedInfo}}},
			protectedView: {info: protectedInfo},
		},
		stat: map[string]os.FileInfo{filepath.Join(base, "external-protected"): protectedInfo},
	}
	if err := set.WholeCopyAllowed(ManagedRootID(hub, base), effect); err == nil {
		t.Fatal("second namespace view of repeated object was pruned")
	}
}

func TestManagedRootEffectProofProtectsMissingReservationAnchor(t *testing.T) {
	base := t.TempDir()
	hub := filepath.Join(base, "hub")
	effect := filepath.Join(hub, "models--owner--model")
	inside := filepath.Join(effect, "mounted-view")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	effectInfo := mustStat(t, effect)
	insideInfo := mustStat(t, inside)
	reservation := filepath.Join(base, "reserved-view", "next")
	set := NewManagedRootSet(base, []ManagedRootSpec{
		{Path: hub, Roles: ManagedRootHub | ManagedRootProtected},
		{Path: reservation, Roles: ManagedRootProtected},
	})
	set.observe = &observedNamespace{
		dirs: map[string]observedDirectory{
			effect: {info: effectInfo, entries: []os.DirEntry{observedEntry{name: "mounted-view", info: insideInfo}}},
			inside: {info: insideInfo},
		},
		stat: map[string]os.FileInfo{filepath.Join(base, "reserved-view"): insideInfo},
	}
	if err := set.WholeCopyAllowed(ManagedRootID(hub, base), effect); err == nil {
		t.Fatal("missing protected reservation lost its existing mounted anchor")
	}
}

func TestManagedRootEffectProofDoesNotExemptReexposedAuthorizingRoot(t *testing.T) {
	base := t.TempDir()
	hub := filepath.Join(base, "hub")
	effect := filepath.Join(hub, "models--owner--model")
	if err := os.MkdirAll(effect, 0o755); err != nil {
		t.Fatal(err)
	}
	hubInfo := mustStat(t, hub)
	set := NewManagedRootSet(base, []ManagedRootSpec{{Path: hub, Roles: ManagedRootHub | ManagedRootProtected}})
	set.observe = &observedNamespace{dirs: map[string]observedDirectory{
		effect:                                  {info: mustStat(t, effect), entries: []os.DirEntry{observedEntry{name: "reexposed-root", info: hubInfo}}},
		filepath.Join(effect, "reexposed-root"): {info: hubInfo},
	}}
	if err := set.WholeCopyAllowed(ManagedRootID(hub, base), effect); err == nil {
		t.Fatal("authorizing root object re-exposed inside effect was exempted")
	}
}

func TestManagedRootEffectProofRefusesDepthLimit(t *testing.T) {
	base := t.TempDir()
	effect := filepath.Join(base, "effect")
	if err := os.Mkdir(effect, 0o755); err != nil {
		t.Fatal(err)
	}
	current := effect
	for i := 0; i <= maxEffectDepth; i++ {
		current = filepath.Join(current, "d")
		if err := os.Mkdir(current, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	set := NewManagedRootSet(base, nil)
	if _, err := set.observeEffectDirectories(effect); err == nil {
		t.Fatal("effect observation beyond the configured depth limit was declared complete")
	}
}

func mustStat(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}
