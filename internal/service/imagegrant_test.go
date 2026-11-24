// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// The rights the installation appends, as its Windows call sites pass them:
// FILE_GENERIC_READ|FILE_GENERIC_EXECUTE (0x001200A9) for the program and
// FILE_TRAVERSE (0x00000020) for a directory of its route. The platform
// constants live in the Windows files; these numbers keep this guard
// buildable on every platform.
const (
	scmTestImageFileRights = 0x001200A9
	scmTestImageDirRights  = 0x00000020
)

// scmTestSDDLHasSection reports whether a descriptor string opens with one of
// the section markers the platform's parser requires: O: (owner), G: (group),
// D: (access list) or S: (audit list).
//
// A bare entry list carries none, and
// ConvertStringSecurityDescriptorToSecurityDescriptorW refuses it with
// ERROR_INVALID_PARAMETER — the "The parameter is incorrect" an installation
// reported after it built its grant entry without a section.
func scmTestSDDLHasSection(sddl string) bool {
	return len(sddl) >= 2 && sddl[1] == ':' && strings.ContainsRune("OGDS", rune(sddl[0]))
}

// scmTestSDDLDACLEntries returns the fields of every entry of the access list
// section of a descriptor string, and stops the test at the first shape the
// platform's parser would refuse: a string without a section, a section
// without an entry, or an entry that is not the six fields of an SDDL entry.
func scmTestSDDLDACLEntries(t *testing.T, sddl string) [][]string {
	t.Helper()
	if !scmTestSDDLHasSection(sddl) {
		t.Fatalf("the descriptor string %q carries no section: the platform's parser refuses it with ERROR_INVALID_PARAMETER (\"The parameter is incorrect\")", sddl)
	}
	body, ok := strings.CutPrefix(sddl, "D:")
	if !ok {
		t.Fatalf("the descriptor string %q does not open with the access list section D:", sddl)
	}
	// The access list section starts with its optional flags, then its
	// entries.
	for len(body) > 0 && body[0] != '(' {
		body = body[1:]
	}
	if body == "" {
		t.Fatalf("the access list section of %q carries no entry", sddl)
	}
	var entries [][]string
	for rest := body; rest != ""; {
		if rest[0] != '(' {
			t.Fatalf("the access list section of %q carries %q between its entries", sddl, rest)
		}
		end := strings.IndexByte(rest, ')')
		if end < 0 {
			t.Fatalf("an entry of the access list %q is not closed", sddl)
		}
		fields := strings.Split(rest[1:end], ";")
		if len(fields) != 6 {
			t.Fatalf("the entry %q of the access list %q has %d fields, want the six of an SDDL entry", rest[:end+1], sddl, len(fields))
		}
		entries = append(entries, fields)
		rest = rest[end+1:]
	}
	return entries
}

