// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package service

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log"

	"golang.org/x/sys/windows"
)

// The program of a registration is started by the service control manager in
// a process of the account of the instance, and that process opens the
// program image with the token of the account: read and execute on the file
// are needed, and traversal of the directories above it. A program in the
// profile of a user carries none of that — a profile grants its access to
// the user, the administrators and the system — and every start of it fails
// with the access denied of the platform (error 5), from the elevated helper
// alike and from the services console.
//
// The installation runs elevated and repairs exactly that before it writes a
// registration: read and execute on the program, traversal on each directory
// of the route above it — every grant one access-allowed entry for the
// account of the instance alone (S-1-5-19), appended to the list the path
// already has. Nothing is removed, the inheritance of a directory is never
// cut, no other trustee gains anything, and a list that already carries the
// access is left alone, so a repeat installation writes nothing.
//
// A grant the machine refuses refuses the installation with the action that
// resolves it; an access list that cannot be read stays inconclusive, the way
// every inspection of this layer is inconclusive, and is logged with the
// consequence it can have instead of being turned into a verdict.

// scmImageGrantTrustees decides the access of the account of the instance:
// only the entries of exactly these trustees are read as its access. The
// account itself (S-1-5-19), Everyone and Authenticated Users are carried by
// its token with certainty. The built-in Users group is deliberately not
// assumed in either direction: an entry of a group the account is not proven
// to belong to neither covers it (a skipped grant could leave a closed route
// closed) nor denies it, and the doubt resolves toward writing the narrow
// grant rather than toward the skip.
var scmImageGrantTrustees = map[string]bool{
	scmServiceIdentitySID: true,
	"S-1-1-0":             true,
	"S-1-5-11":            true,
}

// The access-allowed and access-denied entry types of a Windows access list.
const (
	scmGrantACEAllow = 0
	scmGrantACEDeny  = 1
)

// scmGrantExecutableAccess gives the account of the instance the access a
// start of the registered program needs (the file header states the design).
//
// The variable is the seam of the installation tests: a test that installs a
// program whose access list is not its subject leaves the real grant aside,
// the way it leaves the access check aside.
var scmGrantExecutableAccess = grantServiceImageAccess

// scmServiceImageChain returns the directories a start of the program walks:
// the route from the program to the root of its volume, as written and
// through the resolved location of every link on it.
//
// A route that cannot be fully named returns the directories that could be
// named beside the reason: the caller grants those and logs the reason, the
// way an uninspectable route stays inconclusive everywhere in this layer.
var scmServiceImageChain = func(executable string) ([]string, error) {
	resolved, err := scmResolvePath(executable)
	if err != nil {
		return scmParentDirs(executable), fmt.Errorf("the real location of %q cannot be read: %w", executable, err)
	}
	return scmPathTraversalDirectories(executable, resolved)
}

// scmApplyImageGrantACL writes a list with one appended entry.
//
// The descriptor stays inheritable: the entry is appended to the list the
// path already has and nothing else changes, so the protection the
// installation applies to its own directories is not touched here, and the
// folders above a program the user named keep the inheritance they run on.
//
// The variable is the seam of the tests that drive a refused write.
var scmApplyImageGrantACL = func(path string, dacl *windows.ACL) error {
	return windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION,
		nil,
		nil,
		dacl,
		nil,
	)
}

// The states of one path in the decision of the grant.
const (
	// scmGrantSkip: the list already carries the access for the trustees
	// of the account, or the path carries no list at all — a path without
	// an access list restricts nobody.
	scmGrantSkip = iota
	// scmGrantNeeded: no entry of the trustees covers the access and none
	// of them denies it, so the grant has work to do.
	scmGrantNeeded
	// scmGrantDenied: a deny entry of the trustees already refuses the
	// access, so an appended allow entry would change nothing at the next
	// access check of the start.
	scmGrantDenied
	// scmGrantUnreadable: the list could not be read; no verdict.
	scmGrantUnreadable
)

