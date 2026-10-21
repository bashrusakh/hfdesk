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
