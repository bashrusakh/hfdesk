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

// defaultScope returns the default installation scope on a platform without
// a service mode.
//
// The scope is the user scope: it is the least privileged one, and it is
// the default of every platform that offers a per-user installation.
func defaultScope() Scope {
	return ScopeUser
}

// supportsUserScope reports that the scope is not restricted on a platform
// without a service mode.
//
// The platform itself reports that service mode is unavailable, which is a
// more useful answer than a refusal of the scope.
func supportsUserScope() bool {
	return true
}

// supportsNamedServiceAccount reports that the account is not restricted on
// a platform without a service mode.
//
// The platform itself reports that service mode is unavailable, so the
// account of a machine-wide instance is never the question there.
func supportsNamedServiceAccount() bool {
	return true
}

// holdDesktopSingleton reports that there is nothing to hold.
//
// The per-session desktop singleton belongs to the Windows desktop host.
// A platform without that host has no such object, so the invariant it
// protects has no object to hold here: the release ends nothing.
func holdDesktopSingleton(string) (func(), error) {
	return func() {}, nil
}
