// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package service

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/bashrusakh/hfdesk/internal/runtimepaths"
)

// secretSentinel is an obviously secret value. It is put into every field
// of the installation options that could carry a credential, so the tests
// can prove that no credential reaches the registration of the service.
const secretSentinel = "hfdesk-secret-sentinel"

func TestBuildSCMConfigUsesTheLeastPrivilegedAccount(t *testing.T) {
	opts := scmTestOptions()
	config := buildSCMConfig(opts)

	if config.ServiceStartName != scmLocalServiceAccount {
		t.Errorf("ServiceStartName = %q, want %q", config.ServiceStartName, scmLocalServiceAccount)
	}
	if config.ServiceStartName != `NT AUTHORITY\LocalService` {
		t.Errorf("ServiceStartName = %q, want the LocalService account", config.ServiceStartName)
	}
	if strings.Contains(strings.ToLower(config.ServiceStartName), "localsystem") {
		t.Errorf("ServiceStartName = %q, want the LocalService account and not LocalSystem", config.ServiceStartName)
	}
	if config.Password != "" {
		t.Errorf("Password = %q, want no password at all", config.Password)
	}
	if config.StartType != mgr.StartAutomatic {
		t.Errorf("StartType = %d, want %d", config.StartType, mgr.StartAutomatic)
	}
	if config.ErrorControl != mgr.ErrorNormal {
		t.Errorf("ErrorControl = %d, want %d", config.ErrorControl, mgr.ErrorNormal)
	}
	if config.ServiceType != windows.SERVICE_WIN32_OWN_PROCESS {
		t.Errorf("ServiceType = %d, want %d", config.ServiceType, windows.SERVICE_WIN32_OWN_PROCESS)
	}
	if config.DisplayName == "" || config.Description == "" {
		t.Errorf("DisplayName = %q and Description = %q, want both of them to be set", config.DisplayName, config.Description)
	}
}

// TestSCMDisabledRunningRefusalRefusesBeforeItStopsTheInstance checks the
// decision an installation makes about a running instance whose start type is
// disabled.
//
// A service can run with a disabled start type; the installation stops it to
// put the new program into service, and Windows refuses to start a disabled
// service again, so the installation would leave the machine without a service
// that ran before. The decision refuses a running or starting instance with a
// disabled start type and allows every other state, including a stopped
// instance, because the installation does not start a service that was stopped.
func TestSCMDisabledRunningRefusalRefusesBeforeItStopsTheInstance(t *testing.T) {
	cases := []struct {
		name      string
		present   bool
		state     svc.State
		startType uint32
		refused   bool
	}{
		{"a running instance with a disabled start", true, svc.Running, mgr.StartDisabled, true},
		{"a starting instance with a disabled start", true, svc.StartPending, mgr.StartDisabled, true},
		{"a stopped instance with a disabled start", true, svc.Stopped, mgr.StartDisabled, false},
		{"a stopping instance with a disabled start", true, svc.StopPending, mgr.StartDisabled, false},
		{"a running instance with a manual start", true, svc.Running, mgr.StartManual, false},
		{"a running instance with an automatic start", true, svc.Running, mgr.StartAutomatic, false},
		{"a stopped instance with a manual start", true, svc.Stopped, mgr.StartManual, false},
		{"an absent registration", false, svc.Stopped, mgr.StartDisabled, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			running := scmInstanceRan(tc.present, tc.state)
			err := scmDisabledRunningRefusal(tc.present, running, tc.startType)
			if !tc.refused {
				assertNoError(t, err, "scmDisabledRunningRefusal(%v, %v, %d) = %v, want nil", tc.present, running, tc.startType, err)
				return
			}
			if err == nil {
				t.Fatalf("scmDisabledRunningRefusal(%v, %v, %d) = nil, want the refusal of the installation", tc.present, running, tc.startType)
			}
			assertNames(t, err.Error(), "the refusal", []string{
				"is running, but its start type is disabled",
				"Windows will not start it again after the installation stops it",
				"Set the start type to manual or automatic",
				`"sc config hfdesk start= demand"`,
				"run the installation again",
			})
		})
	}
}

// TestSCMServiceAccountRefusalRefusesOnlyAChangedIdentity checks both sides
// of the update refusal over the account of an existing registration.
//
// A registration that runs as the supported identity keeps updating exactly
// as today — the update never refuses it because of its account, whether the
// registration spells the account by name or by its well-known identifier.
// A registration an administrator moved to any other identity is refused
// with the result (nothing changed, nothing stopped), the cause (the account
// the registration runs as, and the identity an update keeps) and one real
// next action: the command that restores the supported identity. The
// function only reads the account string, so a refusal cannot have touched
// the service; the call site places it before the first write.
func TestSCMServiceAccountRefusalRefusesOnlyAChangedIdentity(t *testing.T) {
	for _, tc := range []struct {
		name    string
		account string
	}{
		{"the registered account name", scmLocalServiceAccount},
		{"another case of the same name", `nt authority\localservice`},
		{"the well-known security identifier", scmLocalServiceAccountSID},
		{"the identifier in lower case", "s-1-5-19"},
	} {
		t.Run("kept "+tc.name, func(t *testing.T) {
			if err := scmServiceAccountRefusal(tc.account); err != nil {
				t.Errorf("scmServiceAccountRefusal(%q) = %v, want nil: an update keeps this identity", tc.account, err)
			}
		})
	}

	for _, tc := range []struct {
		name    string
		account string
		shown   string
	}{
		{"an administrator-changed account", `CONTOSO\svc-other`, `CONTOSO\svc-other`},
		{"the built-in LocalSystem account", "LocalSystem", "LocalSystem"},
		{"an empty start name", "", "LocalSystem"},
		{"the network service account", `NT AUTHORITY\NetworkService`, `NT AUTHORITY\NetworkService`},
	} {
		t.Run("refused "+tc.name, func(t *testing.T) {
			err := scmServiceAccountRefusal(tc.account)
			if err == nil {
				t.Fatalf("scmServiceAccountRefusal(%q) = nil, want the refusal of the update", tc.account)
			}
			if !errors.Is(err, ErrServiceAccountMismatch) {
				t.Errorf("the refusal %v is not classified as ErrServiceAccountMismatch", err)
			}
			assertNames(t, err.Error(), "the refusal", []string{
				fmt.Sprintf("runs as \"%s\"", tc.shown),
				"an HFDesk update keeps the supported identity " + scmLocalServiceAccount,
				"nothing was changed and the service was not stopped",
				`sc config hfdesk obj= "NT AUTHORITY\LocalService" password= ""`,
				"run the installation again",
			})
		})
	}
}

// TestSCMSupportedServiceAccountComparesIdentitiesNotNameLiterals checks
// both sides of the supported-identity comparison behind the refusal.
//
// The service control manager reports the account in whatever form the
// operating system produced: on a localized system the built-in account
// carries a name no English literal matches. A registration that runs as the
// supported identity is therefore never refused, whatever spelling the
// machine resolves it under, while a name the machine cannot confirm as
// LocalService keeps the refusal exactly as before. The known literal
// spellings stay fast paths: they are accepted without any lookup.
func TestSCMSupportedServiceAccountComparesIdentitiesNotNameLiterals(t *testing.T) {
	var resolved []string
	replaceSCMTestSeam(t, &scmAccountSecurityIdentifier, func(account string) (string, error) {
		resolved = append(resolved, account)
		switch account {
		case `NT AUTORITÄT\LOKALER DIENST`:
			// The localized name the account database spells the
			// supported identity with on a German system.
			return scmLocalServiceAccountSID, nil
		case `CONTOSO\svc-other`:
			return "S-1-5-21-1111111111-2222222222-3333333333-1001", nil
		default:
			return "", errors.New("the account name does not exist on this machine")
		}
	})

	t.Run("the localized spelling of the supported identity is never refused", func(t *testing.T) {
		if err := scmServiceAccountRefusal(`NT AUTORITÄT\LOKALER DIENST`); err != nil {
			t.Errorf("scmServiceAccountRefusal of the supported identity = %v, want nil: a localized name is the same identity", err)
		}
	})

	for _, tc := range []struct {
		name    string
		account string
		shown   string
	}{
		{"a resolvable foreign account", `CONTOSO\svc-other`, `CONTOSO\svc-other`},
		{"an unresolvable name", `HFDESK-TEST\unknown`, `HFDESK-TEST\unknown`},
		{"the built-in LocalSystem account", "LocalSystem", "LocalSystem"},
		{"an empty start name", "", "LocalSystem"},
	} {
		t.Run("refused "+tc.name, func(t *testing.T) {
			err := scmServiceAccountRefusal(tc.account)
			if err == nil {
				t.Fatalf("scmServiceAccountRefusal(%q) = nil, want the refusal of the update", tc.account)
			}
			if !errors.Is(err, ErrServiceAccountMismatch) {
				t.Errorf("the refusal %v is not classified as ErrServiceAccountMismatch", err)
			}
			if want := fmt.Sprintf("runs as \"%s\"", tc.shown); !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal %q does not mention %q", err, want)
			}
		})
	}

	t.Run("the literal spellings are accepted without a lookup", func(t *testing.T) {
		resolved = nil
		for _, account := range []string{
			scmLocalServiceAccount,
			`nt authority\localservice`,
			scmLocalServiceAccountSID,
			"s-1-5-19",
			" " + scmLocalServiceAccountSID + " ",
		} {
			if err := scmServiceAccountRefusal(account); err != nil {
				t.Errorf("scmServiceAccountRefusal(%q) = %v, want nil: an update keeps this identity", account, err)
			}
		}
		if len(resolved) != 0 {
			t.Errorf("the known literal spellings were resolved %d times, want the fast path: %v", len(resolved), resolved)
		}
	})

	t.Run("an empty start name is refused without a lookup", func(t *testing.T) {
		resolved = nil
		if err := scmServiceAccountRefusal(""); err == nil {
			t.Fatal("scmServiceAccountRefusal(\"\") = nil, want the refusal: an empty start name is the LocalSystem account")
		}
		if len(resolved) != 0 {
			t.Errorf("the empty start name was resolved %d times, want no lookup: %v", len(resolved), resolved)
		}
	})
}

