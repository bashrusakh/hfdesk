// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package service

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

// TestSystemdVerifiedRollbackRetainsPathsReferencedByPreviousNativeDirective
// checks that a verified rollback still refuses to remove directories the
// prior registration may reference: the base file is canonical, and the
// administrator layer a drop-in provides is exactly the uncertainty that
// keeps the created candidate paths, so rollback restores the saved bytes,
// proves the loaded configuration, and retains the directories.
func TestSystemdVerifiedRollbackRetainsPathsReferencedByPreviousNativeDirective(t *testing.T) {
	logPath := systemdRecordingSupervisor(t)
	systemdTempInstallEnvironment(t)
	root := t.TempDir()
	oldExecutable := systemdTestExecutable(t)
	newExecutable := systemdTestExecutable(t)
	previous := systemdExistingOptions(t, oldExecutable, root)
	manager, unitPath, previousUnit := systemdExistingInstallation(t, previous)
	candidateState := filepath.Join(root, "candidate-state")
	// The loaded configuration carries a drop-in layer beside the canonical
	// base file: while the supervisor reports it, a verified rollback cannot
	// prove the retained registration does not reference the created paths.
	dropInPath := unitDropInPath(unitPath)
	baseReader := manager.readServiceProperties
	manager.readServiceProperties = func(ctx context.Context, name string) (map[string]dbus.Variant, error) {
		properties, err := baseReader(ctx, name)
		if err != nil {
			return nil, err
		}
		properties["DropInPaths"] = dbus.MakeVariant([]string{dropInPath})
		return properties, nil
	}
	for _, dir := range instanceDirs(previous) {
		if err := os.MkdirAll(dir.path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "inactive")
	// The first reload of the write fails, so the registration goes through
	// the verified rollback; a repeat installation never runs enable (D-2),
	// so the reload is the failure this fixture drives.
	t.Setenv("HFDESK_TEST_SYSTEMCTL_DAEMON_RELOAD_FAIL_AT", "1")

	requested := DefaultInstallOptions(ScopeUser, newExecutable)
	requested.Paths.StateDir = candidateState
	requested.Explicit.StateDir = true
	if _, err := manager.Install(context.Background(), requested); err == nil {
		t.Fatal("Install() = nil, want the reload failure and verified rollback")
	}
	if info, err := os.Stat(candidateState); err != nil || !info.IsDir() {
		t.Errorf("the candidate state directory %q was removed after rollback: %v", candidateState, err)
	}
	content, err := os.ReadFile(unitPath)
	if err != nil || string(content) != previousUnit {
		t.Errorf("rollback path (read error %v) = %q, want the prior unit", err, content)
	}
	if calls := systemdSupervisorCallsTo(systemdSupervisorCalls(t, logPath), "restart", unitName); calls != 0 {
		t.Errorf("stopped previous unit was restarted %d times", calls)
	}
}

// TestSystemdDynamicLoadedMismatchRollsBackSource checks the detector-A
// refusal of a dynamic base file end to end: an update over operands the
// renderer never writes is refused before any mutation — the source stays
// byte for byte, the typed configuration is never consulted for a write,
// and the stopped unit is never restarted.
func TestSystemdDynamicLoadedMismatchRollsBackSource(t *testing.T) {
	fixture := newSystemdDynamicPathFixture(t)
	setSystemdTypedPropertySequence(t, fixture.manager, fixture.oldProperties, fixture.newProperties)
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "inactive")

	_, err := fixture.manager.Install(context.Background(), DefaultInstallOptions(ScopeUser, fixture.newExecutable))
	if err == nil {
		t.Fatal("Install() = nil, want the detector-A refusal of the dynamic base file")
	}
	assertSingleDropInAction(t, err, fixture.unitPath)
	if !strings.Contains(err.Error(), "is not the unit HFDesk generates") {
		t.Errorf("Install() = %q, want the cause to name the non-generated unit file", err)
	}
	content, readErr := os.ReadFile(fixture.unitPath)
	if readErr != nil || string(content) != fixture.sourceUnit {
		t.Errorf("source (error %v) =\n%s\nwant the untouched dynamic source\n%s", readErr, content, fixture.sourceUnit)
	}
	if got := systemdSupervisorCallsTo(systemdSupervisorCalls(t, fixture.logPath), "restart", unitName); got != 0 {
		t.Errorf("stopped unit was restarted %d times during the refusal", got)
	}
	if got := systemdSupervisorCallsTo(systemdSupervisorCalls(t, fixture.logPath), "daemon-reload"); got != 0 {
		t.Errorf("the supervisor was reloaded %d times during the refusal", got)
	}
}

