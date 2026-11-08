// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

func scmStatusFromTarget(target interface {
	Query() (svc.Status, error)
	Config() (mgr.Config, error)
}) (Status, svc.Status, error) {
	status, err := target.Query()
	if err != nil {
		return Status{}, svc.Status{}, err
	}
	config, err := target.Config()
	if err != nil {
		return Status{}, svc.Status{}, err
	}
	result, err := scmStatusFrom(ScopeSystem, scmRegistrationFacts{
		present:     true,
		state:       status.State,
		exitCode:    status.Win32ExitCode,
		processID:   status.ProcessId,
		commandLine: config.BinaryPathName,
	})
	return result, status, err
}

// scmRecoveryTarget is the registration a failed installation restores. The
// interface lets the recovery run without a real registration.
type scmRecoveryTarget interface {
	scmServiceControl
	scmConfiguration
	Delete() error
	StartAndVerify(ctx context.Context, executable string, exclude ...uint32) (scmImageCheck, error)
}

// scmServiceRecoveryTarget is the recovery target of a real registration.
type scmServiceRecoveryTarget struct {
	manager *scmManager
	handle  *mgr.Service
}

func (t scmServiceRecoveryTarget) Query() (svc.Status, error) { return scmQueryStatus(t.handle) }

func (t scmServiceRecoveryTarget) ReadRunningImage(processID uint32, executable string) (bool, scmImageCheck, string) {
	return scmReadRunningImage(t.handle, processID, executable)
}

func (t scmServiceRecoveryTarget) Start() error { return scmStartService(t.handle) }

func (t scmServiceRecoveryTarget) Control(cmd svc.Cmd) (svc.Status, error) {
	return scmControlService(t.handle, cmd)
}

func (t scmServiceRecoveryTarget) UpdateConfig(config mgr.Config) error {
	return t.handle.UpdateConfig(config)
}

func (t scmServiceRecoveryTarget) Config() (mgr.Config, error) {
	return scmQueryServiceConfig(t.handle)
}

func (t scmServiceRecoveryTarget) Delete() error { return scmDeleteService(t.handle) }

func (t scmServiceRecoveryTarget) StartAndVerify(ctx context.Context, executable string, exclude ...uint32) (scmImageCheck, error) {
	return t.manager.startAndVerify(ctx, t.handle, executable, exclude...)
}

// scmRunningProcessID returns the process of the running instance, zero when the
// service control manager reports none.
//
// The process is read before a rollback stops the instance of the failed
// installation, so the verification of the restart can tell it apart from the
// instance the rollback starts. A state that cannot be read names no process
// either: the stop that follows reports the refusal.
func scmRunningProcessID(handle scmServiceControl) uint32 {
	status, err := handle.Query()
	if err != nil {
		return 0
	}
	return status.ProcessId
}

// scmImageCheck is how much of the running instance an installation could prove.
type scmImageCheck int

const (
	// scmImageUnverified means the service control manager reports the instance
	// as running and the registration carries the program, but the image of the
	// running process could not be read: the platform refused its handle.
	scmImageUnverified scmImageCheck = iota
	// scmImageVerified means the running process really is the registered
	// program.
	scmImageVerified
)

// startAndVerify starts the instance and verifies that it runs the given
// program in a process that is not one of the excluded ones.
func (m *scmManager) startAndVerify(ctx context.Context, handle *mgr.Service, executable string, exclude ...uint32) (scmImageCheck, error) {
	if err := scmStartForVerification(handle); err != nil {
		return scmImageUnverified, err
	}
	check, err := m.verifyRunningExecutable(ctx, handle, executable, exclude...)
	return check, err
}

func scmStartForVerification(handle *mgr.Service) error {
	if err := scmStartService(handle); err != nil && !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
		return scmRefused("a start", fmt.Errorf("service: start %s: %w", ServiceName, err))
	}
	return nil
}

// startAndVerifyVersion verifies readiness and, when supplied, the version the
// health endpoint reports. An update needs readiness even for dev or unknown
// expected versions; rollback uses startAndVerify's image-only proof instead.
func (m *scmManager) startAndVerifyVersion(ctx context.Context, handle *mgr.Service, executable, listen, expectedVersion string, exclude ...uint32) (scmImageCheck, bool, error) {
	if err := scmStartForVerification(handle); err != nil {
		return scmImageUnverified, false, err
	}
	return m.verifyRunningExecutableVersion(ctx, handle, executable, listen, expectedVersion, exclude...)
}