// TestSCMServiceAccountRefusalKeepsTheCleanupDecisionOfItsInterleave checks
// both sides of the cleanup decision at the refusal call sites of register.
//
// The refusal happens before any write of this attempt. For a registration
// that already existed when the attempt looked it up, the directories the
// attempt created are its own and stay safe to remove; a registration that
// appeared between the read-only lookup and the open may reference those
// very directories, and the appearance refusal of the update path — which
// now ends the attempt before the registration is even read — keeps the
// same keep-paths decision for the same interleave.
func TestSCMServiceAccountRefusalKeepsTheCleanupDecisionOfItsInterleave(t *testing.T) {
	replaceSCMTestSeam(t, &scmOpenService, func(*mgr.Mgr, string) (*mgr.Service, error) {
		return new(mgr.Service), nil
	})
	replaceSCMTestSeam(t, &scmQueryServiceConfig, func(*mgr.Service) (mgr.Config, error) {
		return mgr.Config{
			ServiceType:      windows.SERVICE_WIN32_OWN_PROCESS,
			StartType:        mgr.StartAutomatic,
			ErrorControl:     mgr.ErrorNormal,
			BinaryPathName:   `C:\other\hfdesk.exe service run`,
			ServiceStartName: `CONTOSO\svc-other`,
		}, nil
	})
	replaceSCMTestSeam(t, &scmQueryStatus, func(*mgr.Service) (svc.Status, error) {
		return svc.Status{State: svc.Stopped}, nil
	})

	for _, tc := range []struct {
		name     string
		previous previousSCMRegistration
		appeared bool
		wantSafe string
	}{
		{
			name:     "the registration was known before the attempt",
			previous: previousSCMRegistration{present: true},
			appeared: false,
			wantSafe: "the directories this attempt created are its own: no registration write happened",
		},
		{
			name:     "a registration appeared during the attempt",
			previous: previousSCMRegistration{},
			appeared: true,
			wantSafe: "the appeared registration may reference the directories this attempt created",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &scmManager{}
			result, err := m.register(context.Background(), nil, scmTestOptions(), tc.previous, false)
			if err == nil {
				t.Fatal("register() = nil, want a refusal before any write")
			}
			if tc.appeared {
				// The appearance refusal ends the attempt before the account
				// of the appeared registration is read at all.
				if !strings.Contains(err.Error(), "appeared during the installation") {
					t.Errorf("register() = %v, want the refusal of the registration that appeared", err)
				}
			} else if !errors.Is(err, ErrServiceAccountMismatch) {
				t.Errorf("register() = %v, want the refusal over the account of the registration", err)
			}
			if result.created {
				t.Error("the refusal reported a created registration: it returns before any write")
			}
			if got, want := result.createdDirsSafeToRemove, !tc.appeared; got != want {
				t.Errorf("createdDirsSafeToRemove = %v, want %v: %s", got, want, tc.wantSafe)
			}
		})
	}
}

// TestSCMRefusesARegistrationThatAppearedDuringTheInstallation checks that an
// update whose first read found no registration refuses when the registration
// exists at the write handle — before it reads or changes a single value of it.
//
// The merge ran against the absent state, so the attempt would overwrite the
// settings of the appeared registration with the defaults of its request. The
// refusal reads nothing of the registration (the query seams prove it),
// reports the result, the cause and the one action that resolves both — a
// repeat run takes a fresh snapshot and merges — and keeps the directories
// this attempt created, which the appeared registration may reference.
func TestSCMRefusesARegistrationThatAppearedDuringTheInstallation(t *testing.T) {
	var opened, configReads, statusReads int
	replaceSCMTestSeam(t, &scmOpenService, func(*mgr.Mgr, string) (*mgr.Service, error) {
		opened++
		return new(mgr.Service), nil
	})
	replaceSCMTestSeam(t, &scmQueryServiceConfig, func(*mgr.Service) (mgr.Config, error) {
		configReads++
		return mgr.Config{}, errors.New("the appeared registration must not be read")
	})
	replaceSCMTestSeam(t, &scmQueryStatus, func(*mgr.Service) (svc.Status, error) {
		statusReads++
		return svc.Status{}, errors.New("the appeared registration must not be read")
	})

	m := &scmManager{}
	result, err := m.register(context.Background(), nil, scmTestOptions(), previousSCMRegistration{}, false)
	if err == nil {
		t.Fatal("register() = nil, want the refusal of the registration that appeared")
	}
	assertNames(t, err.Error(), "the appearance refusal", []string{
		"appeared during the installation",
		"nothing was changed",
		`run "hfdesk service install" again to take a fresh snapshot`,
		"installation-created directories were retained because the appeared registration may still reference them",
	})
	if opened != 1 {
		t.Errorf("the registration was opened %d times, want exactly the lookup of the attempt", opened)
	}
	if configReads != 0 || statusReads != 0 {
		t.Errorf("the appeared registration was read before the refusal (config %d, status %d), want no read at all", configReads, statusReads)
	}
	if result.created {
		t.Error("the refusal reported a created registration: it returns before any write")
	}
	if result.createdDirsSafeToRemove {
		t.Error("createdDirsSafeToRemove = true, want false: the appeared registration may reference the directories this attempt created")
	}
}

// TestSCMConfigForUpdatePreservesTheReadableRegistrationSettings checks that
// an update changes the command line but keeps the other readable SCM settings,
// including start type and delayed start. The fixed service identity and blank
// password remain the project policy.
func TestSCMConfigForUpdatePreservesTheReadableRegistrationSettings(t *testing.T) {
	config := buildSCMConfig(scmTestOptions())
	cases := []struct {
		name     string
		previous mgr.Config
	}{
		{"an automatic start", mgr.Config{StartType: mgr.StartAutomatic}},
		{"a manual start", mgr.Config{StartType: mgr.StartManual}},
		{"a disabled start", mgr.Config{StartType: mgr.StartDisabled}},
		{"a delayed automatic start", mgr.Config{StartType: mgr.StartAutomatic, DelayedAutoStart: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			previous := tc.previous
			previous.ServiceType = windows.SERVICE_WIN32_OWN_PROCESS
			previous.ErrorControl = mgr.ErrorSevere
			previous.BinaryPathName = `C:\old\hfdesk.exe service run`
			previous.LoadOrderGroup = "HFDeskGroup"
			previous.TagId = 17
			previous.Dependencies = []string{"LanmanWorkstation", "Tcpip"}
			previous.ServiceStartName = scmLocalServiceAccount
			previous.Password = secretSentinel
			previous.DisplayName = "Operator label"
			previous.Description = "Operator description"
			previous.SidType = 2

			got := scmConfigForUpdate(config, previous)
			want := previous
			want.BinaryPathName = config.BinaryPathName
			want.ServiceStartName = scmLocalServiceAccount
			want.Password = ""
			if !reflect.DeepEqual(got, want) {
				t.Errorf("scmConfigForUpdate() =\n%+v\nwant preserved configuration\n%+v", got, want)
			}
		})
	}
}

func TestSCMCommandLineCarriesNoCredential(t *testing.T) {
	opts := scmTestOptions()
	// Every field of the options that could carry a credential carries
	// the sentinel.
	setCredentialFields(&opts, secretSentinel)

	commandLine := scmCommandLine(opts)
	if strings.Contains(commandLine, secretSentinel) {
		t.Errorf("the command line %q carries a credential", commandLine)
	}

	// The command line holds the executable and the canonical argument
	// vector of the shared contract, nothing else.
	parts, err := windows.DecomposeCommandLine(commandLine)
	assertNoError(t, err, "DecomposeCommandLine(%q) = %v, want nil", commandLine, err)
	want := append([]string{opts.Executable}, RunArgs(opts)...)
	if !reflect.DeepEqual(parts, want) {
		t.Errorf("the command line = %q, want %q", parts, want)
	}
	if !filepath.IsAbs(parts[0]) {
		t.Errorf("the registered executable %q is not an absolute path", parts[0])
	}
	if buildSCMConfig(opts).BinaryPathName != commandLine {
		t.Errorf("BinaryPathName = %q, want the command line %q", buildSCMConfig(opts).BinaryPathName, commandLine)
	}
}

func TestSCMDirectoryCleanupUsesProvenRegistrationOutcome(t *testing.T) {
	failure := errors.New("the install step failed")
	deleteErr := errors.New("the SCM refused deletion")
	writeErr := errors.New("the SCM refused restoration")
	cases := []struct {
		name     string
		prev     previousSCMRegistration
		target   *scmTestRecoveryTarget
		wantSafe bool
	}{
		{"new registration deleted", previousSCMRegistration{}, &scmTestRecoveryTarget{}, true},
		{"new registration deletion unconfirmed", previousSCMRegistration{}, &scmTestRecoveryTarget{deleteErr: deleteErr}, false},
		{"prior config restored", scmTestRunningPrevious(), &scmTestRecoveryTarget{}, true},
		{"prior config restoration failed", scmTestRunningPrevious(), &scmTestRecoveryTarget{writeErrs: []error{writeErr}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			safe, err := (&scmManager{scope: ScopeSystem}).rollbackInstallation(
				context.Background(), tc.target, tc.prev, scmRollbackRegistration, failure)
			if err == nil || !errors.Is(err, failure) {
				t.Fatalf("rollbackInstallation() = %v, want original failure wrapped", err)
			}
			if safe != tc.wantSafe {
				t.Errorf("rollbackInstallation() cleanup-safe = %v, want %v", safe, tc.wantSafe)
			}
		})
	}

	dir := filepath.Join(t.TempDir(), "created-state")
	err := os.Mkdir(dir, 0o700)
	assertNoError(t, err, "Mkdir(%q) = %v", dir, err)
	cleanupCreatedSCMInstallDirs([]string{dir}, scmRegisterResult{createdDirsSafeToRemove: false})
	_, err = os.Stat(dir)
	assertNoError(t, err, "an unproven registration outcome removed %q: %v", dir, err)
	cleanupCreatedSCMInstallDirs([]string{dir}, scmRegisterResult{createdDirsSafeToRemove: true})
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a proven-safe cleanup left %q in place: %v", dir, err)
	}
}

