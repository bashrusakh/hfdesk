// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
)

// Install registers the service with the supervisor.
//
// The installation validates first, merges its options with an existing
// registration, writes the unit file atomically, reloads the supervisor and
// enables the unit for the boot of the scope when the installation has to.
//
// A first installation enables the registration for the boot of the scope; a
// repeat installation keeps the autostart state it finds, including a
// runtime-only enablement, so a unit an administrator disabled is never enabled
// again, a unit enabled with `systemctl enable --runtime` stays enabled at
// runtime, and a masked unit refuses the installation instead of being replaced
// (see reloadAndEnable). The mask is asked of the supervisor and not of the file
// system, so a runtime mask, which lives in /run and leaves no file at the unit
// path, refuses the installation as well.
//
// An installation that replaces a registration keeps every setting the request
// did not name, so a reinstall with a new binary resets no bind address, paths,
// cache layout or account; the executable always comes from the request.
//
// A reinstalled instance that was running before is restarted and verified,
// because a registration alone does not switch the running process. A stopped
// instance and a first installation are never started.
//
// A machine-wide instance runs as the dedicated service account of the
// platform; the account is validated before anything is created, and the
// installation never creates an account and never changes the ownership of a
// model or a cache tree.
//
// A failure after the unit file was written restores the previous unit and
// verifies the loaded configuration; a failed running restart only restarts
// the previous instance after that verification. Created directories are
// removed only after no-write or rollback evidence shows they are unreferenced.
//
// The returned options are the registration as written, so a caller reports a
// preserved setting instead of the default it was not reset to.
func (m *systemdManager) Install(ctx context.Context, opts InstallOptions) (InstallOptions, error) {
	requested := opts
	// The registration of the scope is read before anything is decided:
	// an installation over an existing registration must merge with what
	// the instance really runs with, and it must know whether the instance
	// has to be brought up again.
	path, err := m.unitPath()
	if err != nil {
		return InstallOptions{}, err
	}
	content, existed, err := readUnitFile(path)
	if err != nil {
		return InstallOptions{}, err
	}
	sourceContent, sourceKnown := content, existed
	sourceUnit := parsedUnit{}
	dynamicFields := map[string]bool{}
	registered := existed
	var initialLoaded systemdLoadedConfig
	initialLoadedKnown := false
	if existed {
		sourceUnit = parseUnit(string(content))
	} else if m.scope != ScopeSystem || os.Geteuid() == 0 {
		// A missing writable override is still an existing registration when
		// systemd loaded a vendor or generated fragment for this name.
		initialLoaded, err = m.registrationUnitConfig(ctx)
		if err != nil {
			return opts, fmt.Errorf("service: determine whether %s already has a loaded registration: %w", unitName, err)
		}
		initialLoadedKnown = true
		if initialLoaded.loaded {
			if !initialLoaded.fragmentPathKnown || !filepath.IsAbs(initialLoaded.fragmentPath) {
				return opts, fmt.Errorf("service: the loaded registration of %s has no readable source fragment; refusing to replace unknown native settings", unitName)
			}
			sourceContent, err = os.ReadFile(initialLoaded.fragmentPath)
			if err != nil {
				return opts, fmt.Errorf("service: read the loaded source fragment %q: %w", initialLoaded.fragmentPath, err)
			}
			sourceKnown = true
			sourceUnit = parseUnit(string(sourceContent))
			registered = true
		}
	}
	if registered {
		opts = mergeInstallOptions(opts, registrationOptions(sourceUnit, opts.Scope), true)
		dynamicFields = systemdDynamicInstallFields(sourceUnit, requested)
	}

	validationOpts := systemdValidationOptions(opts, dynamicFields)
	if err := ValidateInstallOptions(validationOpts); err != nil {
		return opts, err
	}
	// A path with a control character would end up in the unit file
	// literally, and a newline in it would split the unit into another
	// directive the installation never wrote. The check runs before the
	// root requirement, before any directory is created and before the
	// unit file is written: a refused installation leaves nothing behind.
	if err := validateUnitPaths(validationOpts, registered); err != nil {
		return opts, err
	}
	// An ExecStart= the update cannot rewrite into the new program is
	// refused here: before the root requirement, before any directory is
	// created and before the unit file is written. An installation must
	// never leave a registration whose command cannot start, and a stopped
	// instance has no later verification that would report it.
	if registered {
		if err := validateSystemdExecStartUpdate(string(sourceContent), opts.Executable); err != nil {
			return opts, err
		}
		// An update of a running instance verifies the restarted start
		// against the live process argv, so the existing command must be
		// verifiable: refused here, at the same boundary and before any
		// mutation, while a stopped instance keeps updating without a
		// start to verify.
		if err := m.refuseUnverifiableRunningExecStart(ctx, string(sourceContent), opts.Executable); err != nil {
			return opts, err
		}
	}
	// A machine-wide instance must not adopt the state of the user who
	// runs the install command. The check runs before the root
	// requirement, before any directory is created and before the unit
	// file is written: a refused installation leaves nothing behind, and
	// every platform reports the same reason for the same command line.
	filesystemOpts := systemdFilesystemOptions(opts, dynamicFields)
	if err := m.checkSystemPaths(opts); err != nil {
		return opts, err
	}
	// The account of a machine-wide instance is a rule of the scope and
	// not a privilege of the process, so it is validated the same way:
	// before the root requirement, before any directory is created and
	// before the unit file is written. A refused installation leaves
	// nothing behind. An update without a named account validates the
	// loaded identity of the registration (which a drop-in may provide)
	// and not the default account of the platform.
	accountOpts := m.accountValidationOptions(ctx, requested, opts, registered)
	account, err := m.serviceAccount(accountOpts)
	if err != nil {
		return opts, err
	}
	preflightLoaded := initialLoaded
	preflightLoadedKnown := initialLoadedKnown
	preflightLoadedAttempted := initialLoadedKnown
	if existed && !preflightLoadedAttempted && m.scope == ScopeSystem && os.Geteuid() == 0 {
		preflightLoadedAttempted = true
		if loaded, readErr := m.registrationUnitConfig(ctx); readErr == nil {
			preflightLoaded = loaded
			preflightLoadedKnown = true
		}
	}
	identity := systemdAccessIdentityForInstall(opts.Scope, requested, registered, preflightLoaded, preflightLoadedKnown, account)
	if err := m.checkServiceAccountAccessWithIdentity(filesystemOpts, identity.accountName, identity.access); err != nil {
		return opts, err
	}
	// The root requirement is checked before any directory is created: a
	// permission error of the machine wide directories is a much less
	// readable answer than the real reason.
	if err := m.requireRoot("an installation"); err != nil {
		return opts, err
	}
	if registered && !preflightLoadedAttempted {
		if loaded, readErr := m.registrationUnitConfig(ctx); readErr == nil {
			preflightLoaded = loaded
			preflightLoadedKnown = true
		}
	}
	// The program is checked before anything is read or written, so a refused
	// program creates no directory, rewrites no unit and stops no instance.
	preflightOpts := validationOpts
	if registered && dynamicFields["state-dir"] {
		preflightOpts.Paths.StateDir = ""
	}
	workingDirectory := systemdWorkingDirectoryForPreflight(preflightOpts, registered, preflightLoaded, preflightLoadedKnown)
	if err := m.preflightWithContext(ctx, preflightOpts, identity.accountName, identity.access, workingDirectory); err != nil {
		return opts, err
	}
	// The state of the replaced registration is read before the unit file
	// is written: only a running instance is restarted, a stopped one has
	// to stay stopped.
	previous := previousRegistration{
		path: path, content: content, sourceContent: sourceContent, sourceKnown: sourceKnown,
		existed: existed, loaded: preflightLoaded, loadedKnown: preflightLoadedKnown,
		dynamicFields: dynamicFields,
	}
	if registered {
		running, err := m.registrationRunning(ctx)
		if err != nil {
			return opts, err
		}
		previous.running = running
	}
	// The enablement is read before the unit file is written, whether or not
	// a unit file exists: the mask of a unit is a question for the
	// supervisor, and a runtime mask (`systemctl mask --runtime`) lives in
	// /run and leaves no file at the unit path, so a unit that is only
	// masked at runtime would otherwise look like a first installation and
	// be registered while it stays masked and cannot start.
	//
	// A masked unit is refused instead of silently unmasked, and a repeat
	// installation keeps the autostart state it finds, in its mode: a unit
	// enabled for this boot only stays enabled for this boot only. A fresh
	// machine is unaffected: "is-enabled" answers "not-found", which is not a
	// mask, and a first installation enables the registration it writes
	// anyway.
	enablement := m.registrationEnablement(ctx)
	if enablement.masked {
		return opts, m.maskedUnitRefusal()
	}
	if registered {
		previous.enablement = enablement.mode
		loaded, readErr := m.registrationUnitConfig(ctx)
		if readErr != nil {
			return opts, fmt.Errorf("service: the loaded configuration of %s could not be read, the registration was not changed: %w", unitName, readErr)
		}
		if !existed && loaded.loaded {
			if !loaded.fragmentPathKnown || loaded.fragmentPath != initialLoaded.fragmentPath {
				return opts, fmt.Errorf("service: the source fragment of the loaded %s changed during installation preflight", unitName)
			}
			currentSource, sourceErr := os.ReadFile(loaded.fragmentPath)
			if sourceErr != nil || !bytes.Equal(currentSource, sourceContent) {
				return opts, fmt.Errorf("service: the loaded source fragment %q changed or became unreadable during installation preflight", loaded.fragmentPath)
			}
		}
		if !loaded.loaded && previous.running {
			return opts, errors.New("service: systemd reported a running unit without a loaded configuration; the registration was not changed")
		}
		if loaded.loaded {
			if err := loadedMatchesUnit(loaded, parseUnit(string(sourceContent))); err != nil {
				return opts, fmt.Errorf("service: the loaded command of %s differs from its unit file; an override may own the effective command, refusing to update it: %w", unitName, err)
			}
		}
		previous.loaded = loaded
		previous.loadedKnown = true
		previous.sourceContent = sourceContent
		previous.sourceKnown = sourceKnown
		contextChanged := !preflightLoadedKnown || !sameSystemdLoadedConfig(preflightLoaded, loaded)
		preflightLoaded = loaded
		preflightLoadedKnown = true
		if contextChanged && loaded.loaded {
			identity = systemdAccessIdentityForInstall(opts.Scope, requested, true, loaded, true, account)
			if err := m.checkServiceAccountAccessWithIdentity(filesystemOpts, identity.accountName, identity.access); err != nil {
				return opts, err
			}
			workingDirectory = systemdWorkingDirectoryForPreflight(preflightOpts, true, loaded, true)
			if err := m.preflightWithContext(ctx, preflightOpts, identity.accountName, identity.access, workingDirectory); err != nil {
				return opts, err
			}
		}
	}
	// The directories the installation is about to create are remembered
	// before they are created, so the effective systemd identity is given
	// ownership of exactly those and of nothing else. Existing directories
	// belong to their current owner and are left alone.
	//
	// The list is also what a failed installation removes again, so a
	// failed installation leaves the machine as it was.
	instanceDirectories := systemdInstanceDirs(opts, dynamicFields)
	created := make(map[string][]string, len(instanceDirectories))
	for _, dir := range instanceDirectories {
		created[dir.path] = createdDirs(dir.path)
	}
	if m.scope == ScopeSystem && identity.owner == nil {
		for _, dir := range instanceDirectories {
			if len(created[dir.path]) > 0 {
				return opts, fmt.Errorf("service: cannot assign ownership of the new %s %q because the effective systemd User/Group is not established", dir.name, dir.path)
			}
		}
	}
	if err := prepareSystemdInstanceDirs(opts, instanceDirectories, dynamicFields); err != nil {
		// The preparation may have created some of the directories
		// before it failed. A previous unit may still reference those
		// paths even though this attempt did not write a registration.
		if systemdCreatedDirsSafeToRemove(previous, created) {
			removeCreatedDirs(createdDirOrderForDirs(instanceDirectories, created))
		} else {
			err = fmt.Errorf("%w (installation-created directories were retained because the prior unit may still reference them)", err)
		}
		return opts, err
	}
	// The result of register decides whether a failed attempt may remove
	// these paths; an uncertain or retained registration keeps them.
	registrationResult, err := m.register(ctx, opts, account, identity, created, previous)
	if err != nil {
		if registrationResult.createdDirsSafeToRemove {
			removeCreatedDirs(createdDirOrderForDirs(instanceDirectories, created))
		} else {
			err = fmt.Errorf("%w (installation-created directories were retained because the supervisor could not prove the restored or retained unit no longer references them)", err)
		}
		return opts, err
	}
	opts.VersionVerified = registrationResult.versionVerified
	return opts, nil
}