// verifyRunningExecutable waits until the instance runs the given program in a
// process that is not one of the excluded ones.
//
// The service control manager reports the process of the running instance, and
// its image is compared with the program: a registration that carries the
// program proves nothing while a process of the failed attempt still runs. The
// wait is bounded by the verification budget and the context.
func (m *scmManager) verifyRunningExecutable(ctx context.Context, handle *mgr.Service, executable string, exclude ...uint32) (scmImageCheck, error) {
	result, err := m.verifyRunningExecutableVersionUsing(ctx,
		scmServiceRecoveryTarget{manager: m, handle: handle}, executable, "", "", false, exclude...)
	return result.imageCheck, err
}

// scmQueryStatus reads the state of the instance during a verification. The
// variable is the seam of the test: a test verifies without a registration of
// the service control manager.
var scmQueryStatus = (*mgr.Service).Query

var scmQueryServiceConfig = (*mgr.Service).Config

// scmOpenService opens the registration for the write path of an
// installation. The variable is the seam of the test: a test drives the
// update path without a registration of its own on the machine, which a
// shell without elevation cannot create.
var scmOpenService = (*mgr.Mgr).OpenService

var scmStartService = func(service *mgr.Service) error { return service.Start() }

var scmControlService = (*mgr.Service).Control

var scmDeleteService = (*mgr.Service).Delete

var scmQueryProcessImageName = queryProcessImageName

var scmQueryServiceCommandLine = queryServiceCommandLine

type scmRunningImageTarget interface {
	Query() (svc.Status, error)
	ReadRunningImage(processID uint32, executable string) (bool, scmImageCheck, string)
}

type scmStartVerification struct {
	imageCheck      scmImageCheck
	versionVerified bool
	processID       uint32
}

// verifyRunningExecutableVersion also waits for the expected version.
func (m *scmManager) verifyRunningExecutableVersion(ctx context.Context, handle *mgr.Service, executable, listen, expectedVersion string, exclude ...uint32) (scmImageCheck, bool, error) {
	result, err := m.verifyRunningExecutableVersionUsing(ctx,
		scmServiceRecoveryTarget{manager: m, handle: handle}, executable, listen, expectedVersion, true, exclude...)
	return result.imageCheck, result.versionVerified, err
}

func (m *scmManager) verifyRunningExecutableVersionUsing(ctx context.Context, target scmRunningImageTarget, executable, listen, expectedVersion string, requireReadiness bool, exclude ...uint32) (scmStartVerification, error) {
	deadline := time.Now().Add(scmVerifyBudget)
	detail := "the service control manager did not report the instance as running"
	var lastProcessID uint32
	lastImageCheck := scmImageUnverified
	for {
		status, err := target.Query()
		switch {
		case err != nil:
			detail = fmt.Sprintf("the state of %s cannot be read: %v", ServiceName, err)
		case status.State != svc.Running:
			detail = fmt.Sprintf("the service control manager reports the state %v", status.State)
		case status.ProcessId == 0:
			detail = "the service control manager reports no process of the running instance"
		case scmProcessExcluded(exclude, status.ProcessId):
			// A process of the attempt still runs: the program of the
			// registration is not in service yet.
			detail = fmt.Sprintf("the process %d that ran before the attempt still runs", status.ProcessId)
		default:
			lastProcessID = status.ProcessId
			settled, check, reason := target.ReadRunningImage(status.ProcessId, executable)
			if settled {
				lastImageCheck = check
				if expectedVersion == "" && !requireReadiness {
					return scmStartVerification{imageCheck: check, processID: status.ProcessId}, nil
				}
				verified, probeErr := serviceInstallProbe(listen, expectedVersion)
				if probeErr == nil {
					return scmStartVerification{imageCheck: check, versionVerified: verified, processID: status.ProcessId}, nil
				}
				if errors.Is(probeErr, errServiceVersionMismatch) {
					return scmStartVerification{imageCheck: check, processID: status.ProcessId}, fmt.Errorf("the instance does not run the expected version: %w", probeErr)
				}
				detail = fmt.Sprintf("the instance does not answer on %s: %v", probeAddressOrListen(listen), probeErr)
			} else {
				// The image read did not settle the question: its reason is
				// the only detail there is.
				detail = reason
			}
		}
		if !time.Now().Before(deadline) {
			return scmStartVerification{imageCheck: lastImageCheck, processID: lastProcessID}, fmt.Errorf("the instance does not run %q within %s: %s", executable, scmVerifyBudget, detail)
		}
		select {
		case <-ctx.Done():
			return scmStartVerification{imageCheck: lastImageCheck, processID: lastProcessID}, ctx.Err()
		case <-time.After(scmVerifyPollInterval):
		}
	}
}