// grantServiceImageAccess grants the account of the instance the access a
// start of the program needs (the file header states the design).
//
// Every list is read and decided before the first one is changed, so a
// refusal over a deny entry leaves every descriptor of the machine as it
// was. The program is written first: when a directory of the route then
// refuses the write, the refusal names the entry that remains instead of
// hiding it.
func grantServiceImageAccess(executable string) error {
	chain, routeErr := scmServiceImageChain(executable)
	if routeErr != nil {
		log.Printf("service: the traversal route of the program %q cannot be fully named (%v): the grant covers the directories that could be named", executable, routeErr)
	}

	imageState, imageErr := scmDecideImageGrant(executable, windows.FILE_EXECUTE)
	switch imageState {
	case scmGrantDenied:
		return scmImageGrantRefusal(executable, errors.New("a deny entry of its access list already refuses the required access to the account"), "")
	case scmGrantUnreadable:
		log.Printf("service: the access list of the program %q cannot be read (%v): the installation could not give the account of the instance its access there, and a start that finds the program unusable fails with the access denied of the platform", executable, imageErr)
	}

	var needed []string
	for _, dir := range chain {
		state, err := scmDecideImageGrant(dir, windows.FILE_TRAVERSE)
		switch state {
		case scmGrantDenied:
			log.Printf("service: a deny entry of %q already refuses traversal to the account of the instance: the installation leaves the entry alone, and a start of the program can still fail with the access denied of the platform", dir)
		case scmGrantUnreadable:
			log.Printf("service: the access list of %q cannot be read (%v): the installation could not grant the account of the instance traversal there", dir, err)
		case scmGrantNeeded:
			needed = append(needed, dir)
		}
	}

	granted := false
	if imageState == scmGrantNeeded {
		if err := scmWriteImageGrant(executable, windows.FILE_GENERIC_READ|windows.FILE_GENERIC_EXECUTE); err != nil {
			if !scmGrantACLUnavailable(err) {
				return scmImageGrantRefusal(executable, err, "")
			}
			log.Printf("service: the object of the program %q carries no access list at all (%v): nothing there can restrict the account of the instance, and the grant writes no entry", executable, err)
		} else {
			granted = true
		}
	}
	for _, dir := range needed {
		if err := scmWriteImageGrant(dir, windows.FILE_TRAVERSE); err != nil {
			if !scmGrantACLUnavailable(err) {
				residue := ""
				if granted {
					residue = fmt.Sprintf("the access entry already written for %q remains: it gives read and execute to the account of the instance alone", executable)
				}
				return scmImageGrantRefusal(dir, err, residue)
			}
			log.Printf("service: the object of %q carries no access list at all (%v): nothing there can restrict the account of the instance, and the grant writes no entry", dir, err)
			continue
		}
	}
	return nil
}

// scmGrantACLUnavailable reports that a write failed because the object
// carries no access control at all: a filesystem such as FAT cannot hold
// access lists, and one that cannot hold them cannot restrict the account of
// the instance either. Such a path is logged and left as it is instead of
// being refused — a refusal with the action stays reserved for the real
// barriers, a deny entry and a list the machine does not let us change.
func scmGrantACLUnavailable(err error) bool {
	return errors.Is(err, windows.ERROR_INVALID_FUNCTION) ||
		errors.Is(err, windows.ERROR_NOT_SUPPORTED) ||
		errors.Is(err, windows.ERROR_NO_SECURITY_ON_OBJECT)
}

// scmDecideImageGrant reads the access list of a path and decides what the
// grant does with it; the error is set for the unreadable state only.
func scmDecideImageGrant(path string, bit uint32) (int, error) {
	dacl, err := scmPathDACL(path)
	if err != nil {
		return scmGrantUnreadable, err
	}
	covered, denied := scmGrantListState(dacl, bit)
	switch {
	case denied:
		return scmGrantDenied, nil
	case covered:
		return scmGrantSkip, nil
	default:
		return scmGrantNeeded, nil
	}
}

// scmWriteImageGrant appends the entry of the account to the list of the
// path and writes the list back. The list is read again at the write, so
// the entry lands in the list the path carries now.
func scmWriteImageGrant(path string, mask uint32) error {
	dacl, err := scmPathDACL(path)
	if err != nil {
		return fmt.Errorf("the access list of that path cannot be read for the grant: %w", err)
	}
	merged, err := scmAppendImageGrant(dacl, mask)
	if err != nil {
		return err
	}
	if err := scmApplyImageGrantACL(path, merged); err != nil {
		return fmt.Errorf("the access list of that path cannot be changed: %w", err)
	}
	return nil
}