func TestSystemdRollbackUsesTypedArgvForSpacesQuotesEmptyArgsAndEnvironment(t *testing.T) {
	fixture := newSystemdLosslessUpdateFixture(t)
	previousProbe := serviceReadinessProbe
	serviceReadinessProbe = probeServiceReady
	t.Cleanup(func() { serviceReadinessProbe = previousProbe })
	process := startSystemdRegisteredProcess(t, fixture.previous, "fixture-version")
	for _, properties := range []map[string]dbus.Variant{fixture.oldProperties, fixture.newProperties} {
		properties["MainPID"] = dbus.MakeVariant(uint32(process.Process.Pid))
	}
	setSystemdTypedPropertySequence(t, fixture.manager, fixture.oldProperties, fixture.newProperties, fixture.oldProperties)
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "active")
	t.Setenv("HFDESK_TEST_SYSTEMCTL_EXECUTABLE", fixture.previous.Executable)
	t.Setenv("HFDESK_TEST_SYSTEMCTL_RESTART_FAIL_ONCE", "1")
	t.Setenv("HFDESK_TEST_SYSTEMCTL_RESTART_MARKER", filepath.Join(t.TempDir(), "restart"))

	_, err := fixture.manager.Install(context.Background(), DefaultInstallOptions(ScopeUser, fixture.updated.Executable))
	if err == nil || !strings.Contains(err.Error(), "the previous registration was restored") {
		t.Fatalf("Install() = %v, want the failed restart and verified typed rollback", err)
	}
	content, readErr := os.ReadFile(fixture.unitPath)
	if readErr != nil || string(content) != fixture.previousUnit {
		t.Errorf("restored unit (error %v) =\n%s\nwant\n%s", readErr, content, fixture.previousUnit)
	}
	if got := systemdSupervisorCallsTo(systemdSupervisorCalls(t, fixture.logPath), "restart", unitName); got != 2 {
		t.Errorf("restart count = %d, want the failed update restart and one verified restore", got)
	}
}

