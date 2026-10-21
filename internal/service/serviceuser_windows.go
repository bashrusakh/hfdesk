// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package service

// defaultServiceUser returns the default account a machine-wide instance
// runs as on Windows.
//
// Windows has no account to name: the instance runs as LocalService, a
// built-in least-privilege service account of the service control manager.
// The value is therefore empty, and the platform manager uses the built-in
// account instead of a name of its own.
func defaultServiceUser() string {
	return ""
}

// defaultScope returns the default installation scope on Windows.
//
// The service control manager registers machine-wide services only, so the
// system scope is the default here.
func defaultScope() Scope {
	return ScopeSystem
}

// supportsUserScope reports that a per-user installation does not exist on
// Windows.
//
// The service control manager registers machine-wide services only, so the
// per-user scope is not available.
func supportsUserScope() bool {
	return false
}

// supportsNamedServiceAccount reports that the account of a machine-wide
// instance cannot be named on Windows.
//
// The instance runs as LocalService, a built-in least-privilege service
// account of the service control manager, which has no name of its own.
func supportsNamedServiceAccount() bool {
	return false
}
