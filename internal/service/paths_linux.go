// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package service

import (
	"fmt"
	"path/filepath"

	"github.com/bashrusakh/hfdesk/internal/runtimepaths"
)

// defaultPathsForScope returns the runtime paths of a service instance in the
// given scope.
//
// A user scope instance keeps its state under the per-user configuration
// directory of the invoking user, a system scope instance under the
// machine-wide directories of the machine. The two roots never overlap.
func defaultPathsForScope(scope Scope) (runtimepaths.Paths, error) {
	switch scope {
	case ScopeUser:
		// The service state of the user lives next to the settings of the
		// interactive run, inside the per-user HFDesk configuration root.
		root := filepath.Join(runtimepaths.AppConfigDir(), "service")
		return runtimepaths.Paths{
			ConfigFile: filepath.Join(root, "hfdesk.json"),
			StateDir:   root,
			LogDir:     filepath.Join(root, "logs"),
		}, nil
	case ScopeSystem:
		// The machine-wide instance never uses the home directory of the
		// user who happens to run the install command.
		return runtimepaths.Paths{
			ConfigFile: "/etc/hfdesk/hfdesk.json",
			StateDir:   "/var/lib/hfdesk",
			LogDir:     "/var/log/hfdesk",
		}, nil
	default:
		return runtimepaths.Paths{}, fmt.Errorf("service: unknown scope %q (want %q or %q)", scope, ScopeUser, ScopeSystem)
	}
}
