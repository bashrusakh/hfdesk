// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package hfdownloader

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// FS_CASEFOLD_FL is the Linux per-directory Unicode casefold flag.
const ownershipFSCasefoldFlag = 0x40000000

// ownershipCaseProbePrefix names transient collation probe entries. The shape
// matches no reserved staging-artifact classifier, so cache surfaces never
// race or reclaim a probe entry mid-observation.
const ownershipCaseProbePrefix = ".hfdesk-caseprobe-"

func ownershipDirectoryCaseProfile(path, _ string) (ownershipCaseProfile, error) {
	file, err := os.Open(path)
	if err == nil {
		defer file.Close()
		flags, err := unix.IoctlGetInt(int(file.Fd()), unix.FS_IOC_GETFLAGS)
		if err != nil {
			return ownershipCaseProfile{}, fmt.Errorf("%w: query casefold flag on %q: %v", errOwnershipCaseProfileUnavailable, path, err)
		}
		return ownershipCaseProfile{Version: ownershipNodeFormatVersion, Provider: "linux-fs-casefold-v1", Sensitive: flags&ownershipFSCasefoldFlag == 0}, nil
	}
	if !errors.Is(err, os.ErrPermission) {
		return ownershipCaseProfile{}, err
	}
	// Opening a directory for read needs READ permission, which the cache
	// permission envelope does not require: a payload directory the downloader
	// writes into may be owner write+search only (mode 0300). Establish the
	// collation from observable lookup behavior instead of the inode flag.
	sensitive, probeErr := probeOwnershipCaseCollation(path)
	if probeErr != nil {
		return ownershipCaseProfile{}, fmt.Errorf("%w: probe case profile on %q without directory read: %v", errOwnershipCaseProfileUnavailable, path, probeErr)
	}
	return ownershipCaseProfile{Version: ownershipNodeFormatVersion, Provider: "linux-case-probe-v1", Sensitive: sensitive}, nil
}

func ownershipControlDirectoryCaseProfile(path, controlProbe string) (ownershipCaseProfile, error) {
	return ownershipDirectoryCaseProfile(path, controlProbe)
}

// ownershipCaseProbeObservation is the raw outcome of one case-collision
// probe round; classifyOwnershipCaseCollation owns the verdict. Identity
// fields are set only when a name still resolves to the entry this probe
// itself created: a lost or changed binding is interference, never collation
// evidence.
type ownershipCaseProbeObservation struct {
	variantErr    error // outcome of creating the ASCII case-variant name
	firstBound    bool  // probe entry name still resolves to its created identity
	variantBound  bool  // created variant name still resolves to its created identity
	variantExists bool  // EEXIST: variant name still resolves to a distinct entry
	sameEntry     bool  // variant name resolves to our probe entry itself
}

// classifyOwnershipCaseCollation turns probe observations into a collation
// verdict: true means case-sensitive. Any inconclusive observation fails
// closed instead of guessing, because a wrong verdict either splits one
// folded item across control identities or merges distinct items.
func classifyOwnershipCaseCollation(obs ownershipCaseProbeObservation) (bool, error) {
	if obs.variantErr != nil && !errors.Is(obs.variantErr, os.ErrExist) {
		return false, obs.variantErr
	}
	if !obs.firstBound {
		return false, fmt.Errorf("case probe entry vanished or changed identity during observation")
	}
	if obs.variantErr == nil {
		if !obs.variantBound {
			return false, fmt.Errorf("case probe variant vanished or changed identity during observation")
		}
		// Both spellings coexisted under our own created entries: case-sensitive.
		return true, nil
	}
	if !obs.variantExists {
		return false, fmt.Errorf("case probe variant vanished during observation")
	}
	if obs.sameEntry {
		// The variant name aliased onto our probe entry: the directory folds case.
		return false, nil
	}
	// A distinct pre-existing entry holds the variant spelling while our probe
	// entry keeps its created identity: case-sensitive.
	return true, nil
}

func probeOwnershipCaseCollation(dir string) (bool, error) {
	first, err := os.CreateTemp(dir, ownershipCaseProbePrefix)
	if err != nil {
		return false, fmt.Errorf("create case probe: %w", err)
	}
	firstPath := first.Name()
	// Capture the created identity from the open handle: later observations
	// must bind to it, so a foreign unlink/replace can never read as evidence.
	createdFirst, statErr := first.Stat()
	_ = first.Close()
	if statErr != nil {
		_ = os.Remove(firstPath)
		return false, fmt.Errorf("identify case probe entry: %w", statErr)
	}
	defer os.Remove(firstPath)

	variantPath := filepath.Join(dir, ownershipCaseFlipASCII(filepath.Base(firstPath)))
	variant, err := os.OpenFile(variantPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	obs := ownershipCaseProbeObservation{variantErr: err}
	var createdVariant os.FileInfo
	if err == nil {
		createdVariant, statErr = variant.Stat()
		_ = variant.Close()
		defer os.Remove(variantPath)
	}
	var firstNow os.FileInfo
	if info, lstatErr := os.Lstat(firstPath); lstatErr == nil && os.SameFile(createdFirst, info) {
		firstNow = info
		obs.firstBound = true
	}
	switch {
	case err == nil:
		if info, lstatErr := os.Lstat(variantPath); lstatErr == nil && createdVariant != nil && os.SameFile(createdVariant, info) {
			obs.variantBound = true
		}
	case errors.Is(err, os.ErrExist):
		if variantInfo, lstatErr := os.Lstat(variantPath); lstatErr == nil {
			obs.variantExists = true
			if firstNow != nil {
				obs.sameEntry = os.SameFile(firstNow, variantInfo)
			}
		}
	}
	return classifyOwnershipCaseCollation(obs)
}

func ownershipCaseFlipASCII(name string) string {
	flipped := []byte(name)
	for i, c := range flipped {
		switch {
		case c >= 'a' && c <= 'z':
			flipped[i] = c - ('a' - 'A')
		case c >= 'A' && c <= 'Z':
			flipped[i] = c + ('a' - 'A')
		}
	}
	return string(flipped)
}

func setOwnershipControlDirectoryCaseProfile(path string, profile ownershipCaseProfile) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	flags, err := unix.IoctlGetInt(int(file.Fd()), unix.FS_IOC_GETFLAGS)
	if err != nil {
		return fmt.Errorf("%w: query control casefold flag on %q: %v", errOwnershipCaseProfileUnavailable, path, err)
	}
	updated := flags &^ ownershipFSCasefoldFlag
	if !profile.Sensitive {
		updated |= ownershipFSCasefoldFlag
	}
	if updated == flags {
		return nil
	}
	if err := unix.IoctlSetInt(int(file.Fd()), unix.FS_IOC_SETFLAGS, updated); err != nil {
		return fmt.Errorf("set control casefold flag on %q: %w", path, err)
	}
	return nil
}