func probeAddressOrListen(listen string) string {
	if listen == "" {
		return "the instance"
	}
	return probeServiceAddress(listen)
}

// scmProcessExcluded reports whether a process is one the verification must not
// accept as the running instance.
//
// A process identifier of zero names no process: the manager reports it for an
// instance that does not run, and it never excludes another process. The list of
// excluded processes can carry it, because the caller passes the process the
// service control manager reported even when it reported none.
func scmProcessExcluded(exclude []uint32, processID uint32) bool {
	if processID == 0 {
		return false
	}
	return slices.Contains(exclude, processID)
}

// scmReadRunningImage reads the image of the running process and compares it
// with the program the registration carries.
//
// The first result reports whether the observation settles the question, the
// second one is the proof the installation has, the third one the reason a look
// did not settle it. The image of a process of another account can be refused
// to the installation; the wait then accepts the evidence it has (the running
// state and the program of the registration) and the caller is told that the
// image was not compared.
func scmReadRunningImage(handle *mgr.Service, processID uint32, executable string) (bool, scmImageCheck, string) {
	if executable == "" {
		// The program is unknown: there is nothing the image of the process
		// could be compared with, so the installation reports the running
		// state alone.
		return true, scmImageUnverified, ""
	}
	image, err := scmQueryProcessImageName(processID)
	switch {
	case err == nil:
		if scmSameExecutablePath(image, executable) {
			return true, scmImageVerified, ""
		}
		return false, scmImageUnverified, fmt.Sprintf("the running process %d runs %q and not %q", processID, image, executable)
	case errors.Is(err, windows.ERROR_ACCESS_DENIED):
		pointsAt, err := scmRegistrationPointsAt(handle, executable)
		if err != nil {
			return false, scmImageUnverified, fmt.Sprintf("the registration of %s cannot be read back: %v", ServiceName, err)
		}
		if pointsAt {
			return true, scmImageUnverified, ""
		}
		return false, scmImageUnverified, fmt.Sprintf("the registration of %s does not carry the program %q", ServiceName, executable)
	default:
		return false, scmImageUnverified, fmt.Sprintf("the image of the running process %d cannot be read: %v", processID, err)
	}
}

// scmRegistrationPointsAt reports whether the registration of the instance
// carries the given program.
//
// The command line is read from the service control manager and not from the
// configuration the installation wrote, so the answer describes the registration
// that is really there.
func scmRegistrationPointsAt(handle *mgr.Service, executable string) (bool, error) {
	commandLine, err := scmQueryServiceCommandLine(handle.Handle)
	if err != nil {
		return false, err
	}
	registration, err := parseSCMCommandLine(commandLine)
	if err != nil {
		return false, nil
	}
	return scmSameExecutablePath(registration.executable, executable), nil
}

// queryProcessImageName returns the image of a process.
//
// The handle of the process asks for the least right that reads its image: the
// right to query the limited information of the process. The manager never opens
// a process with the rights that would let it change one.
func queryProcessImageName(processID uint32) (string, error) {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, processID)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(handle)

	size := uint32(windows.MAX_PATH)
	for {
		buffer := make([]uint16, size)
		length := size
		err := windows.QueryFullProcessImageName(handle, 0, &buffer[0], &length)
		if err == nil {
			return windows.UTF16ToString(buffer[:length]), nil
		}
		if !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) {
			return "", err
		}
		// The image path of a program can be longer than the buffer: the
		// buffer grows until the path fits.
		if length > size {
			size = length
		} else {
			size *= 2
		}
		if size > scmMaxImagePathLength {
			return "", err
		}
	}
}

// scmServiceControl is the part of a registration the stop of an instance
// needs.
//
// The interface is the seam of the test: the manager talks to the service
// control manager through it, so the retry of a refused stop can be driven
// from a test without a registration of its own.
type scmServiceControl interface {
	// Query returns the state of the registration.
	Query() (svc.Status, error)
	// Control sends a control request to the registration.
	Control(c svc.Cmd) (svc.Status, error)
}

