// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package service

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// scmTestGrantACE is one entry of an access list as the grant tests read it.
type scmTestGrantACE struct {
	aceType byte
	flags   byte
	mask    uint32
	trustee string
}

// scmTestGrantACEs reads the access list of a path entry by entry.
func scmTestGrantACEs(t *testing.T, path string) []scmTestGrantACE {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	assertNoError(t, err, "GetNamedSecurityInfo(%q) = %v, want nil", path, err)
	dacl, _, err := sd.DACL()
	assertNoError(t, err, "the access list of %q cannot be read: %v", path, err)
	raw := accessListBytes(dacl)
	if len(raw) < accessListHeaderSize {
		return nil
	}
	count := binary.LittleEndian.Uint16(raw[4:6])
	aces := make([]scmTestGrantACE, 0, count)
	offset := accessListHeaderSize
	for i := uint16(0); i < count; i++ {
		if offset+8 > len(raw) {
			t.Fatalf("the entry %d of %q runs past its access list", i, path)
		}
		size := int(binary.LittleEndian.Uint16(raw[offset+2 : offset+4]))
		if size < 8+4 || offset+size > len(raw) {
			t.Fatalf("the entry %d of %q has an unusable size %d", i, path, size)
		}
		trustee, ok := securityIdentifierString(raw[offset+8 : offset+size])
		if !ok {
			t.Fatalf("the entry %d of %q carries a trustee that cannot be read", i, path)
		}
		aces = append(aces, scmTestGrantACE{
			aceType: raw[offset],
			flags:   raw[offset+1],
			mask:    binary.LittleEndian.Uint32(raw[offset+4 : offset+8]),
			trustee: trustee,
		})
		offset += size
	}
	return aces
}

// scmTestAssertAppendedGrant checks that the list of the path holds every
// entry it held before, unchanged, plus exactly the one expected entry: the
// grant appends and never rewrites. The platform canonicalises a written
// list — an explicit entry is placed before an inherited one — so the
// position of an entry carries no meaning and only the entries themselves
// are compared.
func scmTestAssertAppendedGrant(t *testing.T, path string, before []scmTestGrantACE, want scmTestGrantACE) {
	t.Helper()
	got := scmTestGrantACEs(t, path)
	remaining := slices.Clone(got)
	for _, entry := range before {
		i := slices.Index(remaining, entry)
		if i < 0 {
			t.Fatalf("the entry %+v of %q is no longer in its list after the grant: the grant appends and never rewrites", entry, path)
		}
		remaining = slices.Delete(remaining, i, i+1)
	}
	if len(remaining) != 1 || remaining[0] != want {
		t.Errorf("the entries of %q beyond the list it held before = %+v, want exactly the appended %+v", path, remaining, want)
	}
}

// scmTestImageGrantFixture builds the machine of the confirmed smoke test: a
// program of a user profile whose descriptor carries the account of the
// instance nowhere, with the folders above it. The chain the grant walks is
// confined to the fixture, so a test never writes to the machine it runs on.
func scmTestImageGrantFixture(t *testing.T) (executable string, chain []string) {
	t.Helper()
	profile := filepath.Join(t.TempDir(), "Леонид Скоробогатый")
	if err := os.Mkdir(profile, 0o755); err != nil {
		t.Fatalf("Mkdir(%q) = %v, want nil", profile, err)
	}
	// The profile of a user: full control for the system, the
	// administrators and the user, and no entry for the account of the
	// instance — the descriptor the icacls of the machine reported.
	scmTestSetDACL(t, profile, fmt.Sprintf("D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;%s)", scmTestCurrentUserSID(t)))
	desktop := filepath.Join(profile, "Desktop")
	if err := os.Mkdir(desktop, 0o755); err != nil {
		t.Fatalf("Mkdir(%q) = %v, want nil", desktop, err)
	}
	executable = filepath.Join(desktop, "hfdesk_windows_amd64_1.3.0-beta.10.exe")
	if err := os.WriteFile(executable, []byte("program image"), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) = %v, want nil", executable, err)
	}
	chain = []string{desktop, profile}
	replaceSCMTestSeam(t, &scmServiceImageChain, func(string) ([]string, error) {
		return chain, nil
	})
	return executable, chain
}

