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
		{"foo/../../bar", true}, // cleans to ../bar
		{"/etc/passwd", true},   // absolute
		{`..\..\windows`, true}, // backslash
		{`dir\file`, true},      // backslash anywhere
		{`C:\Windows\System32`, true},
	}
	for _, c := range cases {
		if got := unsafeRepoPath(c.rel); got != c.unsafe {
			t.Errorf("unsafeRepoPath(%q) = %v, want %v", c.rel, got, c.unsafe)
		}
	}
}

func TestUnsafeBlobName(t *testing.T) {
	const hex64 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	cases := []struct {
		sha    string
		unsafe bool
	}{
		// Legitimate SHA shapes must be accepted.
		{"", false},    // absent hash (files without one)
		{hex64, false}, // lfs.sha256 / lfs.oid / top-level sha256 (plain hex)
		{"a6344aac8c09253b3b630fb776ae94478aa0275b", false}, // git sha1-style hex
		{"sha256:" + hex64, false},                          // LFS-spec prefixed oid
		{"tmp-" + hex64, false},                             // staged-name-like value, still one local component

		// Traversal / separator / absolute escapes must be rejected.
		{"..", true},
		{"../../../tmp/x", true},
		{"a/../../b", true},
		{"/etc/passwd", true},
		{`..\..\windows`, true},
		{".", true},
	}
	for _, c := range cases {
		if got := unsafeBlobName(c.sha); got != c.unsafe {
			t.Errorf("unsafeBlobName(%q) = %v, want %v", c.sha, got, c.unsafe)
		}
	}
}
