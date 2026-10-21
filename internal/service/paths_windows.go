// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package service

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/bashrusakh/hfdesk/internal/runtimepaths"
)

// defaultPathsForScope returns the runtime paths of a service instance in the
// given scope.
//
// The Windows service control manager registers machine-wide services only,
// so a user scope installation does not exist on this platform and is
// rejected explicitly instead of silently falling back to a machine-wide
// root.
func defaultPathsForScope(scope Scope) (runtimepaths.Paths, error) {
	switch scope {
	case ScopeUser:
		return runtimepaths.Paths{}, fmt.Errorf("service: scope %q is not supported on windows: the service control manager registers machine-wide services only", scope)
	case ScopeSystem:
		// The machine-wide instance never uses the profile of the user who
		// happens to run the install command.
		root := filepath.Join(programDataDir(), "HFDesk")
		return runtimepaths.Paths{
			ConfigFile: filepath.Join(root, "hfdesk.json"),
			StateDir:   root,
			LogDir:     filepath.Join(root, "logs"),
		}, nil
	default:
		return runtimepaths.Paths{}, fmt.Errorf("service: unknown scope %q (want %q)", scope, ScopeSystem)
	}
}

// programDataDir returns the machine-wide program data directory. The
// environment wins, so an installation follows a relocated ProgramData; the
// documented default is the fallback.
func programDataDir() string {
	if dir := os.Getenv("ProgramData"); dir != "" {
		return dir
	}
	return `C:\ProgramData`
}
