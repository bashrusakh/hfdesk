// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package service

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc/mgr"
)

// scmStorageOfTheInstallationWriteSequence models the registration the
// platform holds once the write sequence of a first installation is over: the
// CreateService call with the writes it performs itself, and nothing else
// before the snapshot the first-start guard reads.
//
// Every field is built from the value the platform stores for it — the
// registry mapping of the call that produced it — and never copied from the
// request, so the probe below asks the discriminating question: does the guard
// accept the storage of the installation's own write sequence? A field the
// platform does not store back as it was asked for fails the probe there.
func scmStorageOfTheInstallationWriteSequence(opts InstallOptions) mgr.Config {
	return mgr.Config{
		// dwServiceType, dwStartType and dwErrorControl are stored as
		// specified by CreateService.
		ServiceType:  windows.SERVICE_WIN32_OWN_PROCESS,
		StartType:    mgr.StartAutomatic,
		ErrorControl: mgr.ErrorNormal,
		// lpBinaryPathName is rebuilt by CreateService from the executable
		// and the argument vector — the vector scmCommandLine renders.
		BinaryPathName: scmCommandLine(opts),
		// lpLoadOrderGroup is NULL: no Group value is stored.
		LoadOrderGroup: "",
		// lpdwTagId is NULL and the service belongs to no load order group,
		// so no tag is assigned: dwTagId reads back zero.
		TagId: 0,
		// An empty MULTI_SZ decodes to an initialized zero-length slice.
		Dependencies: []string{},
		// lpServiceStartName is stored as specified.
		ServiceStartName: scmLocalServiceAccount,
		DisplayName:      scmDisplayName,
		// QueryServiceConfig returns no password at all.
		Password: "",
		// CreateService writes the description it is given.
		Description: scmDescription,
		// CreateService skips the SID write for SERVICE_SID_TYPE_NONE and
		// the platform default of a registration that never set it is the
		// same value.
		SidType: windows.SERVICE_SID_TYPE_NONE,
		// Nothing wrote the delayed auto start flag of a first install, so
		// it reads back false.
		DelayedAutoStart: false,
	}
}

// scmOwnedConfigDifferences names the fields the comparison refuses, so a
// failing probe says which value did not round-trip instead of only that one
// did not.
func scmOwnedConfigDifferences(current, expected mgr.Config) []string {
	var differences []string
	if !scmSameRegistration(current.BinaryPathName, expected.BinaryPathName) {
		differences = append(differences, fmt.Sprintf("BinaryPathName %q != %q", current.BinaryPathName, expected.BinaryPathName))
	}
	if len(current.Dependencies) != len(expected.Dependencies) {
		differences = append(differences, fmt.Sprintf("Dependencies %v != %v", current.Dependencies, expected.Dependencies))
	}
	if current.ServiceType != expected.ServiceType {
		differences = append(differences, fmt.Sprintf("ServiceType %d != %d", current.ServiceType, expected.ServiceType))
	}
	if current.StartType != expected.StartType {
		differences = append(differences, fmt.Sprintf("StartType %d != %d", current.StartType, expected.StartType))
	}
	if current.ErrorControl != expected.ErrorControl {
		differences = append(differences, fmt.Sprintf("ErrorControl %d != %d", current.ErrorControl, expected.ErrorControl))
	}
	if current.LoadOrderGroup != expected.LoadOrderGroup {
		differences = append(differences, fmt.Sprintf("LoadOrderGroup %q != %q", current.LoadOrderGroup, expected.LoadOrderGroup))
	}
	if current.TagId != expected.TagId {
		differences = append(differences, fmt.Sprintf("TagId %d != %d", current.TagId, expected.TagId))
	}
	if current.ServiceStartName != expected.ServiceStartName {
		differences = append(differences, fmt.Sprintf("ServiceStartName %q != %q", current.ServiceStartName, expected.ServiceStartName))
	}
	if current.DisplayName != expected.DisplayName {
		differences = append(differences, fmt.Sprintf("DisplayName %q != %q", current.DisplayName, expected.DisplayName))
	}
	if current.Password != expected.Password {
		differences = append(differences, "Password differs")
	}
	if current.Description != expected.Description {
		differences = append(differences, fmt.Sprintf("Description %q != %q", current.Description, expected.Description))
	}
	if current.SidType != expected.SidType {
		differences = append(differences, fmt.Sprintf("SidType %d != %d", current.SidType, expected.SidType))
	}
	if current.DelayedAutoStart != expected.DelayedAutoStart {
		differences = append(differences, fmt.Sprintf("DelayedAutoStart %t != %t", current.DelayedAutoStart, expected.DelayedAutoStart))
	}
	return differences
}

