// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package service

import (
	"context"
	"encoding/binary"
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
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/bashrusakh/hfdesk/internal/pe"
	"github.com/bashrusakh/hfdesk/internal/runtimepaths"
	"github.com/bashrusakh/hfdesk/internal/server"
)

// secretSentinel is an obviously secret value. It is put into every field
// of the installation options that could carry a credential, so the tests
// can prove that no credential reaches the registration of the service.
const secretSentinel = "hfdesk-secret-sentinel"

// scmTestOptions returns the options of a registration whose executable
// does not exist. The pure helpers of the manager never touch the file
// system, so the options do not need one.
func scmTestOptions() InstallOptions {
	root := filepath.Join("C:\\ProgramData", "HFDesk")
	return InstallOptions{
		Scope:      ScopeSystem,
		Executable: filepath.Join(root, "hfdesk.exe"),
		Listen:     "127.0.0.1:8080",
		Paths: runtimepaths.Paths{
			ConfigFile: filepath.Join(root, "hfdesk.json"),
			StateDir:   root,
			LogDir:     filepath.Join(root, "logs"),
		},
		CacheDir: filepath.Join(root, "cache"),
		HubDir:   filepath.Join(root, "cache", "hub"),
		LocalDir: filepath.Join(root, "models"),
	}
}

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
				if err != nil {
					t.Fatalf("scmDisabledRunningRefusal(%v, %v, %d) = %v, want nil", tc.present, running, tc.startType, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("scmDisabledRunningRefusal(%v, %v, %d) = nil, want the refusal of the installation", tc.present, running, tc.startType)
			}
			for _, want := range []string{
				"is running, but its start type is disabled",
				"Windows will not start it again after the installation stops it",
				"Set the start type to manual or automatic",
				`"sc config hfdesk start= demand"`,
				"run the installation again",
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal %q does not mention %q", err, want)
				}
			}
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
			for _, want := range []string{
				fmt.Sprintf("runs as \"%s\"", tc.shown),
				"an HFDesk update keeps the supported identity " + scmLocalServiceAccount,
				"nothing was changed and the service was not stopped",
				`sc config hfdesk obj= "NT AUTHORITY\LocalService" password= ""`,
				"run the installation again",
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal %q does not mention %q", err, want)
				}
			}
		})
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

func TestSCMStatusUsesAcceptedFlagSyntaxAndLastValue(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable() = %v", err)
	}
	root := t.TempDir()
	configFile := filepath.Join(root, "new", "hfdesk.json")
	stateDir := filepath.Join(root, "new", "state")
	logDir := filepath.Join(root, "new", "logs")
	cacheDir := filepath.Join(root, "new", "cache")
	hubDir := filepath.Join(root, "new", "hub")
	localDir := filepath.Join(root, "new", "models")
	args := []string{
		"-config=C:\\old\\hfdesk.json", "--config", configFile,
		"-state-dir", `C:\old\state`, "--state-dir=" + stateDir,
		"-log-dir=" + logDir,
		"-listen=127.0.0.1:8181", "--listen", "127.0.0.1:9191",
		"-cache-dir=C:\\old\\cache", "--cache-dir=" + cacheDir,
		"-hub-dir=C:\\old\\hub", "--hub-dir=" + hubDir,
		"-local-dir=C:\\old\\models", "--local-dir=" + localDir,
	}
	commandLine := syscall.EscapeArg(executable)
	for _, arg := range args {
		commandLine += " " + syscall.EscapeArg(arg)
	}
	facts := scmRegistrationFacts{
		present:     true,
		state:       svc.Running,
		commandLine: commandLine,
	}
	status, err := scmStatusFrom(ScopeSystem, facts)
	if err != nil {
		t.Fatalf("scmStatusFrom() = %v", err)
	}
	if status.Executable != executable || status.Listen != "127.0.0.1:9191" || status.Paths != (runtimepaths.Paths{ConfigFile: configFile, StateDir: stateDir, LogDir: logDir}) || status.CacheDir != cacheDir {
		t.Errorf("status = %+v, want the last values of the registered command", status)
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
	if err != nil {
		t.Fatalf("DecomposeCommandLine(%q) = %v, want nil", commandLine, err)
	}
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

func TestParseSCMCommandLineReadsTheRegisteredExecutableAndAddress(t *testing.T) {
	opts := scmTestOptions()
	// A path with a space in it exercises the escaping of the command
	// line.
	opts.Executable = filepath.Join("C:\\Program Files", "HFDesk", "hfdesk.exe")

	registration, err := parseSCMCommandLine(scmCommandLine(opts))
	if err != nil {
		t.Fatalf("parseSCMCommandLine() = %v, want nil", err)
	}
	if registration.executable != opts.Executable {
		t.Errorf("the registered executable = %q, want %q", registration.executable, opts.Executable)
	}
	if got, want := registration.listen(), "127.0.0.1:8080"; got != want {
		t.Errorf("the registered listen address = %q, want %q", got, want)
	}
	if got := registration.paths(); got != opts.Paths {
		t.Errorf("the registered paths = %+v, want %+v", got, opts.Paths)
	}
}

func TestSCMStateMapsEverySupervisorState(t *testing.T) {
	cases := []struct {
		name  string
		state svc.State
		want  State
	}{
		{"running", svc.Running, StateRunning},
		{"stopped", svc.Stopped, StateStopped},
		{"start pending", svc.StartPending, StateStarting},
		{"stop pending", svc.StopPending, StateStopping},
		{"paused", svc.Paused, StateUnknown},
		{"pause pending", svc.PausePending, StateUnknown},
		{"continue pending", svc.ContinuePending, StateUnknown},
		{"unknown", svc.State(4711), StateUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := scmState(tc.state); got != tc.want {
				t.Errorf("scmState(%v) = %q, want %q", tc.state, got, tc.want)
			}
		})
	}
}

func TestSCMStateSDDLGrantsOnlyTheMachineAccounts(t *testing.T) {
	sddl := scmStateSDDL()

	// SYSTEM, the administrators of the machine and the account the
	// instance runs as.
	for _, want := range []string{";;;SY)", ";;;BA)", ";;;LS)"} {
		if !strings.Contains(sddl, want) {
			t.Errorf("the descriptor %q does not grant %q", sddl, want)
		}
	}
	// Nobody else.
	for _, forbidden := range []string{"WD", "Everyone", "Users", "AU;", ";;;AU)", "IU)", "BU)", "Authenticated"} {
		if strings.Contains(sddl, forbidden) {
			t.Errorf("the descriptor %q grants access to %q", sddl, forbidden)
		}
	}
	// The inheritance of the parent directory is blocked.
	if !strings.HasPrefix(sddl, "D:P") {
		t.Errorf("the descriptor %q does not block the inheritance", sddl)
	}
}

func TestNewPlatformManagerRejectsTheUserScope(t *testing.T) {
	manager, err := newPlatformManager(ScopeUser)
	if err == nil {
		t.Fatalf("newPlatformManager(ScopeUser) = %v, want an error", manager)
	}
	if !strings.Contains(err.Error(), "machine-wide") {
		t.Errorf("newPlatformManager(ScopeUser) = %v, want an error that names the reason", err)
	}
	if manager != nil {
		t.Errorf("newPlatformManager(ScopeUser) = %v, want no manager", manager)
	}

	manager, err = newPlatformManager(ScopeSystem)
	if err != nil {
		t.Fatalf("newPlatformManager(ScopeSystem) = %v, want nil", err)
	}
	if _, ok := manager.(*scmManager); !ok {
		t.Errorf("newPlatformManager(ScopeSystem) = %T, want a *scmManager", manager)
	}
}

func TestRequireElevatedNamesTheMissingPrivilege(t *testing.T) {
	err := requireElevated("an installation")
	if isElevatedProcess() {
		if err != nil {
			t.Errorf("requireElevated() = %v, want nil for an elevated process", err)
		}
		return
	}
	if err == nil {
		t.Fatal("requireElevated() = nil, want an error for a process that is not elevated")
	}
	if !strings.Contains(err.Error(), "elevated") {
		t.Errorf("requireElevated() = %v, want an error that names the elevated shell", err)
	}
}

// TestSCMClassifyRegistrationError checks that the outcome of the
// existence check tells the three cases apart: the registration is not
// there, the registration is there, and the caller may not look at it.
func TestSCMClassifyRegistrationError(t *testing.T) {
	// A registration that is there.
	state, err := scmClassifyRegistrationError(nil)
	if state != scmRegistrationPresent {
		t.Errorf("scmClassifyRegistrationError(nil) = %v, want %v", state, scmRegistrationPresent)
	}
	if err != nil {
		t.Errorf("scmClassifyRegistrationError(nil) = %v, want no error", err)
	}

	// A registration that is not there is not an error: it is the state
	// the caller asked about.
	state, err = scmClassifyRegistrationError(windows.ERROR_SERVICE_DOES_NOT_EXIST)
	if state != scmRegistrationAbsent {
		t.Errorf("scmClassifyRegistrationError(ERROR_SERVICE_DOES_NOT_EXIST) = %v, want %v", state, scmRegistrationAbsent)
	}
	if err != nil {
		t.Errorf("scmClassifyRegistrationError(ERROR_SERVICE_DOES_NOT_EXIST) = %v, want no error", err)
	}

	// A registration the caller may not look at is not an absent
	// registration, and the raw access denied error is not reported: the
	// elevation requirement is the truthful reason.
	state, err = scmClassifyRegistrationError(windows.ERROR_ACCESS_DENIED)
	if state != scmRegistrationUnreadable {
		t.Errorf("scmClassifyRegistrationError(ERROR_ACCESS_DENIED) = %v, want %v", state, scmRegistrationUnreadable)
	}
	if err != nil {
		t.Errorf("scmClassifyRegistrationError(ERROR_ACCESS_DENIED) = %v, want no error", err)
	}

	// A failure that is not about the registration is reported as it is.
	other := errors.New("the service control manager is gone")
	state, err = scmClassifyRegistrationError(other)
	if state != scmRegistrationUnreadable {
		t.Errorf("scmClassifyRegistrationError(%v) = %v, want %v", other, state, scmRegistrationUnreadable)
	}
	if !errors.Is(err, other) {
		t.Errorf("scmClassifyRegistrationError(%v) = %v, want the error itself", other, err)
	}
}

// TestSCMUninstallActionFor checks the decision of the uninstallation: an
// absent registration needs nothing, a registered one has to be removed,
// and a registration the caller may not look at needs an elevated shell.
func TestSCMUninstallActionFor(t *testing.T) {
	cases := []struct {
		name  string
		state scmRegistrationState
		want  scmUninstallAction
	}{
		{"absent", scmRegistrationAbsent, scmUninstallNothing},
		{"present", scmRegistrationPresent, scmUninstallRemove},
		{"unreadable", scmRegistrationUnreadable, scmUninstallElevate},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := scmUninstallActionFor(tc.state); got != tc.want {
				t.Errorf("scmUninstallActionFor(%v) = %v, want %v", tc.state, got, tc.want)
			}
		})
	}
}

// TestSCMUninstallOfAnAbsentRegistrationIsACleanSuccess checks that the
// uninstallation of an instance that is not registered is a clean success
// in every shell.
//
// The decision is driven from both sides: the state of the registration
// and the state of the token of the process are parameters, so the test
// needs neither a registration nor an elevated shell of its own. The real
// command is checked as well, against the service control manager of the
// machine.
func TestSCMUninstallOfAnAbsentRegistrationIsACleanSuccess(t *testing.T) {
	manager := &scmManager{scope: ScopeSystem}
	ctx := context.Background()

	// The decision does not ask for an elevated shell: a registration
	// that is not there is uninstalled already.
	for _, elevated := range []bool{false, true} {
		if err := manager.uninstall(ctx, scmRegistrationAbsent, elevated); err != nil {
			t.Errorf("uninstall(absent, elevated=%v) = %v, want nil", elevated, err)
		}
	}

	// The real command against the service control manager of the
	// machine.
	state, err := scmRegistrationStateOf()
	if err != nil {
		t.Skipf("the service control manager is not available: %v", err)
	}
	if state != scmRegistrationAbsent {
		t.Skipf("%s is registered, the test would remove a real installation", ServiceName)
	}
	if err := manager.Uninstall(ctx); err != nil {
		t.Errorf("Uninstall() = %v, want nil for an absent registration", err)
	}
}

// TestSCMUninstallOfARegisteredInstanceNeedsElevation checks that the
// removal of a registration asks for an elevated shell.
//
// The state of the registration is a parameter of the decision, so the
// test never opens the registration of the instance: the elevation
// requirement is answered before the service control manager is asked for
// anything.
func TestSCMUninstallOfARegisteredInstanceNeedsElevation(t *testing.T) {
	manager := &scmManager{scope: ScopeSystem}
	err := manager.uninstall(context.Background(), scmRegistrationPresent, false)
	if err == nil {
		t.Fatal("uninstall(present) = nil, want an error that names the elevated shell")
	}
	if !strings.Contains(err.Error(), "elevated") {
		t.Errorf("uninstall(present) = %v, want an error that names the elevated shell", err)
	}
	if !strings.Contains(err.Error(), "administrator") {
		t.Errorf("uninstall(present) = %v, want an error that tells the user what to do", err)
	}
}

// TestSCMUninstallOfAnUnreadableRegistrationNeedsElevation checks that a
// registration the caller may not look at is reported as the elevation
// requirement it is, and not as the raw access denied error of the
// platform.
func TestSCMUninstallOfAnUnreadableRegistrationNeedsElevation(t *testing.T) {
	manager := &scmManager{scope: ScopeSystem}
	for _, elevated := range []bool{false, true} {
		err := manager.uninstall(context.Background(), scmRegistrationUnreadable, elevated)
		if err == nil {
			t.Fatalf("uninstall(unreadable, elevated=%v) = nil, want an error", elevated)
		}
		if !strings.Contains(err.Error(), "elevated") {
			t.Errorf("uninstall(unreadable, elevated=%v) = %v, want an error that names the elevated shell", elevated, err)
		}
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			t.Errorf("uninstall(unreadable, elevated=%v) = %v, want the elevation requirement and not the raw access denied error", elevated, err)
		}
	}
}

// TestSCMRefusedReportsTheElevationRequirement checks that a refusal of the
// service control manager because the shell has no rights is reported as
// the elevation requirement it is, and not as the raw access denied error
// of the platform.
func TestSCMRefusedReportsTheElevationRequirement(t *testing.T) {
	for _, action := range []string{"a start", "a stop"} {
		err := scmRefused(action, fmt.Errorf("service: start %s: %w", ServiceName, windows.ERROR_ACCESS_DENIED))
		if err == nil {
			t.Fatalf("scmRefused(%q, access denied) = nil, want an error", action)
		}
		if !strings.Contains(err.Error(), "elevated") {
			t.Errorf("scmRefused(%q, access denied) = %v, want an error that names the elevated shell", action, err)
		}
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			t.Errorf("scmRefused(%q, access denied) = %v, want the elevation requirement and not the raw access denied error", action, err)
		}
	}

	// A failure that is not about the rights of the shell is reported as
	// it is.
	other := errors.New("the service control manager is gone")
	if got := scmRefused("a start", other); !errors.Is(got, other) {
		t.Errorf("scmRefused(%q, %v) = %v, want the error itself", "a start", other, got)
	}
	if got := scmRefused("a start", nil); got != nil {
		t.Errorf("scmRefused(%q, nil) = %v, want nil", "a start", got)
	}
}

// TestSCMRegistrationStateOfFindsAnAbsentRegistration checks the existence
// check against the service control manager of the machine: a name that
// cannot be registered is reported as absent.
//
// The check reads the registration with the right to query its state only,
// so it works in a normal shell as well.
func TestSCMRegistrationStateOfFindsAnAbsentRegistration(t *testing.T) {
	name := fmt.Sprintf("HfdeskTestAbsentRegistration%d", time.Now().UnixNano())
	state, err := scmRegistrationStateOfName(name)
	if err != nil {
		t.Skipf("the service control manager is not available: %v", err)
	}
	if state == scmRegistrationUnreadable {
		t.Skip("the service control manager cannot be read from this shell")
	}
	if state != scmRegistrationAbsent {
		t.Errorf("scmRegistrationStateOfName(%q) = %v, want %v", name, state, scmRegistrationAbsent)
	}
}

// TestSCMRegistrationStateOfFindsARegisteredService checks the existence
// check against the service control manager of the machine: a service of
// the operating system is reported as registered.
//
// The test needs a service that is registered on the machine. The
// candidates are the services every installation of the operating system
// carries, so the test is skipped when the machine carries none of them.
func TestSCMRegistrationStateOfFindsARegisteredService(t *testing.T) {
	for _, candidate := range []string{"EventLog", "RpcSs", "Dhcp", "W32Time", "NlaSvc", "Schedule", "Power"} {
		state, err := scmRegistrationStateOfName(candidate)
		if err != nil {
			t.Skipf("the service control manager is not available: %v", err)
		}
		switch state {
		case scmRegistrationPresent:
			return
		case scmRegistrationAbsent:
			continue
		default:
			t.Skipf("the registration of %s cannot be read from this shell", candidate)
		}
	}
	t.Skip("the machine carries none of the services of the operating system")
}

// TestSCMStartAndStopOfAnAbsentRegistration checks the lifecycle commands
// against an instance that is not registered: the stop is a clean success
// and the start reports that the instance is not installed.
//
// The test is skipped when the instance is registered, so it never touches
// a real installation.
func TestSCMStartAndStopOfAnAbsentRegistration(t *testing.T) {
	state, err := scmRegistrationStateOf()
	if err != nil {
		t.Skipf("the service control manager is not available: %v", err)
	}
	if state != scmRegistrationAbsent {
		t.Skipf("%s is registered, the test would touch a real installation", ServiceName)
	}

	manager := &scmManager{scope: ScopeSystem}
	ctx := context.Background()

	// An instance that does not run is stopped already.
	if err := manager.Stop(ctx); err != nil {
		t.Errorf("Stop() = %v, want nil for an instance that is not registered", err)
	}
	// An instance that is not registered cannot be started, and the
	// reason is reported in every shell.
	err = manager.Start(ctx)
	if err == nil {
		t.Fatal("Start() = nil, want an error for an instance that is not registered")
	}
	if !strings.Contains(err.Error(), "not installed") {
		t.Errorf("Start() = %v, want an error that names the missing registration", err)
	}
}

// TestSCMStopWaitCoversTheShutdownOfTheServer checks that the wait of the
// manager for the stopped instance covers the graceful shutdown of the
// server.
//
// The server publishes the budget of its own orderly shutdown: the drain of
// the HTTP server and the close of the job manager, each with its own budget.
// The manager has to wait at least the total of those budgets, or it reports
// a failed stop while the instance is still saving its state.
//
// The wait is the shared budget of the service layer and not a number of its
// own: a second copy of the same decision would drift below the budget of
// the server.
func TestSCMStopWaitCoversTheShutdownOfTheServer(t *testing.T) {
	if ShutdownBudget < server.GracefulShutdownTimeout {
		t.Errorf("the shared budget of the service layer = %s, want at least %s", ShutdownBudget, server.GracefulShutdownTimeout)
	}
	if scmStopTimeout < server.GracefulShutdownTimeout {
		t.Errorf("the stop wait of the manager = %s, want at least %s: the wait of the manager has to cover the graceful shutdown of the server", scmStopTimeout, server.GracefulShutdownTimeout)
	}
	if scmStopTimeout != ShutdownBudget {
		t.Errorf("the stop wait of the manager = %s, want the shared budget %s", scmStopTimeout, ShutdownBudget)
	}
}