// previousRegistration is the registration an installation replaces.
type previousRegistration struct {
	// path is the path of the unit file of the scope.
	path string
	// content is the unit file that was there, empty when there was none.
	content []byte
	// sourceContent is the effective fragment on the systemd load path. It may
	// be different from content when no writable override file exists.
	sourceContent []byte
	sourceKnown   bool
	// existed reports whether the scope had a writable unit file at path.
	existed bool
	// running reports whether the instance ran before the installation.
	// Only such an instance is restarted.
	running bool
	// enablement is the autostart state the supervisor reported for the
	// registration, in its mode. Only a registration that existed carries
	// it.
	enablement enablementMode
	// loaded is the effective command and launch configuration reported by
	// systemd before the update. The file on disk alone cannot prove what the
	// supervisor has loaded.
	loaded systemdLoadedConfig
	// loadedKnown reports that loaded was read successfully for an existing
	// registration. A first installation expects no loaded registration.
	loadedKnown bool
	// dynamicFields are untouched ExecStart options whose source uses systemd
	// specifiers or environment references. Their source text is preserved
	// while available manager-reported values are compared with the snapshot.
	dynamicFields map[string]bool
}

func (p previousRegistration) registrationPresent() bool {
	return p.existed || (p.loadedKnown && p.loaded.loaded)
}

