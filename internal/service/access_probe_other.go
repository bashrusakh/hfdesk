// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package service

// internalAccessProbeCommand is Linux-only and remains reserved on other
// platforms so the CLI dispatch stays buildable without a platform branch.
const internalAccessProbeCommand = "--hfdesk-internal-access-check"

// RunInternalAccessProbe handles no command on non-Linux platforms.
func RunInternalAccessProbe([]string) (int, bool) { return 0, false }