// scmTestServiceControl is a registration of the service control manager
// that a test drives.
//
// The registration refuses the stop control while the instance is still
// starting, the way the platform does: the control of an instance that did
// not report the running state yet is refused with
// ERROR_SERVICE_CANNOT_ACCEPT_CTRL.
type scmTestServiceControl struct {
	// state is the state the registration reports.
	state svc.State
	// refusals is the number of times the registration refuses the stop
	// control.
	refusals int
	// controls counts the stop controls the registration received.
	controls int
	// queryErr is the failure of every read of the state.
	queryErr error
	// queryErrs are the failures of the next reads, in order; a read after
	// them answers. They take precedence over queryErr.
	queryErrs []error
	// queryAfterControlErr is the failure of a read after a stop control was
	// delivered.
	queryAfterControlErr error
	// controlErr is the failure of the stop control.
	controlErr error
	// pendingAfterControl reports whether an accepted control leaves the
	// instance in the stopping state instead of stopping it.
	pendingAfterControl bool
	// stateAfterControl is the state an accepted control leaves, when set.
	stateAfterControl svc.State
	// onControl is called after a control request was sent.
	onControl func()
}

// Query returns the state of the registration.
func (c *scmTestServiceControl) Query() (svc.Status, error) {
	if len(c.queryErrs) > 0 {
		err := c.queryErrs[0]
		c.queryErrs = c.queryErrs[1:]
		if err != nil {
			return svc.Status{State: c.state}, err
		}
	}
	if c.queryErr != nil {
		return svc.Status{State: c.state}, c.queryErr
	}
	if c.controls > 0 && c.queryAfterControlErr != nil {
		return svc.Status{State: c.state}, c.queryAfterControlErr
	}
	return svc.Status{State: c.state}, nil
}

// Control sends the stop control to the registration.
//
// The control is refused while the registration has refusals left: the
// instance is still starting then. The control that is accepted stops the
// instance, unless the test asks for an instance that stays in the stopping
// state.
func (c *scmTestServiceControl) Control(cmd svc.Cmd) (svc.Status, error) {
	c.controls++
	if c.onControl != nil {
		c.onControl()
	}
	if c.controlErr != nil {
		return svc.Status{State: c.state}, c.controlErr
	}
	if c.refusals > 0 {
		c.refusals--
		return svc.Status{State: c.state}, windows.ERROR_SERVICE_CANNOT_ACCEPT_CTRL
	}
	if c.stateAfterControl != 0 {
		c.state = c.stateAfterControl
		return svc.Status{State: c.state}, nil
	}
	if c.pendingAfterControl {
		c.state = svc.StopPending
		return svc.Status{State: c.state}, nil
	}
	c.state = svc.Stopped
	return svc.Status{State: svc.Stopped}, nil
}

// TestSCMStopRetriesTheControlOfAnInstanceThatIsStillStarting checks that a
// stop the supervisor refuses because the instance is still starting is
// delivered again, and not given up on.
//
// The service control manager refuses the control of an instance that did not
// report the running state yet (ERROR_SERVICE_CANNOT_ACCEPT_CTRL). The
// refusal is not the end of the stop: the control is delivered again inside
// the wait, until the instance stops or the budget of the wait is over.
func TestSCMStopRetriesTheControlOfAnInstanceThatIsStillStarting(t *testing.T) {
	handle := &scmTestServiceControl{state: svc.StartPending, refusals: 2}

	manager := &scmManager{scope: ScopeSystem}
	if _, err := manager.stopService(context.Background(), handle); err != nil {
		t.Fatalf("stopService() = %v, want nil: the refusal of a starting instance is not the end of the stop", err)
	}
	if handle.controls < 3 {
		t.Errorf("the stop control was delivered %d times, want it delivered again after every refusal", handle.controls)
	}
	if handle.state != svc.Stopped {
		t.Errorf("the state of the instance = %v, want %v", handle.state, svc.Stopped)
	}
}

// shortenSCMStopBudget shortens the wait of a stop that never settles.
func shortenSCMStopBudget(t *testing.T, budget time.Duration) {
	t.Helper()
	previous := scmStopBudget
	scmStopBudget = budget
	t.Cleanup(func() { scmStopBudget = previous })
}

// shortenSCMVerifyBudget shortens the wait of a verification that never
// settles.
func shortenSCMVerifyBudget(t *testing.T, budget time.Duration) {
	t.Helper()
	previous := scmVerifyBudget
	scmVerifyBudget = budget
	t.Cleanup(func() { scmVerifyBudget = previous })
}

// scmTestRunningPrevious returns the registration an installation replaces: an
// instance that ran before the installation and carries the previous program.
func scmTestRunningPrevious() previousSCMRegistration {
	opts := scmTestOptions()
	return previousSCMRegistration{
		present:   true,
		running:   true,
		processID: 4710,
		config:    mgr.Config{BinaryPathName: scmCommandLine(opts)},
		options:   opts,
	}
}

// scmTestRecoveryTarget is a registration a failed installation restores. It
// implements the recovery target without a real registration, which a test
// cannot create without an elevated shell.
type scmTestRecoveryTarget struct {
	scmTestServiceControl
	// config is the configuration the registration holds.
	config mgr.Config
	// writes are the configurations written back, in order.
	writes    []mgr.Config
	writeErrs []error
	// deletes is the number of removals.
	deletes   int
	deleteErr error
	// starts is the number of starts of the previous program.
	starts int
	// started is the program the rollback started.
	started string
	// excluded are the processes the start verification had to exclude.
	excluded []uint32
}

// UpdateConfig records the configuration written back.
func (t *scmTestRecoveryTarget) UpdateConfig(config mgr.Config) error {
	t.writes = append(t.writes, config)
	var err error
	if len(t.writeErrs) > 0 {
		err = t.writeErrs[0]
		t.writeErrs = t.writeErrs[1:]
	}
	if err == nil {
		t.config = config
	}
	return err
}

// Config returns the configuration the registration holds.
func (t *scmTestRecoveryTarget) Config() (mgr.Config, error) { return t.config, nil }

// Delete records the removal of the registration.
func (t *scmTestRecoveryTarget) Delete() error {
	t.deletes++
	return t.deleteErr
}

// StartAndVerify records the start of the previous program.
func (t *scmTestRecoveryTarget) StartAndVerify(_ context.Context, executable string, exclude ...uint32) (scmImageCheck, error) {
	t.starts++
	t.started = executable
	t.excluded = append([]uint32(nil), exclude...)
	return scmImageVerified, nil
}

// TestSCMStopThatDoesNotSettleIsNotReportedAsRunning checks that an accepted stop
// is never reported as a running instance: the delivered control decides the
// outcome, not a later read of the state.
func TestSCMStopThatDoesNotSettleIsNotReportedAsRunning(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(t *testing.T, handle *scmTestServiceControl) context.Context
		wantErr string
	}{
		{"cancellation", func(t *testing.T, handle *scmTestServiceControl) context.Context {
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			handle.onControl = cancel
			return ctx
		}, ""},
		{"query error", func(t *testing.T, handle *scmTestServiceControl) context.Context {
			handle.queryAfterControlErr = errors.New("the service control manager did not answer")
			return context.Background()
		}, "did not answer"},
		{"timeout", func(t *testing.T, handle *scmTestServiceControl) context.Context {
			shortenSCMStopBudget(t, 10*time.Millisecond)
			return context.Background()
		}, "did not stop within"},
		// The accepted control decides the classification, even when a later
		// read still reports the running state.
		{"running read after the control", func(t *testing.T, handle *scmTestServiceControl) context.Context {
			shortenSCMStopBudget(t, 10*time.Millisecond)
			handle.stateAfterControl = svc.Running
			return context.Background()
		}, "did not stop within"},
		{"already stopping", func(t *testing.T, handle *scmTestServiceControl) context.Context {
			shortenSCMStopBudget(t, 10*time.Millisecond)
			handle.state = svc.StopPending
			handle.refusals = 100
			return context.Background()
		}, "did not stop within"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handle := &scmTestServiceControl{state: svc.Running, pendingAfterControl: true}
			outcome, err := (&scmManager{scope: ScopeSystem}).stopService(tc.prepare(t, handle), handle)
			if outcome != scmStopUnsettled {
				t.Errorf("the outcome = %d, want the unsettled stop", outcome)
			}
			switch {
			case err == nil:
				t.Fatal("stopService() = nil, want a failed stop")
			case tc.wantErr == "" && !errors.Is(err, context.Canceled):
				t.Errorf("stopService() = %v, want the cancellation of the wait", err)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Errorf("stopService() = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

// TestSCMRollbackSettlesTheStopItInherits checks the rollback of a failed stop: a
// control that was never delivered restores the registration and starts the
// previous instance again only when the state proved it stopped, while a stop
// that may still put the instance down keeps the registration and starts
// nothing.
func TestSCMRollbackSettlesTheStopItInherits(t *testing.T) {
	shortenSCMStopBudget(t, 10*time.Millisecond)
	readErr := errors.New("the service control manager did not answer")
	cases := []struct {
		name         string
		mode         scmRollbackMode
		controlErr   error
		queryErr     error
		queryErrs    []error
		state        svc.State
		settle       bool
		wantOutcome  scmStopOutcome
		wantWrites   int
		wantStarts   int
		wantDirsSafe bool
		wantReports  []string
	}{
		{
			name:         "a control that was never delivered",
			controlErr:   errors.New("the service control manager refused the stop"),
			wantOutcome:  scmStopNotDelivered,
			wantWrites:   1,
			wantDirsSafe: true,
			wantReports:  []string{"the configuration of the previous registration was restored", "the instance still runs", "no instance was started"},
		},
		{
			name:         "the previous instance then stops",
			settle:       true,
			wantOutcome:  scmStopUnsettled,
			wantWrites:   1,
			wantStarts:   1,
			wantDirsSafe: true,
			wantReports:  []string{"the previous instance was proven stopped", scmRollbackSuccessClaim},
		},
		{
			name:        "a stop that stays pending",
			wantOutcome: scmStopUnsettled,
			wantReports: []string{"was not proven to be stopped", "no instance was started", "hfdesk service status", "did not stop within"},
		},
		{
			name:         "a state that cannot be read",
			queryErr:     readErr,
			wantOutcome:  scmStopNotDelivered,
			wantWrites:   1,
			wantDirsSafe: true,
			wantReports:  []string{"the configuration of the previous registration was restored", "the running state of the instance could not be confirmed", "no instance was started", "hfdesk service status", "did not answer"},
		},
		{
			name:         "a state that cannot be read first and then reads as running",
			queryErrs:    []error{readErr},
			wantOutcome:  scmStopNotDelivered,
			wantWrites:   1,
			wantDirsSafe: true,
			wantReports:  []string{"the configuration of the previous registration was restored", "the instance still runs", "no instance was started"},
		},
		{
			name:         "a state that cannot be read first and then reads as stopped",
			queryErrs:    []error{readErr},
			state:        svc.Stopped,
			wantOutcome:  scmStopNotDelivered,
			wantWrites:   1,
			wantStarts:   1,
			wantDirsSafe: true,
			wantReports:  []string{"the previous instance was proven stopped", "the configuration of the previous registration was restored", scmRollbackSuccessClaim},
		},
		{
			name:         "a state that cannot be read first and then stays stopping",
			queryErrs:    []error{readErr},
			state:        svc.StopPending,
			wantOutcome:  scmStopNotDelivered,
			wantWrites:   1,
			wantDirsSafe: true,
			wantReports:  []string{"the configuration of the previous registration was restored", "the running state of the instance could not be confirmed", "no instance was started", "hfdesk service status"},
		},
		{
			name:        "the instance of a failed start",
			mode:        scmRollbackRestart,
			wantOutcome: scmStopUnsettled,
			wantReports: []string{"the instance it started was not proven to be stopped"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target := &scmTestRecoveryTarget{}
			target.state = svc.Running
			target.pendingAfterControl = true
			target.controlErr = tc.controlErr
			target.queryErr = tc.queryErr
			target.queryErrs = tc.queryErrs
			if tc.state != 0 {
				target.state = tc.state
			}
			manager := &scmManager{scope: ScopeSystem}
			previous := scmTestRunningPrevious()
			failure := errors.New("the stop did not settle")

			outcome, err := manager.stopService(context.Background(), target)
			if err == nil {
				t.Fatal("stopService() = nil, want a failed stop")
			}
			if outcome != tc.wantOutcome {
				t.Errorf("stopService() outcome = %d, want %d", outcome, tc.wantOutcome)
			}
			mode := tc.mode
			if mode == scmRollbackRegistration {
				mode = scmStopRollbackMode(outcome)
			}
			if tc.settle {
				// The accepted control reaches the stopped state after the
				// failed call returned, and the supervisor answers again.
				target.state = svc.Stopped
				target.queryAfterControlErr = nil
			}
			controlsBeforeRollback := target.controls
			dirsSafe, rollbackErr := manager.rollbackInstallation(context.Background(), target, previous, mode, failure)
			if rollbackErr == nil || !errors.Is(rollbackErr, failure) {
				t.Fatalf("rollbackInstallation() = %v, want the original failure wrapped", rollbackErr)
			}
			if dirsSafe != tc.wantDirsSafe {
				t.Errorf("rollbackInstallation() directory cleanup safe = %v, want %v", dirsSafe, tc.wantDirsSafe)
			}
			if outcome == scmStopNotDelivered && target.controls != controlsBeforeRollback {
				t.Errorf("the rollback delivered %d stop controls after a stop that was never delivered, want none", target.controls-controlsBeforeRollback)
			}
			if len(target.writes) != tc.wantWrites {
				t.Errorf("the rollback wrote %d configurations, want %d", len(target.writes), tc.wantWrites)
			}
			for _, write := range target.writes {
				if !scmSameRegistration(write.BinaryPathName, previous.config.BinaryPathName) {
					t.Errorf("the rollback wrote %q, want the previous command line %q", write.BinaryPathName, previous.config.BinaryPathName)
				}
			}
			if target.starts != tc.wantStarts || target.deletes != 0 {
				t.Errorf("the rollback started %d instances and removed %d registrations, want %d starts and no removal",
					target.starts, target.deletes, tc.wantStarts)
			}
			if tc.wantStarts == 1 && (!slices.Contains(target.excluded, previous.processID) ||
				target.started != scmTestOptions().Executable) {
				t.Errorf("the rollback started %q excluding %v, want the previous program without the process %d",
					target.started, target.excluded, previous.processID)
			}
			for _, want := range tc.wantReports {
				if !strings.Contains(rollbackErr.Error(), want) {
					t.Errorf("the report %q does not mention %q", rollbackErr, want)
				}
			}
		})
	}
}

// TestSCMRollbackOfAFailureOutsideTheInstanceRestoresTheRegistration checks that
// a failure that never touched the running instance puts the previous
// configuration back and leaves the instance alone.
func TestSCMRollbackOfAFailureOutsideTheInstanceRestoresTheRegistration(t *testing.T) {
	target := &scmTestRecoveryTarget{}
	target.state = svc.Running
	previous := scmTestRunningPrevious()
	failure := errors.New("the failure recovery could not be configured")

	dirsSafe, err := (&scmManager{scope: ScopeSystem}).rollbackInstallation(context.Background(), target, previous, scmRollbackRegistration, failure)
	if err == nil || !errors.Is(err, failure) {
		t.Fatalf("rollbackInstallation() = %v, want the original failure wrapped", err)
	}
	if !dirsSafe {
		t.Error("rollbackInstallation() says directory cleanup is unsafe after a verified configuration restore")
	}
	if len(target.writes) != 1 || target.starts != 0 || target.controls != 0 || target.deletes != 0 {
		t.Errorf("the rollback wrote %d configurations, started %d instances, delivered %d controls and removed %d registrations, want one write and no other change",
			len(target.writes), target.starts, target.controls, target.deletes)
	}
	for _, want := range []string{"the configuration of the previous registration was restored", "the running instance was not restarted"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the report %q does not mention %q", err, want)
		}
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
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("Mkdir(%q) = %v", dir, err)
	}
	cleanupCreatedSCMInstallDirs([]string{dir}, scmRegisterResult{createdDirsSafeToRemove: false})
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("an unproven registration outcome removed %q: %v", dir, err)
	}
	cleanupCreatedSCMInstallDirs([]string{dir}, scmRegisterResult{createdDirsSafeToRemove: true})
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a proven-safe cleanup left %q in place: %v", dir, err)
	}
}

// TestSCMLifecycle drives the registration of a service against the real
// service control manager.
//
// The test needs an elevated shell: the service control manager only accepts
// a machine-wide registration from an administrator. It is skipped without
// one.
func TestSCMLifecycle(t *testing.T) {
	if !isElevatedProcess() {
		t.Skip("the service control manager needs an elevated shell, run the test as an administrator")
	}

	// A registration that exists already belongs to the machine and not
	// to this test: it is never removed here.
	manager, err := mgr.Connect()
	if err != nil {
		t.Skipf("the service control manager is not available: %v", err)
	}
	existing, err := manager.OpenService(ServiceName)
	if err == nil {
		existing.Close()
		manager.Disconnect()
		t.Skipf("%s is registered already, the test would remove a real installation", ServiceName)
	}
	manager.Disconnect()

	root := t.TempDir()
	executable := filepath.Join("C:\\Windows", "System32", "cmd.exe")
	if _, err := os.Stat(executable); err != nil {
		t.Skipf("the test program %s is not available: %v", executable, err)
	}
	opts := InstallOptions{
		Scope:      ScopeSystem,
		Executable: executable,
		Listen:     "127.0.0.1:8080",
		Paths: runtimepaths.Paths{
			ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
			StateDir:   filepath.Join(root, "var"),
			LogDir:     filepath.Join(root, "var", "logs"),
		},
	}
	service := &scmManager{scope: ScopeSystem}

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

	ctx := context.Background()
	if _, err := service.Install(ctx, opts); err != nil {
		t.Fatalf("Install() = %v, want nil", err)
	}
	// The registration carries the failure recovery of the instance.
	scmTestCheckRecoveryActions(t)

	status, err := service.Status(ctx)
	if err != nil {
		t.Fatalf("Status() = %v, want nil", err)
	}
	if status.State != StateStopped {
		t.Errorf("the state = %q, want %q", status.State, StateStopped)
	}
	// The instance never ran: the service control manager reports the
	// code of a service that was never started for it, and that code is
	// no failure of the instance.
	if status.Detail != "" {
		t.Errorf("the detail = %q, want none for an instance that never ran", status.Detail)
	}
	if status.Name != ServiceName {
		t.Errorf("the name = %q, want %q", status.Name, ServiceName)
	}
	if status.Executable != opts.Executable {
		t.Errorf("the registered executable = %q, want %q", status.Executable, opts.Executable)
	}
	if status.Listen != opts.Listen {
		t.Errorf("the registered listen address = %q, want %q", status.Listen, opts.Listen)
	}
	if status.Paths != opts.Paths {
		t.Errorf("the registered paths = %+v, want %+v", status.Paths, opts.Paths)
	}

	// A second installation updates the registration instead of creating
	// a second one.
	if _, err := service.Install(ctx, opts); err != nil {
		t.Fatalf("the second Install() = %v, want nil", err)
	}
	// The failure recovery is configured on the update path as well: an
	// updated registration whose recovery was lost would not be restarted
	// after a crash either.
	scmTestCheckRecoveryActions(t)
	status, err = service.Status(ctx)
	if err != nil {
		t.Fatalf("Status() = %v, want nil", err)
	}
	if status.Executable != opts.Executable {
		t.Errorf("the registered executable = %q, want %q", status.Executable, opts.Executable)
	}

	// An installation that names one setting keeps the others: only the
	// named bind address moves, the runtime paths of the instance stay what
	// the previous installation wrote. The request names no path at all, so
	// the paths of the registration are what the installation uses.
	named := InstallOptions{
		Scope:      ScopeSystem,
		Executable: opts.Executable,
		Explicit:   InstallFieldSet{Listen: true},
		Listen:     "127.0.0.1:18080",
	}
	effective, err := service.Install(ctx, named)
	if err != nil {
		t.Fatalf("the Install() of a named listen address = %v, want nil", err)
	}
	if effective.Listen != named.Listen {
		t.Errorf("the effective listen address = %q, want the named one %q", effective.Listen, named.Listen)
	}
	if effective.Paths != opts.Paths {
		t.Errorf("the effective paths = %+v, want the kept ones %+v", effective.Paths, opts.Paths)
	}
	status, err = service.Status(ctx)
	if err != nil {
		t.Fatalf("Status() = %v, want nil", err)
	}
	if status.Listen != named.Listen {
		t.Errorf("the registered listen address = %q, want the named one %q", status.Listen, named.Listen)
	}
	if status.Paths != opts.Paths {
		t.Errorf("the registered paths = %+v, want the kept ones %+v", status.Paths, opts.Paths)
	}

	// Stopping an instance that does not run is a stable success.
	if err := service.Stop(ctx); err != nil {
		t.Errorf("Stop() = %v, want nil for an instance that does not run", err)
	}

	// The removal leaves the state of the instance alone.
	if err := service.Uninstall(ctx); err != nil {
		t.Fatalf("Uninstall() = %v, want nil", err)
	}
	status, err = service.Status(ctx)
	if err != nil {
		t.Fatalf("Status() = %v, want nil", err)
	}
	if status.State != StateNotInstalled {
		t.Errorf("the state = %q, want %q", status.State, StateNotInstalled)
	}
	if _, err := os.Stat(opts.Paths.StateDir); err != nil {
		t.Errorf("the state directory of the instance is gone: %v", err)
	}

	// The removal of an absent registration is a clean success.
	if err := service.Uninstall(ctx); err != nil {
		t.Errorf("the second Uninstall() = %v, want nil", err)
	}
}

// scmTestCheckRecoveryActions checks that the registration of the instance
// carries the failure recovery of the platform.
//
// The check reads the recovery from the service control manager, so it proves
// what the registration really holds and not what the installation meant to
// write. The switch for the non-crash failures is part of the policy as well:
// the handler of the instance reports a failed start, a failed host and a
// failed stop with an error code, and only the switch makes the platform
// recover those failures.
func scmTestCheckRecoveryActions(t *testing.T) {
	t.Helper()
	manager, err := mgr.Connect()
	if err != nil {
		t.Fatalf("mgr.Connect() = %v, want nil", err)
	}
	defer manager.Disconnect()
	handle, err := manager.OpenService(ServiceName)
	if err != nil {
		t.Fatalf("OpenService(%q) = %v, want nil", ServiceName, err)
	}
	defer handle.Close()

	actions, err := handle.RecoveryActions()
	if err != nil {
		t.Fatalf("RecoveryActions() = %v, want nil", err)
	}
	if !reflect.DeepEqual(actions, scmRecoveryActions()) {
		t.Errorf("the recovery actions of the registration = %v, want %v", actions, scmRecoveryActions())
	}
	period, err := handle.ResetPeriod()
	if err != nil {
		t.Fatalf("ResetPeriod() = %v, want nil", err)
	}
	if period != scmRecoveryResetPeriod {
		t.Errorf("the reset period of the registration = %d seconds, want %d", period, scmRecoveryResetPeriod)
	}
	nonCrash, err := handle.RecoveryActionsOnNonCrashFailures()
	if err != nil {
		t.Fatalf("RecoveryActionsOnNonCrashFailures() = %v, want nil", err)
	}
	if !nonCrash {
		t.Error("the registration recovers a crash only, want the recovery of a non-crash failure as well: an instance that reports its failure has to be restarted")
	}
}

// TestSCMStatusFromRegistrationReportsTheStateAndTheCommandLine checks
// that the status of a registered instance is built out of the facts of a
// read-only lookup.
//
// The mapping is checked without a registration of its own: reading the
// command line of a registration needs a real service in the service
// control manager, which a test cannot create without an elevated shell.
func TestSCMStatusFromRegistrationReportsTheStateAndTheCommandLine(t *testing.T) {
	root := t.TempDir()
	opts := scmTestOptions()
	opts.Executable = filepath.Join(root, "hfdesk.exe")
	if err := os.WriteFile(opts.Executable, []byte("not a program"), 0o600); err != nil {
		t.Fatalf("WriteFile() = %v, want nil", err)
	}

	facts := scmRegistrationFacts{
		present:     true,
		state:       svc.Running,
		commandLine: scmCommandLine(opts),
	}

	status, err := scmStatusFrom(ScopeSystem, facts)
	if err != nil {
		t.Fatalf("scmStatusFrom() = %v, want nil", err)
	}
	if status.State != StateRunning {
		t.Errorf("the state = %q, want %q", status.State, StateRunning)
	}
	if !status.Running {
		t.Error("a running instance is reported as not running")
	}
	if status.Scope != ScopeSystem {
		t.Errorf("the scope = %q, want %q", status.Scope, ScopeSystem)
	}
	if status.Name != ServiceName {
		t.Errorf("the name = %q, want %q", status.Name, ServiceName)
	}
	if status.UnitPath != scmRegistrationPath {
		t.Errorf("the registration path = %q, want %q", status.UnitPath, scmRegistrationPath)
	}
	if status.Executable != opts.Executable {
		t.Errorf("the registered executable = %q, want %q", status.Executable, opts.Executable)
	}
	if status.Listen != opts.Listen {
		t.Errorf("the registered listen address = %q, want %q", status.Listen, opts.Listen)
	}
	if status.Paths != opts.Paths {
		t.Errorf("the registered paths = %+v, want %+v", status.Paths, opts.Paths)
	}
	if status.CacheDir != opts.CacheDir {
		t.Errorf("the cache dir = %q, want %q", status.CacheDir, opts.CacheDir)
	}
}

// TestSCMStatusFromAnAbsentRegistrationIsNotInstalled checks that an
// instance that is not registered is reported as not installed, without an
// error.
func TestSCMStatusFromAnAbsentRegistrationIsNotInstalled(t *testing.T) {
	status, err := scmStatusFrom(ScopeSystem, scmRegistrationFacts{})
	if err != nil {
		t.Fatalf("scmStatusFrom() = %v, want nil for an absent registration", err)
	}
	if status.State != StateNotInstalled {
		t.Errorf("the state = %q, want %q", status.State, StateNotInstalled)
	}
	if status.Scope != ScopeSystem {
		t.Errorf("the scope = %q, want %q", status.Scope, ScopeSystem)
	}
	if status.Name != ServiceName {
		t.Errorf("the name = %q, want %q", status.Name, ServiceName)
	}
	if !strings.Contains(status.Detail, ServiceName) {
		t.Errorf("the detail = %q, want the name of the missing registration %q", status.Detail, ServiceName)
	}
}

// TestSCMStatusFromABrokenRegistrationNamesTheMissingExecutable checks
// that a registration that points at a program that is gone is reported as
// broken, and that the detail names the missing program.
//
// The broken registration is the case a normal shell has to be able to
// see: the registration is readable, the program behind it is not there
// anymore.
func TestSCMStatusFromABrokenRegistrationNamesTheMissingExecutable(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "gone", "hfdesk.exe")
	opts := scmTestOptions()
	opts.Executable = missing

	facts := scmRegistrationFacts{
		present:     true,
		state:       svc.Stopped,
		commandLine: scmCommandLine(opts),
	}

	status, err := scmStatusFrom(ScopeSystem, facts)
	if err != nil {
		t.Fatalf("scmStatusFrom() = %v, want nil", err)
	}
	if status.State != StateBroken {
		t.Errorf("the state = %q, want %q", status.State, StateBroken)
	}
	if !strings.Contains(status.Detail, missing) {
		t.Errorf("the detail = %q, want the path of the missing executable %q", status.Detail, missing)
	}
	if status.Running {
		t.Error("a stopped instance is reported as running")
	}
}