// TestSCMCreatedDirOrder checks the order of the directories a failed
// installation removes: the directories are listed in the order they are
// created, so a parent is always removed after its children.
//
// The installation creates the directories of the instance only: a named data
// directory belongs to whoever named it, and the instance creates the cache
// tree it needs inside its own directories when it runs.
func TestSCMCreatedDirOrder(t *testing.T) {
	root := t.TempDir()
	opts := scmTestInstallOptions(t, root)

	dirs := scmInstallDirs(opts)
	created := make(map[string][]string, len(dirs))
	for _, dir := range dirs {
		created[dir] = createdDirs(dir)
	}
	order := createdDirOrder(opts, created)

	want := []string{
		filepath.Join(root, "etc"),
		filepath.Join(root, "etc", "hfdesk"),
		filepath.Join(root, "var"),
		filepath.Join(root, "var", "lib"),
		filepath.Join(root, "var", "lib", "hfdesk"),
		filepath.Join(root, "var", "log"),
		filepath.Join(root, "var", "log", "hfdesk"),
	}
	if strings.Join(order, "\n") != strings.Join(want, "\n") {
		t.Errorf("createdDirOrder() =\n%s\nwant\n%s", strings.Join(order, "\n"), strings.Join(want, "\n"))
	}
}

// TestSCMFailedInstallRemovesTheDirectoriesItCreated checks that a failed
// installation removes the directories it created, so a failed attempt
// leaves the machine as it was.
//
// The installation is refused by the preflight: the local directory cannot
// be reached by the account of the instance. The refusal happens after the
// directories of the instance were created, so the cleanup has to remove
// them again.
func TestSCMFailedInstallRemovesTheDirectoriesItCreated(t *testing.T) {
	if !isElevatedProcess() {
		t.Skip("an installation needs an elevated shell, run the test as an administrator")
	}
	root := t.TempDir()
	opts := scmTestInstallOptions(t, root)
	scmTestRefuseLocalDir(t, opts)

	service := &scmManager{scope: ScopeSystem}
	if _, err := service.Install(context.Background(), opts); err == nil {
		t.Fatal("Install() = nil, want the refusal of the local directory")
	}

	// Every directory the installation created is gone again: the
	// directories of the instance are removed before the parents that
	// hold them. The cache root and the hub directory are not created by
	// the installation at all, so they are gone as well.
	for _, dir := range []string{
		filepath.Join(root, "etc"),
		filepath.Join(root, "etc", "hfdesk"),
		filepath.Join(root, "var"),
		filepath.Join(root, "var", "lib"),
		filepath.Join(root, "var", "lib", "hfdesk"),
		filepath.Join(root, "var", "lib", "hfdesk", "cache"),
		filepath.Join(root, "var", "lib", "hfdesk", "cache", "hub"),
		filepath.Join(root, "var", "log"),
		filepath.Join(root, "var", "log", "hfdesk"),
	} {
		if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the directory %q is still there after the failed installation (stat error: %v)", dir, err)
		}
	}
}

// TestSCMFailedInstallKeepsADirectoryThatExistedBefore checks that a failed
// installation never removes a directory that existed before it: such a
// directory belongs to whoever owns it.
//
// The local directory is a directory the installation reuses: the
// installation never describes it, so its owner and its permissions are the
// ones it had. The config directory of the instance is a directory the
// installation owns as well: it is still there as well, with the owner it
// had and with the descriptor it had, because the installation never
// rewrites the descriptor of a path that existed before it.
func TestSCMFailedInstallKeepsADirectoryThatExistedBefore(t *testing.T) {
	if !isElevatedProcess() {
		t.Skip("an installation needs an elevated shell, run the test as an administrator")
	}
	root := t.TempDir()
	opts := scmTestInstallOptions(t, root)
	scmTestRefuseLocalDir(t, opts)

	// The config directory of the instance existed before the
	// installation as well.
	configDir := filepath.Dir(opts.Paths.ConfigFile)
	err := os.MkdirAll(configDir, 0o750)
	assertNoError(t, err, "MkdirAll(%q) = %v, want nil", configDir, err)
	// The directory has to be usable by the account of the instance:
	// the installation refuses it otherwise, before it reaches the local
	// directory.
	scmTestSetDACL(t, configDir, scmTestAllowInstanceIdentityDACL(t))
	configOwner, configMode := scmTestOwnerAndMode(t, configDir)
	configPermissions := scmTestDACL(t, configDir)

	owner, permissions := scmTestSecurity(t, opts.LocalDir)

	service := &scmManager{scope: ScopeSystem}
	if _, err := service.Install(context.Background(), opts); err == nil {
		t.Fatal("Install() = nil, want the refusal of the local directory")
	}

	// The directory the installation reuses is still there, with the
	// owner and the permissions it had.
	info, err := os.Stat(opts.LocalDir)
	assertNoError(t, err, "the local directory %q is gone after the failed installation: %v", opts.LocalDir, err)
	if !info.IsDir() {
		t.Errorf("the path %q is not a directory after the failed installation", opts.LocalDir)
	}
	gotOwner, gotPermissions := scmTestSecurity(t, opts.LocalDir)
	if gotOwner != owner {
		t.Errorf("the owner of the local directory %q = %q, want %q", opts.LocalDir, gotOwner, owner)
	}
	if gotPermissions != permissions {
		t.Errorf("the permissions of the local directory %q = %q, want %q", opts.LocalDir, gotPermissions, permissions)
	}

	// The directory of the instance that existed before the installation
	// is still there as well, with the owner, the mode and the
	// descriptor it had.
	if _, err := os.Stat(configDir); err != nil {
		t.Errorf("the config directory %q is gone after the failed installation: %v", configDir, err)
	}
	gotConfigOwner, gotConfigMode := scmTestOwnerAndMode(t, configDir)
	if gotConfigOwner != configOwner {
		t.Errorf("the owner of the config directory %q = %q, want %q", configDir, gotConfigOwner, configOwner)
	}
	if gotConfigMode != configMode {
		t.Errorf("the mode of the config directory %q = %v, want %v", configDir, gotConfigMode, configMode)
	}
	if gotConfigDACL := scmTestDACL(t, configDir); gotConfigDACL != configPermissions {
		t.Errorf("the access control list of the config directory %q = %q, want %q", configDir, gotConfigDACL, configPermissions)
	}
}

// TestSCMRemoveCreatedDirsKeepsADirectoryThatHoldsData checks that the
// cleanup of a failed installation never deletes data: a directory that
// holds something is kept, and the empty directories around it are removed.
//
// The directory that holds data is checked on the cleanup itself: an
// installation writes no file into the directories it creates, so a
// directory that holds something is a directory that holds the data of
// somebody else.
func TestSCMRemoveCreatedDirsKeepsADirectoryThatHoldsData(t *testing.T) {
	root := t.TempDir()
	holding := filepath.Join(root, "holding")
	err := os.Mkdir(holding, 0o700)
	assertNoError(t, err, "Mkdir() = %v, want nil", err)
	marker := filepath.Join(holding, "marker")
	err = os.WriteFile(marker, []byte("data"), 0o600)
	assertNoError(t, err, "WriteFile() = %v, want nil", err)
	empty := filepath.Join(root, "empty")
	err = os.Mkdir(empty, 0o700)
	assertNoError(t, err, "Mkdir() = %v, want nil", err)

	removeCreatedDirs([]string{holding, empty})

	// The directory that holds data and the file in it are still there.
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("the file of the directory that holds data is gone: %v", err)
	}
	if _, err := os.Stat(holding); err != nil {
		t.Errorf("the directory %q that holds data is gone: %v", holding, err)
	}
	// The empty directory is gone.
	if _, err := os.Stat(empty); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the empty directory %q is still there (stat error: %v)", empty, err)
	}
}

// TestSCMFailedInstallReportsTheOriginalError checks that the cleanup of a
// failed installation never replaces the error of the installation: the
// caller gets the refusal of the preflight itself.
func TestSCMFailedInstallReportsTheOriginalError(t *testing.T) {
	if !isElevatedProcess() {
		t.Skip("an installation needs an elevated shell, run the test as an administrator")
	}
	root := t.TempDir()
	opts := scmTestInstallOptions(t, root)
	access := scmTestRefuseLocalDir(t, opts)

	// The refusal of the preflight is the error the caller gets.
	want := preflightDataPath("local dir", opts.LocalDir, access)
	if want == nil {
		t.Fatal("the local directory is usable by the account of the instance, the test cannot fail the installation")
	}

	service := &scmManager{scope: ScopeSystem}
	_, err := service.Install(context.Background(), opts)
	if err == nil {
		t.Fatal("Install() = nil, want the refusal of the local directory")
	}
	if err.Error() != want.Error() {
		t.Errorf("Install() = %q, want %q", err, want)
	}
}

// TestSCMSuccessfulInstallKeepsItsDirectories checks that a successful
// installation keeps the directories it created: the cleanup of a failed
// installation never touches the directories of an instance that is
// installed.
//
// The installation creates the directories of the instance only: the cache
// root and the hub directory live in the state directory of the instance and
// are created by the instance when it runs, and the local directory belongs
// to whoever named it.
//
// The test registers the instance in the service control manager of the
// machine, so it needs an elevated shell. The registration never survives
// the test.
func TestSCMSuccessfulInstallKeepsItsDirectories(t *testing.T) {
	if !isElevatedProcess() {
		t.Skip("an installation needs an elevated shell, run the test as an administrator")
	}
	// A registration that exists already belongs to the machine and not
	// to this test: it is never touched here.
	state, err := scmRegistrationStateOf()
	if err != nil {
		t.Skipf("the service control manager is not available: %v", err)
	}
	if state != scmRegistrationAbsent {
		t.Skipf("%s is registered, the test would touch a real installation", ServiceName)
	}

	root := t.TempDir()
	opts := scmTestInstallOptions(t, root)
	// The local directory has to be usable by the account of the
	// instance: the preflight refuses the installation otherwise.
	if err := os.MkdirAll(opts.LocalDir, 0o700); err != nil {
		t.Fatalf("MkdirAll(%q) = %v, want nil", opts.LocalDir, err)
	}
	scmTestSetDACL(t, opts.LocalDir, scmTestAllowInstanceIdentityDACL(t))

	// The registration never survives the test.
	t.Cleanup(func() {
		cleanupManager, err := mgr.Connect()
		if err != nil {
			return
		}
		defer cleanupManager.Disconnect()
		handle, err := cleanupManager.OpenService(ServiceName)
		if err != nil {
			return
		}
		defer handle.Close()
		_ = handle.Delete()
	})

	service := &scmManager{scope: ScopeSystem}
	if _, err := service.Install(context.Background(), opts); err != nil {
		t.Fatalf("Install() = %v, want nil", err)
	}

	for _, dir := range []string{
		filepath.Join(root, "etc"),
		filepath.Join(root, "etc", "hfdesk"),
		filepath.Join(root, "var"),
		filepath.Join(root, "var", "lib"),
		filepath.Join(root, "var", "lib", "hfdesk"),
		filepath.Join(root, "var", "log"),
		filepath.Join(root, "var", "log", "hfdesk"),
		// The local directory belongs to whoever named it: the
		// installation reuses it and never removes it.
		filepath.Join(root, "models"),
	} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Errorf("the directory %q is gone after the successful installation: %v", dir, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("the path %q is not a directory after the successful installation", dir)
		}
	}

	// The cache root and the hub directory are not created by the
	// installation: the instance creates the cache tree it needs inside
	// its own state directory when it runs.
	for _, dir := range []string{opts.CacheDir, opts.HubDir} {
		if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the directory %q exists after the successful installation (stat error: %v), want none: the instance creates it when it runs", dir, err)
		}
	}
}