// scmStopOutcome tells what a failed stop proved.
type scmStopOutcome int

const (
	scmStopSettled scmStopOutcome = iota
	// scmStopNotDelivered reports a stop no control reached: the instance was
	// never asked to stop.
	scmStopNotDelivered
	// scmStopUnsettled reports a stop that may still put the instance down.
	scmStopUnsettled
)

// stopService stops the instance and waits for the graceful shutdown to
// finish.
//
// Stopping an instance that does not run is a stable success: the state the
// caller asked for already holds.
//
// The stop control is delivered again while the wait lasts: the service
// control manager refuses the control of an instance that is still starting
// (ERROR_SERVICE_CANNOT_ACCEPT_CTRL), and the instance of the platform
// accepts the control only after it reported the running state. A refusal
// is therefore not the end of the stop: the control is delivered again on
// the next step of the wait, until the instance stops or the budget of the
// wait is over.
//
// The wait is bounded by the shared budget of the service layer, so the
// outcome is the same on every machine: the instance stopped, or the wait
// is over and the instance is still running. A failure distinguishes a control
// that was never delivered from one that may still stop the instance: a state
// that cannot be read before any control was accepted proves no delivery.
func (m *scmManager) stopService(ctx context.Context, handle scmServiceControl) (scmStopOutcome, error) {
	deadline := time.Now().Add(scmStopBudget)
	delivered := false
	stopping := false
	for {
		status, err := handle.Query()
		if err != nil {
			// An accepted control, or an instance that was seen stopping, may
			// still put the instance down. Before that the error proves no
			// stop was requested.
			if delivered || stopping {
				return scmStopUnsettled, scmRefused("a stop", fmt.Errorf("service: query the state of %s: %w", ServiceName, err))
			}
			return scmStopNotDelivered, scmRefused("a stop", fmt.Errorf("service: query the state of %s: %w", ServiceName, err))
		}
		if status.State == svc.StopPending {
			stopping = true
		}
		if status.State == svc.Stopped {
			return scmStopSettled, nil
		}
		if !delivered {
			if _, err := handle.Control(svc.Stop); err != nil {
				switch {
				case errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE):
					// The instance is not running: the wanted state
					// already holds.
					return scmStopSettled, nil
				case errors.Is(err, windows.ERROR_SERVICE_CANNOT_ACCEPT_CTRL):
					// The control is delivered again inside the wait.
					log.Printf("service: %s does not accept a stop control (the state of the instance is %v), the stop request is delivered again", ServiceName, status.State)
				default:
					if status.State == svc.StopPending {
						return scmStopUnsettled, scmRefused("a stop", fmt.Errorf("service: stop %s: %w", ServiceName, err))
					}
					return scmStopNotDelivered, scmRefused("a stop", fmt.Errorf("service: stop %s: %w", ServiceName, err))
				}
			} else {
				// The supervisor accepted the control: the instance
				// stops.
				delivered = true
			}
		}
		if time.Now().After(deadline) {
			if delivered || status.State == svc.StopPending {
				return scmStopUnsettled, fmt.Errorf("service: %s did not stop within %s (the state of the instance was %v)", ServiceName, scmStopBudget, status.State)
			}
			return scmStopNotDelivered, fmt.Errorf("service: %s did not stop within %s (the state of the instance was %v)", ServiceName, scmStopBudget, status.State)
		}
		select {
		case <-ctx.Done():
			if delivered || status.State == svc.StopPending {
				return scmStopUnsettled, ctx.Err()
			}
			return scmStopNotDelivered, ctx.Err()
		case <-time.After(scmStopPollInterval):
		}
	}
}

// queryServiceCommandLine reads the command line of a registration.
//
// The service control manager is asked directly, so the status describes
// the registration and not the intent of the caller.
func queryServiceCommandLine(handle windows.Handle) (string, error) {
	size := uint32(1024)
	for {
		buffer := make([]byte, size)
		config := (*windows.QUERY_SERVICE_CONFIG)(unsafe.Pointer(&buffer[0]))
		var needed uint32
		err := windows.QueryServiceConfig(handle, config, uint32(len(buffer)), &needed)
		if err == nil {
			return windows.UTF16PtrToString(config.BinaryPathName), nil
		}
		if !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) {
			return "", err
		}
		if needed <= uint32(len(buffer)) {
			return "", err
		}
		size = needed
	}
}