// TestSCMStatusFromABrokenRegistrationThatRunsKeepsTheRunningFact checks that a
// registration whose executable is gone is reported as broken while the instance
// it started still runs.
//
// The service control manager keeps the process it started: deleting the program
// stops the next start, not the instance. The state stays the diagnosis of the
// registration, and the running fact is carried next to it, so a caller that has
// to know whether anything serves does not read "broken" as "nothing runs".
func TestSCMStatusFromABrokenRegistrationThatRunsKeepsTheRunningFact(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "gone", "hfdesk.exe")
	opts := scmTestOptions()
	opts.Executable = missing

	facts := scmRegistrationFacts{
		present:     true,
		state:       svc.Running,
		processID:   4242,
		commandLine: scmCommandLine(opts),
	}

	status, err := scmStatusFrom(ScopeSystem, facts)
	if err != nil {
		t.Fatalf("scmStatusFrom() = %v, want nil", err)
	}
	if status.State != StateBroken {
		t.Errorf("the state = %q, want the diagnosis %q of the registration", status.State, StateBroken)
	}
	if !status.Running {
		t.Error("a registration the supervisor reports as running is reported as not running")
	}
	for _, want := range []string{missing, "the instance still runs"} {
		if !strings.Contains(status.Detail, want) {
			t.Errorf("the detail = %q, want it to mention %q", status.Detail, want)
		}
	}
}

// TestSCMStatusFromAFailedInstanceIsFailedAndNamesTheExitCode checks that
// an instance that crashed is reported as failed, and that the detail names
// the exit code it ended with.
//
// The service control manager has no state of its own for a failure: it
// reports a crashed instance as stopped, like a cleanly stopped one. The
// exit code is what tells the two apart, and the managers of the other
// platforms report the same crash as failed.
func TestSCMStatusFromAFailedInstanceIsFailedAndNamesTheExitCode(t *testing.T) {
	root := t.TempDir()
	opts := scmTestOptions()
	opts.Executable = filepath.Join(root, "hfdesk.exe")
	if err := os.WriteFile(opts.Executable, []byte("not a program"), 0o600); err != nil {
		t.Fatalf("WriteFile() = %v, want nil", err)
	}

	facts := scmRegistrationFacts{
		present:     true,
		state:       svc.Stopped,
		exitCode:    3,
		commandLine: scmCommandLine(opts),
	}

	status, err := scmStatusFrom(ScopeSystem, facts)
	if err != nil {
		t.Fatalf("scmStatusFrom() = %v, want nil", err)
	}
	if status.State != StateFailed {
		t.Errorf("the state = %q, want %q", status.State, StateFailed)
	}
	if !strings.Contains(status.Detail, "3") {
		t.Errorf("the detail = %q, want the exit code of the instance", status.Detail)
	}
}

// TestSCMStatusFromACleanlyStoppedInstanceIsStopped checks that an
// instance that ended cleanly is reported as stopped, and not as failed.
//
// The exit code of a clean run is zero, so the instance is not a failed one:
// the two cases are told apart by the exit code alone.
func TestSCMStatusFromACleanlyStoppedInstanceIsStopped(t *testing.T) {
	root := t.TempDir()
	opts := scmTestOptions()
	opts.Executable = filepath.Join(root, "hfdesk.exe")
	if err := os.WriteFile(opts.Executable, []byte("not a program"), 0o600); err != nil {
		t.Fatalf("WriteFile() = %v, want nil", err)
	}

	facts := scmRegistrationFacts{
		present:     true,
		state:       svc.Stopped,
		commandLine: scmCommandLine(opts),
	}

	status, err := scmStatusFrom(ScopeSystem, facts)
	if err != nil {
		t.Fatalf("scmStatusFrom() = %v, want nil", err)
	}
	if status.State != StateStopped {
		t.Errorf("the state = %q, want %q", status.State, StateStopped)
	}
	if status.Detail != "" {
		t.Errorf("the detail = %q, want none", status.Detail)
	}
}

// TestSCMStatusFromANeverStartedInstanceIsStopped checks that an instance
// that never ran is reported as stopped and not as failed.
//
// The service control manager reports the code of a service that was never
// started (ERROR_SERVICE_NEVER_STARTED) as the exit code of an instance
// that never ran. That code is no failure of the instance: it never ran, the
// way a unit that never ran is reported as stopped by the managers of the
// other platforms.
func TestSCMStatusFromANeverStartedInstanceIsStopped(t *testing.T) {
	root := t.TempDir()
	opts := scmTestOptions()
	opts.Executable = filepath.Join(root, "hfdesk.exe")
	if err := os.WriteFile(opts.Executable, []byte("not a program"), 0o600); err != nil {
		t.Fatalf("WriteFile() = %v, want nil", err)
	}

	facts := scmRegistrationFacts{
		present:     true,
		state:       svc.Stopped,
		exitCode:    uint32(windows.ERROR_SERVICE_NEVER_STARTED),
		commandLine: scmCommandLine(opts),
	}

	status, err := scmStatusFrom(ScopeSystem, facts)
	if err != nil {
		t.Fatalf("scmStatusFrom() = %v, want nil", err)
	}
	if status.State != StateStopped {
		t.Errorf("the state = %q, want %q", status.State, StateStopped)
	}
	if status.Detail != "" {
		t.Errorf("the detail = %q, want none", status.Detail)
	}
}

// TestSCMStatusFromKeepsTheOtherSupervisorStates checks that the exit code
// of an earlier run does not turn an instance that runs, that starts or that
// stops into a failed one.
//
// The exit code is only read for an instance that is stopped: every other
// state of the supervisor is reported as it is, the way the managers of the
// other platforms report it.
func TestSCMStatusFromKeepsTheOtherSupervisorStates(t *testing.T) {
	cases := []struct {
		name  string
		state svc.State
		want  State
		// wantRunning is the running fact of the same supervisor answer.
		wantRunning bool
	}{
		{"running", svc.Running, StateRunning, true},
		{"start pending", svc.StartPending, StateStarting, true},
		{"stop pending", svc.StopPending, StateStopping, false},
		{"paused", svc.Paused, StateUnknown, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			opts := scmTestOptions()
			opts.Executable = filepath.Join(root, "hfdesk.exe")
			if err := os.WriteFile(opts.Executable, []byte("not a program"), 0o600); err != nil {
				t.Fatalf("WriteFile() = %v, want nil", err)
			}

			facts := scmRegistrationFacts{
				present:     true,
				state:       tc.state,
				exitCode:    3,
				commandLine: scmCommandLine(opts),
			}

			status, err := scmStatusFrom(ScopeSystem, facts)
			if err != nil {
				t.Fatalf("scmStatusFrom() = %v, want nil", err)
			}
			if status.State != tc.want {
				t.Errorf("the state = %q, want %q", status.State, tc.want)
			}
			if status.Running != tc.wantRunning {
				t.Errorf("the running fact = %v, want %v", status.Running, tc.wantRunning)
			}
			if status.Detail != "" {
				t.Errorf("the detail = %q, want none", status.Detail)
			}
		})
	}
}

// TestSCMLookupErrorReportsTheElevationRequirement checks that a caller
// that may not read the registration is told about the elevation
// requirement, and not about the raw access denied error of the platform.
func TestSCMLookupErrorReportsTheElevationRequirement(t *testing.T) {
	err := scmLookupError("open the registration of "+ServiceName, windows.ERROR_ACCESS_DENIED)
	if err == nil {
		t.Fatal("scmLookupError(access denied) = nil, want an error")
	}
	if !strings.Contains(err.Error(), "elevated") {
		t.Errorf("scmLookupError(access denied) = %v, want an error that names the elevated shell", err)
	}
	if !strings.Contains(err.Error(), "administrator") {
		t.Errorf("scmLookupError(access denied) = %v, want an error that tells the user what to do", err)
	}
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Errorf("scmLookupError(access denied) = %v, want the elevation requirement and not the raw access denied error", err)
	}

	// A failure that is not about the rights of the shell is reported as
	// it is.
	other := errors.New("the service control manager is gone")
	if got := scmLookupError("connect to the service control manager", other); !errors.Is(got, other) {
		t.Errorf("scmLookupError(%v) = %v, want the error itself", other, got)
	}
	if got := scmLookupError("connect to the service control manager", nil); got != nil {
		t.Errorf("scmLookupError(nil) = %v, want nil", got)
	}
}

// TestSCMLookupRegistrationReportsAnAbsentRegistration checks the
// read-only lookup against the service control manager of the machine: a
// name that cannot be registered is reported as absent, without an error.
func TestSCMLookupRegistrationReportsAnAbsentRegistration(t *testing.T) {
	name := fmt.Sprintf("HfdeskTestAbsentRegistration%d", time.Now().UnixNano())
	facts, err := scmLookupRegistration(name)
	if err != nil {
		t.Skipf("the service control manager is not available: %v", err)
	}
	if facts.present {
		t.Errorf("scmLookupRegistration(%q) = a registered instance, want an absent registration", name)
	}

	status, err := scmStatusFrom(ScopeSystem, facts)
	if err != nil {
		t.Fatalf("scmStatusFrom() = %v, want nil for an absent registration", err)
	}
	if status.State != StateNotInstalled {
		t.Errorf("the state = %q, want %q", status.State, StateNotInstalled)
	}
}

// TestSCMLookupRegistrationReadsARegisteredService checks the read-only
// lookup against the service control manager of the machine: a service of
// the operating system is reported with its state and its registered
// command line.
//
// The lookup needs no elevated shell: it opens the service control manager
// with the right to connect and the registration with the right to query
// its state and its configuration.
func TestSCMLookupRegistrationReadsARegisteredService(t *testing.T) {
	name := scmTestFindReadableService(t)
	facts, err := scmLookupRegistration(name)
	if err != nil {
		t.Fatalf("scmLookupRegistration(%q) = %v, want nil", name, err)
	}
	if !facts.present {
		t.Errorf("scmLookupRegistration(%q) = an absent registration, want a registered one", name)
	}
	if facts.commandLine == "" {
		t.Errorf("the command line of %q is empty, want the registered command line", name)
	}
	if facts.state == 0 {
		t.Errorf("the state of %q = %d, want a state of the supervisor", name, facts.state)
	}
}

