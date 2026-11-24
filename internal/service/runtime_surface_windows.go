// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package service

// openHostServiceManager opens the manager of the registration the Windows
// service host was started from.
//
// The service control manager registers machine-wide services only, so the
// host reads the system scope: the instance runs as a Windows service and the
// registration that started it is the machine-wide one. The read uses the
// rights a status needs only, which the default descriptor grants to the
// service accounts, so the instance reads its own registration without
// elevation.
var openHostServiceManager = func() (Manager, error) {
	return New(ScopeSystem)
}
