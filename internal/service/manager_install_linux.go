// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"strings"
)

// Install registers the service with the supervisor.
//
// The installation follows the canonical-unit ownership model. It validates
// its request, reads the registration, refuses any state it does not own
// before anything changes, merges its request with the canonical facts of
// the existing registration, preflights the new program, and only then
// writes the unit atomically as the canonical rendering of the merged facts:
// the write changes the location of the executable and the options the
// request named, and nothing else.
//
// Three refusals happen before any mutation, so a refused installation
// stops nothing, writes nothing and creates no directory: a base file that
// is not the canonical unit HFDesk generates (hand edits and "env" wrappers
// included), an effective ExecStart a drop-in owns, and a loaded fragment
// HFDesk did not write. The mask of the unit is asked of the supervisor
// first, so a masked unit refuses with the unmask action instead of being
// misclassified as a hand edit, and a runtime mask, which leaves no file at
// the unit path, refuses as well.
//
// The enablement is asked of the supervisor in its textual form and is
// preserved by non-interference: a first installation enables the unit for
// the boot of the scope, while a repeat installation never runs enable at
// all, so an administrator who disabled the unit keeps it disabled and a
// unit enabled with `systemctl enable --runtime` never becomes permanent.
//
// A machine-wide instance runs as the dedicated service account of the
// platform; the account is validated before anything is created, and the
// installation never creates an account and never changes the ownership of a
// model or a cache tree.
//
// A reinstalled instance that was running before is restarted and verified:
// the loaded command must be the canonical command that was written, the
// process must answer readiness as its own instance, and the MainPID after
// the restart must be a different process than the one snapshotted before
// it. A stopped instance and a first installation are never started.
//
// A failure after the unit file was written restores the saved unit bytes
// and verifies the loaded configuration; a failed running restart only
// restarts the previous instance after that verification. Created
// directories are removed only after no-write or rollback evidence shows
// they are unreferenced.
//
// The returned options are the registration as written, so a caller reports
// a preserved setting instead of the default it was not reset to.
func (m *systemdManager) Install(ctx context.Context, opts InstallOptions) (InstallOptions, error) {
	requested := opts

	// Step 1: validate the request before anything is read or written. A
	// refused installation leaves nothing behind, and a path with a control
	// character would end up in the unit file literally, splitting it into
	// a directive the installation never wrote.
	if err := ValidateInstallOptions(requested); err != nil {
		return requested, err
	}
	if err := validateUnitPaths(requested, false); err != nil {
		return requested, err
	}
	if err := m.checkSystemPaths(requested); err != nil {
		return requested, err
	}

	// Step 2: read the registration of the scope. The bytes of the file are
	// the fact source of an existing registration; their absence decides
	// whether this is a first installation.
	path, err := m.unitPath()
	if err != nil {
		return InstallOptions{}, err
	}
	content, existed, err := readUnitFile(path)
	if err != nil {
		return InstallOptions{}, err
	}

	// The mask is judged before anything else is reported: a masked unit
	// refuses the installation instead of being replaced, is never
	// unmasked, and its refusal names the unmask command of the observed
	// mask kind. The supervisor answers when it can be reached — a runtime
	// mask ("systemctl mask --runtime") lives in /run and leaves no file
	// at the unit path, and both mask kinds report LoadState="masked", so
	// a masked unit has no loaded registration whose read could describe
	// it — that read must not turn the refusal into a generic read error
	// without the unmask action. The persistent mask answers on its own
	// from the file: the symbolic link to /dev/null at the unit path,
	// whose bytes are no unit at all, so the hand-edit detector must not
	// misclassify it either. An unreachable supervisor degrades the printed
	// state to an empty answer and the link still refuses; a fresh machine
	// answers "not-found" with no link, which is no mask.
	enablement := m.registrationEnablement(ctx)
	if enablement.masked || unitPathPersistentMask(path) {
		return opts, m.maskedUnitRefusal(enablement.maskedRuntime)
	}

	// The loaded registration of a first installation is read before the
	// file is judged: a missing writable override is still an existing
	// registration when systemd loaded a fragment for this name, and such a
	// fragment is refused below instead of being adopted. A machine-wide
	// scope is read only where this process may read it; a non-root first
	// installation of that scope fails the root requirement below instead.
	var initialLoaded systemdLoadedConfig
	initialLoadedKnown := false
	if !existed && (m.scope != ScopeSystem || os.Geteuid() == 0) {
		initialLoaded, err = m.registrationUnitConfig(ctx)
		if err != nil {
			return opts, fmt.Errorf("service: determine whether %s already has a loaded registration: %w", unitName, err)
		}
		initialLoadedKnown = true
	}

	previous := previousRegistration{
		path: path, content: content, existed: existed,
		// The first-installation read above is the captured prior loaded
		// state as well: a rollback of a failed first registration has to
		// prove the supervisor knew nothing before the write.
		loaded: initialLoaded, loadedKnown: initialLoadedKnown,
	}
	registered := existed
	if registered {
		// Detector A: the base file must be the canonical unit HFDesk
		// generates or the update is refused before any change — a hand
		// edit, an "env" wrapper, a dynamic operand or another version's
		// bytes are not the registration this update may rewrite. The
		// facts of that exact form are the merge source.
		facts, detectErr := detectHandEditedUnit(path, content, requested.Scope)
		if detectErr != nil {
			return opts, detectErr
		}
		previous.facts = facts
	} else if initialLoaded.loaded {
		// Detector C: systemd loads a fragment HFDesk did not write under
		// the unit name. A foreign registration is refused and never
		// adopted, so there are no registration facts to merge.
		if detectErr := detectForeignRegistration(path, initialLoaded, m.scope); detectErr != nil {
			return opts, detectErr
		}
		// The fragment is our own unit path although the read found no
		// file: the registration changed underneath this installation.
		// The supervisor holds the vanished file in memory until a
		// reload reads the file system again, so the instruction
		// reloads before the retry — a plain retry reads the same
		// stale load and refuses the same way.
		return opts, fmt.Errorf("service: the update was refused and the registration was not changed: systemd loads %s from %q, but the file was absent when the update read it. Reload the unit files with %q and run %q again", unitName, path, m.supervisorHint()+" daemon-reload", "hfdesk service install")
	}

	// Step 3: the loaded configuration of an existing registration, read
	// once and fail closed — an update that cannot establish what the
	// supervisor loaded changes nothing. Detector B then refuses a drop-in
	// that owns the effective ExecStart before any mutation, naming the
	// DropInPaths the supervisor reports.
	loaded := initialLoaded
	loadedKnown := initialLoadedKnown
	if registered && !loadedKnown && (m.scope != ScopeSystem || os.Geteuid() == 0) {
		loaded, err = m.registrationUnitConfig(ctx)
		if err != nil {
			return opts, fmt.Errorf("service: the loaded configuration of %s could not be read, the registration was not changed: %w", unitName, err)
		}
		loadedKnown = true
	}
	if registered && loaded.loaded {
		if err := detectExecStartOverride(loaded, previous.facts, m.scope); err != nil {
			return opts, err
		}
	}

	// The snapshot of the replaced registration is read before anything is
	// written: the unit bytes, the loaded launch configuration, the
	// textual enablement already read above, the running fact and — for a
	// running instance — the MainPID the restart proof later requires to
	// differ. Each fact fails closed before the mutation it protects.
	if registered && (m.scope != ScopeSystem || os.Geteuid() == 0) {
		running, runErr := m.registrationRunning(ctx)
		if runErr != nil {
			return opts, runErr
		}
		previous.running = running
		if running {
			if !loaded.loaded {
				return opts, errors.New("service: systemd reported a running unit without a loaded configuration; the registration was not changed")
			}
			mainPID, pidErr := m.currentMainPID(ctx)
			if pidErr != nil {
				return opts, fmt.Errorf("service: the running instance of %s exposed no MainPID, the registration was not changed: %w", unitName, pidErr)
			}
			previous.mainPID = mainPID
			previous.mainPIDKnown = true
		}
		previous.loaded = loaded
		previous.loadedKnown = true
	}

	// Step 4: merge. An explicit request field wins; every field the
	// request did not name keeps the value of the canonical registration,
	// including an absent optional directory.
	if registered {
		opts = mergeInstallOptions(opts, previous.facts.registrationOptions(), true)
	}

	// The merged registration is validated as well: its values are the
	// ones the write will emit.
	if err := ValidateInstallOptions(opts); err != nil {
		return opts, err
	}
	// The check runs before the root requirement, before any directory is
	// created and before the unit file is written: a refused installation
	// leaves nothing behind.
	if err := validateUnitPaths(opts, registered); err != nil {
		return opts, err
	}
	// A machine-wide instance must not adopt the state of the user who
	// runs the install command.
	if err := m.checkSystemPaths(opts); err != nil {
		return opts, err
	}
	// The account of a machine-wide instance is a rule of the scope and
	// not a privilege of the process, so it is validated the same way:
	// before the root requirement and before any mutation. An update
	// without a named account validates the loaded identity of the
	// registration (which a drop-in may provide) and not the default
	// account of the platform.
	accountOpts := m.accountValidationOptions(ctx, requested, opts, registered)
	account, err := m.serviceAccount(accountOpts)
	if err != nil {
		return opts, err
	}
	identity := systemdAccessIdentityForInstall(opts.Scope, requested, registered, loaded, loadedKnown, account)
	if err := m.checkServiceAccountAccessWithIdentity(opts, identity.accountName, identity.access); err != nil {
		return opts, err
	}
	// The root requirement is checked before any directory is created: a
	// permission error of the machine wide directories is a much less
	// readable answer than the real reason.
	if err := m.requireRoot("an installation"); err != nil {
		return opts, err
	}

	// Step 5: preflight the new program — format, machine identity and
	// account access — before any stop or write, so a refused program
	// creates no directory, rewrites no unit and stops no instance.
	workingDirectory := systemdWorkingDirectoryForPreflight(opts, registered, loaded, loadedKnown)
	if err := m.preflightWithContext(ctx, opts, identity.accountName, identity.access, workingDirectory); err != nil {
		return opts, err
	}

	// Step 6: the directories the installation is about to create are
	// remembered before they are created, so the effective systemd
	// identity is given ownership of exactly those and of nothing else.
	// Existing directories belong to their current owner and are left
	// alone. The list is also what a failed installation removes again.
	instanceDirectories := instanceDirs(opts)
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
	if err := PrepareDirs(opts); err != nil {
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
	// It is the exact byte snapshot a rollback writes back.
	content []byte
	// existed reports whether the scope had a writable unit file at path.
	existed bool
	// facts are the canonical facts of content. They are only valid for a
	// registration whose file passed detector A, so they stay zero for a
	// first installation.
	facts canonicalUnit
	// running reports whether the instance ran before the installation.
	// Only such an instance is restarted.
	running bool
	// mainPID and mainPIDKnown snapshot the process of the running
	// instance before the update. The restart proof requires the MainPID
	// after the restart to be a different process, so a running instance
	// without a readable MainPID refuses the installation before any
	// mutation.
	mainPID      uint32
	mainPIDKnown bool
	// loaded is the effective command and launch configuration reported by
	// systemd before the update. The file on disk alone cannot prove what
	// the supervisor has loaded.
	loaded systemdLoadedConfig
	// loadedKnown reports that loaded was read successfully for an existing
	// registration. A first installation expects no loaded registration.
	loadedKnown bool
}

func (p previousRegistration) registrationPresent() bool {
	return p.existed || (p.loadedKnown && p.loaded.loaded)
}

// registrationEnablement is what the supervisor reports about the enablement of
// the unit.
type registrationEnablement struct {
	// mode is the autostart state the supervisor reported for the unit, in
	// its textual form. An installation preserves it by not touching the
	// enablement at all on a repeat installation: no enable runs, so a
	// disabled unit stays disabled and a runtime-only enablement never
	// becomes permanent.
	mode enablementMode
	// masked reports whether the unit is masked, persistently or at
	// runtime. A persistent mask is a link to /dev/null, so writing the
	// unit file would replace the mask and silently undo the decision of
	// an administrator. A runtime mask lives in /run and leaves no file at
	// the unit path, so the installation would register a unit that stays
	// masked and cannot start.
	masked bool
	// maskedRuntime reports whether that mask is a runtime mask. The kind
	// names the unmask command of the refusal: a plain `systemctl unmask`
	// removes a persistent mask only, and `systemctl unmask --runtime`
	// removes a runtime mask only, so naming the wrong form leaves the mask
	// in place after the administrator follows the error and retries.
	maskedRuntime bool
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
// enablement. Only "enabled" and "enabled-runtime" are enablements, and they
// differ in whether they survive a reboot — the distinction the printed-state
// oracle of the classification pins. No installation runs a plain "enable" on
// an existing registration anymore: the mode is preserved by non-interference,
// and only the mask part of this answer refuses an installation.
func enablementOf(state string) registrationEnablement {
	switch state {
	case "enabled":
		return registrationEnablement{mode: enablementPersistent}
	case "enabled-runtime":
		return registrationEnablement{mode: enablementRuntime}
	case "masked":
		return registrationEnablement{masked: true}
	case "masked-runtime":
		return registrationEnablement{masked: true, maskedRuntime: true}
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
// The runtime mask is part of the answer for the same reason: "systemctl
// mask --runtime" lives in /run and leaves no file at the unit path, so that
// kind of mask is visible only through the supervisor. The persistent mask —
// the link to /dev/null at the unit path — is checked from the file as well,
// because this answer degrades to "no mask" when the supervisor cannot be
// reached. A fresh machine reports "not-found" and is not masked, which is
// what a first installation needs.
func (m *systemdManager) registrationEnablement(ctx context.Context) registrationEnablement {
	out, _ := m.runSystemctl(ctx, "is-enabled", unitName)
	return enablementOf(strings.TrimSpace(out))
}

// unitPathPersistentMask reports whether the unit path itself is a
// persistent mask: the symbolic link to /dev/null "systemctl mask" leaves
// there. The link is file evidence that answers without the supervisor.
func unitPathPersistentMask(path string) bool {
	target, err := os.Readlink(path)
	return err == nil && target == os.DevNull
}

// maskedUnitRefusal returns the error of an installation over a masked unit.
//
// The installation never replaces the mask: writing the unit file would unmask
// the unit silently, so the administrator who masked it would lose that
// decision without noticing. The error names the unit, the refusal and the one
// unmask command that clears the observed kind of mask, with the scope prefix
// the manager uses for systemctl. The kind matters because each form clears
// only its own mask: a plain "systemctl unmask" exits zero but leaves a
// runtime mask in place, and "systemctl unmask --runtime" does not remove a
// persistent mask — naming the form that does not work would send the
// administrator into a retry loop.
func (m *systemdManager) maskedUnitRefusal(maskedRuntime bool) error {
	unmask := "unmask"
	if maskedRuntime {
		unmask = "unmask --runtime"
	}
	return fmt.Errorf("service: the unit %s is masked: an installation does not replace the mask. Unmask it with %q and run the installation again", unitName, m.supervisorHint()+" "+unmask+" "+unitName)
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
// called. A failure of the registration is rolled back: the saved unit bytes
// are written back, and the result tells the caller whether the remaining
// registration is proven not to reference directories this attempt created.
//
// The write is the atomic canonical rendering of the merged facts, guarded
// against a file that changed since it was read: only the executable
// location and the options the request named differ from the previous
// bytes. The supervisor reloads the unit always, and only a first
// installation enables it, so the enablement a repeat installation found —
// persistent or runtime-only — is preserved untouched.
//
// A replaced registration that was running is restarted and verified: the
// loaded command must be the canonical command that was written, the
// restarted process must answer readiness as its own instance, and its
// MainPID must be a different process than the snapshot taken before the
// restart. A failure of any of those proofs restores the previous
// registration and brings the previous instance back into service. The
// restore reports a started instance only when the supervisor was observed
// running it. The result reports whether the restarted instance reported
// the expected version.
func (m *systemdManager) register(ctx context.Context, opts InstallOptions, account *user.User, identity systemdInstallIdentity, created map[string][]string, previous previousRegistration) (systemdRegisterResult, error) {
	result := systemdRegisterResult{createdDirsSafeToRemove: systemdCreatedDirsSafeToRemove(previous, created)}
	// The instance runs as the service account, so the directories it owns
	// belong to it.
	if err := m.grantInstanceDirsForDirsWithIdentity(opts, identity, created, instanceDirs(opts)); err != nil {
		return result, err
	}

	// The canonical facts of the write: the merged options plus the
	// already-resolved HOME and account identity.
	facts, err := canonicalUnitForInstall(opts, account)
	if err != nil {
		return result, err
	}
	unit, err := renderCanonicalUnit(facts)
	if err != nil {
		return result, err
	}

	// The TOCTOU guard of the atomic write: the file re-read immediately
	// before the write must still be the snapshot this installation read,
	// or the write would overwrite a change it never saw. Nothing was
	// written yet, so the conservative no-write cleanup decision stands.
	current, currentExists, err := readUnitFile(previous.path)
	if err != nil {
		return result, err
	}
	if !unitFileUnchanged(previous.content, previous.existed, current, currentExists) {
		return result, unitChangedDuringUpdateRefusal(previous.path)
	}
	if err := writeUnitFile(previous.path, unit); err != nil {
		return result, err
	}
	// The file is now part of the registration path. Until rollback proves
	// the previous unit restored or the new unit removed, its directories stay.
	result.createdDirsSafeToRemove = false
	// The reload always happens; the enable is asked for by a first
	// installation only. A repeat installation never runs enable at all:
	// the enablement it found is preserved by non-interference, so a
	// runtime-only enablement never becomes permanent and a disabled unit
	// stays disabled. A masked unit was refused before this point.
	if err := m.reloadAndEnable(ctx, !previous.registrationPresent()); err != nil {
		result.createdDirsSafeToRemove, err = m.reportUnitRollback(ctx, previous, created, err)
		return result, err
	}
	if previous.registrationPresent() {
		if err := m.verifyUpdatedUnit(ctx, opts, account, previous, facts); err != nil {
			result.createdDirsSafeToRemove, err = m.reportUnitRollback(ctx, previous, created, err)
			return result, err
		}
	}
	if !previous.running {
		// A first installation and a stopped instance stay stopped:
		// starting is an explicit, separate command.
		return result, nil
	}
	verified, err := m.restartAndVerify(ctx, facts.executable, facts.listen, opts.ExpectedVersion, false)
	if err == nil {
		// The own-PID fact of the restart proof (D-11): the process the
		// supervisor reports after the restart must be a different one
		// than the snapshot before it — a stable MainPID across the
		// restart means the old instance kept serving and the new
		// registration was never put into service.
		newPID, pidErr := m.currentMainPID(ctx)
		switch {
		case pidErr != nil:
			err = fmt.Errorf("service: the restarted %s did not report the MainPID its restart must prove: %w", unitName, pidErr)
		case newPID == previous.mainPID:
			err = fmt.Errorf("service: the restarted %s kept the previous MainPID %d; the restart did not start a new instance", unitName, previous.mainPID)
		}
	}
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

// reloadAndEnable makes the supervisor aware of the unit file, and enables
// the unit for the boot of the scope only when this installation is the
// first registration of it.
//
// The reload always happens. The enable never runs on a repeat
// installation — neither `enable` nor `enable --runtime` — so the
// textual enablement the installation found is preserved untouched: a unit
// an administrator disabled stays disabled, and a unit enabled for this
// boot only ("systemctl enable --runtime") never becomes permanent. A
// first installation enables the registration it writes, persistently. A
// masked unit is refused before this point, so it never reaches the reload.
func (m *systemdManager) reloadAndEnable(ctx context.Context, firstInstallation bool) error {
	if _, err := m.runSystemctl(ctx, "daemon-reload"); err != nil {
		return err
	}
	if firstInstallation {
		if _, err := m.runSystemctl(ctx, "enable", unitName); err != nil {
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