// TestSCMLookupRegistrationNeedsNoElevatedShell checks that the read-only
// lookup reads a registration where the privileged connection of the
// platform is refused.
//
// The privileged connection asks for every right of the service control
// manager, so a normal shell does not have it. The read-only lookup opens
// the manager with the right to connect and the registration with the
// rights a status needs, so it reads the state of a registration in a
// normal shell.
func TestSCMLookupRegistrationNeedsNoElevatedShell(t *testing.T) {
	manager, err := mgr.Connect()
	if err == nil {
		manager.Disconnect()
		t.Skip("the shell of the test is elevated, the read-only path cannot be told apart from the privileged one")
	}
	if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Skipf("the service control manager is not available: %v", err)
	}

	name := scmTestFindReadableService(t)
	facts, err := scmLookupRegistration(name)
	if err != nil {
		t.Fatalf("scmLookupRegistration(%q) = %v, want nil", name, err)
	}
	if !facts.present {
		t.Errorf("scmLookupRegistration(%q) = an absent registration, want a registered one", name)
	}
}

// scmTestFindReadableService returns the name of a service of the
// operating system that this shell may read.
//
// The candidates are the services every installation of the operating
// system carries. The test is skipped when the machine carries none of
// them that this shell may read.
func scmTestFindReadableService(t *testing.T) string {
	t.Helper()
	for _, candidate := range []string{"EventLog", "RpcSs", "Dhcp", "W32Time", "NlaSvc", "Schedule", "Power"} {
		facts, err := scmLookupRegistration(candidate)
		if err != nil || !facts.present || facts.commandLine == "" {
			// The service is not there or not readable from this
			// shell: the next candidate is tried.
			continue
		}
		return candidate
	}
	t.Skip("the machine carries no service that this shell may read")
	return ""
}

// TestSCMStatusOfAnAbsentRegistrationIsNotInstalled checks the status of
// an instance that is not registered: it reports that the instance is not
// installed, and it does so without an error and without an elevated
// shell.
//
// The test is skipped when the instance is registered, so it never reads a
// real installation.
func TestSCMStatusOfAnAbsentRegistrationIsNotInstalled(t *testing.T) {
	state, err := scmRegistrationStateOf()
	if err != nil {
		t.Skipf("the service control manager is not available: %v", err)
	}
	if state != scmRegistrationAbsent {
		t.Skipf("%s is registered, the test would read a real installation", ServiceName)
	}

	manager := &scmManager{scope: ScopeSystem}
	status, err := manager.Status(context.Background())
	if err != nil {
		t.Fatalf("Status() = %v, want nil for an absent registration", err)
	}
	if status.State != StateNotInstalled {
		t.Errorf("the state = %q, want %q", status.State, StateNotInstalled)
	}
	if status.Name != ServiceName {
		t.Errorf("the name = %q, want %q", status.Name, ServiceName)
	}
}

// TestSCMBrokenRegistration checks that a registration that points at a
// program that is gone is reported as broken.
//
// The decision is checked on the registered path alone: reading the command
// line of a registration needs a real service in the service control
// manager, which a test cannot create without an elevated shell.
func TestSCMBrokenRegistration(t *testing.T) {
	root := t.TempDir()
	existing := filepath.Join(root, "hfdesk.exe")
	if err := os.WriteFile(existing, []byte("not a program"), 0o600); err != nil {
		t.Fatalf("WriteFile() = %v, want nil", err)
	}
	missing := filepath.Join(root, "gone", "hfdesk.exe")

	// A registered program that is gone is a broken registration, and the
	// detail names the missing path.
	broken, detail := scmBrokenRegistration(missing)
	if !broken {
		t.Errorf("scmBrokenRegistration(%q) = %v, want a broken registration", missing, broken)
	}
	if !strings.Contains(detail, missing) {
		t.Errorf("the detail = %q, want the path of the missing program %q", detail, missing)
	}

	// A registered program that exists is not a broken registration.
	broken, detail = scmBrokenRegistration(existing)
	if broken {
		t.Errorf("scmBrokenRegistration(%q) = %v, want no broken registration", existing, broken)
	}
	if detail != "" {
		t.Errorf("the detail = %q, want none", detail)
	}

	// A registration that carries no program has nothing that could be
	// missing.
	broken, detail = scmBrokenRegistration("")
	if broken {
		t.Errorf("scmBrokenRegistration(\"\") = %v, want no broken registration", broken)
	}
	if detail != "" {
		t.Errorf("the detail = %q, want none", detail)
	}
}

// scmTestNamesPath reports whether a message of the manager names a path.
//
// The messages of the manager quote the path they talk about, so the
// backslashes of a Windows path are escaped in them: the path of a message
// is the quoted form of the path, not the path itself. The check looks for
// both forms, so it does not depend on the way a message renders the path.
//
// The manager judges a path by its real location: the platform follows the
// links of a path before it uses it, and it reports the name a directory has
// on the disk, so the name of a directory that is reached through a short
// name is the long name of the same directory. A message can therefore name
// the real location of a path instead of the path as it was written, and the
// check looks for that form as well.
//
// The real location of a path that does not exist cannot be read: the path as
// it was written is then the only form a message can name.
func scmTestNamesPath(message, path string) bool {
	if strings.Contains(message, path) || strings.Contains(message, strconv.Quote(path)) {
		return true
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	return strings.Contains(message, resolved) || strings.Contains(message, strconv.Quote(resolved))
}

// TestSCMTestNamesPathAcceptsTheRealLocationOfAPath checks that the
// comparison of a path with a message accepts the real location of the path.
//
// The manager judges a path by its real location, and it names that location
// in its messages: the platform reports the name a directory has on the disk,
// so a directory that is reached through a short name is named by its long
// name. A comparison that accepts only the path as it was written fails on a
// machine that names the directory differently.
//
// The comparison keeps its meaning: the path as it was written is still
// accepted, and a path the message does not name is not a path the message
// names.
func TestSCMTestNamesPathAcceptsTheRealLocationOfAPath(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "cache"), 0o700); err != nil {
		t.Fatalf("Mkdir() = %v, want nil", err)
	}

	// The path as it was written names the directory with another case: the
	// platform reports the name the directory has on the disk.
	spelled := filepath.Join(root, "CACHE")
	resolved, err := filepath.EvalSymlinks(spelled)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q) = %v, want nil", spelled, err)
	}
	if resolved == spelled {
		t.Skipf("the platform reports the path %q as it was written", spelled)
	}

	// The message names the real location of the path.
	message := fmt.Sprintf("service: the cache dir %q does not exist", resolved)
	if !scmTestNamesPath(message, spelled) {
		t.Errorf("scmTestNamesPath(%q, %q) = false, want true: the message names the real location %q of the path", message, spelled, resolved)
	}
	// The path as it was written is still accepted.
	if !scmTestNamesPath(fmt.Sprintf("service: the cache dir %q does not exist", spelled), spelled) {
		t.Errorf("scmTestNamesPath() = false, want true: the message names the path %q", spelled)
	}
	// A path the message does not name is not a path the message names.
	other := filepath.Join(root, "other")
	if scmTestNamesPath(message, other) {
		t.Errorf("scmTestNamesPath(%q, %q) = true, want false: the message does not name the path", message, other)
	}
}

// TestPreflightDataPathRefusesAFile checks that a file where the instance
// needs a directory is refused.
func TestPreflightDataPathRefusesAFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "cache")
	if err := os.WriteFile(path, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("WriteFile() = %v, want nil", err)
	}

	err := preflightDataPath("cache dir", path)
	if err == nil {
		t.Fatalf("preflightDataPath(%q) = nil, want an error", path)
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("preflightDataPath(%q) = %v, want an error that names the reason", path, err)
	}
	if !scmTestNamesPath(err.Error(), path) {
		t.Errorf("preflightDataPath(%q) = %v, want an error that names the path", path, err)
	}
}

// scmTestNamedDir is one directory an installation can name.
type scmTestNamedDir struct {
	// name names the directory in the name of the subtest.
	name string
	// path returns the named directory of the installation.
	path func(opts *InstallOptions, root string) string
}

// scmTestNamedDirs returns the directories an installation can name: the
// cache root, the hub directory and the local output directory.
func scmTestNamedDirs() []scmTestNamedDir {
	return []scmTestNamedDir{
		{"the cache dir", func(opts *InstallOptions, root string) string {
			opts.CacheDir = filepath.Join(root, "cache")
			return opts.CacheDir
		}},
		{"the hub dir", func(opts *InstallOptions, root string) string {
			opts.HubDir = filepath.Join(root, "hub")
			return opts.HubDir
		}},
		{"the local dir", func(opts *InstallOptions, root string) string {
			opts.LocalDir = filepath.Join(root, "models")
			return opts.LocalDir
		}},
	}
}

// scmTestOptionsWithNamedDir returns the options of an installation that
// names one directory only, and the directory it names.
//
// The other directories are not part of the installation: the preflight
// checks the named directories only.
func scmTestOptionsWithNamedDir(root string, named scmTestNamedDir) (InstallOptions, string) {
	opts := scmTestOptions()
	opts.Paths = runtimepaths.Paths{
		ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
		StateDir:   filepath.Join(root, "var"),
		LogDir:     filepath.Join(root, "var", "logs"),
	}
	opts.CacheDir = ""
	opts.HubDir = ""
	opts.LocalDir = ""
	return opts, named.path(&opts, root)
}

// TestPreflightDataPathsAcceptAMissingNamedDirectory checks that a named
// directory that does not exist yet is accepted when the account of the
// instance can create it in its nearest existing ancestor.
//
// The instance creates the directory when it runs, as the account the
// instance runs as, so the installation judges the directory through the
// ancestor it would be created in: the account has to be able to create
// entries there.
//
// The installation creates nothing: the directory belongs to whoever named
// it, and the instance creates it when it runs.
func TestPreflightDataPathsAcceptAMissingNamedDirectory(t *testing.T) {
	for _, named := range scmTestNamedDirs() {
		t.Run(named.name, func(t *testing.T) {
			root := t.TempDir()
			opts, path := scmTestOptionsWithNamedDir(root, named)
			access := scmTestPathAccessFor(t, path, true)
			if err := preflightDataPaths(opts, access); err != nil {
				t.Errorf("preflightDataPaths() = %v, want nil for a directory the account of the instance can create", err)
			}
			// The installation created nothing: the instance creates the
			// directory when it runs.
			if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("the directory %q exists (stat error: %v), want none: the instance creates it when it runs", path, err)
			}
		})
	}
}

// TestPreflightDataPathsRefuseAMissingNamedDirectory checks that a named
// directory that does not exist yet is refused when the account of the
// instance cannot create it in its nearest existing ancestor.
//
// The instance creates the directory when it runs, as the account the
// instance runs as, so the account has to be able to create entries in the
// ancestor. A named data directory belongs to whoever named it: the
// installation only reads the access control list of the ancestor and never
// creates the directory.
//
// The refusal names the directory, the account and the access that is
// missing, so the user knows what to grant.
func TestPreflightDataPathsRefuseAMissingNamedDirectory(t *testing.T) {
	for _, named := range scmTestNamedDirs() {
		t.Run(named.name, func(t *testing.T) {
			root := t.TempDir()
			opts, path := scmTestOptionsWithNamedDir(root, named)
			access := scmTestPathAccessFor(t, path, false)

			err := preflightDataPaths(opts, access)
			if err == nil {
				t.Fatalf("preflightDataPaths() = nil, want the refusal of %s", named.name)
			}
			if !scmTestNamesPath(err.Error(), path) {
				t.Errorf("preflightDataPaths() = %v, want an error that names the path %q", err, path)
			}
			if !strings.Contains(err.Error(), scmServiceIdentityName) {
				t.Errorf("preflightDataPaths() = %v, want an error that names the account of the instance", err)
			}
			if !strings.Contains(err.Error(), "read and write access") {
				t.Errorf("preflightDataPaths() = %v, want an error that names the access that is missing", err)
			}
			if !strings.Contains(err.Error(), "never creates a directory the user named") {
				t.Errorf("preflightDataPaths() = %v, want an error that tells the user that the installation creates no directory of its own", err)
			}
			if !strings.Contains(err.Error(), "grant the account read and write access to the ancestor directory") {
				t.Errorf("preflightDataPaths() = %v, want an error that tells the user what to do", err)
			}
			// The refused directory was not created behind the back of
			// the user.
			if _, statErr := os.Stat(path); !errors.Is(statErr, fs.ErrNotExist) {
				t.Errorf("the refused directory %q exists: %v", path, statErr)
			}
		})
	}
}

// TestPreflightDataPathJudgesTheNearestExistingAncestor checks that a named
// directory that does not exist yet is judged through the nearest existing
// ancestor of it and not through the directory above it.
//
// The instance creates the directory when it runs, as the account the
// instance runs as: the account has to be able to create it in the
// directory that holds it, and the directory that holds it is the nearest
// existing ancestor.
func TestPreflightDataPathJudgesTheNearestExistingAncestor(t *testing.T) {
	root := t.TempDir()
	// The directory that holds the named directory does not exist yet
	// either: the nearest existing ancestor is the one below the root.
	ancestor := filepath.Join(root, "a")
	if err := os.Mkdir(ancestor, 0o750); err != nil {
		t.Fatalf("Mkdir() = %v, want nil", err)
	}
	path := filepath.Join(ancestor, "b", "cache")
	access := scmTestPathAccessFor(t, path, true)
	if err := preflightDataPath("cache dir", path, access); err != nil {
		t.Errorf("preflightDataPath(%q) = %v, want nil for a directory the account of the instance can create in its nearest existing ancestor", path, err)
	}
	// The installation created nothing.
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the directory %q exists (stat error: %v), want none: the instance creates it when it runs", path, err)
	}
}

// TestSCMPreflightDataPathsAcceptADirectoryInsideTheStateRoot checks that a
// named directory that does not exist yet and that really lives inside the
// state root of the instance is not checked against the account of the
// instance.
//
// The installation grants the state root to that account, and the instance
// creates the cache tree it needs inside it. A directory of the state root
// that does not exist yet is therefore neither checked nor created.
//
// A directory that exists already is checked, whatever it lives in: the
// access control list of the child decides what the account of the instance
// can do there, and owning the state root grants no right on it.
func TestSCMPreflightDataPathsAcceptADirectoryInsideTheStateRoot(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "var", "lib", "hfdesk")
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		t.Fatalf("MkdirAll() = %v, want nil", err)
	}
	opts := scmTestOptions()
	opts.Paths = runtimepaths.Paths{
		ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
		StateDir:   stateDir,
		LogDir:     filepath.Join(root, "var", "log", "hfdesk"),
	}
	opts.CacheDir = filepath.Join(stateDir, "cache")
	opts.HubDir = filepath.Join(stateDir, "cache", "hub")
	opts.LocalDir = ""

	access := &scmTestPathAccess{}
	if err := preflightDataPaths(opts, access); err != nil {
		t.Errorf("preflightDataPaths() = %v, want nil for a directory inside the state root", err)
	}
	if len(access.calls) != 0 {
		t.Errorf("preflightDataPaths() checked %v, want the absent state-root default to remain exempt", access.calls)
	}
	// The directory was not created: the instance creates it when it runs.
	if _, err := os.Stat(opts.CacheDir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the cache directory %q exists (stat error: %v), want none: the instance creates it when it runs", opts.CacheDir, err)
	}
}

// TestSCMPreflightDataPathsRefuseAnExistingDirectoryInsideTheStateRoot
// checks that a named directory that exists inside the state root of the
// instance is checked against the account of the instance.
//
// Owning a directory grants no right on a directory inside it: the access
// control list of the child decides what the account of the instance can do
// there. A cache that was moved by hand, a cache of an earlier installation
// and a directory that was named with --cache-dir and exists already
// therefore have to be checked, even when they live inside the state root
// the installation owns.
func TestSCMPreflightDataPathsRefuseAnExistingDirectoryInsideTheStateRoot(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "var", "lib", "hfdesk")
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		t.Fatalf("MkdirAll() = %v, want nil", err)
	}
	// The cache exists already and the account of the instance cannot
	// work with it.
	cacheDir := filepath.Join(stateDir, "cache")
	if err := os.Mkdir(cacheDir, 0o750); err != nil {
		t.Fatalf("Mkdir() = %v, want nil", err)
	}

	opts := scmTestOptions()
	opts.Paths = runtimepaths.Paths{
		ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
		StateDir:   stateDir,
		LogDir:     filepath.Join(root, "var", "log", "hfdesk"),
	}
	opts.CacheDir = cacheDir
	opts.HubDir = ""
	opts.LocalDir = ""

	err := preflightDataPaths(opts, scmTestPathAccessFor(t, cacheDir, false))
	if err == nil {
		t.Fatalf("preflightDataPaths() = nil, want the refusal of an existing directory inside the state root")
	}
	if !scmTestNamesPath(err.Error(), cacheDir) {
		t.Errorf("preflightDataPaths() = %v, want an error that names the path", err)
	}
	if !strings.Contains(err.Error(), scmServiceIdentityName) {
		t.Errorf("preflightDataPaths() = %v, want an error that names the account of the instance", err)
	}
}

// TestSCMPreflightDataPathsCheckAnExistingDirectoryOutsideTheStateRoot
// checks that a named directory that exists outside the state root of the
// instance is checked against the account of the instance, the way it has
// always been.
func TestSCMPreflightDataPathsCheckAnExistingDirectoryOutsideTheStateRoot(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "var", "lib", "hfdesk")
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		t.Fatalf("MkdirAll() = %v, want nil", err)
	}

	// A directory outside of the state root the account of the instance
	// can work with.
	usable := filepath.Join(root, "models")
	if err := os.Mkdir(usable, 0o750); err != nil {
		t.Fatalf("Mkdir() = %v, want nil", err)
	}

	// A directory outside of the state root the account of the instance
	// cannot work with.
	unusable := filepath.Join(root, "other-models")
	if err := os.Mkdir(unusable, 0o750); err != nil {
		t.Fatalf("Mkdir() = %v, want nil", err)
	}

	opts := scmTestOptions()
	opts.Paths = runtimepaths.Paths{
		ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
		StateDir:   stateDir,
		LogDir:     filepath.Join(root, "var", "log", "hfdesk"),
	}
	opts.HubDir = ""
	opts.LocalDir = ""

	// The directory the account of the instance can work with is
	// accepted.
	opts.CacheDir = usable
	if err := preflightDataPaths(opts, scmTestPathAccessFor(t, usable, true)); err != nil {
		t.Errorf("preflightDataPaths() = %v, want nil for a directory the account of the instance can work with", err)
	}

	// The directory it cannot work with is refused.
	opts.CacheDir = unusable
	err := preflightDataPaths(opts, scmTestPathAccessFor(t, unusable, false))
	if err == nil {
		t.Fatalf("preflightDataPaths() = nil, want the refusal of a directory the account of the instance cannot work with")
	}
	if !scmTestNamesPath(err.Error(), unusable) {
		t.Errorf("preflightDataPaths() = %v, want an error that names the path", err)
	}
	if !strings.Contains(err.Error(), scmServiceIdentityName) {
		t.Errorf("preflightDataPaths() = %v, want an error that names the account of the instance", err)
	}
}

