// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package service

// defaultServiceUser returns the default account a machine-wide instance
// runs as on macOS.
//
// The account follows the convention of the platform for a service account:
// a name that starts with an underscore is reserved for a machine level
// identity and is not a login account. It is dedicated, stable and
// unprivileged: the files it owns have to stay writable across reboots, and
// the instance needs no privilege of root.
//
// The installation never creates it: an administrator does that once, and
// the installation refuses to register an instance whose account does not
// exist.
func defaultServiceUser() string {
	return "_hfdesk"
}

// defaultScope returns the default installation scope on macOS.
//
// The launchd supervisor registers a per-user agent as well as a
// machine-wide one, so the user scope is the default here.
func defaultScope() Scope {
	return ScopeUser
}

// supportsUserScope reports that a per-user installation exists on macOS.
//
// The launchd supervisor registers a per-user agent, so the scope is
// available.
func supportsUserScope() bool {
	return true
}

// supportsNamedServiceAccount reports that the account of a machine-wide
// instance can be named on macOS.
//
// The instance runs as a dedicated account of the machine, and the
// installation refuses to register an instance whose account does not
// exist.
func supportsNamedServiceAccount() bool {
	return true
}

// holdDesktopSingleton reports that there is nothing to hold.
//
// The per-session desktop singleton belongs to the Windows desktop host.
// macOS has no desktop host, so no such object exists here and the invariant
// it protects has no object to hold: the release ends nothing.
func holdDesktopSingleton(string) (func(), error) {
	return func() {}, nil
}

// probeDesktopSingleton reports that the object did not pre-exist.
//
// The per-session desktop singleton belongs to the Windows desktop host.
// macOS has no desktop host, so no such object exists here and nobody can
// hold one: the probe reports that it did not pre-exist.
func probeDesktopSingleton(string) (func(), bool, error) {
	return nil, false, nil
}
