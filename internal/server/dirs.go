// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"os"

	"github.com/bashrusakh/hfdesk/internal/runtimepaths"
)

// The permission modes of the directories the server owns.
const (
	// interactiveDirMode is the historical mode of the per-user directories
	// of an interactive run.
	interactiveDirMode os.FileMode = 0o755
	// ownedDirMode is the owner-only mode of the directories a supervised
	// instance owns.
	ownedDirMode os.FileMode = 0o700
)

// serverDirMode returns the permission mode for a directory the server owns.
//
// A supervised instance owns its directories: they hold the job list, the
// download history, the mirror targets and the credentials of the instance,
// so nobody else has to be able to read them.
//
// An interactive run keeps the historical mode of its per-user directories.
// The container image deliberately keeps its data root world-writable so an
// arbitrary UID can create its own state there, and an owner-only directory
// created by one UID would lock a second UID out of the shared tree.
//
// The mode must not depend on which code path created the directory, so
// every directory the server creates uses this helper.
func serverDirMode() os.FileMode {
	if runtimepaths.IsInstalled() {
		return ownedDirMode
	}
	return interactiveDirMode
}