// TestSCMPreflightDataPathsRefuseALinkThatLeavesTheStateRoot checks that a
// named directory that is reached through a link inside the state root is
// checked like every other named directory.
//
// The exemption of the state root is decided on the real location of the two
// paths: a link that leaves the state root points at a directory outside of
// it, and the account of the instance has to be able to work with it. A
// comparison of the paths as they were written would grant the exemption to
// the link, and the installation would be accepted and fail later with an
// access error.
func TestSCMPreflightDataPathsRefuseALinkThatLeavesTheStateRoot(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "var", "lib", "hfdesk")
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		t.Fatalf("MkdirAll() = %v, want nil", err)
	}

	// A directory outside of the state root that the account of the
	// instance cannot use.
	outside := filepath.Join(root, "models")
	if err := os.Mkdir(outside, 0o750); err != nil {
		t.Fatalf("Mkdir() = %v, want nil", err)
	}

	// A link inside the state root that points at it.
	link := filepath.Join(stateDir, "cache")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("the platform does not allow a symbolic link here: %v", err)
	}

	// The link is not inside the state root: the directory it points at is
	// somewhere else.
	inside, err := scmPathInsideStateRoot(stateDir, link)
	if err != nil {
		t.Fatalf("scmPathInsideStateRoot() = %v, want nil", err)
	}
	if inside {
		t.Errorf("scmPathInsideStateRoot(%q, %q) = true, want false: the link points outside of the state root", stateDir, link)
	}

	opts := scmTestOptions()
	opts.Paths = runtimepaths.Paths{
		ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
		StateDir:   stateDir,
		LogDir:     filepath.Join(root, "var", "log", "hfdesk"),
	}
	opts.CacheDir = link
	opts.HubDir = ""
	opts.LocalDir = ""

	err = preflightDataPaths(opts, scmTestPathAccessFor(t, link, false))
	if err == nil {
		t.Fatalf("preflightDataPaths() = nil, want the refusal of a link that leaves the state root")
	}
	if !scmTestNamesPath(err.Error(), link) {
		t.Errorf("preflightDataPaths() = %v, want an error that names the path", err)
	}
	if !strings.Contains(err.Error(), scmServiceIdentityName) {
		t.Errorf("preflightDataPaths() = %v, want an error that names the account of the instance", err)
	}
}

// TestPreflightDataPathRefusesAPathTheIdentityCannotReach checks that a
// directory the account of the instance cannot reach is refused before the
// registration is written.
//
// The directory does not exist: the instance creates it when it runs, as the
// account the instance runs as, so the refusal names the missing directory,
// the nearest existing ancestor it would be created in and the access the
// account is missing there.
//
// The guidance names the account of the instance, because the access rights
// of the ancestor have to be granted to that account.
func TestPreflightDataPathRefusesAPathTheIdentityCannotReach(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "parent")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatalf("Mkdir() = %v, want nil", err)
	}
	path := filepath.Join(parent, "cache")
	err := preflightDataPath("cache dir", path, scmTestPathAccessFor(t, path, false))
	if err == nil {
		t.Fatalf("preflightDataPath(%q) = nil, want an error", path)
	}
	if !scmTestNamesPath(err.Error(), path) {
		t.Errorf("preflightDataPath(%q) = %v, want an error that names the path", path, err)
	}
	// The manager names the real location of the ancestor: the platform
	// reports the name the directory has on the disk, so a directory that is
	// reached through a short name is named by its long name. The name of the
	// ancestor is what makes the guidance actionable, and the check accepts
	// the path as it was written and the real location of it.
	if !scmTestNamesPath(err.Error(), parent) {
		t.Errorf("preflightDataPath(%q) = %v, want an error that names the nearest existing ancestor %q", path, err, parent)
	}
	if !strings.Contains(err.Error(), scmServiceIdentityName) {
		t.Errorf("preflightDataPath(%q) = %v, want an error that names the account of the instance", path, err)
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("preflightDataPath(%q) = %v, want an error that names the missing directory", path, err)
	}
	if !strings.Contains(err.Error(), "read and write access") {
		t.Errorf("preflightDataPath(%q) = %v, want an error that names the access that is missing", path, err)
	}
	if !strings.Contains(err.Error(), "never creates a directory the user named") {
		t.Errorf("preflightDataPath(%q) = %v, want an error that tells the user that the installation creates no directory of its own", path, err)
	}
	if !strings.Contains(err.Error(), "grant the account read and write access to the ancestor directory") {
		t.Errorf("preflightDataPath(%q) = %v, want an error that tells the user what to do", path, err)
	}
	// The refused directory is not created behind the back of the user.
	if _, statErr := os.Stat(path); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("the refused directory %q exists: %v", path, statErr)
	}
}

// TestPreflightDataPathDoesNotInferLocalServiceAccessWithoutItsToken keeps an
// incomplete identity model from becoming a denial oracle.
func TestPreflightDataPathDoesNotInferLocalServiceAccessWithoutItsToken(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "cache")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatalf("Mkdir() = %v, want nil", err)
	}
	err := preflightDataPath("cache dir", path)
	if err != nil {
		t.Fatalf("preflightDataPath(%q) = %v, want inconclusive success without an actual LocalService token", path, err)
	}
}

func TestPreflightDataPathLeavesDescriptorReadFailureInconclusive(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "cache")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatalf("Mkdir() = %v, want nil", err)
	}
	resolved, err := scmResolvePath(path)
	if err != nil {
		t.Fatalf("resolve directory %q: %v", path, err)
	}
	access := &scmTestPathAccess{errors: map[string]error{resolved: errors.New("descriptor unavailable")}}
	if err := preflightDataPath("cache dir", path, access); err != nil {
		t.Fatalf("preflightDataPath(%q) = %v, want unknown AccessCheck evidence to remain inconclusive", path, err)
	}
}

// TestSCMPreflightExecutableAccess checks that a definite denial refuses, an
// allowed descriptor passes and an unreadable one refuses nothing. The
// refusal tells the user to grant execute access; it never demands read
// access to a native program.
func TestSCMPreflightExecutableAccess(t *testing.T) {
	previous := scmCheckExecutableAccess
	t.Cleanup(func() { scmCheckExecutableAccess = previous })

	executable := `C:\ProgramData\HFDesk\hfdesk.exe`
	cases := []struct {
		name    string
		access  scmExecutableAccess
		reason  string
		wantErr bool
	}{
		{"allowed", scmExecutableAccessAllowed, "", false},
		{"denied", scmExecutableAccessDenied, "the test denies the access", true},
		{"inconclusive", scmExecutableAccessInconclusive, "the test cannot read the descriptor", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scmCheckExecutableAccess = func(string, scmPathAccess) (scmExecutableAccess, string) {
				return tc.access, tc.reason
			}
			err := preflightExecutableAccess(executable)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("preflightExecutableAccess() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("preflightExecutableAccess() = nil, want the refusal of the program")
			}
			if !scmTestNamesPath(err.Error(), executable) {
				t.Errorf("preflightExecutableAccess() = %v, want the refused executable path %q", err, executable)
			}
			for _, want := range []string{scmServiceIdentityName, tc.reason, "grant the account execute access"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("preflightExecutableAccess() = %v, want %q in the refusal", err, want)
				}
			}
			for _, unwanted := range []string{"read and execute", "grant the account read"} {
				if strings.Contains(err.Error(), unwanted) {
					t.Errorf("preflightExecutableAccess() = %v, want no demand to grant %q for a native program", err, unwanted)
				}
			}
		})
	}
}

// TestSCMPreflightExecutableAccessAcceptsExecuteOnlyNativeProgram pins the
// Windows contract of the program check: the program image is checked for
// execute only. An NTFS "Execute file" entry that withholds read data passes
// preflight, the preflight asks AccessCheck for the execute right of the
// program and never for its data, and a program the account cannot execute is
// still refused with a message that asks for execute access only.
func TestSCMPreflightExecutableAccessAcceptsExecuteOnlyNativeProgram(t *testing.T) {
	token := scmTestImpersonationToken(t)
	user, err := token.GetTokenUser()
	if err != nil {
		t.Fatalf("GetTokenUser() = %v", err)
	}
	if user.User.Sid == nil {
		t.Fatal("GetTokenUser() returned no user SID")
	}
	sid := user.User.Sid.String()

	root := t.TempDir()
	executable := filepath.Join(root, "hfdesk.exe")
	if err := os.WriteFile(executable, []byte("test program image"), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) = %v", executable, err)
	}
	resolved, err := scmResolvePath(executable)
	if err != nil {
		t.Fatalf("resolve %q: %v", executable, err)
	}

	// The execute-only descriptor carries execute but no read data for the
	// program, so the real AccessCheck accepts the right the fixed preflight
	// asks for and refuses the read+execute mask the preflight used to ask
	// for: the old mask refused exactly this fixture.
	executeOnly := scmTestAccessCheckDescriptor(t, sid, fmt.Sprintf("D:P(A;;FX;;;%s)", sid))
	for _, tc := range []struct {
		name    string
		desired uint32
		allowed bool
	}{
		{"execute right of the program", windows.FILE_EXECUTE, true},
		{"the former read and execute mask", windows.GENERIC_READ | windows.GENERIC_EXECUTE, false},
	} {
		allowed, err := scmAccessCheckDescriptor(token, executeOnly, tc.desired)
		if err != nil {
			t.Fatalf("AccessCheck(%s) = %v", tc.name, err)
		}
		if allowed != tc.allowed {
			t.Errorf("AccessCheck(%s) = %v, want %v", tc.name, allowed, tc.allowed)
		}
	}

	// The preflight passes the execute-only native program...
	access := &scmTestPathAccess{
		check: func(_ string, desired uint32) (bool, error) {
			return scmAccessCheckDescriptor(token, executeOnly, desired)
		},
		// The traverse-bypass privilege isolates this fixture on the
		// program object itself; the traversal route is covered by its
		// own tests.
		privilege: true,
	}
	if err := preflightExecutableAccess(executable, access); err != nil {
		t.Fatalf("preflightExecutableAccess() = %v, want the execute-only program to pass", err)
	}
	if len(access.calls) != 1 {
		t.Fatalf("AccessCheck calls = %+v, want only the program object checked", access.calls)
	}
	if call := access.calls[0]; call.path != resolved || call.desired != windows.FILE_EXECUTE {
		t.Errorf("AccessCheck(%q, 0x%X), want the program checked at %q with FILE_EXECUTE only", call.path, call.desired, resolved)
	}

	// ...and still refuses a program the account cannot execute.
	readOnly := scmTestAccessCheckDescriptor(t, sid, fmt.Sprintf("D:P(A;;FR;;;%s)", sid))
	denied := &scmTestPathAccess{
		check: func(_ string, desired uint32) (bool, error) {
			return scmAccessCheckDescriptor(token, readOnly, desired)
		},
		privilege: true,
	}
	err = preflightExecutableAccess(executable, denied)
	if err == nil {
		t.Fatal("preflightExecutableAccess() = nil, want the refusal of a program without execute access")
	}
	if !strings.Contains(err.Error(), "grant the account execute access") {
		t.Errorf("preflightExecutableAccess() = %v, want the refusal to ask for execute access", err)
	}
	for _, unwanted := range []string{"read and execute", "grant the account read"} {
		if strings.Contains(err.Error(), unwanted) {
			t.Errorf("preflightExecutableAccess() = %v, want no demand to grant %q", err, unwanted)
		}
	}
}

type scmTestPathAccess struct {
	results          map[string]bool
	resultsByDesired map[string]map[uint32]bool
	errors           map[string]error
	check            func(string, uint32) (bool, error)
	defaultSet       bool
	defaultAllow     bool
	privilege        bool
	privilegeErr     error
	calls            []struct {
		path    string
		desired uint32
	}
}

func (a *scmTestPathAccess) Check(path string, desired uint32) (bool, error) {
	a.calls = append(a.calls, struct {
		path    string
		desired uint32
	}{path, desired})
	if err := a.errors[path]; err != nil {
		return false, err
	}
	if a.check != nil {
		return a.check(path, desired)
	}
	if byDesired, ok := a.resultsByDesired[path]; ok {
		if result, ok := byDesired[desired]; ok {
			return result, nil
		}
	}
	result, ok := a.results[path]
	if !ok {
		if a.defaultSet {
			return a.defaultAllow, nil
		}
		return false, fmt.Errorf("no test AccessCheck result for %q", path)
	}
	return result, nil
}

func (a *scmTestPathAccess) HasEnabledPrivilege(name string) (bool, error) {
	if name != scmChangeNotifyPrivilege {
		return false, fmt.Errorf("unexpected privilege query %q", name)
	}
	return a.privilege, a.privilegeErr
}

func (*scmTestPathAccess) Close() {}

func scmTestPathAccessFor(t *testing.T, path string, allowed bool) *scmTestPathAccess {
	t.Helper()
	checked := path
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		var nearestErr error
		checked, nearestErr = scmNearestExistingDir(path)
		if nearestErr != nil {
			t.Fatalf("find the AccessCheck target for %q: %v", path, nearestErr)
		}
	}
	resolved, err := scmResolvePath(checked)
	if err != nil {
		t.Fatalf("resolve the AccessCheck target for %q: %v", path, err)
	}
	return &scmTestPathAccess{
		results:      map[string]bool{resolved: allowed},
		defaultSet:   true,
		defaultAllow: true,
		privilege:    true,
	}
}

func scmTestImpersonationToken(t *testing.T) windows.Token {
	t.Helper()
	var primary windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY|windows.TOKEN_DUPLICATE, &primary); err != nil {
		t.Fatalf("OpenProcessToken() = %v", err)
	}
	t.Cleanup(func() { _ = primary.Close() })
	var token windows.Token
	if err := windows.DuplicateTokenEx(primary, windows.TOKEN_QUERY,
		nil, windows.SecurityImpersonation, windows.TokenImpersonation, &token); err != nil {
		t.Fatalf("DuplicateTokenEx() = %v", err)
	}
	t.Cleanup(func() { _ = token.Close() })
	return token
}

// AccessCheck requires valid owner and group SIDs in the descriptor, not only
// the DACL that determines the result.
func scmTestAccessCheckDescriptor(t *testing.T, ownerSID, dacl string) *windows.SECURITY_DESCRIPTOR {
	t.Helper()
	sddl := fmt.Sprintf("O:%sG:%s%s", ownerSID, ownerSID, dacl)
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatalf("SecurityDescriptorFromString(%q) = %v", sddl, err)
	}
	if !sd.IsValid() {
		t.Fatalf("SecurityDescriptorFromString(%q) returned an invalid descriptor", sddl)
	}
	if owner, _, err := sd.Owner(); err != nil || owner == nil {
		t.Fatalf("security descriptor owner = %v, %v; want a valid SID", owner, err)
	}
	if group, _, err := sd.Group(); err != nil || group == nil {
		t.Fatalf("security descriptor group = %v, %v; want a valid SID", group, err)
	}
	return sd
}

func scmTestCreateDirectorySymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		if errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD) || errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
			errors.Is(err, windows.ERROR_NOT_SUPPORTED) || errors.Is(err, windows.ERROR_INVALID_FUNCTION) {
			t.Skipf("directory reparse points are unavailable: %v", err)
		}
		t.Fatalf("Symlink(%q, %q): %v", target, link, err)
	}
}

func TestSCMAccessCheckMapsGenericRightsAndHonorsDACLOrder(t *testing.T) {
	token := scmTestImpersonationToken(t)
	user, err := token.GetTokenUser()
	if err != nil {
		t.Fatalf("GetTokenUser() = %v", err)
	}
	if user.User.Sid == nil {
		t.Fatal("GetTokenUser() returned no user SID")
	}
	sid := user.User.Sid.String()
	for _, tc := range []struct {
		name    string
		dacl    string
		allowed bool
	}{
		{"earlier complete explicit allow wins before inherited deny", fmt.Sprintf("D:P(A;;FR;;;%s)(A;;FX;;;%s)(D;ID;FA;;;WD)", sid, sid), true},
		{"earlier deny blocks later allow", "D:P(D;;FR;;;WD)(A;;FA;;;" + sid + ")", false},
		{"inherit-only allow does not apply to this object", "D:P(A;OICIIO;FA;;;" + sid + ")", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sd := scmTestAccessCheckDescriptor(t, sid, tc.dacl)
			allowed, err := scmAccessCheckDescriptor(token, sd, windows.GENERIC_READ|windows.GENERIC_EXECUTE)
			if err != nil {
				t.Fatalf("AccessCheck = %v", err)
			}
			if allowed != tc.allowed {
				t.Errorf("AccessCheck(%q) = %v, want %v", tc.dacl, allowed, tc.allowed)
			}
		})
	}
}

func TestSCMPathRouteUsesNativeAccessCheckForSourceDenials(t *testing.T) {
	token := scmTestImpersonationToken(t)
	user, err := token.GetTokenUser()
	if err != nil {
		t.Fatalf("GetTokenUser() = %v", err)
	}
	if user.User.Sid == nil {
		t.Fatal("GetTokenUser() returned no user SID")
	}
	root := t.TempDir()
	restricted := filepath.Join(root, "restricted")
	intermediate := filepath.Join(root, "intermediate")
	target := filepath.Join(root, "target")
	for _, dir := range []string{restricted, intermediate, target} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatalf("Mkdir(%q): %v", dir, err)
		}
	}
	firstLink := filepath.Join(restricted, "junction")
	scmTestCreateDirectorySymlink(t, intermediate, firstLink)
	secondLink := filepath.Join(intermediate, "nested")
	scmTestCreateDirectorySymlink(t, target, secondLink)
	sourcePath := filepath.Join(firstLink, "nested", "hfdesk.exe")
	targetFile := filepath.Join(target, "hfdesk.exe")
	if err := os.WriteFile(targetFile, []byte("test image"), 0o600); err != nil {
		t.Fatalf("WriteFile(%q): %v", targetFile, err)
	}
	resolved, err := scmResolvePath(sourcePath)
	if err != nil {
		t.Fatalf("resolve nested route %q: %v", sourcePath, err)
	}
	resolvedTarget, err := scmResolvePath(targetFile)
	if err != nil {
		t.Fatalf("resolve nested target %q: %v", targetFile, err)
	}
	if !strings.EqualFold(filepath.Clean(resolved), filepath.Clean(resolvedTarget)) {
		t.Fatalf("resolved route = %q, resolved target = %q; want the nested route to name that target", resolved, resolvedTarget)
	}
	resolvedRestricted, err := scmResolvePath(restricted)
	if err != nil {
		t.Fatalf("resolve source ancestor %q: %v", restricted, err)
	}
	resolvedIntermediate, err := scmResolvePath(firstLink)
	if err != nil {
		t.Fatalf("resolve first reparse target %q: %v", firstLink, err)
	}

	sid := user.User.Sid.String()
	allowed := scmTestAccessCheckDescriptor(t, sid, fmt.Sprintf("D:P(A;;FA;;;%s)", sid))
	deniedTraverse := scmTestAccessCheckDescriptor(t, sid, fmt.Sprintf("D:P(D;;FX;;;%s)(A;;FA;;;WD)", sid))
	descriptors := map[string]*windows.SECURITY_DESCRIPTOR{
		strings.ToUpper(filepath.Clean(resolved)):           allowed,
		strings.ToUpper(filepath.Clean(resolvedRestricted)): deniedTraverse,
	}
	// The zero-value privilege flag forces the no-bypass route through AccessCheck.
	access := &scmTestPathAccess{
		check: func(path string, desired uint32) (bool, error) {
			descriptor := descriptors[strings.ToUpper(filepath.Clean(path))]
			if descriptor == nil {
				descriptor = allowed
			}
			return scmAccessCheckDescriptor(token, descriptor, desired)
		},
	}
	decision, reason := scmCheckPathAccess(sourcePath, windows.GENERIC_READ|windows.GENERIC_EXECUTE, access)
	if decision != scmExecutableAccessDenied || !scmTestNamesPath(reason, restricted) {
		t.Fatalf("native AccessCheck with an allowed target and denied source ancestor = %v, %q; want source traversal denied", decision, reason)
	}

	descriptors[strings.ToUpper(filepath.Clean(resolvedRestricted))] = allowed
	descriptors[strings.ToUpper(filepath.Clean(resolvedIntermediate))] = deniedTraverse
	decision, reason = scmCheckPathAccess(sourcePath, windows.GENERIC_READ|windows.GENERIC_EXECUTE, access)
	if decision != scmExecutableAccessDenied || !scmTestNamesPath(reason, intermediate) {
		t.Fatalf("native AccessCheck with a denied nested reparse target = %v, %q; want the intermediate route denial", decision, reason)
	}

	descriptors[strings.ToUpper(filepath.Clean(resolvedIntermediate))] = allowed
	decision, reason = scmCheckPathAccess(sourcePath, windows.GENERIC_READ|windows.GENERIC_EXECUTE, access)
	if decision != scmExecutableAccessAllowed {
		t.Fatalf("native AccessCheck over the complete nested route = %v, %q; want allowed", decision, reason)
	}

	access.errors = map[string]error{resolvedIntermediate: errors.New("nested route descriptor unavailable")}
	decision, _ = scmCheckPathAccess(sourcePath, windows.GENERIC_READ|windows.GENERIC_EXECUTE, access)
	if decision != scmExecutableAccessInconclusive {
		t.Errorf("native route with an unavailable nested descriptor = %v, want inconclusive", decision)
	}
}

