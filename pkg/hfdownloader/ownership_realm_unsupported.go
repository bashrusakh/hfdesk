// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build !windows && (!unix || solaris || aix)

package hfdownloader

import "fmt"

const ownershipOSLockAvailable = false

func openOwnershipLock(string, ownershipLockMode) (ownershipLockHandle, error) {
	return nil, errOwnershipUnavailable
}

func ownershipDirectoryIdentity(path string) (string, error) {
	return "", fmt.Errorf("%w: directory identity is unavailable for %q", errOwnershipUnavailable, path)
}

func ownershipComponentKey(_ string, name string) (string, error) {
	return "case-sensitive:" + name, nil
}

func ownershipVolumeKey(root string) string { return root }

func ownershipDirectoryCaseProfile(string, string) (ownershipCaseProfile, error) {
	return ownershipCaseProfile{}, errOwnershipCaseProfileUnavailable
}

func ownershipControlDirectoryCaseProfile(string, string) (ownershipCaseProfile, error) {
	return ownershipCaseProfile{}, errOwnershipCaseProfileUnavailable
}

func setOwnershipControlDirectoryCaseProfile(string, ownershipCaseProfile) error {
	return errOwnershipCaseProfileUnavailable
}