// scmAppendImageGrant returns the list with one appended access-allowed
// entry for the account of the instance and the given mask.
func scmAppendImageGrant(dacl *windows.ACL, mask uint32) (*windows.ACL, error) {
	if dacl == nil {
		return nil, errors.New("the path carries no access list to append the grant to")
	}
	sddl := fmt.Sprintf("(A;;0x%08X;;;%s)", mask, scmServiceIdentitySID)
	grant, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return nil, fmt.Errorf("build the access entry of the account of the instance: %w", err)
	}
	grantList, _, err := grant.DACL()
	if err != nil {
		return nil, fmt.Errorf("read the access entry of the account of the instance: %w", err)
	}
	return appendACLEntry(dacl, grantList)
}

// scmPathDACL reads the access list of a path; a path with no list at all
// reports a nil list, which restricts nobody.
func scmPathDACL(path string) (*windows.ACL, error) {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return nil, err
	}
	if sd == nil {
		return nil, nil
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		if errors.Is(err, windows.ERROR_OBJECT_NOT_FOUND) {
			return nil, nil
		}
		return nil, err
	}
	return dacl, nil
}

// scmGrantListState reports whether the entries of the trustees of the
// account already cover the access bit on the path, and whether one of them
// denies it. A deny wins over an allow, the way the platform evaluates the
// list at the start. A generic execute or generic all entry covers the bit
// the way the platform maps it for a file object; a generic read entry does
// not carry it.
func scmGrantListState(list *windows.ACL, bit uint32) (covered, denied bool) {
	if list == nil {
		return true, false
	}
	raw := accessListBytes(list)
	if len(raw) < accessListHeaderSize {
		return false, false
	}
	count := binary.LittleEndian.Uint16(raw[4:6])
	offset := accessListHeaderSize
	for i := uint16(0); i < count; i++ {
		if offset+8 > len(raw) {
			break
		}
		size := int(binary.LittleEndian.Uint16(raw[offset+2 : offset+4]))
		if size < 8+4 || offset+size > len(raw) {
			break
		}
		mask := binary.LittleEndian.Uint32(raw[offset+4 : offset+8])
		if trustee, ok := securityIdentifierString(raw[offset+8 : offset+size]); ok &&
			scmImageGrantTrustees[trustee] && scmGrantMaskCovers(mask, bit) {
			switch raw[offset] {
			case scmGrantACEDeny:
				denied = true
			case scmGrantACEAllow:
				covered = true
			}
		}
		offset += size
	}
	return covered && !denied, denied
}

// scmGrantMaskCovers reports whether an access mask carries the bit, either
// directly or through a generic right the platform maps onto it for a file
// object.
func scmGrantMaskCovers(mask, bit uint32) bool {
	return mask&bit != 0 || mask&windows.GENERIC_EXECUTE != 0 || mask&windows.GENERIC_ALL != 0
}

// scmImageGrantRefusal is the refusal of an installation whose program the
// account of the instance cannot be given the access a start needs: the
// result (nothing was registered), the cause, the entry a partial grant
// already left behind, when there is one, and the single action that
// resolves the cause — a pair the account can read. The action names the
// command, so the surfaces append no second route to the report.
func scmImageGrantRefusal(path string, cause error, residue string) error {
	if residue != "" {
		return fmt.Errorf("service: the account the instance runs as (%s) cannot be given the access it needs on %q: %w: the installation refuses instead of registering a program the service cannot start (%s). Move the HFDesk pair into a folder the service account can read, then run %q from an elevated shell again",
			scmServiceIdentityName, path, cause, residue, "hfdesk service install")
	}
	return fmt.Errorf("service: the account the instance runs as (%s) cannot be given the access it needs on %q: %w: the installation refuses instead of registering a program the service cannot start. Move the HFDesk pair into a folder the service account can read, then run %q from an elevated shell again",
		scmServiceIdentityName, path, cause, "hfdesk service install")
}