// TestSCMRegistrationOptionsRecoversTheRegistration checks that the options of
// an existing registration are read back from the command line the service
// control manager holds: the program, the bind address, the runtime paths and
// the cache layout.
//
// The recovery is what makes an installation over an existing registration keep
// the settings the instance really runs with.
func TestSCMRegistrationOptionsRecoversTheRegistration(t *testing.T) {
	cases := []struct {
		name string
		opts InstallOptions
	}{
		{"the full layout", scmTestOptions()},
		{
			name: "without a cache layout",
			opts: func() InstallOptions {
				opts := scmTestOptions()
				opts.CacheDir = ""
				opts.HubDir = ""
				opts.LocalDir = ""
				return opts
			}(),
		},
		{
			name: "with a path that carries a space",
			opts: func() InstallOptions {
				opts := scmTestOptions()
				opts.Executable = filepath.Join("C:\\Program Files", "HFDesk", "hfdesk.exe")
				opts.Paths.StateDir = filepath.Join("C:\\Program Files", "HFDesk", "state")
				return opts
			}(),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := scmRegistrationOptions(scmCommandLine(tc.opts), ScopeSystem)
			if !reflect.DeepEqual(got, tc.opts) {
				t.Errorf("scmRegistrationOptions() =\n%+v\nwant\n%+v", got, tc.opts)
			}
			if got.Explicit != (InstallFieldSet{}) {
				t.Errorf("the recovered options mark fields explicit: %+v", got.Explicit)
			}
			// The account of a Windows instance is the built-in
			// LocalService account and never a setting of the
			// registration.
			if got.ServiceUser != "" {
				t.Errorf("the recovered account = %q, want none", got.ServiceUser)
			}
		})
	}
}

// TestSCMRegistrationOptionsOfAnUnreadableCommandLineNameNothing checks that a
// registration without a readable command line records no setting.
//
// An installation is the way a damaged registration is repaired, so the
// recovery never fails: the options are empty, and the merge falls back to the
// defaults of the request.
func TestSCMRegistrationOptionsOfAnUnreadableCommandLineNameNothing(t *testing.T) {
	for _, commandLine := range []string{"", "   "} {
		got := scmRegistrationOptions(commandLine, ScopeSystem)
		want := InstallOptions{Scope: ScopeSystem}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("scmRegistrationOptions(%q) =\n%+v\nwant\n%+v", commandLine, got, want)
		}
	}
}

func TestSCMRegistrationOptionsDefaultsListenForAParseableBrokenRegistration(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing", "hfdesk.exe")
	commandLine := syscall.EscapeArg(missing)

	options := scmRegistrationOptions(commandLine, ScopeSystem)
	if options.Executable != missing || options.Listen != DefaultListen {
		t.Fatalf("scmRegistrationOptions() = %+v, want the readable program and parser-default listen %q", options, DefaultListen)
	}

	status, err := scmStatusFrom(ScopeSystem, scmRegistrationFacts{
		present:     true,
		state:       svc.Stopped,
		commandLine: commandLine,
	})
	assertNoError(t, err, "scmStatusFrom() = %v, want a readable broken registration", err)
	if status.State != StateBroken || status.Listen != DefaultListen {
		t.Errorf("status = %+v, want broken with parser-default listen %q", status, DefaultListen)
	}
}

// TestSCMReinstallKeepsTheRegistrationOfTheInstance checks the merge of a
// request with the registration of the instance: a setting the request did not
// name keeps the value the instance runs with, and only the program comes from
// the request.
func TestSCMReinstallKeepsTheRegistrationOfTheInstance(t *testing.T) {
	registered := scmTestOptions()
	registered.Executable = filepath.Join("C:\\Old\\HFDesk", "hfdesk.exe")
	registered.Listen = "127.0.0.1:9090"

	existing := scmRegistrationOptions(scmCommandLine(registered), ScopeSystem)

	// A request that names nothing but the new program: the defaults of the
	// scope apply only where the registration has no value of its own.
	requested := InstallOptions{
		Scope:      ScopeSystem,
		Executable: filepath.Join("C:\\New\\HFDesk", "hfdesk.exe"),
		Listen:     DefaultListen,
		Paths: runtimepaths.Paths{
			ConfigFile: filepath.Join("C:\\ProgramData", "HFDesk", "hfdesk.json"),
			StateDir:   filepath.Join("C:\\ProgramData", "HFDesk"),
			LogDir:     filepath.Join("C:\\ProgramData", "HFDesk", "logs"),
		},
		CacheDir: filepath.Join("C:\\ProgramData", "HFDesk", "cache"),
		HubDir:   filepath.Join("C:\\ProgramData", "HFDesk", "cache", "hub"),
	}

	merged := mergeInstallOptions(requested, existing, true)

	if merged.Executable != requested.Executable {
		t.Errorf("the executable = %q, want the one of the request %q", merged.Executable, requested.Executable)
	}
	if merged.Listen != registered.Listen {
		t.Errorf("the listen address = %q, want the registered one %q", merged.Listen, registered.Listen)
	}
	if merged.Paths != registered.Paths {
		t.Errorf("the paths = %+v, want the registered ones %+v", merged.Paths, registered.Paths)
	}
	if merged.CacheDir != registered.CacheDir || merged.HubDir != registered.HubDir || merged.LocalDir != registered.LocalDir {
		t.Errorf("the cache layout = %q/%q/%q, want the registered one %q/%q/%q",
			merged.CacheDir, merged.HubDir, merged.LocalDir, registered.CacheDir, registered.HubDir, registered.LocalDir)
	}
}

// TestSCMReinstallHonoursTheExplicitFields checks that a setting the request
// named itself wins over the registration of the instance.
func TestSCMReinstallHonoursTheExplicitFields(t *testing.T) {
	registered := scmTestOptions()
	existing := scmRegistrationOptions(scmCommandLine(registered), ScopeSystem)

	requested := InstallOptions{
		Scope:      ScopeSystem,
		Executable: filepath.Join("C:\\New\\HFDesk", "hfdesk.exe"),
		Explicit: InstallFieldSet{
			Listen:   true,
			Config:   true,
			StateDir: true,
			LogDir:   true,
			CacheDir: true,
			HubDir:   true,
			LocalDir: true,
		},
		Listen: "127.0.0.1:7070",
		Paths: runtimepaths.Paths{
			ConfigFile: filepath.Join("C:\\Named", "hfdesk.json"),
			StateDir:   filepath.Join("C:\\Named", "state"),
			LogDir:     filepath.Join("C:\\Named", "log"),
		},
		CacheDir: filepath.Join("C:\\Named", "cache"),
		HubDir:   filepath.Join("C:\\Named", "cache", "hub"),
		LocalDir: filepath.Join("C:\\Named", "models"),
	}

	merged := mergeInstallOptions(requested, existing, true)
	if !reflect.DeepEqual(merged, requested) {
		t.Errorf("mergeInstallOptions() =\n%+v\nwant the request\n%+v", merged, requested)
	}
}

// TestSCMRecoveryActionsRestartTheFailedInstance checks the failure recovery
// of the registration: the platform restarts an instance that ended without
// reporting its stop, after five, thirty and sixty seconds, and it forgets the
// failures of the instance after one day.
//
// The platform never reboots the machine and never runs a command for a failed
// instance: only a restart of the instance is one of the actions.
func TestSCMRecoveryActionsRestartTheFailedInstance(t *testing.T) {
	actions := scmRecoveryActions()
	want := []mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 60 * time.Second},
	}
	if !reflect.DeepEqual(actions, want) {
		t.Errorf("scmRecoveryActions() = %v, want %v", actions, want)
	}
	for _, action := range actions {
		switch action.Type {
		case mgr.ServiceRestart:
		case mgr.NoAction, mgr.ComputerReboot, mgr.RunCommand:
			t.Errorf("the recovery action %v is not a restart of the instance", action)
		}
		if action.Delay < 0 {
			t.Errorf("the recovery action %v has a negative delay", action)
		}
	}

	if scmRecoveryResetPeriod != 24*60*60 {
		t.Errorf("the reset period = %d seconds, want one day (86400)", scmRecoveryResetPeriod)
	}
}

// TestSCMRecoveryDecisionKeepsARegisteredPolicy checks that only a successful
// read of an empty action list authorizes default recovery setup. A failed read
// is not evidence that the registration lacks recovery.
func TestSCMRecoveryDecisionKeepsARegisteredPolicy(t *testing.T) {
	administrator := []mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 10 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 10 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 10 * time.Second},
	}
	readFailure := errors.New("the failure actions of the registration cannot be read")

	tests := []struct {
		name     string
		existing []mgr.RecoveryAction
		readErr  error
		want     scmRecoveryDecision
	}{
		{"a registration without actions", nil, nil, scmRecoveryConfigure},
		{"an empty list of actions", []mgr.RecoveryAction{}, nil, scmRecoveryConfigure},
		{"the policy of HFDesk is registered already", scmRecoveryActions(), nil, scmRecoveryKeep},
		{"a policy of an administrator", administrator, nil, scmRecoveryKeep},
		{"an explicitly registered policy of no action", []mgr.RecoveryAction{{Type: mgr.NoAction}}, nil, scmRecoveryKeep},
		{"a read that fails", nil, readFailure, scmRecoveryUnreadable},
		{"a read that fails although actions were returned", administrator, readFailure, scmRecoveryUnreadable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := scmRecoveryDecisionFor(test.existing, test.readErr); got != test.want {
				t.Errorf("scmRecoveryDecisionFor(%v, %v) = %v, want %v",
					test.existing, test.readErr, got, test.want)
			}
		})
	}
}