// TestSCMFirstInstallGuardAcceptsTheStorageOfItsOwnWriteSequence is the probe
// for the self-inflicted class of the first-start guard: the registration as
// the platform stores the installation's own write sequence is the
// configuration the installation asked for, so the guard does not report our
// own write as a change made before the first start.
//
// It fails when a field of the request does not round-trip through the
// platform — the shape that made the guard catch itself.
func TestSCMFirstInstallGuardAcceptsTheStorageOfItsOwnWriteSequence(t *testing.T) {
	opts := scmTestOptions()
	requested := buildSCMConfig(opts)
	stored := scmStorageOfTheInstallationWriteSequence(opts)

	if differences := scmOwnedConfigDifferences(stored, requested); len(differences) != 0 {
		t.Fatalf("the first-start guard would refuse the installation's own write sequence: %v", differences)
	}
	if !firstInstallRequestComparison(stored, requested) {
		t.Fatal("site №1 refused the stored registration of a first installation")
	}
}

// TestSCMFirstInstallGuardStillRefusesAChangedProgram proves the protected
// essence of the guard survives unchanged: a registration whose program the
// installation did not register is refused both against the configuration the
// installation asked for and against the snapshot of the first read, so the
// kept-registration outcome of an altered registration holds at both guard
// sites.
func TestSCMFirstInstallGuardStillRefusesAChangedProgram(t *testing.T) {
	opts := scmTestOptions()
	requested := buildSCMConfig(opts)
	stored := scmStorageOfTheInstallationWriteSequence(opts)

	// The same argument vector behind another program, the registered
	// program with an argument the installation never passed, and a command
	// line no parser accepts: every one of them is a substitution of the
	// program or of its vector, and none of them may pass.
	otherProgram := opts
	otherProgram.Executable = filepath.Join("C:\\Windows", "System32", "notepad.exe")
	extraArgument := opts
	extraArgument.Listen = "127.0.0.1:1"

	cases := []struct {
		name        string
		commandLine string
	}{
		{"another program", scmCommandLine(otherProgram)},
		{"the program with another argument vector", scmCommandLine(extraArgument)},
		{"a command line that cannot be parsed at all", `C:\Program`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			altered := stored
			altered.BinaryPathName = tc.commandLine

			// The same substitution seen from the request baseline of
			// site №1: narrowing that baseline to the fields the
			// installation wrote must not take the program out of them.
			if firstInstallRequestComparison(altered, requested) {
				t.Error("site №1 accepted the altered command line against the requested configuration")
			}
			if scmSameOwnedConfig(altered, stored) {
				t.Error("scmSameOwnedConfig() = true, want the altered command line refused against the snapshot of the first read")
			}
			replaceSCMTestSeam(t, &scmQueryServiceConfig, func(*mgr.Service) (mgr.Config, error) {
				return altered, nil
			})
			target := scmServiceRecoveryTarget{manager: &scmManager{scope: ScopeSystem}, handle: &mgr.Service{}}
			if scmFirstInstallRegistrationStillOwned(target, stored) {
				t.Error("scmFirstInstallRegistrationStillOwned() = true, want the altered registration no longer owned by this attempt")
			}
		})
	}
}

// firstInstallRequestComparison is the comparison site №1 of the first
// installation performs: the read-back of the registration it just created
// against the configuration it asked CreateService for
// (manager_install_windows.go:416). The probes below state what that
// comparison has to accept and what it must keep refusing.
func firstInstallRequestComparison(current, requested mgr.Config) bool {
	return scmSameRequestedConfig(current, requested)
}

// TestSCMFirstInstallRequestComparisonAcceptsTheRealRegisteredCommandLine is
// the round-trip of the program a real installation registers: a path with
// spaces and non-ASCII characters, quoted the way the request quotes it, read
// back and parsed by the reader the guard uses.
//
// It proves the request side of the comparison is symmetric — scmCommandLine
// and x/sys CreateService escape the same input with the same function and
// join it the same way — and that the quoting survives parseSCMCommandLine.
func TestSCMFirstInstallRequestComparisonAcceptsTheRealRegisteredCommandLine(t *testing.T) {
	opts := scmTestOptions()
	opts.Executable = `C:\Users\Леонид Скоробогатый\Desktop\hfdesk_windows_amd64_1.3.0-beta.10.exe`
	opts.CacheDir = `C:\Users\Леонид Скоробогатый\AppData\Local\HFDesk\cache`

	requested := buildSCMConfig(opts)
	stored := scmStorageOfTheInstallationWriteSequence(opts)

	parsed, err := parseSCMCommandLine(stored.BinaryPathName)
	assertNoError(t, err, "parseSCMCommandLine(%q) = %v, want the registered command line read back", stored.BinaryPathName, err)
	if !scmSameExecutablePath(parsed.executable, opts.Executable) {
		t.Errorf("the parsed executable = %q, want the registered program %q", parsed.executable, opts.Executable)
	}
	if !scmSameRegistration(stored.BinaryPathName, requested.BinaryPathName) {
		t.Errorf("scmSameRegistration() = false for the registered command line %q", stored.BinaryPathName)
	}
	if differences := scmOwnedConfigDifferences(stored, requested); len(differences) != 0 {
		t.Fatalf("the round-trip of the registered command line differed: %v", differences)
	}
	if !firstInstallRequestComparison(stored, requested) {
		t.Fatal("site №1 refused the round-trip of the program it registered")
	}
}

