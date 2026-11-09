// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build unix && !linux && !darwin && !solaris && !aix

package hfdownloader

// These Unix targets lack a read-only native case-profile provider here; cache
// ownership fails closed until one is implemented for the target filesystem.
func ownershipDirectoryCaseProfile(string, string) (ownershipCaseProfile, error) {
	return ownershipCaseProfile{}, errOwnershipCaseProfileUnavailable
}

func ownershipControlDirectoryCaseProfile(path, controlProbe string) (ownershipCaseProfile, error) {
	return ownershipDirectoryCaseProfile(path, controlProbe)
}

func setOwnershipControlDirectoryCaseProfile(string, ownershipCaseProfile) error {
	return errOwnershipCaseProfileUnavailable
}
