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
	for _, rel := range []string{"../escape", "a/../../escape", "../../etc/passwd"} {
		if _, err := SafeJoin(base, rel); err == nil {
			t.Errorf("SafeJoin(%q,%q) expected error, got nil", base, rel)
		}
	}
	// An absolute-looking rel is absorbed by filepath.Join into base, so it
	// stays contained rather than escaping.
	if got, err := SafeJoin(base, "/abs/escape"); err != nil {
		t.Errorf("SafeJoin abs rel: unexpected error %v (got %q)", err, got)
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
