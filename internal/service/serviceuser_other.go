// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build !linux && !darwin && !windows

package service

// defaultServiceUser returns the default account a machine-wide instance
// runs as on a platform without a service mode.
//
// The shared layer never branches on the operating system, and a platform
// without a supervisor has no account to name either.
func defaultServiceUser() string {
	return ""
}