// TestSystemdFailedRestartRestoresThePreviousRegistration checks that a
// restart the supervisor refuses neither leaves the new registration behind
// nor keeps the instance down: the previous unit file is written back, the
// previous instance is started again and the running state of the restore is
// verified before it is reported.
func TestSystemdFailedRestartRestoresThePreviousRegistration(t *testing.T) {
	logPath := systemdRecordingSupervisor(t)
	root := t.TempDir()
	oldExecutable := systemdTestExecutable(t)
	newExecutable := systemdTestExecutable(t)
	previous := systemdExistingOptions(t, oldExecutable, root)
	previous.Listen = "127.0.0.1:" + strconv.Itoa(freePort(t))
	for _, dir := range []string{filepath.Dir(previous.Paths.ConfigFile), previous.Paths.StateDir, previous.Paths.LogDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	m, unitPath, previousUnit := systemdExistingInstallation(t, previous)
	process := startSystemdRegisteredProcess(t, previous, "fixture-version")
	restartCount := filepath.Join(root, "restart-count")
	systemdRuntimePropertyReader(t, m, process.Process.Pid, process.Process.Pid, restartCount)
	t.Setenv("HFDESK_TEST_SYSTEMCTL_RESTART_COUNT", restartCount)
	previousProbe := serviceReadinessProbe
	serviceReadinessProbe = probeServiceReady
	t.Cleanup(func() { serviceReadinessProbe = previousProbe })
	// The supervisor reports the running state the restore comes back to:
	// the refused restart never reached the verification of the new
	// executable, so the program it reports is the one of the restored
	// registration.
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "active")
	t.Setenv("HFDESK_TEST_SYSTEMCTL_EXECUTABLE", oldExecutable)
	t.Setenv("HFDESK_TEST_SYSTEMCTL_RESTART_FAIL_ONCE", "1")
	t.Setenv("HFDESK_TEST_SYSTEMCTL_RESTART_MARKER", filepath.Join(t.TempDir(), "restart"))

	requested := DefaultInstallOptions(ScopeUser, newExecutable)
	requested.Paths.StateDir = filepath.Join(root, "candidate-state")
	requested.Explicit.StateDir = true
	_, err := m.Install(context.Background(), requested)
	if err == nil {
		t.Fatal("Install() = nil, want the failure of the restart")
	}
	if !strings.Contains(err.Error(), "the previous registration was restored") {
		t.Errorf("Install() = %q, want an error that reports the restored registration", err)
	}
	// The restart of the restore is verified, so the sentence that names
	// the started instance is allowed here.
	if !strings.Contains(err.Error(), "the previous instance was started again") {
		t.Errorf("Install() = %q, want the verified restart of the previous instance", err)
	}

	content, readErr := os.ReadFile(unitPath)
	if readErr != nil {
		t.Fatalf("read the unit file: %v", readErr)
	}
	if string(content) != previousUnit {
		t.Errorf("the unit file was not restored:\n%s\nwant\n%s", content, previousUnit)
	}
	if _, statErr := os.Stat(requested.Paths.StateDir); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("candidate-only state directory %q survived a verified restore: %v", requested.Paths.StateDir, statErr)
	}

	calls := systemdSupervisorCalls(t, logPath)
	// The failed restart and the restart that brings the previous
	// instance back.
	if got := systemdSupervisorCallsTo(calls, "restart", unitName); got != 2 {
		t.Errorf("the unit was restarted %d times, want the failed restart and the restore:\n%s", got, strings.Join(calls, "\n"))
	}
}