func (p previousRegistration) sourceUnitContent() ([]byte, bool) {
	if p.sourceKnown {
		return p.sourceContent, true
	}
	if p.existed {
		return p.content, true
	}
	return nil, false
}

func systemdInstallFlagExplicit(opts InstallOptions, flag string) bool {
	switch flag {
	case "config":
		return opts.Explicit.Config
	case "state-dir":
		return opts.Explicit.StateDir
	case "log-dir":
		return opts.Explicit.LogDir
	case "listen":
		return opts.Explicit.Listen
	case "cache-dir":
		return opts.Explicit.CacheDir
	case "hub-dir":
		return opts.Explicit.HubDir
	case "local-dir":
		return opts.Explicit.LocalDir
	default:
		return false
	}
}

func systemdDynamicInstallFields(unit parsedUnit, opts InstallOptions) map[string]bool {
	dynamic := make(map[string]bool)
	for flag, unresolved := range unit.DynamicRunArgs {
		if unresolved && !systemdInstallFlagExplicit(opts, flag) {
			dynamic[flag] = true
		}
	}
	if unit.WorkingDirectoryDynamic && !opts.Explicit.StateDir {
		if _, hasStateArg := unit.DynamicRunArgs["state-dir"]; !hasStateArg {
			dynamic["state-dir"] = true
		}
	}
	return dynamic
}