// TestSCMPathRouteChecksEveryAncestorOfAResolvedReparseRoute proves the
// traversal route covers the ancestors of every resolved prefix, not only the
// final endpoint: a nested reparse route whose intermediate target lives
// under a directory outside either endpoint's parents passes through that
// directory, and without SeChangeNotifyPrivilege the token needs
// FILE_TRAVERSE there.
func TestSCMPathRouteChecksEveryAncestorOfAResolvedReparseRoute(t *testing.T) {
	root := t.TempDir()
	sourceDir := filepath.Join(root, "source")
	deniedDir := filepath.Join(root, "denied")
	middleDir := filepath.Join(deniedDir, "middle")
	targetDir := filepath.Join(root, "target")
	for _, dir := range []string{sourceDir, deniedDir, middleDir, targetDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("MkdirAll(%q): %v", dir, err)
		}
	}
	// source/junction -> denied/middle, denied/middle/nested -> target: the
	// route to the file passes through denied, which is an ancestor of
	// neither the source path nor the resolved endpoint.
	firstLink := filepath.Join(sourceDir, "junction")
	scmTestCreateDirectorySymlink(t, middleDir, firstLink)
	nestedLink := filepath.Join(middleDir, "nested")
	scmTestCreateDirectorySymlink(t, targetDir, nestedLink)
	sourcePath := filepath.Join(firstLink, "nested", "hfdesk.exe")
	if err := os.WriteFile(filepath.Join(targetDir, "hfdesk.exe"), []byte("test image"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	resolvedDenied, err := scmResolvePath(deniedDir)
	if err != nil {
		t.Fatalf("resolve the intermediate ancestor %q: %v", deniedDir, err)
	}
	denyTraverse := func(path string, desired uint32) bool {
		return desired&windows.FILE_TRAVERSE != 0 &&
			strings.EqualFold(filepath.Clean(path), filepath.Clean(resolvedDenied))
	}

	// Without the traverse privilege the intermediate ancestor is checked
	// and its denial stops the route.
	access := &scmTestPathAccess{
		check: func(path string, desired uint32) (bool, error) {
			return !denyTraverse(path, desired), nil
		},
	}
	decision, reason := scmCheckPathAccess(sourcePath, windows.FILE_EXECUTE, access)
	if decision != scmExecutableAccessDenied || !scmTestNamesPath(reason, resolvedDenied) {
		t.Fatalf("route with a denied intermediate ancestor = %v, %q; want the traversal of %q denied",
			decision, reason, resolvedDenied)
	}

	// The other side: the same route passes when the ancestor grants
	// traversal.
	allowed := &scmTestPathAccess{
		check: func(string, uint32) (bool, error) { return true, nil },
	}
	if decision, reason := scmCheckPathAccess(sourcePath, windows.FILE_EXECUTE, allowed); decision != scmExecutableAccessAllowed {
		t.Fatalf("route with a granted intermediate ancestor = %v, %q; want allowed", decision, reason)
	}

	// The privilege keeps its semantics: with SeChangeNotifyPrivilege
	// enabled the traversal route is not required.
	bypass := &scmTestPathAccess{
		check: func(path string, desired uint32) (bool, error) {
			return !denyTraverse(path, desired), nil
		},
		privilege: true,
	}
	if decision, reason := scmCheckPathAccess(sourcePath, windows.FILE_EXECUTE, bypass); decision != scmExecutableAccessAllowed {
		t.Fatalf("route with the traverse-bypass privilege = %v, %q; want allowed without the traversal", decision, reason)
	}
}

func TestSCMPathAccessHonorsTraversePrivilegeAndUnknownEvidence(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "parent")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatalf("Mkdir(%q): %v", parent, err)
	}
	path := filepath.Join(parent, "hfdesk.exe")
	if err := os.WriteFile(path, []byte("test image"), 0o600); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
	resolved, err := scmResolvePath(path)
	if err != nil {
		t.Fatalf("resolve executable %q: %v", path, err)
	}
	resolvedParent, err := scmResolvePath(parent)
	if err != nil {
		t.Fatalf("resolve parent %q: %v", parent, err)
	}
	withPrivilege := &scmTestPathAccess{
		results:   map[string]bool{resolved: true, resolvedParent: false},
		privilege: true,
	}
	decision, _ := scmCheckPathAccess(path, windows.GENERIC_READ|windows.GENERIC_EXECUTE, withPrivilege)
	if decision != scmExecutableAccessAllowed {
		t.Fatalf("a denied parent traverse ACE overrode enabled SeChangeNotifyPrivilege: decision %v", decision)
	}
	if len(withPrivilege.calls) != 1 || !strings.EqualFold(filepath.Clean(withPrivilege.calls[0].path), filepath.Clean(resolved)) {
		t.Errorf("AccessCheck calls = %+v, want only the executable object when traverse bypass is enabled", withPrivilege.calls)
	}

	withoutPrivilege := &scmTestPathAccess{
		results: map[string]bool{resolved: true, resolvedParent: false},
	}
	decision, _ = scmCheckPathAccess(path, windows.GENERIC_READ|windows.GENERIC_EXECUTE, withoutPrivilege)
	if decision != scmExecutableAccessDenied {
		t.Errorf("a real parent traverse denial without the bypass privilege = %v, want denied", decision)
	}

	decision, _ = scmCheckPathAccess(path, windows.GENERIC_READ|windows.GENERIC_EXECUTE, nil)
	if decision != scmExecutableAccessInconclusive {
		t.Errorf("missing token evidence = %v, want inconclusive", decision)
	}
	unknown := &scmTestPathAccess{errors: map[string]error{resolved: errors.New("descriptor unavailable")}}
	decision, _ = scmCheckPathAccess(path, windows.GENERIC_READ|windows.GENERIC_EXECUTE, unknown)
	if decision != scmExecutableAccessInconclusive {
		t.Errorf("unavailable descriptor evidence = %v, want inconclusive", decision)
	}
	privilegeUnknown := &scmTestPathAccess{
		results:      map[string]bool{resolved: true},
		privilegeErr: errors.New("token privileges unavailable"),
	}
	decision, _ = scmCheckPathAccess(path, windows.GENERIC_READ|windows.GENERIC_EXECUTE, privilegeUnknown)
	if decision != scmExecutableAccessInconclusive {
		t.Errorf("unavailable traverse-privilege evidence = %v, want inconclusive", decision)
	}
}

func TestSCMPathAccessChecksTraversalOfAWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	resolved, err := scmResolvePath(dir)
	if err != nil {
		t.Fatalf("resolve directory %q: %v", dir, err)
	}
	access := &scmTestPathAccess{
		defaultSet:   true,
		defaultAllow: true,
		resultsByDesired: map[string]map[uint32]bool{
			resolved: {
				windows.GENERIC_READ | windows.GENERIC_WRITE: true,
				windows.FILE_TRAVERSE:                        false,
			},
		},
	}
	decision, reason := scmCheckPathAccess(dir, windows.GENERIC_READ|windows.GENERIC_WRITE, access)
	if decision != scmExecutableAccessDenied || !scmTestNamesPath(reason, resolved) {
		t.Fatalf("a working directory without traverse access = %v, %q; want the actual directory denial", decision, reason)
	}
}

func TestSCMDataDirectoryPreflightUsesTheSameAccessCheckOwner(t *testing.T) {
	dir := t.TempDir()
	denied := scmTestPathAccessFor(t, dir, false)
	err := preflightDataPath("cache dir", dir, denied)
	if err == nil || !strings.Contains(err.Error(), "Windows AccessCheck denied") {
		t.Fatalf("preflightDataPath() = %v, want the actual AccessCheck denial", err)
	}
	unknown := preflightDataPath("cache dir", dir)
	if unknown != nil {
		t.Fatalf("preflightDataPath() without a service token = %v, want inconclusive success", unknown)
	}
}

// TestPreflightExecutableImageRefusesAFileThatIsNotAProgram proves that an
// installation refuses a file that is not a Windows program: a text file
// renamed to hfdesk.exe is not a program the service control manager could
// start.
func TestPreflightExecutableImageRefusesAFileThatIsNotAProgram(t *testing.T) {
	path := scmTestExecutable(t, []byte(strings.Repeat("this is not a Windows program\n", 8)))

	err := preflightExecutableImage(path)
	if err == nil {
		t.Fatal("preflightExecutableImage accepted a file that is not a Windows program")
	}
	if !scmTestNamesPath(err.Error(), path) {
		t.Errorf("preflightExecutableImage(%q) = %v, want an error that names the file", path, err)
	}
	if !strings.Contains(err.Error(), "not a Windows executable") {
		t.Errorf("preflightExecutableImage(%q) = %v, want an error that says the file is not a Windows executable", path, err)
	}
}

// TestPreflightExecutableImageRefusesATruncatedProgram proves that a program
// whose header was cut off is refused: a download that ended in the middle of
// the header is not a program the service control manager could start.
func TestPreflightExecutableImageRefusesATruncatedProgram(t *testing.T) {
	image := scmTestPEImage(pe.MachineAMD64)
	// The image ends before the PE header its DOS header points at.
	path := scmTestExecutable(t, image[:scmTestDosHeaderSize])

	err := preflightExecutableImage(path)
	if err == nil {
		t.Fatal("preflightExecutableImage accepted a truncated program")
	}
	if !strings.Contains(err.Error(), "unexpected EOF") {
		t.Errorf("preflightExecutableImage(%q) = %v, want the truncated header to be reported", path, err)
	}
}

// TestPreflightExecutableImageUsesTheHostSupportResult proves that candidate
// acceptance follows Windows host support rather than the architecture of the
// installer process.
func TestPreflightExecutableImageUsesTheHostSupportResult(t *testing.T) {
	cases := []struct {
		name    string
		machine uint16
		support scmExecutableSupport
		wantErr bool
	}{
		{"host-supported native image", pe.MachineAMD64, scmExecutableSupported, false},
		{"host-supported emulated image", pe.MachineARM64, scmExecutableSupported, false},
		{"known unsupported image", pe.MachineI386, scmExecutableUnsupported, true},
		{"unknown host support is not incompatibility", pe.MachineARM64, scmExecutableSupportUnknown, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := scmTestExecutable(t, scmTestPEImage(tc.machine))

			err := preflightExecutableImageWithSupport(path, func(uint16) scmExecutableSupport {
				return tc.support
			})
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("preflightExecutableImageWithSupport(%q) = %v, want nil", path, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("preflightExecutableImageWithSupport(%q) = nil, want the refusal of machine 0x%04X", path, tc.machine)
			}
			if !scmTestNamesPath(err.Error(), path) {
				t.Errorf("preflightExecutableImageWithSupport(%q) = %v, want an error that names the file", path, err)
			}
			if !strings.Contains(err.Error(), "reports this host cannot execute") {
				t.Errorf("preflightExecutableImageWithSupport(%q) = %v, want a known host incompatibility", path, err)
			}
		})
	}
}

func TestPreflightExecutableImageAcceptsTheActualWindowsTestExecutable(t *testing.T) {
	path, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable() = %v", err)
	}
	if err := preflightExecutableImage(path); err != nil {
		t.Fatalf("preflightExecutableImage(%q) = %v, want the actual running test image to pass", path, err)
	}
}

func TestPreflightExecutableImageRequiresACompletePEExecutable(t *testing.T) {
	path := scmTestExecutable(t, []byte(strings.Repeat("this is not a Windows program\n", 8)))

	if err := preflightExecutableImageWithSupport(path, nil); err == nil {
		t.Fatal("preflightExecutableImageWithSupport accepted a file that is not a Windows program")
	}
}

func TestPreflightExecutableImageSeparatesInspectionAccessFromDamage(t *testing.T) {
	for _, readErr := range []error{windows.ERROR_ACCESS_DENIED, fmt.Errorf("read optional header: %w", fs.ErrPermission)} {
		supportQueried := false
		err := preflightExecutableImageUsing(`C:\Restricted\hfdesk.exe`,
			func(string) (uint16, error) { return 0, readErr },
			func(uint16) scmExecutableSupport {
				supportQueried = true
				return scmExecutableUnsupported
			})
		if err != nil {
			t.Errorf("preflightExecutableImageUsing(access error) = %v, want inconclusive success", err)
		}
		if supportQueried {
			t.Error("host support was queried without a machine field")
		}
	}

	structureErr := errors.New("truncated optional header")
	err := preflightExecutableImageUsing(`C:\Restricted\hfdesk.exe`,
		func(string) (uint16, error) { return 0, structureErr }, nil)
	if err == nil || !errors.Is(err, structureErr) || !strings.Contains(err.Error(), "not a Windows executable") {
		t.Errorf("preflightExecutableImageUsing(structural error) = %v, want the invalid-image refusal", err)
	}

	missing := filepath.Join(t.TempDir(), "missing.exe")
	if err := preflightExecutableImage(missing); err == nil || !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("preflightExecutableImage(%q) = %v, want the demonstrably missing candidate refused", missing, err)
	}
}

// TestApplyRegistrationReportsTheEffectiveCacheDir checks that the status
// reports the cache root the instance uses: the cache directory of the
// config file of the instance wins over the registered one.
func TestApplyRegistrationReportsTheEffectiveCacheDir(t *testing.T) {
	root := t.TempDir()
	configFile := filepath.Join(root, "hfdesk.json")
	registered := filepath.ToSlash(filepath.Join(root, "registered-cache"))
	fromConfig := filepath.ToSlash(filepath.Join(root, "cache-of-the-config-file"))

	opts := scmTestOptions()
	opts.Paths.ConfigFile = configFile
	opts.CacheDir = registered

	registration, err := parseSCMCommandLine(scmCommandLine(opts))
	if err != nil {
		t.Fatalf("parseSCMCommandLine() = %v, want nil", err)
	}

	status := Status{}
	applyRegistration(&status, registration)
	if status.CacheDir != registered {
		t.Errorf("the cache dir = %q, want the registered %q", status.CacheDir, registered)
	}
	if status.Paths.ConfigFile != configFile {
		t.Errorf("the config file = %q, want %q", status.Paths.ConfigFile, configFile)
	}

	// The config file of the instance wins over the registration.
	if err := os.WriteFile(configFile, []byte(`{"cache-dir":"`+fromConfig+`"}`), 0o600); err != nil {
		t.Fatalf("WriteFile() = %v, want nil", err)
	}
	status = Status{}
	applyRegistration(&status, registration)
	if status.CacheDir != fromConfig {
		t.Errorf("the cache dir = %q, want the cache dir of the config file %q", status.CacheDir, fromConfig)
	}

	// A registration without a cache root and a config file without one
	// report no cache root at all.
	opts.CacheDir = ""
	opts.Paths.ConfigFile = filepath.Join(root, "without-cache.json")
	registration, err = parseSCMCommandLine(scmCommandLine(opts))
	if err != nil {
		t.Fatalf("parseSCMCommandLine() = %v, want nil", err)
	}
	status = Status{}
	applyRegistration(&status, registration)
	if status.CacheDir != "" {
		t.Errorf("the cache dir = %q, want none", status.CacheDir)
	}
}

// TestInstanceStatePathsTellTheCreatedPathsFromTheExistingOnes checks that
// the installation knows which paths of the instance it owns: the
// directories it creates carry the restrictive descriptor, the paths that
// existed before the installation do not.
//
// A directory that existed before the installation belongs to whoever owns
// it, so the installation reads its descriptor and changes nothing.
func TestInstanceStatePathsTellTheCreatedPathsFromTheExistingOnes(t *testing.T) {
	root := t.TempDir()
	opts := scmTestOptions()
	opts.Paths = runtimepaths.Paths{
		ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
		StateDir:   filepath.Join(root, "var"),
		LogDir:     filepath.Join(root, "var", "logs"),
	}
	opts.CacheDir = filepath.Join(root, "cache")
	opts.HubDir = filepath.Join(root, "cache", "hub")
	opts.LocalDir = filepath.Join(root, "models")

	// The config directory and the config file existed before the
	// installation.
	if err := os.MkdirAll(filepath.Dir(opts.Paths.ConfigFile), 0o700); err != nil {
		t.Fatalf("MkdirAll() = %v, want nil", err)
	}
	if err := os.WriteFile(opts.Paths.ConfigFile, []byte("{}"), 0o600); err != nil {
		t.Fatalf("WriteFile() = %v, want nil", err)
	}

	snapshot := newSCMInstallSnapshot(opts)
	paths := instanceStatePaths(opts, snapshot)

	created := make(map[string]bool, len(paths))
	existing := make(map[string]bool, len(paths))
	for _, path := range paths {
		if path.created {
			created[path.path] = true
			continue
		}
		existing[path.path] = true
	}

	// The directories the installation creates are the state directory
	// and the log directory: the config directory existed already.
	for _, want := range []string{opts.Paths.StateDir, opts.Paths.LogDir} {
		if !created[want] {
			t.Errorf("the state directory %q is not marked as created: %v", want, paths)
		}
	}
	for _, want := range []string{filepath.Dir(opts.Paths.ConfigFile), opts.Paths.ConfigFile} {
		if !existing[want] {
			t.Errorf("the path %q is not marked as existing: %v", want, paths)
		}
	}
	for path := range created {
		if existing[path] {
			t.Errorf("the path %q is marked as created and as existing: %v", path, paths)
		}
	}
}