type scmTestRecoveryConfig struct {
	actions         []mgr.RecoveryAction
	resetPeriod     uint32
	nonCrash        bool
	command         string
	rebootMessage   string
	actionsReadErr  error
	resetReadErr    error
	nonCrashReadErr error
	commandReadErr  error
	rebootReadErr   error
	actionWriteErrs []error
	flagWriteErrs   []error
	actionWrites    []scmRecoverySettings
	flagWrites      []bool
}

func (c *scmTestRecoveryConfig) RecoveryActions() ([]mgr.RecoveryAction, error) {
	return slices.Clone(c.actions), c.actionsReadErr
}

func (c *scmTestRecoveryConfig) ResetPeriod() (uint32, error) {
	return c.resetPeriod, c.resetReadErr
}

func (c *scmTestRecoveryConfig) RecoveryActionsOnNonCrashFailures() (bool, error) {
	return c.nonCrash, c.nonCrashReadErr
}

func (c *scmTestRecoveryConfig) RecoveryCommand() (string, error) {
	return c.command, c.commandReadErr
}

func (c *scmTestRecoveryConfig) RebootMessage() (string, error) {
	return c.rebootMessage, c.rebootReadErr
}

func (c *scmTestRecoveryConfig) SetActions(actions []mgr.RecoveryAction, reset uint32, command, reboot string) error {
	c.actionWrites = append(c.actionWrites, scmRecoverySettings{
		actions: slices.Clone(actions), resetPeriod: reset, command: command, rebootMessage: reboot,
	})
	var err error
	if len(c.actionWriteErrs) > 0 {
		err = c.actionWriteErrs[0]
		c.actionWriteErrs = c.actionWriteErrs[1:]
	}
	if err == nil {
		c.actions, c.resetPeriod, c.command, c.rebootMessage = slices.Clone(actions), reset, command, reboot
	}
	return err
}

func (c *scmTestRecoveryConfig) SetNonCrashFailures(enabled bool) error {
	c.flagWrites = append(c.flagWrites, enabled)
	var err error
	if len(c.flagWriteErrs) > 0 {
		err = c.flagWriteErrs[0]
		c.flagWriteErrs = c.flagWriteErrs[1:]
	}
	if err == nil {
		c.nonCrash = enabled
	}
	return err
}

func TestSCMConfigureRecoveryUsesOnlyEstablishedMissingPolicy(t *testing.T) {
	customPolicies := []struct {
		name    string
		actions []mgr.RecoveryAction
	}{
		{"intentional no-action", []mgr.RecoveryAction{{Type: mgr.NoAction}}},
		{"custom restart", []mgr.RecoveryAction{{Type: mgr.ServiceRestart, Delay: 12 * time.Second}}},
		{"computer reboot", []mgr.RecoveryAction{{Type: mgr.ComputerReboot, Delay: 13 * time.Second}}},
		{"run command", []mgr.RecoveryAction{{Type: mgr.RunCommand, Delay: 14 * time.Second}}},
		{"mixed ordered actions", []mgr.RecoveryAction{
			{Type: mgr.NoAction},
			{Type: mgr.ServiceRestart, Delay: 15 * time.Second},
			{Type: mgr.ComputerReboot, Delay: 16 * time.Second},
			{Type: mgr.RunCommand, Delay: 17 * time.Second},
		}},
	}
	for _, policy := range customPolicies {
		for _, nonCrash := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/non-crash-%t", policy.name, nonCrash), func(t *testing.T) {
				custom := slices.Clone(policy.actions)
				config := &scmTestRecoveryConfig{
					actions: custom, resetPeriod: 17, nonCrash: nonCrash,
					command: "custom recovery command", rebootMessage: "custom reboot message",
				}
				err := configureSCMRecoveryWith(config)
				assertNoError(t, err, "configureSCMRecoveryWith() = %v, want the custom policy preserved", err)
				if !slices.Equal(config.actions, custom) || config.nonCrash != nonCrash {
					t.Errorf("the custom policy became actions=%v nonCrash=%v, want actions=%v nonCrash=%v",
						config.actions, config.nonCrash, custom, nonCrash)
				}
				if len(config.actionWrites) != 0 || len(config.flagWrites) != 0 {
					t.Errorf("the custom policy received writes: actions=%v flags=%v", config.actionWrites, config.flagWrites)
				}
				if config.command != "custom recovery command" || config.rebootMessage != "custom reboot message" {
					t.Errorf("the custom ancillary recovery settings changed: %+v", config)
				}
			})
		}
	}
	t.Run("sets required defaults after successful empty read", func(t *testing.T) {
		config := &scmTestRecoveryConfig{
			resetPeriod: 17, command: "custom recovery command", rebootMessage: "custom reboot message",
		}
		err := configureSCMRecoveryWith(config)
		assertNoError(t, err, "configureSCMRecoveryWith() = %v, want defaults configured", err)
		if !slices.Equal(config.actions, scmRecoveryActions()) || config.resetPeriod != scmRecoveryResetPeriod || !config.nonCrash {
			t.Errorf("configured recovery = %+v, want defaults and non-crash failure recovery", config)
		}
		if config.command != "custom recovery command" || config.rebootMessage != "custom reboot message" {
			t.Errorf("the ancillary recovery settings changed: %+v", config)
		}
		if len(config.flagWrites) != 1 || !config.flagWrites[0] {
			t.Errorf("non-crash flag writes = %v, want one enable after a successful read of false", config.flagWrites)
		}
	})
	t.Run("does not rewrite an already-enabled non-crash flag", func(t *testing.T) {
		config := &scmTestRecoveryConfig{nonCrash: true}
		err := configureSCMRecoveryWith(config)
		assertNoError(t, err, "configureSCMRecoveryWith() = %v, want defaults configured", err)
		if len(config.flagWrites) != 0 {
			t.Errorf("non-crash flag writes = %v, want none", config.flagWrites)
		}
	})
}

func TestSCMConfigureRecoveryDoesNotWriteAfterUnreadableEvidence(t *testing.T) {
	readErr := errors.New("the recovery policy could not be read")
	cases := []struct {
		name   string
		config *scmTestRecoveryConfig
	}{
		{"actions", &scmTestRecoveryConfig{actionsReadErr: readErr}},
		{"reset period", &scmTestRecoveryConfig{resetReadErr: readErr}},
		{"non-crash flag on an empty action list", &scmTestRecoveryConfig{nonCrashReadErr: readErr}},
		{"recovery command", &scmTestRecoveryConfig{commandReadErr: readErr}},
		{"reboot message", &scmTestRecoveryConfig{rebootReadErr: readErr}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := configureSCMRecoveryWith(tc.config)
			if err == nil {
				t.Fatal("configureSCMRecoveryWith() = nil, want the unreadable policy error")
			}
			if !errors.Is(err, readErr) {
				t.Errorf("configureSCMRecoveryWith() = %v, want the original read error", err)
			}
			if len(tc.config.actionWrites) != 0 || len(tc.config.flagWrites) != 0 {
				t.Errorf("the unreadable policy caused writes: actions=%v flags=%v", tc.config.actionWrites, tc.config.flagWrites)
			}
		})
	}
	t.Run("an unreadable custom flag is preserved without writes", func(t *testing.T) {
		custom := []mgr.RecoveryAction{{Type: mgr.ComputerReboot, Delay: 5 * time.Second}}
		config := &scmTestRecoveryConfig{
			actions: custom, nonCrash: false, nonCrashReadErr: readErr,
		}
		err := configureSCMRecoveryWith(config)
		assertNoError(t, err, "configureSCMRecoveryWith() = %v, want the custom policy untouched", err)
		if !slices.Equal(config.actions, custom) || config.nonCrash || len(config.actionWrites) != 0 || len(config.flagWrites) != 0 {
			t.Errorf("the unreadable custom policy changed: %+v", config)
		}
	})
}

func TestSCMConfigureRecoveryRestoresAfterActionWriteFailure(t *testing.T) {
	writeErr := errors.New("the SCM refused failure actions")
	config := &scmTestRecoveryConfig{
		resetPeriod:     23,
		command:         "custom command",
		rebootMessage:   "custom message",
		actionWriteErrs: []error{writeErr, nil},
	}
	err := configureSCMRecoveryWith(config)
	if err == nil || !errors.Is(err, writeErr) {
		t.Fatalf("configureSCMRecoveryWith() = %v, want the action-write error", err)
	}
	if !strings.Contains(err.Error(), "previous empty recovery policy was restored and verified") {
		t.Errorf("error = %v, want verified recovery restoration", err)
	}
	if len(config.actionWrites) != 2 || len(config.actionWrites[1].actions) != 0 || config.actionWrites[1].resetPeriod != 23 {
		t.Errorf("action writes = %+v, want the default write and restoration of the prior empty policy", config.actionWrites)
	}
	if len(config.flagWrites) != 0 {
		t.Errorf("non-crash flag writes = %v, want none after the action write failed", config.flagWrites)
	}
}

func TestSCMConfigureRecoveryRestoresThePriorPolicyWhenSetupFails(t *testing.T) {
	flagErr := errors.New("the SCM refused the non-crash flag")
	config := &scmTestRecoveryConfig{
		resetPeriod:   19,
		command:       "custom command",
		rebootMessage: "custom message",
		flagWriteErrs: []error{flagErr, nil},
	}
	err := configureSCMRecoveryWith(config)
	if err == nil || !errors.Is(err, flagErr) {
		t.Fatalf("configureSCMRecoveryWith() = %v, want the original flag error", err)
	}
	if !strings.Contains(err.Error(), "previous empty recovery policy was restored and verified") {
		t.Errorf("error = %v, want the verified recovery restore outcome", err)
	}
	if len(config.actionWrites) != 2 || len(config.actionWrites[1].actions) != 0 || config.actionWrites[1].resetPeriod != 19 {
		t.Errorf("action writes = %+v, want defaults followed by the original empty policy and reset period", config.actionWrites)
	}
	if config.nonCrash || len(config.flagWrites) != 2 || config.flagWrites[0] != true || config.flagWrites[1] != false {
		t.Errorf("non-crash state/writes = %v/%v, want the original false value restored", config.nonCrash, config.flagWrites)
	}
}