// TestSystemdRollbackReloadFailureDoesNotRestartOrClaimRecovery checks the
// honest reporting of a rollback whose reload fails: the original
// verification failure is preserved, the saved unit bytes are written back
// to disk, and the error claims neither a restored loaded configuration nor
// a restarted instance while the supervisor still holds the failed update.
func TestSystemdRollbackReloadFailureDoesNotRestartOrClaimRecovery(t *testing.T) {
	logPath := systemdRecordingSupervisor(t)
	root := t.TempDir()
	executable := systemdTestExecutable(t)
	previous := systemdExistingOptions(t, executable, root)
	m, unitPath, previousUnit := systemdExistingInstallation(t, previous)
	// The drop-in layer the supervisor loads carries an extra environment
	// value and an environment file the base file does not; the snapshot
	// below is what the update has to preserve.
	home, err := unitHomeDir(previous)
	if err != nil {
		t.Fatalf("unitHomeDir() = %v", err)
	}
	oldEnvironment := []string{"HF_HUB_CACHE=/operator/hub", "HOME=" + home}
	oldProperties := systemdTestPropertiesFromUnit(previousUnit)
	oldProperties["Environment"] = dbus.MakeVariant(oldEnvironment)
	// The snapshot of the running instance needs a MainPID; the restart
	// proof never runs here because V1 fails first.
	oldProperties["MainPID"] = dbus.MakeVariant(uint32(4242))
	oldConfig, err := loadedConfigFromProperties(oldProperties)
	if err != nil {
		t.Fatalf("decode the previous typed manager properties: %v", err)
	}
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "active")
	t.Setenv("HFDESK_TEST_SYSTEMCTL_DAEMON_RELOAD_FAIL_AT", "2")

	requested := DefaultInstallOptions(ScopeUser, executable)
	requested.Paths.ConfigFile = filepath.Join(root, "new-config", "hfdesk.json")
	requested.Explicit.Config = true
	updated := previous
	updated.Paths.ConfigFile = requested.Paths.ConfigFile
	updatedArgv := append([]string{executable}, RunArgs(updated)...)
	updatedEnvironment := slices.Clone(oldEnvironment)
	updatedEnvironment[0] = "HF_HUB_CACHE=/operator/new-hub"
	updatedProperties := systemdTypedProperties(executable, updatedArgv, updatedEnvironment, oldConfig.workingDirectory, oldConfig.user, oldConfig.group, oldConfig.environmentFiles)
	setSystemdTypedPropertySequence(t, m, oldProperties, updatedProperties)
	_, err = m.Install(context.Background(), requested)
	if err == nil {
		t.Fatal("Install() = nil, want the environment mismatch and unconfirmed rollback")
	}
	for _, want := range []string{"did not preserve the loaded environment", "reload the restored unit"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Install() = %q, want the original/rollback failure %q", err, want)
		}
	}
	for _, unsupported := range []string{"captured loaded command/launch settings were restored", "previous instance was started again"} {
		if strings.Contains(err.Error(), unsupported) {
			t.Errorf("Install() = %q, must not claim %q", err, unsupported)
		}
	}

	disk, readErr := os.ReadFile(unitPath)
	if readErr != nil || string(disk) != previousUnit {
		t.Errorf("stored unit after rollback (error %v) =\n%s\nwant\n%s", readErr, disk, previousUnit)
	}
	loadedBytes, readErr := os.ReadFile(os.Getenv("HFDESK_TEST_SYSTEMCTL_LOADED_UNIT"))
	if readErr != nil {
		t.Fatalf("read the supervisor-loaded unit: %v", readErr)
	}
	loaded := parseUnit(string(loadedBytes))
	if loaded.ExecStart != executable || loaded.listen() != previous.Listen || loaded.flagValue("--config") == previous.Paths.ConfigFile {
		t.Errorf("the supervisor-loaded command does not distinguish the failed update: %+v", loaded)
	}
	loadedConfig, err := loadedConfigFromProperties(updatedProperties)
	if err != nil {
		t.Fatalf("decode the still-loaded typed manager properties: %v", err)
	}
	if loadedConfig.execPath != executable || runArgValue(loadedConfig.execArgs[1:], "--listen") != previous.Listen || runArgValue(loadedConfig.execArgs[1:], "--config") == previous.Paths.ConfigFile || slices.Equal(loadedConfig.environment, oldConfig.environment) {
		t.Errorf("typed loaded state = %+v, want same executable/listen but changed config and environment", loadedConfig)
	}
	if info, statErr := os.Stat(filepath.Dir(requested.Paths.ConfigFile)); statErr != nil || !info.IsDir() {
		t.Errorf("candidate config directory %q was removed while the supervisor still has its command loaded: %v", filepath.Dir(requested.Paths.ConfigFile), statErr)
	}

	calls := systemdSupervisorCalls(t, logPath)
	if got := systemdSupervisorCallsTo(calls, "restart", unitName); got != 0 {
		t.Errorf("the instance was restarted %d times, want no restart while the typed loaded state is unconfirmed:\n%s", got, strings.Join(calls, "\n"))
	}
}

