// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build !windows && (!unix || solaris || aix)

package hfdownloader

// currentOwnershipFileLimit reports no soft/hard model on this platform; OS
// ownership locking is unavailable here anyway (see
// ownership_realm_unsupported.go), so tree admission never runs.
func currentOwnershipFileLimit() (soft, hard uint64, ok bool) { return 0, 0, false }

// setOwnershipSoftFileLimit has nothing to set on this platform.
func setOwnershipSoftFileLimit(uint64) error { return nil }

// ownershipFileDescriptorPressure reports nothing without a limit model.
func ownershipFileDescriptorPressure(error) bool { return false }
