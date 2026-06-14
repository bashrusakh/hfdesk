// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import "testing"

func TestUnsafeRepoPath(t *testing.T) {
	cases := []struct {
		rel    string
		unsafe bool
	}{
		// Legitimate repo-relative paths must be accepted.
		{"model.safetensors", false},
		{"subdir/model.bin", false},
		{"a/b/c/config.json", false},
		{"foo/./bar.txt", false}, // cleans to foo/bar.txt, stays in root
		{"a/../b.txt", false},    // cleans to b.txt, stays in root

		// Traversal / absolute / separator escapes must be rejected.
		{"", true},
		{".", true},
		{"..", true},
		{"../secret", true},
		{"foo/../../bar", true},  // cleans to ../bar
		{"/etc/passwd", true},    // absolute
		{`..\..\windows`, true},  // backslash
		{`dir\file`, true},       // backslash anywhere
		{`C:\Windows\System32`, true},
	}
	for _, c := range cases {
		if got := unsafeRepoPath(c.rel); got != c.unsafe {
			t.Errorf("unsafeRepoPath(%q) = %v, want %v", c.rel, got, c.unsafe)
		}
	}
}
