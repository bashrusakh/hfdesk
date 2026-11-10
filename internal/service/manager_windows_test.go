// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/bashrusakh/hfdesk/internal/runtimepaths"
)

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

func TestParseSCMCommandLineReadsTheRegisteredExecutableAndAddress(t *testing.T) {
	opts := scmTestOptions()
	// A path with a space in it exercises the escaping of the command
	// line.
	opts.Executable = filepath.Join("C:\\Program Files", "HFDesk", "hfdesk.exe")

	registration, err := parseSCMCommandLine(scmCommandLine(opts))
	assertNoError(t, err, "parseSCMCommandLine() = %v, want nil", err)
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
	assertNoError(t, err, "newPlatformManager(ScopeSystem) = %v, want nil", err)
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
	assertNoError(t, err, "the security descriptor of %q cannot be read: %v", path, err)
	control, _, err := sd.Control()
	assertNoError(t, err, "the control bits of %q cannot be read: %v", path, err)
	return control&windows.SE_DACL_PROTECTED != 0
}

// scmTestSetDACL applies an access control list to a path.
//
// The descriptor is applied to the path itself and never recursively.
func scmTestSetDACL(t *testing.T, path, sddl string) {
	t.Helper()
	sd, err := windows.SecurityDescriptorFromString(sddl)
	assertNoError(t, err, "SecurityDescriptorFromString(%q) = %v, want nil", sddl, err)
	dacl, _, err := sd.DACL()
	assertNoError(t, err, "the access control list of %q cannot be read: %v", sddl, err)
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
	assertNoError(t, err, "the security descriptor of %q cannot be read: %v", path, err)
	owner, _, err := sd.Owner()
	assertNoError(t, err, "the owner of %q cannot be read: %v", path, err)
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
	assertNoError(t, err, "the owner of %q cannot be read: %v", path, err)
	owner, _, err := sd.Owner()
	assertNoError(t, err, "the owner of %q cannot be read: %v", path, err)
	if owner == nil {
		t.Fatalf("the owner of %q is unknown", path)
	}
	info, err := os.Stat(path)
	assertNoError(t, err, "the mode of %q cannot be read: %v", path, err)
	return owner.String(), info.Mode().Perm()
}

func replaceSCMTestSeam[T any](t *testing.T, seam *T, value T) {
	t.Helper()
	previous := *seam
	*seam = value
	t.Cleanup(func() { *seam = previous })
}