// scmTestCurrentUserSID returns the SID of the account the test runs as.
//
// The account is needed by the tests that build an access control list: the
// list has to keep the access of the test itself, so the test can still
// read the directory it restricted.
func scmTestCurrentUserSID(t *testing.T) string {
	t.Helper()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Skipf("the account of the process is unknown: %v", err)
	}
	if user.User.Sid == nil {
		t.Skip("the account of the process carries no SID")
	}
	return user.User.Sid.String()
}

// scmTestAliases maps the aliases of the trustees of an access control
// entry onto the SID they stand for.
//
// The platform writes a trustee as an alias or as a raw SID, depending on
// the entry. The tests compare the trustees as SIDs, so the form the
// platform uses does not matter.
var scmTestAliases = map[string]string{
	"SY": "S-1-5-18",     // LocalSystem
	"BA": "S-1-5-32-544", // Administrators
	"LS": "S-1-5-19",     // LocalService
	"WD": "S-1-1-0",      // Everyone
}

// scmTestDACL returns the access control list of a path in its SDDL form.
//
// The descriptor of a path carries the owner and the group next to the
// access control list: only the list is compared, so a change of the access
// rights is told apart from a change of the owner.
func scmTestDACL(t *testing.T, path string) string {
	t.Helper()
	_, permissions := scmTestSecurity(t, path)
	index := strings.Index(permissions, "D:")
	if index < 0 {
		t.Fatalf("the security descriptor of %q = %q, want an access control list", path, permissions)
	}
	return permissions[index:]
}

// sddlACE and its parser are test-only descriptor-inspection helpers. They do
// not classify access; production uses the Windows AccessCheck API.
type sddlACE struct{ trustee string }

func parseSDDLAces(sddl string) []sddlACE {
	var aces []sddlACE
	for i := 0; i < len(sddl); {
		open := strings.IndexByte(sddl[i:], '(')
		if open < 0 {
			break
		}
		i += open + 1
		end := strings.IndexByte(sddl[i:], ')')
		if end < 0 {
			break
		}
		fields := strings.Split(sddl[i:i+end], ";")
		if len(fields) >= 6 {
			if trustee := strings.TrimSpace(fields[len(fields)-1]); trustee != "" {
				aces = append(aces, sddlACE{trustee: trustee})
			}
		}
		i += end + 1
	}
	return aces
}

// scmTestTrustees returns the trustees of the access control list of a path.
//
// The trustees are returned as SIDs: the aliases of the platform are
// resolved, so the assertions do not depend on the form the platform writes
// them in.
func scmTestTrustees(t *testing.T, path string) []string {
	t.Helper()
	var trustees []string
	for _, ace := range parseSDDLAces(scmTestDACL(t, path)) {
		trustee := strings.ToUpper(strings.TrimSpace(ace.trustee))
		if sid, ok := scmTestAliases[trustee]; ok {
			trustee = sid
		}
		trustees = append(trustees, trustee)
	}
	return trustees
}

// scmTestDACLProtected reports whether the access control list of a path
// blocks the inheritance of its parent directory.
//
// The flag is read from the descriptor of the path itself and not from its
// SDDL form, so the assertion does not depend on the way the platform writes
// the flag.
func scmTestDACLProtected(t *testing.T, path string) bool {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		t.Fatalf("the security descriptor of %q cannot be read: %v", path, err)
	}
	control, _, err := sd.Control()
	if err != nil {
		t.Fatalf("the control bits of %q cannot be read: %v", path, err)
	}
	return control&windows.SE_DACL_PROTECTED != 0
}

// TestSCMInstanceStateRestrictsTheDirectoriesItCreated checks that the
// directories the installation creates carry the restrictive descriptor of
// the instance.
//
// The descriptor is the one of the instance: SYSTEM, the administrators of
// the machine and the account the instance runs as, nobody else, and the
// inheritance of the parent directory is cut off.
func TestSCMInstanceStateRestrictsTheDirectoriesItCreated(t *testing.T) {
	if !isElevatedProcess() {
		t.Skip("an installation needs an elevated shell, run the test as an administrator")
	}
	root := t.TempDir()
	opts := scmTestInstallOptions(t, root)

	snapshot := newSCMInstallSnapshot(opts)
	if err := PrepareDirs(opts); err != nil {
		t.Fatalf("PrepareDirs() = %v, want nil", err)
	}
	if err := restrictInstanceState(opts, snapshot); err != nil {
		t.Fatalf("restrictInstanceState() = %v, want nil", err)
	}

	want := []string{"S-1-5-18", "S-1-5-19", "S-1-5-32-544"}
	for _, dir := range []string{
		filepath.Dir(opts.Paths.ConfigFile),
		opts.Paths.StateDir,
		opts.Paths.LogDir,
	} {
		if !scmTestDACLProtected(t, dir) {
			t.Errorf("the access control list of %q is not protected, want a protected one", dir)
		}
		got := scmTestTrustees(t, dir)
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("the trustees of %q = %v, want %v", dir, got, want)
		}
	}
}

// TestSCMInstanceStateKeepsTheDescriptorOfADirectoryThatExistedBefore
// checks that a directory the user named and that existed before the
// installation keeps its descriptor.
//
// The descriptor of the directory grants the access to everybody, so a
// rewrite of it by the restrictive descriptor of the instance would be
// visible in the comparison.
func TestSCMInstanceStateKeepsTheDescriptorOfADirectoryThatExistedBefore(t *testing.T) {
	if !isElevatedProcess() {
		t.Skip("an installation needs an elevated shell, run the test as an administrator")
	}
	root := t.TempDir()
	opts := scmTestInstallOptions(t, root)

	// The state directory existed before the installation.
	stateDir := opts.Paths.StateDir
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		t.Fatalf("MkdirAll(%q) = %v, want nil", stateDir, err)
	}
	scmTestSetDACL(t, stateDir, fmt.Sprintf("D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;WD)"))
	before := scmTestDACL(t, stateDir)

	snapshot := newSCMInstallSnapshot(opts)
	if err := PrepareDirs(opts); err != nil {
		t.Fatalf("PrepareDirs() = %v, want nil", err)
	}
	if err := restrictInstanceState(opts, snapshot); err != nil {
		t.Fatalf("restrictInstanceState() = %v, want nil", err)
	}

	if after := scmTestDACL(t, stateDir); after != before {
		t.Errorf("the access control list of the state directory %q = %q, want %q", stateDir, after, before)
	}
	// The restrictive descriptor of the instance was not applied to it:
	// everybody still has the access the directory had.
	trustees := scmTestTrustees(t, stateDir)
	if !slices.Contains(trustees, "S-1-1-0") {
		t.Errorf("the trustees of the state directory %q = %v, want everybody", stateDir, trustees)
	}
}

// TestSCMInstanceStateRefusesADirectoryTheIdentityCannotUse checks that an
// established AccessCheck denial for an existing state directory is refused,
// and that no descriptor was changed.
//
// The refusal happens before the first descriptor is changed, so the
// directories the installation created keep the descriptor they inherited.
func TestSCMInstanceStateRefusesADirectoryTheIdentityCannotUse(t *testing.T) {
	if !isElevatedProcess() {
		t.Skip("an installation needs an elevated shell, run the test as an administrator")
	}
	root := t.TempDir()
	opts := scmTestInstallOptions(t, root)

	// The state directory existed before the installation. Access denial is
	// injected through the native-query seam, without changing its ACL.
	stateDir := opts.Paths.StateDir
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		t.Fatalf("MkdirAll(%q) = %v, want nil", stateDir, err)
	}
	// The path access result is injected below; no ACL is changed by this test.

	snapshot := newSCMInstallSnapshot(opts)
	if err := PrepareDirs(opts); err != nil {
		t.Fatalf("PrepareDirs() = %v, want nil", err)
	}
	before := make(map[string]string, 3)
	for _, path := range []string{
		stateDir,
		filepath.Dir(opts.Paths.ConfigFile),
		opts.Paths.LogDir,
	} {
		before[path] = scmTestDACL(t, path)
	}

	err := restrictInstanceState(opts, snapshot, scmTestPathAccessFor(t, stateDir, false))
	if err == nil {
		t.Fatal("restrictInstanceState() = nil, want the refusal of the state directory")
	}
	if !scmTestNamesPath(err.Error(), stateDir) {
		t.Errorf("restrictInstanceState() = %v, want an error that names the path", err)
	}
	if !strings.Contains(err.Error(), scmServiceIdentityName) {
		t.Errorf("restrictInstanceState() = %v, want an error that names the account of the instance", err)
	}
	if !strings.Contains(err.Error(), "grant the account") {
		t.Errorf("restrictInstanceState() = %v, want an error that tells the user what to do", err)
	}

	// No descriptor was changed anywhere: not on the directory that
	// existed before and not on the directories the installation
	// created.
	for path, want := range before {
		if got := scmTestDACL(t, path); got != want {
			t.Errorf("the access control list of %q = %q, want %q", path, got, want)
		}
	}
}

// TestSCMInstanceStateKeepsTheDescriptorOfAConfigFileThatExistedBefore
// checks that the config file of the instance keeps its descriptor when it
// existed before the installation.
//
// The config file holds the credentials of the instance, but it belongs to
// whoever wrote it: the installation reads its descriptor and changes
// nothing.
func TestSCMInstanceStateKeepsTheDescriptorOfAConfigFileThatExistedBefore(t *testing.T) {
	if !isElevatedProcess() {
		t.Skip("an installation needs an elevated shell, run the test as an administrator")
	}
	root := t.TempDir()
	opts := scmTestInstallOptions(t, root)

	// The config directory and the config file existed before the
	// installation.
	configDir := filepath.Dir(opts.Paths.ConfigFile)
	if err := os.MkdirAll(configDir, 0o750); err != nil {
		t.Fatalf("MkdirAll(%q) = %v, want nil", configDir, err)
	}
	// The directory has to be usable by the account of the instance,
	// otherwise the installation refuses it.
	scmTestSetDACL(t, configDir, scmTestAllowInstanceIdentityDACL(t))
	if err := os.WriteFile(opts.Paths.ConfigFile, []byte("{}"), 0o600); err != nil {
		t.Fatalf("WriteFile() = %v, want nil", err)
	}
	scmTestSetDACL(t, opts.Paths.ConfigFile, fmt.Sprintf("D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;WD)"))
	before := scmTestDACL(t, opts.Paths.ConfigFile)

	snapshot := newSCMInstallSnapshot(opts)
	if err := PrepareDirs(opts); err != nil {
		t.Fatalf("PrepareDirs() = %v, want nil", err)
	}
	if err := restrictInstanceState(opts, snapshot); err != nil {
		t.Fatalf("restrictInstanceState() = %v, want nil", err)
	}

	if after := scmTestDACL(t, opts.Paths.ConfigFile); after != before {
		t.Errorf("the access control list of the config file %q = %q, want %q", opts.Paths.ConfigFile, after, before)
	}
}

// TestSCMInstanceStateKeepsTheDescriptorOfTheReusedDirectories checks that
// the directories the installation reuses keep their descriptor.
//
// The cache root, the hub directory and the local output directory can hold
// the models of the user: the installation reads their descriptor and never
// changes it.
func TestSCMInstanceStateKeepsTheDescriptorOfTheReusedDirectories(t *testing.T) {
	if !isElevatedProcess() {
		t.Skip("an installation needs an elevated shell, run the test as an administrator")
	}
	root := t.TempDir()
	opts := scmTestInstallOptions(t, root)

	// The reused directories existed before the installation. The
	// descriptor of every one of them is protected, so the inheritance
	// of the platform cannot change it behind the back of the test.
	reused := []string{opts.CacheDir, opts.HubDir, opts.LocalDir}
	before := make(map[string]string, len(reused))
	for _, dir := range reused {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("MkdirAll(%q) = %v, want nil", dir, err)
		}
		scmTestSetDACL(t, dir, scmTestAllowInstanceIdentityDACL(t))
		before[dir] = scmTestDACL(t, dir)
	}
	// The cache root lives in the state directory of the instance, so the
	// state directory existed before the installation as well: it has to
	// be usable by the account of the instance.
	scmTestSetDACL(t, opts.Paths.StateDir, scmTestAllowInstanceIdentityDACL(t))

	snapshot := newSCMInstallSnapshot(opts)
	if err := PrepareDirs(opts); err != nil {
		t.Fatalf("PrepareDirs() = %v, want nil", err)
	}
	if err := restrictInstanceState(opts, snapshot); err != nil {
		t.Fatalf("restrictInstanceState() = %v, want nil", err)
	}
	// The preflight reads the reused directories as well.
	if err := preflightDataPaths(opts); err != nil {
		t.Fatalf("preflightDataPaths() = %v, want nil", err)
	}

	for _, dir := range reused {
		if after := scmTestDACL(t, dir); after != before[dir] {
			t.Errorf("the access control list of the reused directory %q = %q, want %q", dir, after, before[dir])
		}
	}
}

// scmTestSetDACL applies an access control list to a path.
//
// The descriptor is applied to the path itself and never recursively.
func scmTestSetDACL(t *testing.T, path, sddl string) {
	t.Helper()
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatalf("SecurityDescriptorFromString(%q) = %v, want nil", sddl, err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatalf("the access control list of %q cannot be read: %v", sddl, err)
	}
	if err := windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		dacl,
		nil,
	); err != nil {
		t.Fatalf("the access control list %q cannot be applied to %q: %v", sddl, path, err)
	}
}

// The layout of the structurally complete PE image the tests synthesize.
const (
	// scmTestDosHeaderSize is the size of the DOS header, the part of an
	// image the offset of the PE header is read from.
	scmTestDosHeaderSize = 64
	// scmTestPeHeaderOffset is the offset of the PE header in the
	// synthesized image.
	scmTestPeHeaderOffset       = 0x80
	scmTestOptionalHeaderSize   = 240
	scmTestOptionalHeader32Size = 224
	scmTestSectionHeaderSize    = 40
	scmTestSectionRawOffset     = 0x200
	scmTestSectionRawSize       = 0x200
)

// scmTestPEImage builds a structurally complete PE image with one section.
// It exercises header validation, not actual Windows loadability.
func scmTestPEImage(machine uint16) []byte {
	image := make([]byte, scmTestSectionRawOffset+scmTestSectionRawSize)
	optionalSize := scmTestOptionalHeaderSize
	magic := uint16(0x020B)
	if machine == pe.MachineI386 {
		optionalSize = scmTestOptionalHeader32Size
		magic = 0x010B
	}
	copy(image[0:], []byte("MZ"))
	binary.LittleEndian.PutUint32(image[0x3C:], scmTestPeHeaderOffset)
	copy(image[scmTestPeHeaderOffset:], []byte("PE\x00\x00"))
	binary.LittleEndian.PutUint16(image[scmTestPeHeaderOffset+4:], machine)
	binary.LittleEndian.PutUint16(image[scmTestPeHeaderOffset+4+2:], 1)
	binary.LittleEndian.PutUint16(image[scmTestPeHeaderOffset+4+16:], uint16(optionalSize))
	binary.LittleEndian.PutUint16(image[scmTestPeHeaderOffset+4+18:], 0x0002)
	optional := scmTestPeHeaderOffset + 24
	binary.LittleEndian.PutUint16(image[optional:], magic)
	binary.LittleEndian.PutUint32(image[optional+16:], 0x1000)
	section := optional + optionalSize
	copy(image[section:], []byte(".text"))
	binary.LittleEndian.PutUint32(image[section+8:], scmTestSectionRawSize)
	binary.LittleEndian.PutUint32(image[section+12:], 0x1000)
	binary.LittleEndian.PutUint32(image[section+16:], scmTestSectionRawSize)
	binary.LittleEndian.PutUint32(image[section+20:], scmTestSectionRawOffset)
	return image
}

// scmTestExecutable writes data as hfdesk.exe into a temporary directory and
// returns its path.
func scmTestExecutable(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hfdesk.exe")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %q: %v", path, err)
	}
	return path
}

// scmTestInstallOptions returns the options of an installation whose
// directories live below the given root.
//
// The executable is a program of the operating system: the preflight needs a
// regular file it can stat. The cache root lives in the state directory of
// the instance, the way the default paths of a machine-wide instance build
// it.
func scmTestInstallOptions(t *testing.T, root string) InstallOptions {
	t.Helper()
	executable := filepath.Join("C:\\Windows", "System32", "cmd.exe")
	if _, err := os.Stat(executable); err != nil {
		t.Skipf("the test program %s is not available: %v", executable, err)
	}
	// The ACL of the system program is not the subject of the tests that
	// install it: the access check has tests of its own.
	previousAccess := scmCheckExecutableAccess
	scmCheckExecutableAccess = func(string, scmPathAccess) (scmExecutableAccess, string) {
		return scmExecutableAccessAllowed, ""
	}
	t.Cleanup(func() { scmCheckExecutableAccess = previousAccess })
	return InstallOptions{
		Scope:      ScopeSystem,
		Executable: executable,
		Listen:     DefaultListen,
		Paths: runtimepaths.Paths{
			ConfigFile: filepath.Join(root, "etc", "hfdesk", "hfdesk.json"),
			StateDir:   filepath.Join(root, "var", "lib", "hfdesk"),
			LogDir:     filepath.Join(root, "var", "log", "hfdesk"),
		},
		CacheDir: filepath.Join(root, "var", "lib", "hfdesk", "cache"),
		HubDir:   filepath.Join(root, "var", "lib", "hfdesk", "cache", "hub"),
		LocalDir: filepath.Join(root, "models"),
	}
}

// scmTestAllowInstanceIdentityDACL returns a descriptor used by tests that
// inspect state-directory ACL changes. It does not substitute for the
// effective-token AccessCheck used by installation preflight.
func scmTestAllowInstanceIdentityDACL(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("%s(A;OICI;FA;;;%s)", scmStateSDDL(), scmTestCurrentUserSID(t))
}

// scmTestSecurity returns the owner and the permissions of a path.
//
// The owner is the account that owns the path, the permissions are the
// security descriptor of the path in its SDDL form.
func scmTestSecurity(t *testing.T, path string) (string, string) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.OWNER_SECURITY_INFORMATION|windows.GROUP_SECURITY_INFORMATION,
	)
	if err != nil {
		t.Fatalf("the security descriptor of %q cannot be read: %v", path, err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		t.Fatalf("the owner of %q cannot be read: %v", path, err)
	}
	if owner == nil {
		t.Fatalf("the owner of %q is unknown", path)
	}
	return owner.String(), sd.String()
}

