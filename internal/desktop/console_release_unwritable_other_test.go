// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package desktop

// denyNewFiles refuses the creation of new files in a folder.
//
// On POSIX systems the read-only mode bits the caller set on the folder
// already refuse every create, so there is nothing to add here; the shared
// probe still verifies that the refusal is real before the test asserts on
// it.
func denyNewFiles(string) error { return nil }