func systemdValidationOptions(opts InstallOptions, dynamic map[string]bool) InstallOptions {
	defaults := DefaultInstallOptions(opts.Scope, opts.Executable)
	if dynamic["config"] {
		opts.Paths.ConfigFile = defaults.Paths.ConfigFile
	}
	if dynamic["state-dir"] {
		opts.Paths.StateDir = defaults.Paths.StateDir
	}
	if dynamic["log-dir"] {
		opts.Paths.LogDir = defaults.Paths.LogDir
	}
	if dynamic["listen"] {
		opts.Listen = defaults.Listen
	}
	if dynamic["cache-dir"] {
		opts.CacheDir = ""
	}
	if dynamic["hub-dir"] {
		opts.HubDir = ""
	}
	if dynamic["local-dir"] {
		opts.LocalDir = ""
	}
	return opts
}

func systemdFilesystemOptions(opts InstallOptions, dynamic map[string]bool) InstallOptions {
	if dynamic["config"] {
		opts.Paths.ConfigFile = ""
	}
	if dynamic["state-dir"] {
		opts.Paths.StateDir = ""
	}
	if dynamic["log-dir"] {
		opts.Paths.LogDir = ""
	}
	if dynamic["cache-dir"] {
		opts.CacheDir = ""
	}
	if dynamic["hub-dir"] {
		opts.HubDir = ""
	}
	if dynamic["local-dir"] {
		opts.LocalDir = ""
	}
	return opts
}