// TestSystemdActiveUnitThatDoesNotServeIsAFailedUpdate checks that a restarted
// instance counts as verified only when it really serves.
//
// The supervisor reports the unit active with the registered program, which is
// all a Type=simple unit proves: the process was forked. The instance of the
// test does not answer, the way a process that died on a bind error does not, so
// the update is a failure: the previous registration is restored and the
// previous instance is started again, which is verified before it is reported.
func TestSystemdActiveUnitThatDoesNotServeIsAFailedUpdate(t *testing.T) {
	logPath := systemdRecordingSupervisor(t)
	root := t.TempDir()
	oldExecutable := systemdTestExecutable(t)
	newExecutable := systemdTestExecutable(t)
	previous := systemdExistingOptions(t, oldExecutable, root)
	previous.Listen = "127.0.0.1:" + strconv.Itoa(freePort(t))

	m, unitPath, previousUnit := systemdExistingInstallation(t, previous)
	process := startSystemdRegisteredProcess(t, previous, "fixture-version")
	restartCount := filepath.Join(t.TempDir(), "restarts")
	systemdRuntimePropertyReader(t, m, process.Process.Pid, process.Process.Pid, restartCount)
	t.Setenv("HFDESK_TEST_SYSTEMCTL_RESTART_COUNT", restartCount)
	// The supervisor reports what a restarted simple unit reports: the unit
	// is active with the program it was started with, and the restart of the
	// restore brings the previous program back.
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "active")
	t.Setenv("HFDESK_TEST_SYSTEMCTL_EXECUTABLE", newExecutable)
	m.restartWait = 250 * time.Millisecond

	// The process fixture provides a real MainPID and argv. The readiness seam
	// fails the first probe and accepts the restore to exercise rollback reporting.
	t.Setenv("HFDESK_TEST_SYSTEMCTL_EXECUTABLE_AFTER_SECOND_RESTART", oldExecutable)
	serviceReadinessProbe = func(listen string) error {
		count := 0
		if content, err := os.ReadFile(restartCount); err == nil {
			count, _ = strconv.Atoi(strings.TrimSpace(string(content)))
		}
		if count < 2 {
			return errors.New("dial tcp " + probeServiceAddress(listen) + ": connect: connection refused")
		}
		return nil
	}

	_, err := m.Install(context.Background(), DefaultInstallOptions(ScopeUser, newExecutable))
	if err == nil {
		t.Fatal("Install() = nil, want the failure of the verification of the instance that does not serve")
	}
	for _, want := range []string{
		"does not answer on",
		previous.Listen,
		"the previous registration was restored",
		"the previous instance was started again",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Install() = %q, want an error that mentions %q", err, want)
		}
	}

	content, readErr := os.ReadFile(unitPath)
	if readErr != nil {
		t.Fatalf("read the unit file: %v", readErr)
	}
	if string(content) != previousUnit {
		t.Errorf("the unit file was not restored:\n%s\nwant\n%s", content, previousUnit)
	}

	calls := systemdSupervisorCalls(t, logPath)
	if got := systemdSupervisorCallsTo(calls, "restart", unitName); got != 2 {
		t.Errorf("the unit was restarted %d times, want the failed update and the restore:\n%s", got, strings.Join(calls, "\n"))
	}
}

