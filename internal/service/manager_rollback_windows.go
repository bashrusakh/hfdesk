// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package service

import (
	"context"
	"errors"
	"fmt"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
)

// scmRollbackMode tells a rollback how much of the previous installation to put
// back.
type scmRollbackMode int

const (
	// scmRollbackRegistration writes the previous registration back and nothing
	// else. It is the rollback of a failure that never touched the running
	// instance: the process that runs keeps the image it was started from.
	scmRollbackRegistration scmRollbackMode = iota
	// scmRollbackRestart stops the instance the failed installation started,
	// writes the previous registration back and brings the previous program of
	// a running instance back into service.
	scmRollbackRestart
	// scmRollbackStop settles a stop the installation accepted or that may
	// still put the instance down.
	scmRollbackStop
	// scmRollbackUndeliveredStop restores the registration after a stop that
	// was never delivered. No control reached the instance, so the restore
	// does not touch the running process.
	scmRollbackUndeliveredStop
)

// scmStopRollbackMode returns the rollback a failed stop needs: an accepted or
// unreadable stop may still put the instance down, while an undelivered control
// leaves it running behind a registration that has to be restored.
func scmStopRollbackMode(outcome scmStopOutcome) scmRollbackMode {
	switch outcome {
	case scmStopUnsettled:
		return scmRollbackStop
	case scmStopNotDelivered:
		return scmRollbackUndeliveredStop
	default:
		return scmRollbackRegistration
	}
}

// rollbackInstallation restores the configuration of the registration a failed
// installation replaced.
//
// The failure is the error the caller gets, and the outcome of the restore is
// reported next to it. The restore is the configuration and not the whole
// registration: the failure recovery is left as the installation configured it.
//
// The running instance is only touched when the installation switched it, or
// when its stop may still put it down; the stop is proven before the restore.
// A stop that was never delivered is not such a case: the restore does not
// touch the process, and the previous registration describes it again.
func (m *scmManager) rollbackInstallation(ctx context.Context, target scmRecoveryTarget, previous previousSCMRegistration, mode scmRollbackMode, failure error) (createdDirsSafeToRemove bool, err error) {
	if previous.present && previous.running {
		switch mode {
		case scmRollbackRestart:
			return m.rollbackRestart(ctx, target, previous, failure, scmRollbackStartedInstance)
		case scmRollbackStop:
			return m.rollbackRestart(ctx, target, previous, failure, scmRollbackPreviousInstance)
		case scmRollbackUndeliveredStop:
			return m.rollbackUndeliveredStop(ctx, target, previous, failure)
		}
	}
	if err := restoreSCMRegistration(target, previous); err != nil {
		return false, fmt.Errorf("%w (the registration of the failed installation could not be put back: %v)", failure, err)
	}
	if !previous.present {
		// The failed installation created the registration: there was no
		// previous one to restore, and the registration is removed again.
		return true, fmt.Errorf("%w (the registration of the failed installation was removed)", failure)
	}
	return true, fmt.Errorf("%w (the configuration of the previous registration was restored, the running instance was not restarted; the failure recovery of the registration is not part of the restore)", failure)
}

type scmRollbackAttempt int

const (
	scmRollbackStartedInstance scmRollbackAttempt = iota
	// scmRollbackPreviousInstance is an instance that may still run.
	scmRollbackPreviousInstance
	// scmRollbackUndeliveredInstance is the instance the failed installation
	// replaced, whose stop was never delivered.
	scmRollbackUndeliveredInstance
)

// scmRestartRollback is what the restart rollback of a failed installation
// proved about the machine.
//
// The facts are what was observed, not what was attempted, so no sentence
// composed from them claims more than the rollback can prove. They describe the
// configuration and the running instance, not the whole registration: the
// restore leaves the failure recovery in place.
type scmRestartRollback struct {
	// attempt names the instance the rollback had to settle.
	attempt scmRollbackAttempt
	stopped bool
	// restored reports whether the configuration of the previous
	// registration is in place again. The failure recovery is not part of
	// that configuration, and the rollback never restores it.
	restored bool
	// programKnown reports whether the restored registration names the
	// program the previous instance ran.
	programKnown bool
	// started reports whether the instance was started again and the start
	// was accepted.
	started bool
	// running reports that the state of the instance was read as running
	// after the restore. A stop that was never delivered leaves it running.
	running bool
	// verified reports whether the running process was proven to be the
	// previous program, in a process that is none of the processes of the
	// failed attempt.
	verified bool
}

// scmStopStatusHint is the next step after a stop that was not settled.
const scmStopStatusHint = `run "hfdesk service status" to read the state of the service before another start`

