// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"slices"
)

// verifyUpdatedUnit checks the command and preserved launch settings after
// daemon-reload and before a running instance can be restarted.
func (m *systemdManager) verifyUpdatedUnit(ctx context.Context, opts InstallOptions, account *user.User, previous previousRegistration, unitContent string) error {
	loaded, err := m.registrationUnitConfig(ctx)
	if err != nil {
		return fmt.Errorf("service: read the loaded configuration after daemon-reload: %w", err)
	}
	updatedUnit := parseUnit(unitContent)
	if err := loadedMatchesUnit(loaded, updatedUnit); err != nil {
		return errors.New("service: systemd did not load the requested executable and arguments; an override may own the effective command")
	}
	if !previous.loadedKnown || !previous.loaded.loaded {
		return nil
	}
	for flag, dynamic := range updatedUnit.DynamicRunArgs {
		if !dynamic || systemdInstallFlagExplicit(opts, flag) {
			continue
		}
		before := runArgValue(previous.loaded.execArgs[1:], "--"+flag)
		after := runArgValue(loaded.execArgs[1:], "--"+flag)
		if before != after {
			return fmt.Errorf("service: systemd's typed --%s argument changed across the update", flag)
		}
	}
	if !slices.Equal(loaded.environment, previous.loaded.environment) || !slices.Equal(loaded.environmentFiles, previous.loaded.environmentFiles) {
		return errors.New("service: systemd did not preserve the loaded environment and environment files")
	}
	if previous.loaded.supplementaryGroupsKnown && loaded.supplementaryGroupsKnown && !slices.Equal(loaded.supplementaryGroups, previous.loaded.supplementaryGroups) {
		return errors.New("service: systemd did not preserve the loaded supplementary groups")
	}
	if !opts.Explicit.StateDir && loaded.workingDirectory != previous.loaded.workingDirectory {
		return errors.New("service: systemd did not preserve the loaded working directory")
	}
	if !opts.Explicit.ServiceUser && (loaded.user != previous.loaded.user || loaded.group != previous.loaded.group) {
		return errors.New("service: systemd did not preserve the loaded service identity")
	}
	if opts.Explicit.StateDir && loaded.workingDirectory != opts.Paths.StateDir {
		return errors.New("service: systemd did not load the requested working directory")
	}
	if opts.Explicit.ServiceUser {
		identity, err := unitIdentity(opts, account)
		if err != nil {
			return err
		}
		identityUnit := parseUnit("[Service]\n" + identity)
		if loaded.user != identityUnit.User || loaded.group != identityUnit.Group {
			return errors.New("service: systemd did not load the requested service identity")
		}
	}
	return nil
}

// restorePreviousRegistration restores the registration an installation
// replaced and the running state it had.
//
// The unit file is restored and read back, then the supervisor is reloaded and
// its loaded command and launch properties are compared with the pre-update
// snapshot. The restart below is issued only after both states are confirmed.
//
// A restart that the supervisor accepted is not a running instance: a
// Type=simple unit is reported restarted as soon as the process was forked, so
// the running state and the readiness are read back, and an unconfirmed restart
// is an error that says exactly that. The restore never replaces the original
// failure.
func (m *systemdManager) restorePreviousRegistration(ctx context.Context, previous previousRegistration) error {
	// The caller may have cancelled the context, the previous registration
	// still has to be put back into service.
	cleanup := context.WithoutCancel(ctx)
	if err := m.rollbackUnit(cleanup, previous); err != nil {
		return err
	}
	if !previous.running {
		return nil
	}
	if _, err := m.runSystemctl(cleanup, "restart", unitName); err != nil {
		return fmt.Errorf("the registration was restored, but the restart of the previous instance was refused: %w", err)
	}
	// The restored unit supplies its static Listen source, while the restored
	// MainPID must still expose a live HFDesk service-run argv before readiness
	// can be claimed, even when the old ExecStart path was unavailable.
	sourceContent, ok := previous.sourceUnitContent()
	if !ok {
		return errors.New("the source fragment of the previous registration was not captured")
	}
	restored := parseUnit(string(sourceContent))
	if _, err := m.verifyRunningExecutableWithListenSource(cleanup, restored.ExecStart, restored.listen(), "", restored.DynamicRunArgs["listen"]); err != nil {
		return fmt.Errorf("the registration was restored and the restart of the previous instance was issued, but the restart was not confirmed: %w", err)
	}
	return nil
}