// TestSCMFirstInstallConfigCarriesTheDisplayDataItIsWrittenWith pins why the
// classic empty-name asymmetry cannot be the cause here: a first installation
// never asks for an empty display name or description, so the platform never
// falls back to the service name and never reports an unset description back.
func TestSCMFirstInstallConfigCarriesTheDisplayDataItIsWrittenWith(t *testing.T) {
	config := buildSCMConfig(scmTestOptions())
	if config.DisplayName == "" {
		t.Error("buildSCMConfig() carries an empty DisplayName: the platform would report the service name back instead")
	}
	if config.Description == "" {
		t.Error("buildSCMConfig() carries an empty Description: CreateService would skip the write entirely")
	}
	if config.Password != "" {
		t.Error("buildSCMConfig() carries a password: QueryServiceConfig never returns one, so the comparison could never round-trip it")
	}
}

// TestSCMFirstInstallRequestComparisonAcceptsWhatTheInstallationNeverWrote is
// the probe for the derived-field class: values the platform reports that no
// write of this installation established.
//
// Site №1 compares the request with the read-back. For a field the
// installation never wrote — a SID type CreateService skips when it is
// SERVICE_SID_TYPE_NONE, a delayed-auto-start flag it skips when it is false,
// a tag that needs a load order group, an account name the operating system
// reports in its own spelling — the request's zero value is an assumption
// about a platform default, not an expectation this attempt established. The
// comparison therefore reports our own registration as changed.
//
// Each case is a separate representative: they are refused today and must be
// accepted once site №1 compares only what the installation wrote.
func TestSCMFirstInstallRequestComparisonAcceptsWhatTheInstallationNeverWrote(t *testing.T) {
	// The spelling of an account belongs to the machine a test runs on, so
	// the identity comparison is driven through its seam instead of the
	// account database of the host.
	replaceSCMTestSeam(t, &scmAccountSecurityIdentifier, func(account string) (string, error) {
		if strings.EqualFold(account, "LocalService") || strings.EqualFold(account, "NT AUTHORITY\\LocalService") {
			return scmLocalServiceAccountSID, nil
		}
		return "", fmt.Errorf("the test knows no account %q", account)
	})
	opts := scmTestOptions()
	requested := buildSCMConfig(opts)
	platformForm := scmStorageOfTheInstallationWriteSequence(opts)

	cases := []struct {
		name  string
		apply func(*mgr.Config)
	}{
		{"the account in the well-known identifier the installation already accepts elsewhere",
			func(c *mgr.Config) { c.ServiceStartName = scmLocalServiceAccountSID }},
		{"the account in another spelling of the same identity",
			func(c *mgr.Config) { c.ServiceStartName = "LocalService" }},
		{"a SID type no write of this installation set",
			func(c *mgr.Config) { c.SidType = windows.SERVICE_SID_TYPE_UNRESTRICTED }},
		{"a delayed auto start no write of this installation set",
			func(c *mgr.Config) { c.DelayedAutoStart = true }},
		{"a load order group and tag no write of this installation set",
			func(c *mgr.Config) { c.LoadOrderGroup = "HFDeskGroup"; c.TagId = 7 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			readBack := platformForm
			tc.apply(&readBack)
			if !firstInstallRequestComparison(readBack, requested) {
				differences := scmOwnedConfigDifferences(readBack, requested)
				t.Errorf("site №1 reported its own registration as changed over a value it never wrote: %v", differences)
			}
			// The same difference seen between two reads is an external
			// change and stays refused: only the request baseline of site
			// №1 stops treating an unestablished expectation as a fact.
			if scmSameOwnedConfig(readBack, platformForm) {
				t.Error("scmSameOwnedConfig() = true between two reads: a changed registration must stay refused")
			}
		})
	}
}

// TestSCMFirstInstallRequestComparisonRefusesAnotherAccount pins that the
// account stays guarded after its comparison moved from a spelling to an
// identity: the write establishes the supported identity, and a registration
// that runs as another account is refused at site №1 exactly as before.
func TestSCMFirstInstallRequestComparisonRefusesAnotherAccount(t *testing.T) {
	replaceSCMTestSeam(t, &scmAccountSecurityIdentifier, func(account string) (string, error) {
		return "", fmt.Errorf("the test knows no account %q", account)
	})
	opts := scmTestOptions()
	requested := buildSCMConfig(opts)
	platformForm := scmStorageOfTheInstallationWriteSequence(opts)

	readBack := platformForm
	readBack.ServiceStartName = `CONTOSO\svc-other`
	if firstInstallRequestComparison(readBack, requested) {
		t.Error("site №1 accepted a registration that runs as another account")
	}
	if scmSameOwnedConfig(readBack, platformForm) {
		t.Error("scmSameOwnedConfig() = true between two reads over a changed account")
	}
}