func TestSCMConfigureRecoveryReportsAnUnverifiedRestoreWithoutLosingTheCause(t *testing.T) {
	setupErr := errors.New("the SCM refused the non-crash flag")
	restoreErr := errors.New("the SCM refused the recovery reset")
	config := &scmTestRecoveryConfig{
		flagWriteErrs:   []error{setupErr, nil},
		actionWriteErrs: []error{nil, restoreErr},
	}
	err := configureSCMRecoveryWith(config)
	if err == nil || !errors.Is(err, setupErr) {
		t.Fatalf("configureSCMRecoveryWith() = %v, want the setup error preserved", err)
	}
	if !strings.Contains(err.Error(), "could not be restored or verified") || strings.Contains(err.Error(), "was restored and verified") {
		t.Errorf("error = %v, want an unverified recovery restore", err)
	}
	if !strings.Contains(err.Error(), restoreErr.Error()) {
		t.Errorf("error = %v, want the restore failure reason", err)
	}
}

// TestSCMSameExecutablePath checks the comparison of two paths that name a
// program: the platform stores the path of a program in the case and the
// spelling it was given, so the comparison is case-insensitive and made on the
// cleaned path.
func TestSCMSameExecutablePath(t *testing.T) {
	cases := []struct {
		name string
		a    string
		b    string
		want bool
	}{
		{"the same path", `C:\ProgramData\HFDesk\hfdesk.exe`, `C:\ProgramData\HFDesk\hfdesk.exe`, true},
		{"another case", `C:\ProgramData\HFDesk\hfdesk.exe`, `c:\programdata\hfdesk\HFDESK.EXE`, true},
		{"a redundant part of the path", `C:\ProgramData\HFDesk\hfdesk.exe`, `C:\ProgramData\HFDesk\.\hfdesk.exe`, true},
		{"another program in the same directory", `C:\ProgramData\HFDesk\hfdesk.exe`, `C:\ProgramData\HFDesk\hfdesk-old.exe`, false},
		{"another directory", `C:\ProgramData\HFDesk\hfdesk.exe`, `C:\ProgramData\HFDesk2\hfdesk.exe`, false},
		{"a program below the directory", `C:\ProgramData\HFDesk\hfdesk.exe`, `C:\ProgramData\HFDesk\bin\hfdesk.exe`, false},
		{"an empty path against a program", "", `C:\ProgramData\HFDesk\hfdesk.exe`, false},
		{"a program against an empty path", `C:\ProgramData\HFDesk\hfdesk.exe`, "", false},
		{"two empty paths", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := scmSameExecutablePath(tc.a, tc.b); got != tc.want {
				t.Errorf("scmSameExecutablePath(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
			if got := scmSameExecutablePath(tc.b, tc.a); got != tc.want {
				t.Errorf("scmSameExecutablePath(%q, %q) = %v, want %v", tc.b, tc.a, got, tc.want)
			}
		})
	}
}

// TestSCMInstanceRan checks the classification of the state of a registration
// before an installation.
//
// Only an instance that runs, or that is still starting, is restarted by the
// installation. A stopped, broken, absent or stopping instance is left alone:
// an installation never starts a service that was not running, and it never
// brings back a service the user stopped.
func TestSCMInstanceRan(t *testing.T) {
	cases := []struct {
		name    string
		present bool
		state   svc.State
		want    bool
	}{
		{"running", true, svc.Running, true},
		{"starting", true, svc.StartPending, true},
		{"stopped", true, svc.Stopped, false},
		{"stopping", true, svc.StopPending, false},
		{"paused", true, svc.Paused, false},
		{"pause pending", true, svc.PausePending, false},
		{"continue pending", true, svc.ContinuePending, false},
		{"an unknown state", true, svc.State(4711), false},
		{"a running state without a registration", false, svc.Running, false},
		{"a stopped state without a registration", false, svc.Stopped, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := scmInstanceRan(tc.present, tc.state); got != tc.want {
				t.Errorf("scmInstanceRan(%v, %v) = %v, want %v", tc.present, tc.state, got, tc.want)
			}
		})
	}
}

// fakeSCMConfiguration is the configuration of a registration of a test.
//
// It implements scmConfiguration, so the failure path of an update can be driven
// without a registration of its own. A refused write can apply the configuration
// first, which is what the platform leaves behind when a later call of the
// non-atomic update fails, and a read can be refused as well. The fake has no
// start and no stop: the failure path of an update never touches the running
// instance, and a type that cannot do it proves that.
type fakeSCMConfiguration struct {
	// config is the configuration the registration holds.
	config mgr.Config
	// writes are the configurations the writes tried to store, in order.
	writes []mgr.Config
	// writeErrs are the errors of the writes, per call: a missing entry
	// keeps the write. A refused write stores the configuration first when
	// applyBeforeRefusing is set, so a partial update can be simulated.
	writeErrs []error
	// applyBeforeRefusing stores a configuration before its write refuses
	// it.
	applyBeforeRefusing bool
	// readErr is the error of every read.
	readErr error
	// partialReadErr is the error of a read that happens while the
	// registration still holds the configuration of the refused update:
	// the update is the first write and the restore is the second one, so a
	// read between them is refused and the read-back of the restore is
	// answered. It models an observation of the partially changed
	// registration, which the failure path must not depend on.
	partialReadErr error
}

// UpdateConfig records the write and returns the error of the call.
func (f *fakeSCMConfiguration) UpdateConfig(config mgr.Config) error {
	index := len(f.writes)
	f.writes = append(f.writes, config)
	var err error
	if index < len(f.writeErrs) {
		err = f.writeErrs[index]
	}
	if err == nil || f.applyBeforeRefusing {
		f.config = config
	}
	return err
}

// Config returns the configuration the registration holds.
func (f *fakeSCMConfiguration) Config() (mgr.Config, error) {
	if f.readErr != nil {
		return mgr.Config{}, f.readErr
	}
	// The update is the first write and the restore the second one: a read
	// of the configuration between them is refused.
	if f.partialReadErr != nil && len(f.writes) == 1 {
		return mgr.Config{}, f.partialReadErr
	}
	return f.config, nil
}

// TestSCMFailedUpdateConfigRestoresThePreviousConfiguration checks the failure
// path of a configuration write that the platform refuses.
//
// Service.UpdateConfig is not atomic: the command line can be replaced before a
// later call of the same update fails. The failure path always attempts the
// restore, even when the registration cannot be observed first, and the report
// says exactly what was proved: a restore whose write and read-back succeeded, a
// restore that failed, and a read-back that failed all name their outcome, and
// the original failure stays the error the caller gets.
func TestSCMFailedUpdateConfigRestoresThePreviousConfiguration(t *testing.T) {
	previousConfig := mgr.Config{
		ServiceType:      windows.SERVICE_WIN32_OWN_PROCESS,
		StartType:        mgr.StartAutomatic,
		ErrorControl:     mgr.ErrorNormal,
		BinaryPathName:   `C:\old\hfdesk.exe service run --listen 127.0.0.1:8080`,
		ServiceStartName: scmLocalServiceAccount,
	}
	requestedConfig := buildSCMConfig(scmTestOptions())
	updateErr := errors.New("the service control manager refused the description")
	restoreErr := errors.New("the service control manager refused the command line")
	readErr := errors.New("the service control manager is gone")

	cases := []struct {
		name string
		// fake is the registration the update is written to.
		fake *fakeSCMConfiguration
		// want is a sentence the report has to carry.
		want []string
		// wantUnproven is true when the report must not claim a restore.
		wantUnproven bool
		// wantDirsSafe follows the actual restore/read-back result.
		wantDirsSafe bool
	}{
		{
			name: "the configuration is restored",
			fake: &fakeSCMConfiguration{
				config:              previousConfig,
				writeErrs:           []error{updateErr},
				applyBeforeRefusing: true,
			},
			want:         []string{"update the registration", updateErr.Error(), "was restored"},
			wantDirsSafe: true,
		},
		{
			name: "the restore also fails",
			fake: &fakeSCMConfiguration{
				config:              previousConfig,
				writeErrs:           []error{updateErr, restoreErr},
				applyBeforeRefusing: true,
			},
			want:         []string{"update the registration", updateErr.Error(), "may be partially changed", "could not be restored or verified", restoreErr.Error()},
			wantUnproven: true,
		},
		{
			name: "the restore cannot be read back",
			fake: &fakeSCMConfiguration{
				config:              previousConfig,
				writeErrs:           []error{updateErr},
				applyBeforeRefusing: true,
				readErr:             readErr,
			},
			want:         []string{"update the registration", updateErr.Error(), "may be partially changed", "could not be restored or verified", readErr.Error()},
			wantUnproven: true,
		},
		{
			// The preliminary observation of the old failure path is
			// gone: a fake that refuses a read of the partially changed
			// registration still reaches the restore, and the restore's
			// read-back proves the previous command line.
			name: "a failed observation does not prevent the restore",
			fake: &fakeSCMConfiguration{
				config:              previousConfig,
				writeErrs:           []error{updateErr},
				applyBeforeRefusing: true,
				partialReadErr:      readErr,
			},
			want:         []string{"update the registration", updateErr.Error(), "was restored"},
			wantDirsSafe: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &scmManager{scope: ScopeSystem}
			previous := previousSCMRegistration{present: true, config: previousConfig}

			dirsSafe, err := m.updateRegistrationConfig(tc.fake, requestedConfig, previous)
			if err == nil {
				t.Fatal("updateRegistrationConfig() = nil, want the failure of the update")
			}
			if dirsSafe != tc.wantDirsSafe {
				t.Errorf("updateRegistrationConfig() directory cleanup safe = %v, want %v", dirsSafe, tc.wantDirsSafe)
			}
			if !errors.Is(err, updateErr) {
				t.Errorf("updateRegistrationConfig() = %v, want the original failure wrapped", err)
			}
			assertNames(t, err.Error(), "updateRegistrationConfig()", tc.want)
			if tc.wantUnproven && strings.Contains(err.Error(), "was restored") {
				t.Errorf("updateRegistrationConfig() = %q, want no claim that the configuration was restored", err)
			}

			// The update wrote once and the restore is always the second
			// write: a failed observation never prevents the repair.
			if len(tc.fake.writes) != 2 {
				t.Fatalf("the registration was written %d times, want the update and the restore", len(tc.fake.writes))
			}
			if !scmSameRegistration(tc.fake.writes[0].BinaryPathName, requestedConfig.BinaryPathName) {
				t.Errorf("the first write carries %q, want the requested registration", tc.fake.writes[0].BinaryPathName)
			}
			if !scmSameRegistration(tc.fake.writes[1].BinaryPathName, previousConfig.BinaryPathName) {
				t.Errorf("the second write carries %q, want the previous registration", tc.fake.writes[1].BinaryPathName)
			}
		})
	}
}

