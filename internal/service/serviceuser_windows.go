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
