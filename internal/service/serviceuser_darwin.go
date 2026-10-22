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