// TestSCMFailedUpdateConfigKeepsASuccessfulWrite checks that a configuration
// write the platform accepts needs no restore and reports no failure.
func TestSCMFailedUpdateConfigKeepsASuccessfulWrite(t *testing.T) {
	previousConfig := mgr.Config{BinaryPathName: `C:\old\hfdesk.exe service run`}
	requestedConfig := buildSCMConfig(scmTestOptions())
	registration := &fakeSCMConfiguration{config: previousConfig}

	m := &scmManager{scope: ScopeSystem}
	dirsSafe, err := m.updateRegistrationConfig(registration, requestedConfig, previousSCMRegistration{present: true, config: previousConfig})
	assertNoError(t, err, "updateRegistrationConfig() = %v, want nil", err)
	if dirsSafe {
		t.Error("a successful update says newly created directories are safe to remove")
	}
	if len(registration.writes) != 1 {
		t.Errorf("the registration was written %d times, want the update only", len(registration.writes))
	}
}

// TestSCMSameRegistration checks the comparison of two command lines of a
// registration: the program is compared like every other path, and the
// arguments are compared as they are.
func TestSCMSameRegistration(t *testing.T) {
	opts := scmTestOptions()
	commandLine := scmCommandLine(opts)

	otherArgs := opts
	otherArgs.Listen = "127.0.0.1:9090"

	otherProgram := opts
	otherProgram.Executable = filepath.Join("C:\\Program Files", "HFDesk", "hfdesk.exe")

	// The platform stores the path of a program in the case it was given:
	// the same registration in another case is the same registration.
	otherCase := opts
	otherCase.Executable = strings.ToUpper(opts.Executable)

	cases := []struct {
		name string
		got  string
		want string
		same bool
	}{
		{"the same command line", commandLine, commandLine, true},
		{"the same registration in another case", scmCommandLine(otherCase), commandLine, true},
		{"another bind address", scmCommandLine(otherArgs), commandLine, false},
		{"another program", scmCommandLine(otherProgram), commandLine, false},
		{"a command line that cannot be read", "", commandLine, false},
		{"two command lines that cannot be read", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := scmSameRegistration(tc.got, tc.want); got != tc.same {
				t.Errorf("scmSameRegistration(%q, %q) = %v, want %v", tc.got, tc.want, got, tc.same)
			}
		})
	}
}

type scmTestFirstInstallTarget struct {
	status      svc.Status
	config      mgr.Config
	imageCheck  scmImageCheck
	imageReason string
	onImageRead func()
	startErr    error
	controlErr  error
	starts      int
	controls    int
	deletes     int
}

func (t *scmTestFirstInstallTarget) Query() (svc.Status, error) { return t.status, nil }

func (t *scmTestFirstInstallTarget) ReadRunningImage(uint32, string) (bool, scmImageCheck, string) {
	if t.onImageRead != nil {
		t.onImageRead()
	}
	return true, t.imageCheck, t.imageReason
}

func (t *scmTestFirstInstallTarget) Start() error {
	t.starts++
	return t.startErr
}

func (t *scmTestFirstInstallTarget) Control(cmd svc.Cmd) (svc.Status, error) {
	t.controls++
	if t.controlErr != nil {
		return t.status, t.controlErr
	}
	t.status = svc.Status{State: svc.Stopped}
	return t.status, nil
}

func (t *scmTestFirstInstallTarget) Config() (mgr.Config, error) { return t.config, nil }

func (t *scmTestFirstInstallTarget) Delete() error {
	t.deletes++
	return nil
}