// reportUnitRollback preserves the failure that triggered a rollback and names
// only the outcome the storage and loaded-configuration checks proved.
func (m *systemdManager) reportUnitRollback(ctx context.Context, previous previousRegistration, created map[string][]string, failure error) (bool, error) {
	if err := m.rollbackUnit(context.WithoutCancel(ctx), previous); err != nil {
		return false, fmt.Errorf("%w (the previous unit file or captured loaded settings could not be restored or verified: %v)", failure, err)
	}
	if previous.registrationPresent() {
		if !previous.loaded.loaded {
			return systemdCreatedDirsSafeToRemove(previous, created), fmt.Errorf("%w (the previous registration and command were restored and loaded; no instance was restarted)", failure)
		}
		return systemdCreatedDirsSafeToRemove(previous, created), fmt.Errorf("%w (the previous registration and captured loaded command/launch settings were restored)", failure)
	}
	return true, fmt.Errorf("%w (the failed registration was removed from disk and the supervisor)", failure)
}

// verifyRestoredUnit checks that the unit file holds the previous registration
// again before systemd is asked to reload it.
func verifyRestoredUnit(previous previousRegistration) error {
	content, existed, err := readUnitFile(previous.path)
	if err != nil {
		return fmt.Errorf("the restored unit file could not be read back: %w", err)
	}
	switch {
	case !previous.existed && existed:
		return fmt.Errorf("the unit file of the failed installation is still there: %s", previous.path)
	case previous.existed && !existed:
		return fmt.Errorf("the unit file %s is gone", previous.path)
	case previous.existed && !bytes.Equal(content, previous.content):
		return fmt.Errorf("the unit file %s does not hold the previous registration", previous.path)
	}
	return nil
}

// rollbackUnit restores the registration of a failed installation and checks
// both the file on disk and the captured command/launch properties systemd
// reported before the update. A successful file write or daemon-reload command
// alone does not prove those loaded properties returned to their prior values.
func (m *systemdManager) rollbackUnit(ctx context.Context, previous previousRegistration) error {
	// The caller may have cancelled the context, the rollback still has to
	// clean the registration up.
	cleanup := context.WithoutCancel(ctx)
	if previous.existed {
		if err := writeUnitFile(previous.path, string(previous.content)); err != nil {
			return fmt.Errorf("write the previous unit file back: %w", err)
		}
	} else {
		if err := os.Remove(previous.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove the failed unit file: %w", err)
		}
	}
	if err := verifyRestoredUnit(previous); err != nil {
		return err
	}
	if _, err := m.runSystemctl(cleanup, "daemon-reload"); err != nil {
		return fmt.Errorf("reload the restored unit: %w", err)
	}
	loaded, err := m.registrationUnitConfig(cleanup)
	if err != nil {
		return fmt.Errorf("read the restored loaded configuration: %w", err)
	}
	if previous.existed {
		if !previous.loadedKnown {
			return errors.New("the previous loaded configuration was not captured before the update")
		}
		if previous.loaded.loaded && !sameSystemdLoadedConfig(loaded, previous.loaded) {
			return errors.New("the supervisor did not load the previous command and captured launch settings")
		}
		if !previous.loaded.loaded {
			if !loaded.loaded {
				return errors.New("the restored unit remains unknown to systemd")
			}
			sourceContent, ok := previous.sourceUnitContent()
			if !ok {
				return errors.New("the source fragment of the previous unit was not captured")
			}
			if err := loadedMatchesUnit(loaded, parseUnit(string(sourceContent))); err != nil {
				return fmt.Errorf("the restored unit file is present but its command did not load: %w", err)
			}
		}
	} else {
		if !previous.loadedKnown {
			return errors.New("the prior loaded state was not captured before the update")
		}
		if previous.loaded.loaded {
			if !loaded.loaded || !sameSystemdLoadedConfig(loaded, previous.loaded) {
				return errors.New("systemd did not restore the loaded fragment after the override was removed")
			}
		} else if loaded.loaded {
			return errors.New("the failed unit is still loaded after its file was removed")
		}
	}
	return nil
}