// TestSCMImageGrantRepairsAProgramTheAccountOfTheInstanceCannotRead is the
// fix for the confirmed smoke failure: the program in the profile of a user
// carries no entry for the account of the instance, so the state before the
// grant is exactly the state a start fails in (error 5), and the grant turns
// it into read and execute on the program and traversal on the route, each
// entry appended for the account alone and nothing else.
func TestSCMImageGrantRepairsAProgramTheAccountOfTheInstanceCannotRead(t *testing.T) {
	executable, chain := scmTestImageGrantFixture(t)

	// The state of the machine of the smoke test: no entry of the account
	// and none of the trustees of its token covers the program, and no deny
	// entry is involved — the grant has work to do.
	state, err := scmDecideImageGrant(executable, windows.FILE_EXECUTE)
	assertNoError(t, err, "scmDecideImageGrant(%q) = %v, want a readable list", executable, err)
	if state != scmGrantNeeded {
		t.Fatalf("the state of the program of the fixture = %d, want the unusable state %d", state, scmGrantNeeded)
	}

	paths := append([]string{executable}, chain...)
	before := make(map[string][]scmTestGrantACE, len(paths))
	for _, path := range paths {
		before[path] = scmTestGrantACEs(t, path)
	}

	if err := grantServiceImageAccess(executable); err != nil {
		t.Fatalf("grantServiceImageAccess() = %v, want nil", err)
	}

	imageGrant := scmTestGrantACE{
		mask:    windows.FILE_GENERIC_READ | windows.FILE_GENERIC_EXECUTE,
		trustee: scmServiceIdentitySID,
	}
	dirGrant := scmTestGrantACE{mask: windows.FILE_TRAVERSE, trustee: scmServiceIdentitySID}
	for path, want := range map[string]scmTestGrantACE{
		executable: imageGrant,
		chain[0]:   dirGrant,
		chain[1]:   dirGrant,
	} {
		scmTestAssertAppendedGrant(t, path, before[path], want)
	}

	// The inheritance of the route stays what it was: the profile keeps its
	// protected descriptor, the folder inside it stays inheritable.
	if !scmTestDACLProtected(t, chain[1]) {
		t.Errorf("the profile %q lost the protection of its access list", chain[1])
	}
	if scmTestDACLProtected(t, chain[0]) {
		t.Errorf("the access list of %q was cut off from its parent by the grant", chain[0])
	}

	// The program is usable for the account now, and a second grant writes
	// nothing anywhere.
	state, err = scmDecideImageGrant(executable, windows.FILE_EXECUTE)
	assertNoError(t, err, "scmDecideImageGrant(%q) = %v, want a readable list", executable, err)
	if state != scmGrantSkip {
		t.Errorf("the state of the granted program = %d, want the covered state %d", state, scmGrantSkip)
	}
	lists := make(map[string]string, len(paths))
	for _, path := range paths {
		lists[path] = scmTestDACL(t, path)
	}
	if err := grantServiceImageAccess(executable); err != nil {
		t.Fatalf("the second grantServiceImageAccess() = %v, want nil", err)
	}
	for path, want := range lists {
		if got := scmTestDACL(t, path); got != want {
			t.Errorf("the second grant changed the access list of %q from %q to %q", path, want, got)
		}
	}

	// An update that moved the pair registers another program: the grant
	// follows the program it is asked about, and the already granted route
	// is left alone.
	moved := filepath.Join(chain[1], "hfdesk_windows_amd64_1.4.0-beta.1.exe")
	if err := os.WriteFile(moved, []byte("program image"), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) = %v, want nil", moved, err)
	}
	beforeMoved := scmTestGrantACEs(t, moved)
	if err := grantServiceImageAccess(moved); err != nil {
		t.Fatalf("the grant of the moved program = %v, want nil", err)
	}
	scmTestAssertAppendedGrant(t, moved, beforeMoved, imageGrant)
	for path, want := range lists {
		if got := scmTestDACL(t, path); got != want {
			t.Errorf("the grant of the moved program changed the access list of %q from %q to %q", path, want, got)
		}
	}
}