// TestSystemdDynamicListenRollbackProbesTheRestoredProcessAddress checks
// the detector-A refusal on the dynamic-listen fixture: an update over the
// `${HF_LISTEN}` base file is refused before anything changes, so no
// readiness probe of a candidate address runs, the dynamic source and its
// EnvironmentFile stay as they are, and neither the update restart nor the
// restore restart happens.
func TestSystemdDynamicListenRollbackProbesTheRestoredProcessAddress(t *testing.T) {
	logPath := systemdRecordingSupervisor(t)
	realProbe := serviceReadinessProbe
	serviceReadinessProbe = probeServiceReady
	t.Cleanup(func() { serviceReadinessProbe = realProbe })
	realInstallProbe := serviceInstallProbe
	var probed []string
	serviceInstallProbe = func(listen, expectedVersion string) (bool, error) {
		probed = append(probed, listen)
		return realInstallProbe(listen, expectedVersion)
	}
	t.Cleanup(func() { serviceInstallProbe = realInstallProbe })

	root := t.TempDir()
	oldExecutable := systemdTestExecutable(t)
	newExecutable := systemdTestExecutable(t)
	previous := systemdExistingOptions(t, oldExecutable, root)
	manager, unitPath, previousUnit := systemdExistingInstallation(t, previous)
	oldListen := "127.0.0.1:" + strconv.Itoa(freePort(t))
	environmentFile := filepath.Join(root, "hfdesk.env")
	if err := os.WriteFile(environmentFile, []byte("HF_LISTEN="+oldListen+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dynamicPreviousUnit := systemdDynamicListenUnit(t, unitPath, previousUnit, previous.Listen, "EnvironmentFile="+environmentFile)
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "active")
	t.Setenv("HFDESK_TEST_SYSTEMCTL_EXECUTABLE", newExecutable)

	requested := DefaultInstallOptions(ScopeUser, newExecutable)
	requested.ExpectedVersion = "old-version"
	_, err := manager.Install(context.Background(), requested)
	if err == nil {
		t.Fatal("Install() = nil, want the detector-A refusal of the dynamic base file")
	}
	assertSingleDropInAction(t, err, unitPath)
	if !strings.Contains(err.Error(), "is not the unit HFDesk generates") {
		t.Errorf("Install() = %q, want the cause to name the non-generated unit file", err)
	}
	if len(probed) != 0 {
		t.Errorf("the refused update probed addresses %v; a refusal before the mutation probes nothing", probed)
	}
	content, readErr := os.ReadFile(unitPath)
	if readErr != nil || string(content) != dynamicPreviousUnit || !strings.Contains(string(content), "${HF_LISTEN}") {
		t.Errorf("the refused update changed the dynamic source (error %v):\n%s\nwant:\n%s", readErr, content, dynamicPreviousUnit)
	}
	if got := systemdSupervisorCallsTo(systemdSupervisorCalls(t, logPath), "restart", unitName); got != 0 {
		t.Errorf("restart count = %d, want no restart on a refusal", got)
	}
}

// TestSystemdRestoreThatIsNotConfirmedIsNotReportedAsStartedAgain checks that a
// restart of the restored registration which the supervisor accepted but which
// left the unit down is not reported as a started instance: the error says that
// the registration was restored and that the restart was issued, and it never
// claims that the previous instance runs again.
func TestSystemdRestoreThatIsNotConfirmedIsNotReportedAsStartedAgain(t *testing.T) {
	logPath := systemdRecordingSupervisor(t)
	root := t.TempDir()
	oldExecutable := systemdTestExecutable(t)
	newExecutable := systemdTestExecutable(t)
	previous := systemdExistingOptions(t, oldExecutable, root)

	m, unitPath, previousUnit := systemdExistingInstallation(t, previous)
	// The snapshot of the running instance needs a non-zero MainPID, and
	// the process behind it is not observable: the update verification and
	// the confirmation of the restore both fail on process evidence, which
	// is exactly the honesty this test pins.
	baseReader := m.readServiceProperties
	m.readServiceProperties = func(ctx context.Context, name string) (map[string]dbus.Variant, error) {
		properties, err := baseReader(ctx, name)
		if err != nil {
			return nil, err
		}
		properties["MainPID"] = dbus.MakeVariant(uint32(424242))
		return properties, nil
	}
	// The unit runs before the installation, and the restart of the failure
	// leaves it down: the supervisor accepts every restart, and
	// `systemctl restart` returns as soon as the unit was forked, so its
	// exit status is not a running instance.
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "active")
	t.Setenv("HFDESK_TEST_SYSTEMCTL_EXECUTABLE", oldExecutable)
	t.Setenv("HFDESK_TEST_SYSTEMCTL_RESTARTED", filepath.Join(t.TempDir(), "restarted"))
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE_AFTER_RESTART", "inactive")
	m.restartWait = 250 * time.Millisecond

	_, err := m.Install(context.Background(), DefaultInstallOptions(ScopeUser, newExecutable))
	if err == nil {
		t.Fatal("Install() = nil, want the unconfirmed state of the new executable")
	}
	for _, want := range []string{
		"the registration was restored",
		"the restart of the previous instance was issued",
		"but the restart was not confirmed",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Install() = %q, want an error that mentions %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "the previous instance was started again") {
		t.Errorf("Install() = %q, want no claim that the previous instance was started again", err)
	}

	content, readErr := os.ReadFile(unitPath)
	if readErr != nil {
		t.Fatalf("read the unit file: %v", readErr)
	}
	if string(content) != previousUnit {
		t.Errorf("the unit file was not restored:\n%s\nwant\n%s", content, previousUnit)
	}

	calls := systemdSupervisorCalls(t, logPath)
	if got := systemdSupervisorCallsTo(calls, "restart", unitName); got != 2 {
		t.Errorf("the unit was restarted %d times, want the failed attempt and the restore:\n%s", got, strings.Join(calls, "\n"))
	}
}