// TestSCMImageGrantSDDLNamesItsAccessListSection is the cross-platform guard
// of the confirmed installation failure: the grant built its entry as a bare
// "(A;;0x…;;;S-1-5-19)", the platform's parser refused the string with
// ERROR_INVALID_PARAMETER, and every installation of a program the account of
// the instance cannot read refused before it wrote a registration.
//
// The guard pins the section, the exact string, the empty flags of the entry
// ("this folder only", no inheritance), the mask and the account of the
// instance as the only trustee. It runs on every platform, so a Linux run of
// `go test ./internal/service` reports this class without a Windows machine.
func TestSCMImageGrantSDDLNamesItsAccessListSection(t *testing.T) {
	cases := []struct {
		name string
		mask uint32
		want string
	}{
		{"the program of the registration", scmTestImageFileRights, "D:(A;;0x001200A9;;;S-1-5-19)"},
		{"a directory of its route", scmTestImageDirRights, "D:(A;;0x00000020;;;S-1-5-19)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sddl := scmImageGrantSDDL(tc.mask)
			if sddl != tc.want {
				t.Fatalf("scmImageGrantSDDL(0x%08X) = %q, want %q", tc.mask, sddl, tc.want)
			}

			entries := scmTestSDDLDACLEntries(t, sddl)
			if len(entries) != 1 {
				t.Fatalf("the access list of %q carries %d entries, want the one appended grant", sddl, len(entries))
			}
			fields := entries[0]
			if fields[0] != "A" {
				t.Errorf("the type of the entry of %q = %q, want the access-allowed A", sddl, fields[0])
			}
			if fields[1] != "" {
				t.Errorf("the ACE flags of %q = %q, want none: the entry applies to the object alone and inherits nowhere", sddl, fields[1])
			}
			if want := fmt.Sprintf("0x%08X", tc.mask); fields[2] != want {
				t.Errorf("the mask of the entry of %q = %q, want %q", sddl, fields[2], want)
			}
			if fields[3] != "" || fields[4] != "" {
				t.Errorf("the entry of %q names object or inherit GUIDs (%q, %q), want none", sddl, fields[3], fields[4])
			}
			if fields[5] != scmServiceIdentitySID {
				t.Errorf("the trustee of the entry of %q = %q, want the account of the instance %q alone", sddl, fields[5], scmServiceIdentitySID)
			}
		})
	}

	t.Run("a bare entry list is the string the platform refuses", func(t *testing.T) {
		// The form the grant shipped with, and the form the section check
		// has to reject: without a section the platform's parser answers
		// ERROR_INVALID_PARAMETER and the installation never registers.
		bare := fmt.Sprintf("(A;;0x%08X;;;%s)", scmTestImageFileRights, scmServiceIdentitySID)
		if scmTestSDDLHasSection(bare) {
			t.Fatalf("the section check accepts the bare entry list %q: it would not catch the defect", bare)
		}
	})
}

// TestSCMImageGrantRefusalCarriesTheClassMarker pins the seam the surfaces
// read: the refusal of a grant reports as ErrServiceImageAccessUnrepaired
// through wrapping, keeps its cause reachable, and keeps the message it
// showed verbatim — the marker adds no text of its own.
//
// The refusal happens before the registration is written, so the class is the
// fact a surface uses to keep a registration report away from an installation
// that never reached one.
func TestSCMImageGrantRefusalCarriesTheClassMarker(t *testing.T) {
	const identity = `NT AUTHORITY\LocalService`
	path := `C:\Users\Леонид Скоробогатый\Desktop\hfdesk_windows_amd64_1.3.0-beta.10.exe`
	cause := errors.New("build the access entry of the account of the instance: The parameter is incorrect.")
	residue := fmt.Sprintf("the access entry already written for %q remains: it gives read and execute to the account of the instance alone", path)

	cases := []struct {
		name    string
		residue string
		want    string
	}{
		{
			"the program is refused before anything is written",
			"",
			fmt.Sprintf("service: the account the instance runs as (%s) cannot be given the access it needs on %q: %s: the installation refuses instead of registering a program the service cannot start. Move the HFDesk pair into a folder the service account can read, then run %q from an elevated shell again",
				identity, path, cause, "hfdesk service install"),
		},
		{
			"a directory of the route is refused after the program",
			residue,
			fmt.Sprintf("service: the account the instance runs as (%s) cannot be given the access it needs on %q: %s: the installation refuses instead of registering a program the service cannot start (%s). Move the HFDesk pair into a folder the service account can read, then run %q from an elevated shell again",
				identity, path, cause, residue, "hfdesk service install"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := serviceImageAccessRefusal(identity, path, cause, tc.residue)
			if got := err.Error(); got != tc.want {
				t.Errorf("the refusal = %q, want the verbatim message %q", got, tc.want)
			}
			if !errors.Is(err, ErrServiceImageAccessUnrepaired) {
				t.Errorf("the refusal %v is not classified as ErrServiceImageAccessUnrepaired", err)
			}
			if !errors.Is(err, cause) {
				t.Errorf("the refusal %v does not carry its cause %v", err, cause)
			}
			wrapped := fmt.Errorf("hfdesk service install: %w", err)
			if !errors.Is(wrapped, ErrServiceImageAccessUnrepaired) {
				t.Errorf("the wrapped refusal %v is not classified as ErrServiceImageAccessUnrepaired", wrapped)
			}
			if text := ErrServiceImageAccessUnrepaired.Error(); text != "" {
				t.Errorf("the class marker carries the text %q, want none: %%w must add nothing to the verbatim message", text)
			}
		})
	}
}