// scmRestartRollbackSentence returns the sentence that reports the proven
// outcome of a restart rollback next to the failure of the installation.
//
// It is the only place the outcome is described, so a caller reads no sentence
// for a state the rollback did not reach: the claim that the previous instance
// was started again is made for a verified rollback only, and the sentence of a
// completed rollback says that the failure recovery stays in place.
//
// A stop that was never delivered is reported as the restore it is: the instance
// was not asked to stop, so the restore needed no stop and the sentence names
// the running state instead.
func scmRestartRollbackSentence(facts scmRestartRollback, reason string) string {
	previousInstance := facts.attempt == scmRollbackPreviousInstance || facts.attempt == scmRollbackUndeliveredInstance
	if facts.attempt == scmRollbackUndeliveredInstance {
		switch {
		case !facts.restored:
			return fmt.Sprintf("no stop control was delivered to the instance the failed installation replaced, but the configuration of the previous registration could not be put back: %s", reason)
		case facts.running:
			return "no stop control was delivered to the instance the failed installation replaced and the configuration of the previous registration was restored; the instance still runs and no instance was started"
		case !facts.stopped:
			return fmt.Sprintf("no stop control was delivered to the instance the failed installation replaced and the configuration of the previous registration was restored, but the running state of the instance could not be confirmed, so no instance was started: %s; %s", reason, scmStopStatusHint)
		}
	}
	switch {
	case !facts.stopped:
		// No instance is started: a second one would compete with it.
		if previousInstance {
			return fmt.Sprintf("the registration of the failed installation was kept because the instance it replaced was not proven to be stopped: %s: the previous registration was not restored and no instance was started; %s", reason, scmStopStatusHint)
		}
		return fmt.Sprintf("the registration of the failed installation was kept because the instance it started was not proven to be stopped: %s: the previous registration was not restored and no instance was started; %s", reason, scmStopStatusHint)
	case !facts.restored:
		if previousInstance {
			return fmt.Sprintf("the previous instance was proven stopped, but the previous registration could not be put back: %s", reason)
		}
		return fmt.Sprintf("the instance of the failed installation was stopped, but the previous registration could not be put back: %s", reason)
	case !facts.programKnown:
		// The restored registration carries a command line that cannot be
		// read, so there is no program to start: the rollback reports the
		// registration and the state of the instance and never claims a
		// start it did not make.
		if previousInstance {
			return fmt.Sprintf("the previous instance was proven stopped and the configuration of the previous registration was restored, but the restored registration names no program, so no instance was started: the instance of %s is stopped", ServiceName)
		}
		return fmt.Sprintf("the instance of the failed installation was stopped and the configuration of the previous registration was restored, but the restored registration names no program, so no instance was started: the instance of %s is stopped", ServiceName)
	case !facts.started:
		// The start was refused, or it was accepted and the instance never
		// proved to run the previous program. Neither is a claim about the
		// program that runs: the reason names what was observed.
		if previousInstance {
			return fmt.Sprintf("the previous instance was proven stopped and the configuration of the previous registration was restored, but the previous instance was not proven to be in service again: %s", reason)
		}
		return fmt.Sprintf("the instance of the failed installation was stopped and the configuration of the previous registration was restored, but the previous instance was not proven to be in service again: %s", reason)
	case !facts.verified:
		if previousInstance {
			return "the previous instance was proven stopped and the configuration of the previous registration was restored and an instance was started, but the running process was not proven to be the previous program: the image of the process could not be read"
		}
		return "the instance of the failed installation was stopped and the configuration of the previous registration was restored and an instance was started, but the running process was not proven to be the previous program: the image of the process could not be read"
	default:
		if previousInstance {
			return "the previous instance was proven stopped, the configuration of the previous registration was restored and the previous instance was started again; the failure recovery of the registration was not part of the restore and stays as the installation left it"
		}
		return "the instance of the failed installation was stopped, the configuration of the previous registration was restored and the previous instance was started again; the failure recovery of the registration was not part of the restore and stays as the installation left it"
	}
}

