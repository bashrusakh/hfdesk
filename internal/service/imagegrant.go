// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"errors"
	"fmt"
)

// This file holds the part of the image grant (imagegrant_windows.go) that is
// not a Windows detail: the descriptor string the grant appends, the class of
// its refusal and the text of that refusal. The platform files own the access
// list itself; these facts are strings, so they build on every platform and a
// run of `go test ./internal/service` on any of them guards them.

// scmServiceIdentitySID is the well-known identifier of the account a
// machine-wide instance runs as: NT AUTHORITY\LocalService. It is declared
// beside the shared descriptor string that names it, so the trustee of the
// appended entry and the trustee the decision reads are the same identifier.
const scmServiceIdentitySID = "S-1-5-19"

// ErrServiceImageAccessUnrepaired classifies the refusal of an installation
// that could not give the account of the instance the access a start of its
// registered program needs: read and execute on the program, traversal on the
// directories above it.
//
// The installation refuses before it writes a registration, so no
// registration of this attempt exists and none remains. It carries no text of
// its own so that %w adds nothing to the refusal message shown to the user
// verbatim; classify with errors.Is, never by wording (the rule is stated
// with ErrAlreadyRegistered).
var ErrServiceImageAccessUnrepaired = errors.New("")

// scmImageGrantSDDL returns the security descriptor string of the single entry
// the image grant appends: access allowed, no ACE flags, the given mask and
// the account of the instance as the trustee.
//
// The "D:" section is not decoration. The platform parses a descriptor string
// with ConvertStringSecurityDescriptorToSecurityDescriptorW, which requires at
// least one section and refuses a bare entry list with ERROR_INVALID_PARAMETER
// — "The parameter is incorrect" on the installation of a service. The entry
// keeps its flags empty, so it applies to the object alone ("this folder
// only") and inherits nowhere, and the list it is appended to keeps its own
// inheritance untouched.
func scmImageGrantSDDL(mask uint32) string {
	return fmt.Sprintf("D:(A;;0x%08X;;;%s)", mask, scmServiceIdentitySID)
}

// serviceImageAccessRefusal is the refusal of an installation whose program
// the account of the instance cannot be given the access a start needs: the
// result (nothing was registered), the cause, the entry a partial grant
// already left behind, when there is one, and the single action that resolves
// the cause — a pair the account can read. The action names the command, so
// the surfaces append no second route to the report.
//
// identity names the account of the instance, the platform's name for it. The
// returned error classifies as ErrServiceImageAccessUnrepaired through
// wrapping, which the surfaces use to keep a registration report away from a
// refusal that never reached a registration.
func serviceImageAccessRefusal(identity, path string, cause error, residue string) error {
	if residue != "" {
		return fmt.Errorf("service: the account the instance runs as (%s) cannot be given the access it needs on %q: %w: the installation refuses instead of registering a program the service cannot start (%s). Move the HFDesk pair into a folder the service account can read, then run %q from an elevated shell again%w",
			identity, path, cause, residue, "hfdesk service install", ErrServiceImageAccessUnrepaired)
	}
	return fmt.Errorf("service: the account the instance runs as (%s) cannot be given the access it needs on %q: %w: the installation refuses instead of registering a program the service cannot start. Move the HFDesk pair into a folder the service account can read, then run %q from an elevated shell again%w",
		identity, path, cause, "hfdesk service install", ErrServiceImageAccessUnrepaired)
}