func scmTestHealthEndpoint(t *testing.T, version string, challenge bool) string {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	assertNoError(t, err, "listen for the real health probe: %v", err)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != serviceProbePath {
			http.NotFound(w, r)
			return
		}
		if challenge {
			w.Header().Set("WWW-Authenticate", serviceProbeAuthChallenge)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"status":"ok","version":%q}`, version)
	}))
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	return strings.TrimPrefix(server.URL, "http://")
}

func scmTestProbeResponse(t *testing.T, status int, body, challenge string) (string, *atomic.Int32) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	assertNoError(t, err, "listen for the readiness response: %v", err)
	var requests atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		if challenge != "" {
			w.Header().Set("WWW-Authenticate", challenge)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = fmt.Fprint(w, body)
	}))
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	return strings.TrimPrefix(server.URL, "http://"), &requests
}

func scmTestClosedProbeAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	assertNoError(t, err, "listen for the closed readiness address: %v", err)
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("close the readiness listener: %v", err)
	}
	return address
}

func scmTestFirstInstallOptions(t *testing.T, listen, expectedVersion string) InstallOptions {
	t.Helper()
	executable, err := os.Executable()
	assertNoError(t, err, "read the actual Windows test executable: %v", err)
	root := t.TempDir()
	return InstallOptions{
		Scope:           ScopeSystem,
		Executable:      executable,
		Listen:          listen,
		ExpectedVersion: expectedVersion,
		Paths: runtimepaths.Paths{
			ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
			StateDir:   filepath.Join(root, "state"),
			LogDir:     filepath.Join(root, "logs"),
		},
	}
}

func scmTestRunningVerificationTarget(t *testing.T, opts InstallOptions, imageReadable bool) (*mgr.Service, *int) {
	t.Helper()
	handle, starts := &mgr.Service{Handle: windows.Handle(123)}, 0
	replaceSCMTestSeam(t, &scmStartService, func(*mgr.Service) error { starts++; return nil })
	replaceSCMTestSeam(t, &scmQueryStatus, func(*mgr.Service) (svc.Status, error) {
		return svc.Status{State: svc.Running, ProcessId: 8142}, nil
	})
	replaceSCMTestSeam(t, &scmQueryProcessImageName, func(uint32) (string, error) {
		if imageReadable {
			return opts.Executable, nil
		}
		return "", windows.ERROR_ACCESS_DENIED
	})
	replaceSCMTestSeam(t, &scmQueryServiceCommandLine, func(windows.Handle) (string, error) {
		return scmCommandLine(opts), nil
	})
	return handle, &starts
}

// TestSCMFirstInstallUsesTheRealImageAccessAndHealthProbePaths drives the
// production first-start adapter with nonmutating SCM API seams and a real
// local HTTP health endpoint. It verifies that an access-denied image lookup
// does not suppress an observable version mismatch.
func TestSCMFirstInstallUsesTheRealImageAccessAndHealthProbePaths(t *testing.T) {
	cases := []struct {
		name       string
		health     string
		expected   string
		challenge  bool
		wantErr    bool
		wantVerify bool
	}{
		{"matching release", "1.4.0", "1.4.0", false, false, true},
		{"observable mismatch", "1.3.0", "1.4.0", false, true, false},
		{"authentication challenge", "", "1.4.0", true, false, false},
		{"dev build", "1.3.0", "dev", false, false, false},
		{"unversioned build", "1.3.0", "", false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			listen := scmTestHealthEndpoint(t, tc.health, tc.challenge)
			opts := scmTestFirstInstallOptions(t, listen, tc.expected)
			commandLine := scmCommandLine(opts)
			expectedConfig := buildSCMConfig(opts)
			scmConfig := expectedConfig
			// mgr.Service.Config decodes an empty dependency MULTI_SZ to an
			// initialized zero-length slice.
			scmConfig.Dependencies = []string{}
			var controls, deletes, starts int
			handle := &mgr.Service{Handle: windows.Handle(123)}
			wrongHandle := false
			checkHandle := func(got *mgr.Service) {
				if got != handle {
					wrongHandle = true
				}
			}
			state := svc.Running
			replaceSCMTestSeam(t, &scmQueryStatus, func(got *mgr.Service) (svc.Status, error) {
				checkHandle(got)
				return svc.Status{State: state, ProcessId: 8142}, nil
			})
			replaceSCMTestSeam(t, &scmQueryServiceConfig, func(got *mgr.Service) (mgr.Config, error) {
				checkHandle(got)
				return scmConfig, nil
			})
			replaceSCMTestSeam(t, &scmStartService, func(got *mgr.Service) error {
				checkHandle(got)
				starts++
				return nil
			})
			replaceSCMTestSeam(t, &scmQueryProcessImageName, func(uint32) (string, error) {
				return "", windows.ERROR_ACCESS_DENIED
			})
			replaceSCMTestSeam(t, &scmQueryServiceCommandLine, func(got windows.Handle) (string, error) {
				if got != handle.Handle {
					wrongHandle = true
				}
				return commandLine, nil
			})
			replaceSCMTestSeam(t, &scmControlService, func(got *mgr.Service, _ svc.Cmd) (svc.Status, error) {
				checkHandle(got)
				controls++
				state = svc.Stopped
				return svc.Status{State: state}, nil
			})
			replaceSCMTestSeam(t, &scmDeleteService, func(got *mgr.Service) error {
				checkHandle(got)
				deletes++
				return nil
			})

			target := scmServiceRecoveryTarget{manager: &scmManager{scope: ScopeSystem}, handle: handle}
			verified, status, err := (&scmManager{scope: ScopeSystem}).startFirstRegistration(
				context.Background(), target, opts, expectedConfig)
			if wrongHandle {
				t.Error("the first-start verification or cleanup did not use the creation handle")
			}
			if tc.wantErr {
				var startErr *InstallStartError
				if !errors.As(err, &startErr) || !errors.Is(err, errServiceVersionMismatch) || !startErr.RegistrationRemoved {
					t.Fatalf("startFirstRegistration() error = %v, want the observed mismatch and owned cleanup", err)
				}
				if controls != 1 || deletes != 1 {
					t.Errorf("mismatch cleanup controls=%d deletes=%d, want one stop and removal on the same handle", controls, deletes)
				}
				return
			}
			assertNoError(t, err, "startFirstRegistration() = %v, want success", err)
			if starts != 1 || controls != 0 || deletes != 0 {
				t.Errorf("successful first launch starts=%d controls=%d deletes=%d", starts, controls, deletes)
			}
			if status.State != StateRunning || !status.Running || verified != tc.wantVerify {
				t.Errorf("status=%+v versionVerified=%v, want running and verified=%v", status, verified, tc.wantVerify)
			}
		})
	}
}

func TestSCMSameOwnedConfigNormalizesEmptyDependencyRepresentation(t *testing.T) {
	expected := buildSCMConfig(scmTestOptions())
	current := expected
	current.Dependencies = []string{}
	if !scmSameOwnedConfig(current, expected) {
		t.Fatal("an empty SCM dependency list differed from the nil creation value")
	}

	current.Dependencies = []string{"Tcpip"}
	if scmSameOwnedConfig(current, expected) {
		t.Error("a changed dependency list was accepted as the owned registration")
	}

	current = expected
	current.DisplayName = "changed registration"
	if scmSameOwnedConfig(current, expected) {
		t.Error("a changed display name was accepted as the owned registration")
	}
}

func TestSCMRunningUpdateRejectsAnObservableMismatchWithoutImageProof(t *testing.T) {
	listen := scmTestHealthEndpoint(t, "1.3.0", false)
	opts := scmTestFirstInstallOptions(t, listen, "1.4.0")
	handle, _ := scmTestRunningVerificationTarget(t, opts, false)

	check, verified, err := (&scmManager{scope: ScopeSystem}).verifyRunningExecutableVersion(
		context.Background(), handle, opts.Executable, listen, opts.ExpectedVersion)
	if err == nil || !errors.Is(err, errServiceVersionMismatch) {
		t.Fatalf("verifyRunningExecutableVersion() = %v, want the actual health-version mismatch", err)
	}
	if check != scmImageUnverified || verified {
		t.Errorf("verification = image %v, version %v, want an inconclusive image and failed version check", check, verified)
	}
}

func TestSCMRunningUpdateKeepsReadinessSeparateFromImageAndVersionProof(t *testing.T) {
	cases := []struct {
		name       string
		health     string
		expected   string
		challenge  bool
		wantVerify bool
	}{
		{"matching release", "1.4.0", "1.4.0", false, true},
		{"auth challenge", "", "1.4.0", true, false},
		{"dev build", "dev", "dev", false, false},
		{"unknown expected version", "1.4.0", "", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			listen := scmTestHealthEndpoint(t, tc.health, tc.challenge)
			opts := scmTestFirstInstallOptions(t, listen, tc.expected)
			handle, starts := scmTestRunningVerificationTarget(t, opts, false)

			image, verified, err := (&scmManager{scope: ScopeSystem}).startAndVerifyVersion(
				context.Background(), handle, opts.Executable, listen, tc.expected)
			assertNoError(t, err, "startAndVerifyVersion() = %v, want readiness success", err)
			if *starts != 1 || image != scmImageUnverified || verified != tc.wantVerify {
				t.Errorf("start=%d image=%v versionVerified=%v, want one start, inaccessible image and versionVerified=%v",
					*starts, image, verified, tc.wantVerify)
			}
		})
	}
}

func TestSCMRunningUpdateDoesNotHideReadinessFailuresBehindAnUnreadableImage(t *testing.T) {
	cases := []struct {
		name          string
		imageReadable bool
		listen        func(*testing.T) (string, *atomic.Int32)
		wantDetail    string
	}{
		{"connection refused", false, func(t *testing.T) (string, *atomic.Int32) {
			return scmTestClosedProbeAddress(t), nil
		}, "connect"},
		{"HTTP 500", false, func(t *testing.T) (string, *atomic.Int32) {
			return scmTestProbeResponse(t, http.StatusInternalServerError, `{"error":"foreign"}`, "")
		}, "500"},
		{"foreign health document", false, func(t *testing.T) (string, *atomic.Int32) {
			return scmTestProbeResponse(t, http.StatusOK, `{"status":"not-hfdesk","version":"1.4.0"}`, "")
		}, `"not-hfdesk"`},
		{"HTTP 500 with image proof", true, func(t *testing.T) (string, *atomic.Int32) {
			return scmTestProbeResponse(t, http.StatusInternalServerError, `{"error":"foreign"}`, "")
		}, "500"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			listen, requests := tc.listen(t)
			opts := scmTestFirstInstallOptions(t, listen, "1.4.0")
			replaceSCMTestSeam(t, &scmVerifyBudget, 250*time.Millisecond)
			handle, _ := scmTestRunningVerificationTarget(t, opts, tc.imageReadable)

			started := time.Now()
			image, verified, err := (&scmManager{scope: ScopeSystem}).startAndVerifyVersion(
				context.Background(), handle, opts.Executable, listen, opts.ExpectedVersion)
			if err == nil || errors.Is(err, errServiceVersionMismatch) {
				t.Fatalf("startAndVerifyVersion() = %v, want the bounded readiness failure", err)
			}
			if !strings.Contains(err.Error(), "within") || !strings.Contains(err.Error(), tc.wantDetail) {
				t.Errorf("startAndVerifyVersion() = %v, want bounded failure mentioning %q", err, tc.wantDetail)
			}
			if time.Since(started) < 100*time.Millisecond {
				t.Errorf("readiness failed after %s, want the bounded retry path", time.Since(started))
			}
			wantImage := scmImageUnverified
			if tc.imageReadable {
				wantImage = scmImageVerified
			}
			if image != wantImage || verified {
				t.Errorf("image=%v versionVerified=%v, want independent image=%v and no version claim", image, verified, wantImage)
			}
			if requests != nil && requests.Load() < 2 {
				t.Errorf("readiness probe requests = %d, want retries within the original verification budget", requests.Load())
			}
		})
	}
}

func TestSCMFirstInstallCleanupDoesNotTouchAChangedRegistrationOrProcess(t *testing.T) {
	executable, err := os.Executable()
	assertNoError(t, err, "read the actual Windows test executable: %v", err)
	other, err := os.Executable()
	assertNoError(t, err, "read the actual Windows test executable: %v", err)
	listen := scmTestHealthEndpoint(t, "1.3.0", false)
	cases := []struct {
		name   string
		change func(*scmTestFirstInstallTarget, string)
	}{
		{"registration command changed", func(target *scmTestFirstInstallTarget, _ string) {
			target.onImageRead = func() {
				opts := scmTestFirstInstallOptions(t, listen, "1.4.0")
				opts.Executable = other + ".other"
				target.config.BinaryPathName = scmCommandLine(opts)
			}
		}},
		{"registration start setting changed", func(target *scmTestFirstInstallTarget, _ string) {
			target.onImageRead = func() { target.config.StartType = mgr.StartManual }
		}},
		{"process changed", func(target *scmTestFirstInstallTarget, _ string) {
			target.onImageRead = func() { target.status.ProcessId = 9917 }
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := scmTestFirstInstallOptions(t, listen, "1.4.0")
			opts.Executable = executable
			commandLine := scmCommandLine(opts)
			target := &scmTestFirstInstallTarget{
				status:     svc.Status{State: svc.Running, ProcessId: 8142},
				config:     mgr.Config{BinaryPathName: commandLine},
				imageCheck: scmImageUnverified,
			}
			tc.change(target, commandLine)
			_, _, err := (&scmManager{scope: ScopeSystem}).startFirstRegistration(
				context.Background(), target, opts, mgr.Config{BinaryPathName: commandLine})
			var startErr *InstallStartError
			if !errors.As(err, &startErr) || startErr.RegistrationRemoved || startErr.CleanupError == nil {
				t.Fatalf("startFirstRegistration() error = %v, want unproven ownership and a kept registration", err)
			}
			if target.controls != 0 || target.deletes != 0 {
				t.Errorf("cleanup controlled %d times and deleted %d registrations after ownership changed", target.controls, target.deletes)
			}
		})
	}
}

// TestSCMConfigExecutableNamesTheProgramOfTheRegistration checks the decision
// that tells a restored registration with a program from one without.
//
// The restart rollback starts the previous program again only when the restored
// registration names it. A command line that cannot be read names no program, so
// the rollback reports the restored registration and the state of the instance
// instead of claiming a start it never made.
func TestSCMConfigExecutableNamesTheProgramOfTheRegistration(t *testing.T) {
	opts := scmTestOptions()
	readable := mgr.Config{BinaryPathName: scmCommandLine(opts)}

	if got := scmConfigExecutable(readable); got != opts.Executable {
		t.Errorf("scmConfigExecutable(a readable command line) = %q, want %q", got, opts.Executable)
	}

	// The same program in another case is still the program of the registration:
	// the comparison of the image of a process is made without regard to the
	// case of the path.
	otherCase := opts
	otherCase.Executable = strings.ToUpper(opts.Executable)
	got := scmConfigExecutable(mgr.Config{BinaryPathName: scmCommandLine(otherCase)})
	if got == "" {
		t.Fatal("scmConfigExecutable(a command line in another case) = \"\", want the program of the registration")
	}
	if !scmSameExecutablePath(got, opts.Executable) {
		t.Errorf("scmConfigExecutable(a command line in another case) = %q, want the same program as %q", got, opts.Executable)
	}

	// A registration whose command line is empty names no program: that is the
	// registration a failed installation can leave behind, and the rollback
	// reports it instead of starting an instance it cannot name.
	if got := scmConfigExecutable(mgr.Config{BinaryPathName: ""}); got != "" {
		t.Errorf("scmConfigExecutable(an empty command line) = %q, want no program", got)
	}
}