// TestSCMImageGrantDecidesEachDirectoryOnItsOwnList pins the decision the
// grant is made of: the entries of the account and the two groups certainly
// in its token decide the access, a group whose membership is not assumed
// decides nothing, a deny entry makes the grant futile, and a list that
// carries the bit is never written to.
func TestSCMImageGrantDecidesEachDirectoryOnItsOwnList(t *testing.T) {
	user := scmTestCurrentUserSID(t)
	cases := []struct {
		name string
		sddl string
		want int
	}{
		{"the account itself already traverses", "D:P(A;;0x00000020;;;S-1-5-19)", scmGrantSkip},
		{"an entry of a group certainly in its token covers the bit", "D:P(A;;0x001200A9;;;S-1-1-0)", scmGrantSkip},
		{"a generic execute entry covers it the way the platform maps it", "D:P(A;;GX;;;S-1-5-11)", scmGrantSkip},
		{"a read-only entry does not carry the bit", "D:P(A;;FR;;;S-1-1-0)", scmGrantNeeded},
		{"an entry of a group whose membership is assumed in no direction decides nothing", "D:P(A;;0x001200A9;;;S-1-5-32-545)", scmGrantNeeded},
		{"a deny entry of the account makes the grant futile", "D:P(D;;0x00000020;;;S-1-5-19)(A;;FA;;;S-1-5-18)", scmGrantDenied},
		{"a foreign list without any of the trustees needs the grant", "D:P(A;;FA;;;" + user + ")", scmGrantNeeded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			scmTestSetDACL(t, dir, tc.sddl)
			got, err := scmDecideImageGrant(dir, windows.FILE_TRAVERSE)
			assertNoError(t, err, "scmDecideImageGrant(%q) = %v, want a readable list", dir, err)
			if got != tc.want {
				t.Errorf("scmDecideImageGrant() = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestSCMImageGrantRefusalsCarryTheAction and its faces: a deny entry
// refuses before the first descriptor is changed, a refused write on the
// program refuses with no partial grant to hide, a refused write on the
// route names the entry that remains, and an object that cannot hold an
// access list at all is logged instead of refused. Every refusal carries
// the account, the path, the single action and the marker of a named
// command, so the surfaces append no second route.
func TestSCMImageGrantRefusalsCarryTheAction(t *testing.T) {
	assertRefusal := func(t *testing.T, err error, wantPath string) {
		t.Helper()
		if err == nil {
			t.Fatal("grantServiceImageAccess() = nil, want the refusal")
		}
		// The refusal quotes the path with %q, so the message names its
		// quoted form: scmTestNamesPath accepts that form of the path.
		if !scmTestNamesPath(err.Error(), wantPath) {
			t.Errorf("the refusal = %v, want it to name %q on its own", err, wantPath)
		}
		for _, want := range []string{scmServiceIdentityName, "Move the HFDesk pair", "hfdesk service install"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal = %v, want %q in it", err, want)
			}
		}
		if !HasNamedAction(err.Error()) {
			t.Errorf("the refusal = %v, want the command of its action to be a named route", err)
		}
	}

	t.Run("a deny entry refuses before anything is written", func(t *testing.T) {
		executable, chain := scmTestImageGrantFixture(t)
		user := scmTestCurrentUserSID(t)
		scmTestSetDACL(t, executable, fmt.Sprintf("D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;%s)(D;;0x00000020;;;%s)", user, scmServiceIdentitySID))
		paths := append([]string{executable}, chain...)
		lists := make(map[string]string, len(paths))
		for _, path := range paths {
			lists[path] = scmTestDACL(t, path)
		}
		writes := 0
		replaceSCMTestSeam(t, &scmApplyImageGrantACL, func(string, *windows.ACL) error {
			writes++
			return nil
		})

		err := grantServiceImageAccess(executable)
		assertRefusal(t, err, executable)
		if !strings.Contains(err.Error(), "deny entry") {
			t.Errorf("the refusal = %v, want the deny entry as its cause", err)
		}
		if writes != 0 {
			t.Errorf("the writes of the refused grant = %d, want none: the refusal happens before the first descriptor is changed", writes)
		}
		for path, want := range lists {
			if got := scmTestDACL(t, path); got != want {
				t.Errorf("the refused grant changed the access list of %q from %q to %q", path, want, got)
			}
		}
	})

	t.Run("a refused write on the program refuses with no partial grant", func(t *testing.T) {
		executable, _ := scmTestImageGrantFixture(t)
		writes := 0
		replaceSCMTestSeam(t, &scmApplyImageGrantACL, func(string, *windows.ACL) error {
			writes++
			return windows.ERROR_ACCESS_DENIED
		})

		err := grantServiceImageAccess(executable)
		assertRefusal(t, err, executable)
		if strings.Contains(err.Error(), "remains") {
			t.Errorf("the refusal = %v, want no partial grant named when nothing was written", err)
		}
		if writes != 1 {
			t.Errorf("the writes = %d, want the program only: the route is not touched after its refusal", writes)
		}
	})

	t.Run("a refused write on the route names what remains", func(t *testing.T) {
		executable, chain := scmTestImageGrantFixture(t)
		var writes []string
		replaceSCMTestSeam(t, &scmApplyImageGrantACL, func(path string, _ *windows.ACL) error {
			writes = append(writes, path)
			if path == chain[0] {
				return windows.ERROR_ACCESS_DENIED
			}
			return nil
		})

		err := grantServiceImageAccess(executable)
		assertRefusal(t, err, chain[0])
		if !strings.Contains(err.Error(), "remains") || !scmTestNamesPath(err.Error(), executable) {
			t.Errorf("the refusal = %v, want the entry already written for the program named as what remains", err)
		}
		if strings.Join(writes, "\n") != strings.Join([]string{executable, chain[0]}, "\n") {
			t.Errorf("the written paths = %v, want the program first and the route stopped at its refusal", writes)
		}
	})

	t.Run("an object without an access list is logged, not refused", func(t *testing.T) {
		executable, chain := scmTestImageGrantFixture(t)
		writes := 0
		replaceSCMTestSeam(t, &scmApplyImageGrantACL, func(string, *windows.ACL) error {
			writes++
			return windows.ERROR_NOT_SUPPORTED
		})

		// A filesystem that cannot hold access lists cannot restrict the
		// account of the instance either: a pair on such a drive keeps the
		// ordinary start the removable-drive warning already tolerates.
		if err := grantServiceImageAccess(executable); err != nil {
			t.Fatalf("grantServiceImageAccess() = %v, want no refusal over an object without access lists", err)
		}
		if writes != 1+len(chain) {
			t.Errorf("the attempted writes = %d, want the program and every directory of the route (%d)", writes, 1+len(chain))
		}
	})
}

// TestSCMImageGrantKeepsGoingWhenTheRouteCannotBeFullyNamed pins the
// inspection rule of the layer: a route that cannot be fully named is not a
// refusal. The directories that could be named are granted and the program
// is repaired anyway.
func TestSCMImageGrantKeepsGoingWhenTheRouteCannotBeFullyNamed(t *testing.T) {
	executable, chain := scmTestImageGrantFixture(t)
	replaceSCMTestSeam(t, &scmServiceImageChain, func(string) ([]string, error) {
		return chain[:1], errors.New("the test cannot name the rest of the route")
	})
	before := map[string][]scmTestGrantACE{
		executable: scmTestGrantACEs(t, executable),
		chain[0]:   scmTestGrantACEs(t, chain[0]),
		chain[1]:   scmTestGrantACEs(t, chain[1]),
	}

	if err := grantServiceImageAccess(executable); err != nil {
		t.Fatalf("grantServiceImageAccess() = %v, want the partial route granted, not a refusal", err)
	}
	scmTestAssertAppendedGrant(t, executable, before[executable], scmTestGrantACE{
		mask:    windows.FILE_GENERIC_READ | windows.FILE_GENERIC_EXECUTE,
		trustee: scmServiceIdentitySID,
	})
	scmTestAssertAppendedGrant(t, chain[0], before[chain[0]], scmTestGrantACE{
		mask:    windows.FILE_TRAVERSE,
		trustee: scmServiceIdentitySID,
	})
	if got := scmTestGrantACEs(t, chain[1]); len(got) != len(before[chain[1]]) {
		t.Errorf("the entries of the unnamed directory %q = %d, want them untouched at %d", chain[1], len(got), len(before[chain[1]]))
	}
}

// TestSCMServiceImageChainNamesTheRouteOfTheProgram pins the production
// route the grant walks: the directories above the program, the nearest
// first, up to the root of its volume. The route is read only — this test
// grants nothing, so the machine it runs on is not written to.
func TestSCMServiceImageChainNamesTheRouteOfTheProgram(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "Леонид Скоробогатый")
	if err := os.Mkdir(profile, 0o755); err != nil {
		t.Fatalf("Mkdir(%q) = %v, want nil", profile, err)
	}
	desktop := filepath.Join(profile, "Desktop")
	if err := os.Mkdir(desktop, 0o755); err != nil {
		t.Fatalf("Mkdir(%q) = %v, want nil", desktop, err)
	}
	executable := filepath.Join(desktop, "hfdesk.exe")
	if err := os.WriteFile(executable, []byte("program image"), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) = %v, want nil", executable, err)
	}

	chain, err := scmServiceImageChain(executable)
	assertNoError(t, err, "scmServiceImageChain(%q) = %v, want the route of an ordinary program", executable, err)
	if len(chain) < 2 || chain[0] != desktop || chain[1] != profile {
		t.Fatalf("the route = %v, want the directory of the program first and the profile above it second", chain)
	}
	volumeRoot := filepath.VolumeName(executable) + string(filepath.Separator)
	if !slices.Contains(chain, volumeRoot) {
		t.Errorf("the route = %v, want the root of the volume %q on it", chain, volumeRoot)
	}
}

// TestSCMInstallGrantsTheProgramAccessBeforeItChecksIt pins the place of the
// grant in the installation: every install repairs the program before the
// access check runs, with the program of the request, and a grant the
// installation refuses ends it before the check. The installation is driven
// to its refusal of the local directory, which happens after the grant and
// before any live control manager operation.
func TestSCMInstallGrantsTheProgramAccessBeforeItChecksIt(t *testing.T) {
	if !isElevatedProcess() {
		t.Skip("an installation needs an elevated shell, run the test as an administrator")
	}
	root := t.TempDir()
	opts := scmTestInstallOptions(t, root)
	var order []string
	replaceSCMTestSeam(t, &scmGrantExecutableAccess, func(executable string) error {
		if executable != opts.Executable {
			t.Errorf("the grant of the installation = %q, want the program of the request %q", executable, opts.Executable)
		}
		order = append(order, "grant")
		return nil
	})
	replaceSCMTestSeam(t, &scmCheckExecutableAccess, func(string, scmPathAccess) (scmExecutableAccess, string) {
		order = append(order, "check")
		return scmExecutableAccessAllowed, ""
	})
	scmTestRefuseLocalDir(t, opts)

	service := &scmManager{scope: ScopeSystem}
	if _, err := service.Install(context.Background(), opts); err == nil {
		t.Fatal("Install() = nil, want the refusal of the local directory")
	}
	if strings.Join(order, ",") != "grant,check" {
		t.Errorf("the order of the program preparation = %v, want the grant before the access check", order)
	}
}