// registrationEnablement is what the supervisor reports about the enablement of
// the unit.
type registrationEnablement struct {
	// mode is the autostart state the supervisor reported for the unit.
	// Only an enabled unit is enabled again by a repeat installation, and
	// the mode is kept: a runtime-only enablement stays runtime-only.
	mode enablementMode
	// masked reports whether the unit is masked, persistently or at
	// runtime. A persistent mask is a link to /dev/null, so writing the
	// unit file would replace the mask and silently undo the decision of
	// an administrator. A runtime mask lives in /run and leaves no file at
	// the unit path, so the installation would register a unit that stays
	// masked and cannot start.
	masked bool
}

// enablementMode is the autostart state of a unit, as far as an installation is
// concerned: whether the unit is enabled for the boot of the scope, and whether
// that enablement survives a reboot.
type enablementMode int

const (
	// enablementNone means the unit is not enabled for boot, or the
	// supervisor reported a state no installation can keep. The
	// installation then enables nothing.
	enablementNone enablementMode = iota
	// enablementPersistent means the unit is enabled for boot and stays
	// enabled across reboots.
	enablementPersistent
	// enablementRuntime means the unit is enabled for this boot only: an
	// administrator ran `systemctl enable --runtime`.
	enablementRuntime
)

// enablementOf returns the enablement the printed state of "systemctl
// is-enabled" describes.
//
// The printed state decides, not the exit status: "is-enabled" exits zero for
// "static", "indirect", "alias" and "linked" as well, and none of those is an
// enablement an installation could keep. Only "enabled" and "enabled-runtime"
// are enablements, and they differ in whether they survive a reboot, so the
// installation preserves the mode instead of running a plain "enable".
func enablementOf(state string) registrationEnablement {
	switch state {
	case "enabled":
		return registrationEnablement{mode: enablementPersistent}
	case "enabled-runtime":
		return registrationEnablement{mode: enablementRuntime}
	case "masked", "masked-runtime":
		return registrationEnablement{masked: true}
	default:
		// "disabled", "not-found", "static", "indirect", "alias",
		// "linked", an empty answer and a state a later release adds all
		// name no enablement to keep.
		return registrationEnablement{}
	}
}

