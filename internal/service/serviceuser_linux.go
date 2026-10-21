// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package service

// defaultServiceUser returns the default account a machine-wide instance
// runs as on Linux.
//
// The account is a dedicated, stable, unprivileged service account: the
// files it owns have to stay writable across reboots, and the instance
// needs no privilege of root. The installation never creates it: an
// administrator does that once, and the installation refuses to register an
// instance whose account does not exist.
func defaultServiceUser() string {
	return "hfdesk"
}

// defaultScope returns the default installation scope on Linux.
//
// The systemd supervisor registers a per-user unit as well as a
// machine-wide one, so the user scope is the default here.
func defaultScope() Scope {
	return ScopeUser
}

// supportsUserScope reports that a per-user installation exists on Linux.
//
// The systemd supervisor registers a per-user unit, so the scope is
// available.
func supportsUserScope() bool {
	return true
}

// supportsNamedServiceAccount reports that the account of a machine-wide
// instance can be named on Linux.
//
// The instance runs as a dedicated account of the machine, and the
// installation refuses to register an instance whose account does not
// exist.
func supportsNamedServiceAccount() bool {
	return true
}
