// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package service

// grantLogReadToInteractiveUsers does nothing outside Windows.
//
// On Unix platforms the log directory and the log file carry the owner-only
// mode of the instance, there is no Windows descriptor to widen, and the
// companion window that reads a machine-wide log across accounts does not
// exist there: widening the modes would be a permission change nobody asked
// for.
func grantLogReadToInteractiveUsers(path string, container bool) error {
	return nil
}