// registrationEnablement reads whether the supervisor had the unit enabled for
// the boot of the scope, and in which mode.
//
// The answer comes from the supervisor, not from the file system: an
// enablement is the links the supervisor keeps for the [Install] section, and
// only the supervisor knows them. The printed state decides, so a state that
// exits zero without being an enablement ("static", "indirect", "alias",
// "linked") is not taken for one. A supervisor that cannot be reached prints
// nothing and describes no enablement either, which leaves the autostart state
// alone on a repeat installation, and the unreachable supervisor still fails the
// installation at the reload.
//
// The mask is part of the answer for the same reason: a persistent mask is a
// link to /dev/null at the unit path, but a runtime mask ("systemctl mask
// --runtime") lives in /run and leaves no file there, so the masked state is
// only visible through the supervisor. A fresh machine reports "not-found" and
// is not masked, which is what a first installation needs.
func (m *systemdManager) registrationEnablement(ctx context.Context) registrationEnablement {
	out, _ := m.runSystemctl(ctx, "is-enabled", unitName)
	return enablementOf(strings.TrimSpace(out))
}

// maskedUnitRefusal returns the error of an installation over a masked unit.
//
// The installation never replaces the mask: writing the unit file would unmask
// the unit silently, so the administrator who masked it would lose that
// decision without noticing. The error names the unit, the refusal and the
// command that unmask it, with the scope prefix the manager uses for systemctl.
func (m *systemdManager) maskedUnitRefusal() error {
	return fmt.Errorf("service: the unit %s is masked: an installation does not replace the mask. Unmask it with %q and run the installation again", unitName, m.supervisorHint()+" unmask "+unitName)
}

// systemdRegisterResult distinguishes a successful version check from proof
// that created directories are safe to remove after a failure.
type systemdRegisterResult struct {
	versionVerified         bool
	createdDirsSafeToRemove bool
}

// register writes the registration of the instance and hands it to the
// supervisor.
//
// The directories of the instance are already created when the function is
// called. A failure of the registration is rolled back: the unit file is
// restored, and the result tells the caller whether the remaining registration
// is proven not to reference directories this attempt created.
//
// The unit is enabled for boot only when the installation has to enable it
// (see reloadAndEnable).
//
// A replaced registration that was running is restarted and verified; a failure
// of that restart or of the verification restores the previous registration and
// brings the previous instance back into service. The restore reports a started
// instance only when the supervisor was observed running it. The result reports
// whether the restarted instance reported the expected version.

func (m *systemdManager) register(ctx context.Context, opts InstallOptions, account *user.User, identity systemdInstallIdentity, created map[string][]string, previous previousRegistration) (systemdRegisterResult, error) {
	result := systemdRegisterResult{createdDirsSafeToRemove: systemdCreatedDirsSafeToRemove(previous, created)}
	// The instance runs as the service account, so the directories it owns
	// belong to it.
	if err := m.grantInstanceDirsForDirsWithIdentity(opts, identity, created, systemdInstanceDirs(opts, previous.dynamicFields)); err != nil {
		return result, err
	}

	var unit string
	var err error
	if previous.registrationPresent() {
		sourceContent, ok := previous.sourceUnitContent()
		if !ok {
			return result, errors.New("service: the prior registration source fragment was not captured")
		}
		unit, err = updateUnit(string(sourceContent), opts, account)
	} else {
		unit, err = renderUnit(opts, account)
	}
	if err != nil {
		return result, err
	}

	if err := writeUnitFile(previous.path, unit); err != nil {
		return result, err
	}
	// The file is now part of the registration path. Until rollback proves
	// the previous unit restored or the new unit removed, its directories stay.
	result.createdDirsSafeToRemove = false
	// A first installation enables the unit for boot; a repeat installation
	// keeps the autostart state it finds, in the mode it found it.
	enable := enablementPersistent
	if previous.registrationPresent() {
		enable = previous.enablement
	}
	if err := m.reloadAndEnable(ctx, enable); err != nil {
		result.createdDirsSafeToRemove, err = m.reportUnitRollback(ctx, previous, created, err)
		return result, err
	}
	if previous.registrationPresent() {
		if err := m.verifyUpdatedUnit(ctx, opts, account, previous, unit); err != nil {
			result.createdDirsSafeToRemove, err = m.reportUnitRollback(ctx, previous, created, err)
			return result, err
		}
	}
	if !previous.running {
		// A first installation and a stopped instance stay stopped:
		// starting is an explicit, separate command.
		return result, nil
	}
	updatedUnit := parseUnit(unit)
	verified, err := m.restartAndVerify(ctx, opts.Executable, updatedUnit.listen(), opts.ExpectedVersion, updatedUnit.DynamicRunArgs["listen"])
	if err != nil {
		if restoreErr := m.restorePreviousRegistration(ctx, previous); restoreErr != nil {
			return result, fmt.Errorf("service: the installation did not put the new executable into service: %w (the previous registration could not be put back into service: %v)", err, restoreErr)
		}
		result.createdDirsSafeToRemove = systemdCreatedDirsSafeToRemove(previous, created)
		return result, fmt.Errorf("service: the installation did not put the new executable into service: %w (the previous registration was restored and the previous instance was started again)", err)
	}
	result.versionVerified = verified
	return result, nil
}