// scmTestOwnerAndMode returns the owner and the mode of a path.
//
// The owner is the account that owns the path, the mode are the permission
// bits of the path.
func scmTestOwnerAndMode(t *testing.T, path string) (string, os.FileMode) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION,
	)
	if err != nil {
		t.Fatalf("the owner of %q cannot be read: %v", path, err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		t.Fatalf("the owner of %q cannot be read: %v", path, err)
	}
	if owner == nil {
		t.Fatalf("the owner of %q is unknown", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("the mode of %q cannot be read: %v", path, err)
	}
	return owner.String(), info.Mode().Perm()
}

// scmTestRefuseLocalDir injects an established AccessCheck denial without
// mutating the ACL of the local directory.
//
// The installation fails after creating its own directories, exercising that
// cleanup boundary before any live SCM operation.
func scmTestRefuseLocalDir(t *testing.T, opts InstallOptions) *scmTestPathAccess {
	t.Helper()
	if err := os.MkdirAll(opts.LocalDir, 0o700); err != nil {
		t.Fatalf("MkdirAll(%q) = %v, want nil", opts.LocalDir, err)
	}
	access := scmTestPathAccessFor(t, opts.LocalDir, false)
	previous := scmOpenPathAccess
	scmOpenPathAccess = func(uint32) (scmPathAccess, error) {
		return access, nil
	}
	t.Cleanup(func() { scmOpenPathAccess = previous })
	return access
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
	if err := os.MkdirAll(configDir, 0o750); err != nil {
		t.Fatalf("MkdirAll(%q) = %v, want nil", configDir, err)
	}
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
	if err != nil {
		t.Fatalf("the local directory %q is gone after the failed installation: %v", opts.LocalDir, err)
	}
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
	if err := os.Mkdir(holding, 0o700); err != nil {
		t.Fatalf("Mkdir() = %v, want nil", err)
	}
	marker := filepath.Join(holding, "marker")
	if err := os.WriteFile(marker, []byte("data"), 0o600); err != nil {
		t.Fatalf("WriteFile() = %v, want nil", err)
	}
	empty := filepath.Join(root, "empty")
	if err := os.Mkdir(empty, 0o700); err != nil {
		t.Fatalf("Mkdir() = %v, want nil", err)
	}

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
	if err != nil {
		t.Fatalf("scmStatusFrom() = %v, want a readable broken registration", err)
	}
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
				if err := configureSCMRecoveryWith(config); err != nil {
					t.Fatalf("configureSCMRecoveryWith() = %v, want the custom policy preserved", err)
				}
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
		if err := configureSCMRecoveryWith(config); err != nil {
			t.Fatalf("configureSCMRecoveryWith() = %v, want defaults configured", err)
		}
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
		if err := configureSCMRecoveryWith(config); err != nil {
			t.Fatalf("configureSCMRecoveryWith() = %v, want defaults configured", err)
		}
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
		if err := configureSCMRecoveryWith(config); err != nil {
			t.Fatalf("configureSCMRecoveryWith() = %v, want the custom policy untouched", err)
		}
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
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("updateRegistrationConfig() = %q, want it to mention %q", err, want)
				}
			}
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
	if err != nil {
		t.Fatalf("updateRegistrationConfig() = %v, want nil", err)
	}
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

// desktopVerifyBudget is the independent outer wait of the desktop service
// helper (internal/desktop/service_helper_windows.go, serviceVerifyBudget).
//
// The desktop package imports this one, so this test cannot read the value back
// without an import cycle; the number is repeated here to prove that the
// verification of a single start still fits inside the wait that wraps an
// update.
const desktopVerifyBudget = 60 * time.Second

// TestSCMVerifyWaitCoversTheReadinessWait checks that the wait for a restarted
// instance to run the registered program is sized above the readiness wait of
// the instance.
//
// The service control manager reports a started instance as running only after
// the instance itself waited for its HTTP server, so a verification that gave up
// at the readiness budget would fail a start that is still inside its own
// budget. The wait is derived from that budget, and it stays inside the
// independent outer wait of the desktop helper that wraps an update.
func TestSCMVerifyWaitCoversTheReadinessWait(t *testing.T) {
	if scmVerifyTimeout <= scmReadinessTimeout {
		t.Errorf("the verify wait of the manager = %s, want more than the readiness budget %s: the instance reports the running state only after it waited for its HTTP server", scmVerifyTimeout, scmReadinessTimeout)
	}
	if scmVerifyTimeout > desktopVerifyBudget {
		t.Errorf("the verify wait of the manager = %s, want at most the outer wait %s of the desktop service helper", scmVerifyTimeout, desktopVerifyBudget)
	}
	if scmVerifyPollInterval <= 0 || scmVerifyPollInterval > scmVerifyTimeout {
		t.Errorf("the verify poll interval = %s, want a positive pause inside the wait %s", scmVerifyPollInterval, scmVerifyTimeout)
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

func replaceSCMTestSeam[T any](t *testing.T, seam *T, value T) {
	t.Helper()
	previous := *seam
	*seam = value
	t.Cleanup(func() { *seam = previous })
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
	if err != nil {
		t.Fatalf("listen for the real health probe: %v", err)
	}
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
	if err != nil {
		t.Fatalf("listen for the readiness response: %v", err)
	}
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
	if err != nil {
		t.Fatalf("listen for the closed readiness address: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("close the readiness listener: %v", err)
	}
	return address
}

func scmTestFirstInstallOptions(t *testing.T, listen, expectedVersion string) InstallOptions {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("read the actual Windows test executable: %v", err)
	}
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
			if err != nil {
				t.Fatalf("startFirstRegistration() = %v, want success", err)
			}
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

func TestSCMRollbackStartVerificationRemainsImageOnly(t *testing.T) {
	opts := scmTestOptions()
	probed := false
	replaceSCMTestSeam(t, &scmStartService, func(*mgr.Service) error { return nil })
	replaceSCMTestSeam(t, &scmQueryStatus, func(*mgr.Service) (svc.Status, error) {
		return svc.Status{State: svc.Running, ProcessId: 8142}, nil
	})
	replaceSCMTestSeam(t, &scmQueryProcessImageName, func(uint32) (string, error) {
		return opts.Executable, nil
	})
	replaceSCMTestSeam(t, &serviceInstallProbe, func(string, string) (bool, error) {
		probed = true
		return false, errors.New("readiness is not part of rollback image proof")
	})

	image, err := (&scmManager{scope: ScopeSystem}).startAndVerify(
		context.Background(), &mgr.Service{}, opts.Executable)
	if err != nil || image != scmImageVerified {
		t.Fatalf("startAndVerify() = %v, %v, want only the verified restored image", image, err)
	}
	if probed {
		t.Error("rollback image verification unexpectedly required HTTP readiness")
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
			if err != nil {
				t.Fatalf("startAndVerifyVersion() = %v, want readiness success", err)
			}
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
	if err != nil {
		t.Fatalf("read the actual Windows test executable: %v", err)
	}
	other, err := os.Executable()
	if err != nil {
		t.Fatalf("read the actual Windows test executable: %v", err)
	}
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

// TestSCMVerifyRunningExecutableKeepsIndependentImageAndProbeEvidence checks
// that readiness failure reaches the timeout while a verified process image
// remains a separate fact.
//
// The service control manager reports the instance as running with the image of
// the test process, so the probe is the only step that fails. The error has to
// name the address and the probe failure: a verification that keeps only the
// empty reason of a settled image read hides why the instance never came up.
func TestSCMVerifyRunningExecutableKeepsTheProbeCause(t *testing.T) {
	processID := uint32(os.Getpid())
	executable, err := queryProcessImageName(processID)
	if err != nil {
		t.Fatalf("the image of the test process: %v", err)
	}

	previousQuery := scmQueryStatus
	t.Cleanup(func() { scmQueryStatus = previousQuery })
	scmQueryStatus = func(*mgr.Service) (svc.Status, error) {
		return svc.Status{State: svc.Running, ProcessId: processID}, nil
	}

	previousProbe := serviceInstallProbe
	t.Cleanup(func() { serviceInstallProbe = previousProbe })
	probeErr := errors.New("the health endpoint did not answer")
	probed := false
	serviceInstallProbe = func(string, string) (bool, error) {
		probed = true
		return false, probeErr
	}

	shortenSCMVerifyBudget(t, 10*time.Millisecond)

	const listen = "127.0.0.1:8080"
	check, verified, err := (&scmManager{scope: ScopeSystem}).verifyRunningExecutableVersion(
		context.Background(), &mgr.Service{}, executable, listen, "1.2.3",
	)
	if err == nil {
		t.Fatal("verifyRunningExecutableVersion() = nil, want the timeout of the instance that does not answer")
	}
	if !probed {
		t.Fatalf("verifyRunningExecutableVersion() = %q before the readiness probe was asked", err)
	}
	if check != scmImageVerified || verified {
		t.Errorf("verifyRunningExecutableVersion() = %d, %t, want verified image evidence and no version claim", check, verified)
	}
	for _, want := range []string{listen, probeErr.Error()} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("verifyRunningExecutableVersion() = %q, want an error that mentions %q", err, want)
		}
	}
}

// scmRollbackSuccessClaim is the phrase that reports the previous instance as in
// service again. It is the claim a restart rollback may only make for a verified
// restart, so the test of the outcome sentences looks for exactly this phrase.
const scmRollbackSuccessClaim = "the previous instance was started again"

// TestSCMRestartRollbackSentence checks that the outcome of a restart rollback is
// only described as far as the rollback proved it.
//
// The sentence is the whole report a caller reads next to the failure of the
// installation, so it is the place the truthfulness rule lives: the previous
// instance is only reported as started again for a verified restart, a
// registration that could not name its program is reported as such, and a
// registration that could not be put back is never reported as restored. The
// stop of the instance of the failed installation comes before the restore, and
// the sentence of a failed stop says so: the registration of the failed
// installation is kept, because its instance may still run.
func TestSCMRestartRollbackSentence(t *testing.T) {
	const stopReason = "the instance did not stop within 2m0s"
	const restoreReason = "the service control manager refused the write of the registration"
	const startReason = `the instance does not run "C:\\ProgramData\\HFDesk\\hfdesk.exe" within 2m0s`
	const readReason = "the service control manager did not answer"

	stopped := scmRestartRollback{stopped: true}
	restored := scmRestartRollback{stopped: true, restored: true}
	programKnown := scmRestartRollback{stopped: true, restored: true, programKnown: true}
	started := scmRestartRollback{stopped: true, restored: true, programKnown: true, started: true}
	undeliveredRestored := scmRestartRollback{attempt: scmRollbackUndeliveredInstance, restored: true}
	undeliveredRunning := scmRestartRollback{attempt: scmRollbackUndeliveredInstance, restored: true, running: true}
	undeliveredStopped := scmRestartRollback{attempt: scmRollbackUndeliveredInstance, stopped: true, restored: true, programKnown: true, started: true, verified: true}

	cases := []struct {
		name string
		// facts are the observations of the rollback up to the step that did
		// not reach its goal.
		facts scmRestartRollback
		// reason is the detail the rollback embeds for a step that failed.
		reason string
		// want contains the phrases the sentence has to carry.
		want []string
		// verified reports whether the sentence may claim the previous
		// instance as started again.
		verified bool
	}{
		{
			name:     "the instance of the failed installation did not stop",
			facts:    scmRestartRollback{},
			reason:   stopReason,
			want:     []string{"was kept", "was not proven to be stopped", stopReason, "the previous registration was not restored"},
			verified: false,
		},
		{
			name:     "the instance the failed installation replaced did not stop",
			facts:    scmRestartRollback{attempt: scmRollbackPreviousInstance},
			reason:   stopReason,
			want:     []string{"instance it replaced was not proven to be stopped", stopReason, "no instance was started", "hfdesk service status"},
			verified: false,
		},
		{
			name:     "the previous registration could not be put back",
			facts:    stopped,
			reason:   restoreReason,
			want:     []string{"was stopped", "could not be put back", restoreReason},
			verified: false,
		},
		{
			name:     "the restored registration names no program",
			facts:    restored,
			reason:   "",
			want:     []string{"was stopped", "was restored", "names no program", "no instance was started", "is stopped"},
			verified: false,
		},
		{
			name:     "the previous instance was not proven to be in service again",
			facts:    programKnown,
			reason:   startReason,
			want:     []string{"was stopped", "was restored", "was not proven to be in service again", startReason},
			verified: false,
		},
		{
			name:     "the started instance was not proven to be the previous program",
			facts:    started,
			reason:   "",
			want:     []string{"was stopped", "was restored", "was not proven to be the previous program", "the image of the process could not be read"},
			verified: false,
		},
		{
			name:     "the previous instance was started again",
			facts:    scmRestartRollback{stopped: true, restored: true, programKnown: true, started: true, verified: true},
			reason:   "",
			want:     []string{"was stopped", "was restored", scmRollbackSuccessClaim, "the failure recovery of the registration was not part of the restore"},
			verified: true,
		},
		{
			name:     "the previous instance was started again after its stop",
			facts:    scmRestartRollback{attempt: scmRollbackPreviousInstance, stopped: true, restored: true, programKnown: true, started: true, verified: true},
			reason:   "",
			want:     []string{"the previous instance was proven stopped", "was restored", scmRollbackSuccessClaim},
			verified: true,
		},
		{
			name:     "a stop that was never delivered and the instance still runs",
			facts:    undeliveredRunning,
			reason:   "",
			want:     []string{"no stop control was delivered", "was restored", "still runs", "no instance was started"},
			verified: false,
		},
		{
			name:     "a stop that was never delivered and the state cannot be read",
			facts:    undeliveredRestored,
			reason:   readReason,
			want:     []string{"no stop control was delivered", "was restored", "could not be confirmed", "no instance was started", "hfdesk service status", readReason},
			verified: false,
		},
		{
			name:     "a stop that was never delivered and the previous registration could not be put back",
			facts:    scmRestartRollback{attempt: scmRollbackUndeliveredInstance},
			reason:   restoreReason,
			want:     []string{"no stop control was delivered", "could not be put back", restoreReason},
			verified: false,
		},
		{
			name:     "the previous instance was stopped after its stop was never delivered",
			facts:    undeliveredStopped,
			reason:   "",
			want:     []string{"the previous instance was proven stopped", "was restored", scmRollbackSuccessClaim},
			verified: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sentence := scmRestartRollbackSentence(tc.facts, tc.reason)
			if sentence == "" {
				t.Fatal("scmRestartRollbackSentence() is empty, want the outcome of the rollback")
			}
			for _, want := range tc.want {
				if !strings.Contains(sentence, want) {
					t.Errorf("the sentence %q does not carry %q", sentence, want)
				}
			}
			// The success claim is only made for a verified restart: every
			// other outcome reports what it proved instead.
			if claimed := strings.Contains(sentence, scmRollbackSuccessClaim); claimed != tc.verified {
				t.Errorf("the sentence %q claims the previous instance was started again = %v, want %v", sentence, claimed, tc.verified)
			}
			// A rollback that did not prove the stop never claims the
			// restore, unless the stop was never delivered: there was no
			// control that could have put the instance down.
			undelivered := tc.facts.attempt == scmRollbackUndeliveredInstance
			if !tc.facts.stopped && !undelivered && strings.Contains(sentence, "the configuration of the previous registration was restored") {
				t.Errorf("the sentence %q claims the previous configuration was restored while the instance of the failed installation was not proven to be stopped", sentence)
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

// TestSCMProcessExcluded checks the decision that keeps a process of the failed
// attempt from being taken for the running instance.
//
// The installation excludes the process it replaced; the restart rollback
// excludes the process of the failed attempt next to it. A process identifier of
// zero names no process, so it never excludes one: the caller passes the process
// the service control manager reported even when it reported none.
func TestSCMProcessExcluded(t *testing.T) {
	cases := []struct {
		name      string
		exclude   []uint32
		processID uint32
		want      bool
	}{
		{"no excluded process", nil, 4711, false},
		{"the process of the attempt", []uint32{4711}, 4711, true},
		{"another process", []uint32{4711}, 4712, false},
		{"the process of the failed attempt next to the replaced one", []uint32{4710, 4711}, 4711, true},
		{"a replaced process the manager reported as none", []uint32{0, 4711}, 4711, true},
		{"no process at all", []uint32{0}, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := scmProcessExcluded(tc.exclude, tc.processID); got != tc.want {
				t.Errorf("scmProcessExcluded(%v, %d) = %v, want %v", tc.exclude, tc.processID, got, tc.want)
			}
		})
	}
}

// TestSCMStopOfAnInstanceThatIsStoppedIsACleanNoOp checks the first step of the
// restart rollback on an instance that does not run.
//
// The rollback stops the instance of the failed installation before it restores
// the registration. An instance the failed installation never started, or one
// whose process already ended, is stopped already: the stop is a clean no-op with
// no control delivered, so the restore proceeds instead of failing over a service
// that has nothing to stop.
func TestSCMStopOfAnInstanceThatIsStoppedIsACleanNoOp(t *testing.T) {
	handle := &scmTestServiceControl{state: svc.Stopped}

	manager := &scmManager{scope: ScopeSystem}
	if _, err := manager.stopService(context.Background(), handle); err != nil {
		t.Fatalf("stopService() = %v, want nil for an instance that does not run", err)
	}
	if handle.controls != 0 {
		t.Errorf("the stop control was delivered %d times, want none for an instance that does not run", handle.controls)
	}
	if handle.state != svc.Stopped {
		t.Errorf("the state of the instance = %v, want %v", handle.state, svc.Stopped)
	}
}

// TestSCMStartAndVerifyExcludesEveryProcessOfTheAttempt checks the seam the
// restart rollback needs at compile time.
//
// The verification of a start takes the processes it must not accept as the
// running instance, so the installation excludes the process it replaced and the
// rollback excludes the process of the failed attempt next to it. A verification
// that could only exclude one process would let the rollback accept the process
// it did not start.
//
// The sequence itself (stop the instance of the failed installation, restore the
// registration, start the previous program, verify it) needs a registration of
// the service control manager: a test cannot create one without an elevated
// shell, so the sequence is only proven on a machine that runs the elevated
// tests.
func TestSCMStartAndVerifyExcludesEveryProcessOfTheAttempt(t *testing.T) {
	var start func(*scmManager, context.Context, *mgr.Service, string, ...uint32) (scmImageCheck, error) = (*scmManager).startAndVerify
	var verify func(*scmManager, context.Context, *mgr.Service, string, ...uint32) (scmImageCheck, error) = (*scmManager).verifyRunningExecutable
	if start == nil || verify == nil {
		t.Fatal("the verification of a start is gone, the restart rollback cannot prove the instance it started")
	}

	// The rollback excludes the process the installation replaced and the
	// process of the failed attempt: the exclusion list it passes keeps the
	// process it did not start out of the answer.
	exclude := []uint32{4710, 4711}
	if !scmProcessExcluded(exclude, 4711) {
		t.Errorf("scmProcessExcluded(%v, 4711) = false, want the process of the failed attempt excluded", exclude)
	}
	if !scmProcessExcluded(exclude, 4710) {
		t.Errorf("scmProcessExcluded(%v, 4710) = false, want the replaced process excluded", exclude)
	}
	if scmProcessExcluded(exclude, 4712) {
		t.Errorf("scmProcessExcluded(%v, 4712) = true, want the process the rollback started accepted", exclude)
	}
}
