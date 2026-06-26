// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"path/filepath"
	"testing"
)

func TestPathInside(t *testing.T) {
	base := filepath.Join("home", "cache")
	cases := []struct {
		target string
		want   bool
	}{
		{filepath.Join(base), true},
		{filepath.Join(base, "a", "b"), true},
		{filepath.Join(base, "..", "etc"), false},
		{filepath.Join("home", "cacheX"), false}, // shared prefix but not nested
		{filepath.Join("etc", "passwd"), false},
	}
	for _, c := range cases {
		if got := PathInside(base, c.target); got != c.want {
			t.Errorf("PathInside(%q,%q)=%v want %v", base, c.target, got, c.want)
		}
	}
}

func TestSafeJoin(t *testing.T) {
	base := filepath.Join("home", "cache")
	if _, err := SafeJoin(base, filepath.Join("a", "b.txt")); err != nil {
		t.Errorf("expected ok, got %v", err)
	}
	// Note: "C:\\win" is only rejected on Windows (filepath.VolumeName is
	// OS-specific), so it is not part of this cross-platform list.
	for _, rel := range []string{"../escape", "a/../../escape", "../../etc/passwd", "/abs/escape"} {
		if _, err := SafeJoin(base, rel); err == nil {
			t.Errorf("SafeJoin(%q,%q) expected error, got nil", base, rel)
		}
	}
}

func TestRepoRejectsTraversal(t *testing.T) {
	c := NewHFCache("/root", 0)
	for _, bad := range []string{"../foo", "foo/..", "owner/..", "a/b/c", ""} {
		if _, err := c.Repo(bad, RepoTypeModel); err == nil {
			t.Errorf("Repo(%q) expected error, got nil", bad)
		}
	}
	if _, err := c.Repo("owner/name", RepoTypeModel); err != nil {
		t.Errorf("Repo(owner/name) unexpected error: %v", err)
	}
}

func TestRefAndSnapshotPropagateError(t *testing.T) {
	c := NewHFCache("/root", 0)
	r, err := c.Repo("owner/name", RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.RefPath("../escape"); err == nil {
		t.Error("RefPath traversal: expected error, got nil")
	}
	if _, err := r.SnapshotDir("../escape"); err == nil {
		t.Error("SnapshotDir traversal: expected error, got nil")
	}
	if _, err := r.SnapshotPath("commit", "../escape"); err == nil {
		t.Error("SnapshotPath traversal: expected error, got nil")
	}
}

func TestIsValidModelNameRejectsTraversal(t *testing.T) {
	bad := []string{"../foo", "foo/..", "../..", "owner/..", "..\\name", "owner/na\x00me"}
	for _, b := range bad {
		if IsValidModelName(b) {
			t.Errorf("IsValidModelName(%q)=true, want false", b)
		}
	}
	good := []string{"TheBloke/Mistral-7B", "facebook/opt-1.3b", "owner/name..v2"}
	for _, g := range good {
		if !IsValidModelName(g) {
			t.Errorf("IsValidModelName(%q)=false, want true", g)
		}
	}
}