// reloadAndEnable makes the supervisor aware of the unit file and enables the
// unit for the boot of the scope in the mode the installation keeps. The reload
// always happens; the enable is only asked for a first installation or a
// registration that was enabled before, so a reinstall never re-enables a unit
// an administrator disabled.
//
// The mode is part of what is kept: a unit an administrator enabled at runtime
// ("systemctl enable --runtime", which survives no reboot) is enabled at runtime
// again, so a repeat installation neither makes that enablement permanent nor
// drops it. A masked unit is refused before this point, so it never reaches the
// reload.
func (m *systemdManager) reloadAndEnable(ctx context.Context, mode enablementMode) error {
	if _, err := m.runSystemctl(ctx, "daemon-reload"); err != nil {
		return err
	}
	switch mode {
	case enablementPersistent:
		if _, err := m.runSystemctl(ctx, "enable", unitName); err != nil {
			return err
		}
	case enablementRuntime:
		if _, err := m.runSystemctl(ctx, "enable", "--runtime", unitName); err != nil {
			return err
		}
	}
	return nil
}

// validateUnitPaths checks paths whose source bytes the operation will emit
// into the unit. A first install writes every option; an update writes the
// executable and only the explicitly named path operands.
//
// Unit-file controls can split newly written directives. An emitted
// WorkingDirectory is also checked using its native value rules; an inherited
// app state path is not that directive.
func validateUnitPaths(opts InstallOptions, existing bool) error {
	for _, p := range []struct {
		name     string
		path     string
		explicit bool
	}{
		{"executable", opts.Executable, true},
		{"config file", opts.Paths.ConfigFile, opts.Explicit.Config},
		{"state dir", opts.Paths.StateDir, opts.Explicit.StateDir},
		{"log dir", opts.Paths.LogDir, opts.Explicit.LogDir},
		{"cache dir", opts.CacheDir, opts.Explicit.CacheDir},
		{"hub dir", opts.HubDir, opts.Explicit.HubDir},
		{"local dir", opts.LocalDir, opts.Explicit.LocalDir},
	} {
		if p.path == "" || existing && !p.explicit {
			continue
		}
		if r, ok := controlCharacter(p.path); ok {
			return fmt.Errorf("service: the %s %q contains the control character %q: a path of an installation must not contain a control character", p.name, p.path, r)
		}
	}
	if !existing || opts.Explicit.StateDir {
		if _, err := systemdWorkingDirectoryValue(opts.Paths.StateDir); err != nil {
			return err
		}
	}
	return nil
}

// controlCharacter returns the first control character of the value.
//
// A control character is a character of the C0 control set: the unit file is
// a line based file, so a newline in a path would end the directive it is
// written in.
func controlCharacter(value string) (rune, bool) {
	for _, r := range value {
		if r < 0x20 {
			return r, true
		}
	}
	return 0, false
}