// rollbackRestart stops the instance the rollback has to remove, restores the
// previous registration and brings the previous instance back into service.
//
// The instance is stopped before the previous registration is written back,
// because the restored registration would otherwise describe another program
// than the running one. The stop is a clean no-op for an instance that does not
// run, and it is bounded by the shared budget.
//
// A stop that does not reach its goal leaves the machine as the failed
// installation left it, and the sentence says exactly that. The previous
// program is started again only when the restored registration names it, and
// the restart is reported as a success only when it was verified.
func (m *scmManager) rollbackRestart(ctx context.Context, target scmRecoveryTarget, previous previousSCMRegistration, failure error, attempt scmRollbackAttempt) (createdDirsSafeToRemove bool, err error) {
	// The caller may have cancelled the context, the machine still has to be
	// put back the way it was.
	cleanup := context.WithoutCancel(ctx)
	facts := scmRestartRollback{attempt: attempt}
	report := func(reason string) error {
		return fmt.Errorf("%w (%s)", failure, scmRestartRollbackSentence(facts, reason))
	}

	// The process of the failed attempt is read before it is stopped: the
	// verification below excludes it, so a process the rollback did not start
	// is never taken for the previous instance.
	failedProcessID := scmRunningProcessID(target)

	if _, err := m.stopService(cleanup, target); err != nil {
		return false, report(err.Error())
	}
	facts.stopped = true

	if err := restoreSCMConfig(target, previous); err != nil {
		return false, report(err.Error())
	}
	facts.restored = true
	err = startPreviousProgram(cleanup, target, previous, &facts, report, previous.processID, failedProcessID)
	return facts.restored, err
}

// rollbackUndeliveredStop restores the registration after a stop that was never
// delivered and brings the previous instance back into service only when its
// state proved it stopped.
//
// No stop control reached the instance, so it was never asked to stop: writing
// the previous configuration back does not touch the process, and the restored
// registration names the program it was started from again. The state is read
// after the restore: a running instance needs nothing more, a settled stopped
// one is started and verified, and an unreadable or unsettled state is reported
// without a start.
func (m *scmManager) rollbackUndeliveredStop(ctx context.Context, target scmRecoveryTarget, previous previousSCMRegistration, failure error) (createdDirsSafeToRemove bool, err error) {
	// The machine still has to be put back after the caller cancelled.
	cleanup := context.WithoutCancel(ctx)
	facts := scmRestartRollback{attempt: scmRollbackUndeliveredInstance}
	report := func(reason string) error {
		return fmt.Errorf("%w (%s)", failure, scmRestartRollbackSentence(facts, reason))
	}

	if err := restoreSCMConfig(target, previous); err != nil {
		return false, report(err.Error())
	}
	facts.restored = true

	status, err := target.Query()
	if err != nil {
		return true, report(err.Error())
	}
	switch status.State {
	case svc.Running:
		facts.running = true
		return true, report("")
	case svc.Stopped:
		facts.stopped = true
	default:
		return true, report(fmt.Sprintf("the state of the instance was %v", status.State))
	}
	return true, startPreviousProgram(cleanup, target, previous, &facts, report, previous.processID)
}

// startPreviousProgram starts the program of the restored registration and
// records what the start proved. A registration that names no program has
// nothing to start; the caller's report says that.
func startPreviousProgram(ctx context.Context, target scmRecoveryTarget, previous previousSCMRegistration, facts *scmRestartRollback, report func(string) error, exclude ...uint32) error {
	executable := scmConfigExecutable(previous.config)
	if executable == "" {
		return report("")
	}
	facts.programKnown = true
	check, err := target.StartAndVerify(ctx, executable, exclude...)
	if err != nil {
		return report(err.Error())
	}
	facts.started = true
	facts.verified = check == scmImageVerified
	return report("")
}

// restoreSCMRegistration puts the configuration of the registration back that a
// failed installation replaced.
//
// A registration of a first installation is removed. A replaced registration is
// written back, which proves the command line and thus the program of the
// instance. The write back leaves the failure recovery as the installation
// configured it.
func restoreSCMRegistration(target scmRecoveryTarget, previous previousSCMRegistration) error {
	if !previous.present {
		if err := target.Delete(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE) {
			return fmt.Errorf("service: remove the registration of %s: %w", ServiceName, err)
		}
		return nil
	}
	return restoreSCMConfig(target, previous)
}

// restoreSCMConfig writes the configuration of the previous registration back
// and reads it back afterwards.
//
// It is the one place the configuration of a replaced registration is restored:
// the rollback of a failed installation and the failure path of a refused update
// both call it, so the two cannot drift apart. The read back proves the command
// line and thus the program of the instance; the failure recovery is not part of
// the configuration this restores.
func restoreSCMConfig(registration scmConfiguration, previous previousSCMRegistration) error {
	if !previous.present {
		return errors.New("service: the failed installation created the registration, so there is no configuration to restore")
	}
	if err := registration.UpdateConfig(previous.config); err != nil {
		return fmt.Errorf("service: write the previous registration of %s back: %w", ServiceName, err)
	}
	config, err := registration.Config()
	if err != nil {
		return fmt.Errorf("service: read the restored registration of %s back: %w", ServiceName, err)
	}
	if !scmSameRegistration(config.BinaryPathName, previous.config.BinaryPathName) {
		return fmt.Errorf("service: the registration of %s does not hold the previous command line again", ServiceName)
	}
	return nil
}