// TestSystemdRestoreOfAUnitWithoutAProgramIsNotReportedAsRunning checks that
// active metadata without a live HFDesk MainPID is not treated as a verified
// restored instance.
func TestSystemdRestoreOfAUnitWithoutAProgramIsNotReportedAsRunning(t *testing.T) {
	logPath := systemdRecordingSupervisor(t)
	root := t.TempDir()
	systemdTempInstallEnvironment(t)
	m := &systemdManager{scope: ScopeUser}
	path, err := m.unitPath()
	if err != nil {
		t.Fatalf("unitPath() = %v", err)
	}
	// A registration whose command line cannot be read: the section holds
	// no ExecStart, so the restored registration names no program.
	content := []byte("[Unit]\nDescription=HFDesk\n\n[Service]\nWorkingDirectory=" + root + "\n")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create the unit directory: %v", err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("write the unit file: %v", err)
	}
	if err := os.WriteFile(os.Getenv("HFDESK_TEST_SYSTEMCTL_LOADED_UNIT"), content, 0o644); err != nil {
		t.Fatalf("write the loaded unit: %v", err)
	}
	m.readServiceProperties = func(context.Context, string) (map[string]dbus.Variant, error) {
		return systemdTypedProperties("", []string{}, []string{}, root, "", "", []systemdEnvironmentFile{}), nil
	}

	m.restartWait = 250 * time.Millisecond
	loaded, err := m.registrationUnitConfig(context.Background())
	if err != nil {
		t.Fatalf("registrationUnitConfig() = %v", err)
	}
	previous := previousRegistration{path: path, content: content, existed: true, running: true, loaded: loaded, loadedKnown: true}

	// The supervisor reports the unit active, but the typed service MainPID is
	// zero and the registration names no HFDesk process.
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "active")
	err = m.restorePreviousRegistration(context.Background(), previous)
	if err == nil {
		t.Fatal("restorePreviousRegistration() succeeded with no live HFDesk MainPID")
	} else if !strings.Contains(err.Error(), "service process could not be verified") {
		t.Errorf("restorePreviousRegistration() = %v, want an explicit process-evidence failure", err)
	}
	if strings.Contains(err.Error(), "the previous instance was started again") {
		t.Errorf("restorePreviousRegistration() = %v, must not claim a started instance", err)
	}

	// The state is verified and not assumed: a unit the supervisor does not
	// report active is a restore that was not confirmed.
	t.Setenv("HFDESK_TEST_SYSTEMCTL_ACTIVE", "inactive")
	err = m.restorePreviousRegistration(context.Background(), previous)
	if err == nil {
		t.Fatal("restorePreviousRegistration() = nil, want the unconfirmed state of the restored unit")
	}
	if !strings.Contains(err.Error(), "is not active") {
		t.Errorf("restorePreviousRegistration() = %q, want an error that names the state it verified", err)
	}
	if !strings.Contains(err.Error(), "the restart of the previous instance was issued") {
		t.Errorf("restorePreviousRegistration() = %q, want an error that reports the issued restart", err)
	}

	calls := systemdSupervisorCalls(t, logPath)
	if got := systemdSupervisorCallsTo(calls, "restart", unitName); got != 2 {
		t.Errorf("the unit was restarted %d times, want the restore of each call:\n%s", got, strings.Join(calls, "\n"))
	}
}
