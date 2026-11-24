// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package service

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bashrusakh/hfdesk/internal/runtimepaths"
)

// TestTheInstanceLogTheWindowNamesIsReadable starts an instance whose log
// directory and active log file carry the restrictive descriptor the
// installation applies to the paths it creates — the state a real
// machine-wide installation leaves behind — and then reads the file the way
// the Logs window reads it.
//
// The point is the refusal the window used to show: that descriptor grants
// full control to SYSTEM, the administrators and LocalService only, so an
// interactive user cannot read the very file the window names. After the
// start, the directory and the file must carry exactly one more entry —
// read for interactive users — and the file must read. No other entry may
// appear and the descriptor must stay protected: the grant lives on the log
// alone and changes nothing else.
func TestTheInstanceLogTheWindowNamesIsReadable(t *testing.T) {
	root := t.TempDir()
	model := runtimepaths.Paths{
		ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
		StateDir:   filepath.Join(root, "var"),
		LogDir:     filepath.Join(root, "var", "logs"),
	}
	t.Cleanup(runtimepaths.Reset)

	err := os.MkdirAll(model.LogDir, 0o755)
	assertNoError(t, err, "MkdirAll(%q) = %v, want nil", model.LogDir, err)
	logPath := filepath.Join(model.LogDir, logFileName)
	err = os.WriteFile(logPath, []byte("the record the window has to show\n"), 0o644)
	assertNoError(t, err, "WriteFile(%q) = %v, want nil", logPath, err)

	// Exactly the descriptor the installation applies to the directories
	// it creates — on the log directory and on the active file, the state
	// that made the window report "Access is denied".
	err = applyRestrictiveACL(model.LogDir)
	assertNoError(t, err, "applyRestrictiveACL(%q) = %v, want nil", model.LogDir, err)
	err = applyRestrictiveACL(logPath)
	assertNoError(t, err, "applyRestrictiveACL(%q) = %v, want nil", logPath, err)

	withoutInteractiveRead := []string{"S-1-5-18", "S-1-5-19", "S-1-5-32-544"}
	for _, path := range []string{model.LogDir, logPath} {
		got := scmTestTrustees(t, path)
		slices.Sort(got)
		if !slices.Equal(got, withoutInteractiveRead) {
			t.Fatalf("the trustees of %q = %v, want %v: the fixture is not the restricted state of an installation", path, got, withoutInteractiveRead)
		}
	}

	// A start of the instance, the way the supervised instance starts.
	err = runService(t, RunOptions{Paths: model})
	assertNoError(t, err, "Run() = %v, want nil", err)

	// The way the window reads: the named file opens and holds its
	// content.
	data, err := os.ReadFile(logPath)
	assertNoError(t, err, "the log the window names cannot be read: %v", err)
	if !strings.Contains(string(data), "the record the window has to show") {
		t.Errorf("the readable log = %q, want it to still hold the record it was written with", string(data))
	}

	// The access lists gained exactly the interactive read and nothing
	// else, and the descriptor of the instance stays protected.
	withInteractiveRead := []string{"S-1-5-18", "S-1-5-19", "S-1-5-32-544", "S-1-5-4"}
	for _, path := range []string{model.LogDir, logPath} {
		got := scmTestTrustees(t, path)
		slices.Sort(got)
		if !slices.Equal(got, withInteractiveRead) {
			t.Errorf("the trustees of %q = %v, want %v: the instance accounts plus read for interactive users and nothing else", path, got, withInteractiveRead)
		}
		if !scmTestDACLProtected(t, path) {
			t.Errorf("the access list of %q is not protected, want the installation's descriptor to stay protected", path)
		}
	}
}

// TestTheInteractiveLogGrantLeavesForeignDescriptorsAlone proves the other
// side of the narrow grant: a descriptor that is not the untouched
// restrictive list of the instance is never changed, not even by one entry —
// a list somebody widened, a list the user built for a directory they named,
// a list with a deny entry, and the ordinary descriptor a log file inherits
// in a profile directory.
func TestTheInteractiveLogGrantLeavesForeignDescriptorsAlone(t *testing.T) {
	user := scmTestCurrentUserSID(t)
	cases := []struct {
		name string
		sddl string
	}{
		{
			name: "a list that already has a reader beside the instance",
			sddl: "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;LS)(A;OICI;FR;;;WD)",
		},
		{
			name: "a list of the right shape but another trustee in it",
			sddl: "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;LS)(A;OICI;FA;;;" + user + ")",
		},
		{
			name: "a list with a deny entry in the place of an allow",
			sddl: "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(D;;FR;;;LS)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			scmTestSetDACL(t, dir, tc.sddl)
			file := filepath.Join(dir, "hfdesk.log")
			if err := os.WriteFile(file, []byte("record\n"), 0o644); err != nil {
				t.Fatalf("WriteFile(%q) = %v, want nil", file, err)
			}
			// The file keeps the descriptor it inherited in an ordinary
			// directory — the state of a log in the profile of the
			// desktop host.
			dirBefore := scmTestDACL(t, dir)
			fileBefore := scmTestDACL(t, file)

			if err := grantLogReadToInteractiveUsers(dir, true); err != nil {
				t.Errorf("grantLogReadToInteractiveUsers(%q) = %v, want nil", dir, err)
			}
			if err := grantLogReadToInteractiveUsers(file, false); err != nil {
				t.Errorf("grantLogReadToInteractiveUsers(%q) = %v, want nil", file, err)
			}

			if after := scmTestDACL(t, dir); after != dirBefore {
				t.Errorf("the access list of %q changed from %q to %q, want it untouched", dir, dirBefore, after)
			}
			if after := scmTestDACL(t, file); after != fileBefore {
				t.Errorf("the access list of %q changed from %q to %q, want it untouched", file, fileBefore, after)
			}
		})
	}
}

// TestTheInteractiveLogGrantKeepsItsOwnWork proves the grant is idempotent:
// after the first grant the list is no longer the untouched restrictive list
// of the instance, so the second grant leaves it alone — one interactive
// entry, never two.
func TestTheInteractiveLogGrantKeepsItsOwnWork(t *testing.T) {
	dir := t.TempDir()
	if err := applyRestrictiveACL(dir); err != nil {
		t.Fatalf("applyRestrictiveACL(%q) = %v, want nil", dir, err)
	}
	if err := grantLogReadToInteractiveUsers(dir, true); err != nil {
		t.Fatalf("grantLogReadToInteractiveUsers() = %v, want nil", err)
	}
	first := scmTestDACL(t, dir)
	if err := grantLogReadToInteractiveUsers(dir, true); err != nil {
		t.Fatalf("the second grantLogReadToInteractiveUsers() = %v, want nil", err)
	}
	if second := scmTestDACL(t, dir); second != first {
		t.Errorf("the second grant changed the list from %q to %q, want it to keep its own work", first, second)
	}
	want := []string{"S-1-5-18", "S-1-5-19", "S-1-5-32-544", "S-1-5-4"}
	got := scmTestTrustees(t, dir)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("the trustees of %q = %v, want %v exactly once", dir, got, want)
	}
}
