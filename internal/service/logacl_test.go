// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package service

import "testing"

// TestSCMLogReadGrantSDDLNamesItsAccessListSection is the cross-platform guard
// of the confirmed defect of the log read grant: the grant built its entry as
// a bare "(A;;FR;;;IU)" ("(A;OICI;FR;;;IU)" for the log directory), the
// platform's parser refused the string with ERROR_INVALID_PARAMETER, and the
// grant therefore never ran — an interactive user stayed without read access
// to the very log the Logs window names, on every instance.
//
// The guard pins the section, the exact string, the flags of the entry
// (inheritance on a directory, "this folder only" on a file), the mask and the
// Interactive group as the only trustee. It runs on every platform, so a Linux
// run of `go test ./internal/service` reports this class without a Windows
// machine; the grant itself is exercised on Windows by
// TestTheInstanceLogTheWindowNamesIsReadable.
func TestSCMLogReadGrantSDDLNamesItsAccessListSection(t *testing.T) {
	cases := []struct {
		name      string
		container bool
		want      string
		flags     string
	}{
		{"the active log file", false, "D:(A;;FR;;;S-1-5-4)", ""},
		{"the log directory", true, "D:(A;OICI;FR;;;S-1-5-4)", "OICI"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sddl := scmLogReadGrantSDDL(tc.container)
			if sddl != tc.want {
				t.Fatalf("scmLogReadGrantSDDL(%t) = %q, want %q", tc.container, sddl, tc.want)
			}
			if !scmTestSDDLHasSection(sddl) {
				t.Fatalf("the descriptor string %q of the log grant carries no section: the platform's parser refuses it with ERROR_INVALID_PARAMETER (\"The parameter is incorrect\")", sddl)
			}

			entries := scmTestSDDLDACLEntries(t, sddl)
			if len(entries) != 1 {
				t.Fatalf("the access list of %q carries %d entries, want the one appended grant", sddl, len(entries))
			}
			fields := entries[0]
			if fields[0] != "A" {
				t.Errorf("the type of the entry of %q = %q, want the access-allowed A", sddl, fields[0])
			}
			if fields[1] != tc.flags {
				t.Errorf("the ACE flags of %q = %q, want %q: the entry inherits on the log directory and applies to the active log file alone", sddl, fields[1], tc.flags)
			}
			// FR is the descriptor spelling of FILE_GENERIC_READ
			// (0x00120089): the read access the window needs and nothing
			// more.
			if fields[2] != "FR" {
				t.Errorf("the mask of the entry of %q = %q, want FR (FILE_GENERIC_READ)", sddl, fields[2])
			}
			if fields[3] != "" || fields[4] != "" {
				t.Errorf("the entry of %q names object or inherit GUIDs (%q, %q), want none", sddl, fields[3], fields[4])
			}
			if fields[5] != scmInteractiveUsersSID {
				t.Errorf("the trustee of the entry of %q = %q, want the Interactive group %q (IU) alone", sddl, fields[5], scmInteractiveUsersSID)
			}
		})
	}

	t.Run("a bare entry list is the string the platform refuses", func(t *testing.T) {
		// The exact strings the grant shipped with, and the form the
		// section check has to reject: without a section the platform's
		// parser answers ERROR_INVALID_PARAMETER, the grant never runs and
		// the log stays unreadable.
		for _, bare := range []string{"(A;;FR;;;IU)", "(A;OICI;FR;;;IU)"} {
			if scmTestSDDLHasSection(bare) {
				t.Fatalf("the section check accepts the bare entry list %q: it would not catch the defect", bare)
			}
		}
	})
}
