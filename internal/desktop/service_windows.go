// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package desktop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bashrusakh/hfdesk/internal/service"
	"golang.org/x/sys/windows"
)

// serviceStatusTimeout bounds the read of the service registration at the
// startup of the desktop host, and the data-directory read of the
// installation confirmation, which names this budget for itself.
//
// The lookup is a local read of the service control manager, so the budget only
// covers a busy machine or one that does not answer, and a service that is
// starting or stopping is read again until it settled. It is a variable so the
// tests can shorten the wait.
var serviceStatusTimeout = 10 * time.Second

// serviceGatePollInterval is the pause between two reads of the service state
// while the service is starting or stopping.
//
// The state is read again shortly, so a launch that found the service in the
// middle of a transition decides on the state it reached and not on the
// transition.
const serviceGatePollInterval = 200 * time.Millisecond

// errServiceGatePending reports a service that is still starting or stopping
// when the budget of the service gate ends.
//
// A pending state is not a state to decide on: it is neither a running nor a
// stopped service, so the gate offers no action for it.
var errServiceGatePending = errors.New("the service is still starting or stopping")

// gateDecision is what the launch gate decides for THIS launch.
//
// The gate is the one place that decides whether the launch may bind the
// listen address, so its answer is a three-valued decision instead of a
// boolean: the launch either ends here, continues into the ordinary desktop
// mode with its own server, or continues as a tray companion WITHOUT one.
// The two continuing decisions are distinct values, so the caller cannot
// mistake the companion choice for the desktop mode (which would start a
// server beside the service) or the other way round.
type gateDecision int

const (
	// gateEnd ends the launch: it starts no server. Every answer that
	// performs an action for this one launch (open, start, update,
	// cancel) and every state that could not be decided on ends here.
	gateEnd gateDecision = iota
	// gateDesktop continues the launch like a launch without a
	// registration: the application is created and the server starts.
	gateDesktop
	// gateCompanion continues the launch as a tray companion: the tray
	// and the window run, and no application — and therefore no server
	// and no listener of any kind — is ever created for this launch.
	gateCompanion
)

// serviceStatus reads the registration of the Windows service.
//
// It is a package level variable so the tests can drive the startup of the
// host without a service: a test cannot register a machine-wide service
// without an elevated shell.
var serviceStatus = lookupServiceStatus

// serviceStateReader is the one owner of every read of the service
// registration in the desktop host.
//
// The host reads the registration in two shapes, and each caller names its
// own budget or semantics — one mechanism with explicit policies, never one
// averaged budget:
//
//   - Single reads without settle run through readSingle under the budget
//     their caller names:
//   - the display surfaces (the tray line, the Logs window) name
//     serviceDisplayStatusTimeout (2s): the read is bounded, never waits
//     for a pending state to settle and never holds its caller longer than
//     that budget — it runs behind a menu refresh and behind the HTTP
//     handler of the window, never in front of either. A read that fails or
//     runs out of budget reports the error, and every consumer classifies
//     that as the honest unknown state: a timeout is never proof of a
//     machine without a service.
//   - the data-directory read of the installation confirmation names
//     serviceStatusTimeout (10s — the budget this read had before it joined
//     this owner), so the confirmation text names the registered directory
//     as reliably as it did then, even behind a service control manager
//     slower than the display budget; its fallback to the default of the
//     scope is kept.
//   - The action policy (readForAction) backs every decision: the gate, the
//     re-reads beside its dialogs and the tray actions. It keeps the settle
//     wait and the pending report exactly as the gate had them, on the
//     caller's budget.
//
// Every read of the state goes through this owner — the data directory read
// included — so a surface that shows the state and a surface that decides
// on it can no longer be separate readers that disagree.
type serviceStateReader struct{}

// serviceState is the reader the whole desktop host reads through.
var serviceState serviceStateReader

// serviceDisplayStatusTimeout bounds the display policy's read.
//
// The line is shown on a cadence — the tray re-reads it every few seconds
// and the Runtime window with its own status refresh — so the read ends
// before the next one is due instead of stacking reads behind a service
// control manager that does not answer. A read that runs out of budget is
// not a machine without a service: the caller reports the unknown state.
var serviceDisplayStatusTimeout = 2 * time.Second

// readForDisplay reads the registration for the display surfaces: the
// single read without settle under their own named budget,
// serviceDisplayStatusTimeout. It is the zero-argument reader the tray menu
// and the Runtime window seam take.
//
// Unlike the action policy it does not wait for a pending state to settle:
// a status surface only reports what the machine has right now ("starting"
// and "stopping" are truthful words for it), and every action reads the
// registration again — through the settle wait of readForAction — before it
// decides anything on the state.
func (serviceStateReader) readForDisplay() (service.Status, error) {
	return serviceState.readSingle(serviceDisplayStatusTimeout)
}

// serviceReadStamp is what one state read reported: the state word, the
// running fact and — when the read failed — its error text.
type serviceReadStamp struct {
	state   string
	running bool
	errText string // "" when the read succeeded
	seen    bool
}

// serviceReadLogMu guards serviceReadLogLast: the menu refresh goroutine,
// the tick reads and the HTTP handlers of the Logs window read beside
// each other.
var serviceReadLogMu sync.Mutex

// serviceReadLogLast is the last read the host recorded in its log.
//
// The display surfaces re-read on a cadence — the tray menu refresh and
// the status polls of the open Logs window — so a record per read would
// add lines every few seconds while nothing moves. The stamp turns the
// record into a CHANGE record: the first read, a state that moved (the
// record carries the previous state word beside the new one), a read
// that starts failing and one that recovers. A repeated read of what is
// already on the record writes nothing — the open window at rest adds no
// lines — while every transition stays on the record, so the diagnostic
// value of the reads survives.
//
// It is a package level variable so a test can start from an empty stamp
// with the established seam.
var serviceReadLogLast serviceReadStamp

// recordServiceRead stores the stamp of a read and reports whether it is
// a change beside the previous record — plus the previous state word
// when the record names a different one.
func recordServiceRead(stamp serviceReadStamp) (changed bool, previous string) {
	serviceReadLogMu.Lock()
	defer serviceReadLogMu.Unlock()
	if serviceReadLogLast.seen && serviceReadLogLast == stamp {
		return false, ""
	}
	if serviceReadLogLast.seen && serviceReadLogLast.state != stamp.state {
		previous = serviceReadLogLast.state
	}
	serviceReadLogLast = stamp
	return true, previous
}

// readSingle reads the registration exactly once under the budget its
// caller names: the one mechanism of the owner for every read that does not
// wait for a transition. The callers differ in the budget they name, never
// in the mechanism.
//
// A read that CHANGES what the host last recorded is written as the read
// it was — never as the outcome of an action this host did not perform —
// and a read that reports what is already on the record writes nothing:
// the periodic reads of the open status surfaces then stay silent while
// the machine rests (the record of every change, and the menu's own
// transition record, carry the diagnostics).
func (serviceStateReader) readSingle(budget time.Duration) (service.Status, error) {
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	status, err := serviceStatus(ctx)
	stamp := serviceReadStamp{state: string(status.State), running: status.Running, seen: true}
	if err != nil {
		stamp.errText = err.Error()
	}
	changed, previous := recordServiceRead(stamp)
	if !changed {
		return status, err
	}
	if err != nil {
		desktopLog.Warn("read the Windows service state", "error", err)
		return status, err
	}
	args := []any{"state", string(status.State), "running", status.Running}
	if previous != "" {
		args = append(args, "previous", previous)
	}
	desktopLog.Info("read the Windows service state", args...)
	return status, nil
}

// readRuntimeServiceState answers the state read of the Logs
// window under the display policy.
//
// Every display read asks the tray menu to catch up right after it ran, so a
// result read for the window can never stay invisible in the menu until the
// next tick: the menu re-reads and applies on its own goroutine, and the
// request is the host's non-blocking refresh signal, so this caller — the
// HTTP handler of the window — is never held by the menu. The signal follows
// a failed read as well: the honest unknown state a failed read classifies
// as must reach the menu too.
func readRuntimeServiceState(requestRefresh func()) (string, error) {
	status, err := serviceState.readForDisplay()
	requestRefresh()
	if err != nil {
		return "", err
	}
	return string(status.State), nil
}

// lookupServiceStatus reads the registration of the Windows service with the
// factory of the service layer. The desktop host drives no supervisor of its
// own: it only asks the shared layer for the state of the registration.
func lookupServiceStatus(ctx context.Context) (service.Status, error) {
	manager, err := service.New(service.ScopeSystem)
	if err != nil {
		return service.Status{}, err
	}
	return manager.Status(ctx)
}

// serviceGate decides how this launch continues when the machine has a
// service registration.
//
// The Windows service is a second HFDesk on the same machine and binds the same
// address, so a launch that finds a registration asks the user what to do and
// ends without a server of its own — with two exceptions: a positively confirmed
// stopped service has nothing serving the address, and its dialog offers to
// work in desktop mode as the default answer (gateDesktop: the launch is
// continued exactly like a launch without a registration and never starts the
// service itself), and every settled registration offers the tray companion
// (gateCompanion: the launch continues as a tray icon plus window WITHOUT a
// server, so the registration keeps the address alone).
//
// The gate runs before the application is created and reports gateDesktop only
// when the machine has no registration and when the user chose the desktop
// mode for a positively confirmed stopped service. Every other registration
// state that performs an action for this one launch ends as gateEnd: the
// running service keeps its gate unchanged, and a state that was not confirmed
// stopped is never continued as desktop. The companion choice is honored for
// every state the dialog was built from, because the companion never binds an
// address. A registration that cannot be read is not a machine without one:
// the gate starts no server and reports the failure with a notice that names
// the way to read the state and to try again.
//
// A service that is starting or stopping is not a state to decide on, so the
// gate waits for it to settle first; one that did not settle within the budget
// is reported with a notice and ends the launch as well.
//
// instanceMutex is the per-session singleton of this launch: the update choice
// hands it to the elevated helper, whose mutation outlives this process.
func serviceGate(instanceMutex string) gateDecision {
	ctx, cancel := context.WithTimeout(context.Background(), serviceStatusTimeout)
	defer cancel()

	status, err := serviceState.readForAction(ctx)
	if err != nil {
		if errors.Is(err, errServiceGatePending) {
			desktopLog.Info("the Windows service is still starting or stopping: this launch changes nothing",
				"state", string(status.State))
			showMessage(serviceGatePendingCaption,
				serviceGatePendingText(status.State), windows.MB_ICONINFORMATION)
			return gateEnd
		}
		// The launch cannot tell a machine without a service from a machine
		// with one, so it decides on neither: it starts no server and leaves
		// the state of the machine as it is.
		desktopLog.Error("could not read the Windows service registration: this launch starts no server", "error", err)
		showMessage(serviceGateUnreadableCaption,
			serviceGateUnreadableText(err), windows.MB_ICONERROR)
		return gateEnd
	}
	if status.State == service.StateNotInstalled {
		return gateDesktop
	}

	prompt := serviceChoicePrompt{
		State:          status.State,
		Running:        status.Running,
		Listen:         status.Listen,
		DataDir:        status.Paths.StateDir,
		Detail:         status.Detail,
		OfferCompanion: true,
	}
	if url, err := serviceURL(status.Listen); err == nil {
		prompt.URL = url
	}
	// The record is the read, not the outcome of an action: the user
	// reads the log of the launch beside the reports of real service
	// actions, and a line that claimed the installation as done would be
	// mistaken for one. The facts of the read — the state, the running
	// fact, the address and the data directory — stay on the record.
	desktopLog.Info("read the Windows service state at the launch gate",
		"state", string(status.State),
		"running", status.Running,
		"url", prompt.URL,
		"dataDir", prompt.DataDir)

	switch showServiceChoice(prompt) {
	case serviceChoiceDesktop:
		// The desktop mode is the default answer of the stopped dialog and
		// the only answer that continues this launch WITH a server. It is
		// honored only for a positively confirmed stopped service:
		// nothing serves the address this launch would bind, and this
		// launch never starts the service, so the registration stays
		// untouched. Every other state ends the launch here — a running
		// service and a state that was not confirmed stopped never get a
		// server next to themselves, even if an answer reached the gate
		// that no dialog of theirs offers.
		if !servicePromptStopped(prompt) {
			desktopLog.Info("the desktop mode was not offered for this service state: this launch starts no server",
				"state", string(prompt.State))
			return gateEnd
		}
		// The dialog is modal: while it was open, another actor may have
		// started the service, and the snapshot the dialog was built from
		// cannot admit a server next to a service that now serves the same
		// address. The state is therefore read again — with a fresh
		// budget, because the dialog itself may have consumed the budget
		// of this launch — and only a state that is still positively
		// stopped continues. An unreadable or pending re-read is not a
		// confirmed stopped service and ends the launch with its notice.
		recheckCtx, recheckCancel := context.WithTimeout(context.Background(), serviceStatusTimeout)
		fresh, err := serviceState.readForAction(recheckCtx)
		recheckCancel()
		if err != nil {
			if errors.Is(err, errServiceGatePending) {
				desktopLog.Info("the service changed while the dialog was open: this launch starts no server",
					"state", string(fresh.State))
				showMessage(serviceGatePendingCaption,
					serviceGatePendingText(fresh.State), windows.MB_ICONINFORMATION)
				return gateEnd
			}
			desktopLog.Error("the service state could not be read again after the desktop-mode choice: this launch starts no server",
				"error", err)
			showMessage(serviceGateUnreadableCaption,
				serviceGateUnreadableText(err), windows.MB_ICONERROR)
			return gateEnd
		}
		freshPrompt := serviceChoicePrompt{State: fresh.State, Running: fresh.Running}
		if !servicePromptStopped(freshPrompt) {
			desktopLog.Info("the service state changed while the dialog was open: this launch starts no server",
				"state", string(fresh.State),
				"running", fresh.Running)
			showMessage(serviceGateChangedCaption,
				serviceGateChangedText(freshPrompt), windows.MB_ICONINFORMATION)
			return gateEnd
		}
		desktopLog.Info("the launch continues in desktop mode: the service stays stopped and the registration is untouched",
			"state", string(freshPrompt.State))
		return gateDesktop
	case serviceChoiceCompanion:
		// The tray companion binds no address: nothing of this launch can
		// ever compete with the service — or with any other instance —
		// for the listen address, so the choice is honored for every
		// state the dialog was built from and needs no state re-read like
		// the desktop mode. Once the tray runs, its menu and every one of
		// its actions read the registration again through the established
		// reader. This launch creates no application, therefore no server
		// and no listener of any kind, and it never touches the
		// registration.
		desktopLog.Info("the launch continues as a tray companion: it starts no server and the registration is untouched",
			"state", string(prompt.State))
		return gateCompanion
	case serviceChoiceOpen:
		// The URL is derived again from the registration: the dialog only
		// carried it, so the address the browser opens is the address of
		// the registration and nothing else.
		url, err := serviceURL(status.Listen)
		if err != nil {
			reportBrowserFailure(status.Listen, err)
			return gateEnd
		}
		if err := openBrowser(url); err != nil {
			reportBrowserFailure(url, err)
		}
	case serviceChoiceStart:
		// The start takes the service out of the positively stopped state
		// this gate decided on, so the helper holds this launch's singleton
		// through the start: a relaunch in that window must not bind the
		// address the starting instance needs.
		if _, err := launchServiceHelper(serviceActionStart, 0, instanceMutex); err != nil {
			reportServiceLaunchFailure(serviceActionStart, err)
		}
	case serviceChoiceUpdate:
		// The gate started no server, so nothing holds the listen address
		// and the helper has nothing to wait for. The singleton is
		// different from that address: the update stops and restarts a
		// running instance after this launch has ended, so the helper
		// holds this launch's singleton through the whole mutation,
		// including a rollback and the restart.
		if _, err := launchServiceHelper(serviceActionUpdate, 0, instanceMutex); err != nil {
			reportServiceLaunchFailure(serviceActionUpdate, err)
		}
	default:
		desktopLog.Info("the service gate changed nothing")
	}
	return gateEnd
}

// readForAction reads the registration under the action policy: it waits for
// a pending state to settle before it reports.
//
// The service control manager reports a started or a stopped instance as pending
// first, and deciding on a stopping instance would offer the start of a service
// that is still on its way down. A state that did not settle within the budget
// is returned with errServiceGatePending.
func (serviceStateReader) readForAction(ctx context.Context) (service.Status, error) {
	for {
		status, err := serviceStatus(ctx)
		if err != nil {
			return status, err
		}
		if serviceStateSettled(status.State) {
			return status, nil
		}
		select {
		case <-ctx.Done():
			return status, errServiceGatePending
		case <-time.After(serviceGatePollInterval):
		}
	}
}

// serviceActionRunning is the guard of every service action of the tray
// process: the installation, and the start, update, stop and removal of a
// registered service, from every surface that can ask for one — the install
// item, the Service... dialog, the lifecycle items of the menu and the
// install button of the Logs window.
//
// The tray keeps delivering clicks while the confirmation of a click is open
// and while an already launched helper still works: the action runs on a
// goroutine of its own, so the message loop of the tray is never blocked by
// the message box. Only one service action may run at a time: a second one
// would ask the same confirmation again, raise a second prompt of the user
// account control and launch a second elevated helper, and two helpers would
// write the same registration and move the service next to each other. A
// second request is refused with its own report (beginServiceAction).
//
// The guard is held for the whole action and released by every path that
// ends without a running helper: a declined confirmation, a refused
// elevation, an early failure or an answer of the dialog that changes
// nothing leaves the items working again. A launched helper keeps the guard
// until THAT HELPER ENDS (keepServiceActionGuardUntilHelperEnds), so the
// guard spans the mutation and not only the click that asked for it; a
// helper the shell started but named no process for cannot be waited for
// and keeps the guard for the bounded hold of its action budget instead
// (keepServiceActionGuardForUntrackedHelper). An installation handover keeps
// it until the host exits.
var serviceActionRunning atomic.Bool

// beginServiceAction takes the single service-action guard for one request
// of a tray surface.
//
// false means another action is already running: the refusal is reported to
// the user — a second click deserves an honest answer instead of silently
// doing nothing — and the caller performs no action, asks no confirmation
// and launches no helper. surface names the item for the record of the host.
func beginServiceAction(surface string) bool {
	if !serviceActionRunning.CompareAndSwap(false, true) {
		desktopLog.Info("another service action is already running: this request is refused",
			"surface", surface)
		showMessage(serviceActionBusyCaption, serviceActionBusyText(), windows.MB_ICONWARNING)
		return false
	}
	return true
}

// installServiceFromTray registers the Windows service from the tray.
//
// The action asks the user to confirm the installation before anything is
// changed, because the registration is a machine-wide change that survives the
// desktop app. The console program is settled before the user is asked — a
// folder without it gets the notice with this release's own pair on offer, or
// the manual path — so the folder is never first discovered after the
// elevation prompt.
//
// The helper is launched elevated and must open the ready/success events, the
// original process handle, and this instance's singleton before it becomes
// ready. The caller commits shutdown only after that acknowledgement.
func installServiceFromTray(instanceMutex string) *desktopHandover {
	if !beginServiceAction("Install service... item") {
		return nil
	}

	// The guard is released when this call ends without a running helper,
	// so the tray items work again after every pre-shutdown failure. The
	// registered-service choice below may launch a start or an update
	// instead of an installation: that helper inherits the guard through
	// handedToHelper and releases it when it ends.
	handedOver := false
	handedToHelper := false
	defer func() {
		if !handedOver && !handedToHelper {
			serviceActionRunning.Store(false)
		}
	}()

	executable, err := desktopExecutable()
	if err != nil {
		reportServiceLaunchFailure(serviceActionInstall, err)
		return nil
	}

	console, ok := offerMissingConsoleProgram(serviceActionInstall, executable)
	if !ok {
		return nil
	}

	if !confirmServiceInstall(installServiceText(serviceDataDir(),
		programOnRemovableOrNetworkDrive(console))) {
		desktopLog.Info("the service installation was not confirmed")
		return nil
	}

	// The registration is read again before the handover begins: this very
	// launch may have chosen the desktop mode for an existing service, or a
	// registration appeared since. Handing a first-install helper a
	// registration it must refuse would cost the desktop its server for
	// nothing, so the choice every launch with a registration gets is
	// offered here instead and no handover starts.
	recheckCtx, recheckCancel := context.WithTimeout(context.Background(), serviceStatusTimeout)
	status, err := serviceState.readForAction(recheckCtx)
	recheckCancel()
	if err != nil {
		if errors.Is(err, errServiceGatePending) {
			desktopLog.Info("the service state settled into a transition before the installation handover: nothing was handed over",
				"state", string(status.State))
			showMessage(serviceGatePendingCaption,
				serviceGatePendingText(status.State), windows.MB_ICONINFORMATION)
			return nil
		}
		desktopLog.Error("the service registration could not be read again before the installation handover: nothing was handed over",
			"error", err)
		showMessage(serviceGateUnreadableCaption,
			serviceGateUnreadableText(err), windows.MB_ICONERROR)
		return nil
	}
	if status.State != service.StateNotInstalled {
		handedToHelper = offerRegisteredServiceFromTray(status, instanceMutex)
		return nil
	}

	handover, err := launchServiceHandover(serviceActionInstall, os.Getpid(), instanceMutex,
		handoverRelaunchCompanion)
	if err != nil {
		reportServiceLaunchFailure(serviceActionInstall, err)
		return nil
	}
	handedOver = true
	desktopLog.Info("the elevated service installation is running",
		"helperPID", handover.helper.pid, "console", console)
	return handover
}

// offerRegisteredServiceFromTray asks the choice of the service gate for a
// registration that exists at the installation step and performs the answer
// beside the running desktop.
//
// The dialog is the gate's dialog: the question and its buttons are the ones
// every launch with a registration sees, so the vocabulary cannot drift
// apart. No answer starts a handover of its own — this host keeps running
// for the answers that change nothing: the desktop
// mode and Cancel change nothing, the browser action opens the URL of the
// registration, and the start and the update decide below exactly like the
// lifecycle items of the menu do, where the helper's own guards stay
// authoritative.
//
// The invariant at this surface is that a mutation beside a desktop of this
// session that serves its own address never reaches a doomed elevation: the
// start and the update hand the host over instead, so the helper acts after
// this desktop is gone — the very decision the lifecycle items take, through
// the same two functions, so the two surfaces can never disagree about which
// mutations the serving desktop allows. Where no handover can be established
// at all, the honest refusal of the serving desktop ends the answer instead
// of the elevation (serviceActionHandover).
//
// The second result reports whether a helper was launched for the answer:
// such a helper inherits the single service-action guard its caller holds
// and releases it when it ends, so the caller must not release the guard
// itself — unless the answer committed a handover, in which case this host
// quits on it and the guard stays until it does. false means nothing was
// launched and the caller keeps the ownership of the guard.
func offerRegisteredServiceFromTray(status service.Status, instanceMutex string) bool {
	desktopLog.Info("the Windows service is registered already: the tray offers the actions of an existing service instead of an installation",
		"state", string(status.State),
		"running", status.Running)
	prompt := serviceChoicePrompt{
		State:   status.State,
		Running: status.Running,
		Listen:  status.Listen,
		DataDir: status.Paths.StateDir,
		Detail:  status.Detail,
	}
	if url, err := serviceURL(status.Listen); err == nil {
		prompt.URL = url
	}
	switch showServiceChoice(prompt) {
	case serviceChoiceOpen:
		url, err := serviceURL(status.Listen)
		if err != nil {
			reportBrowserFailure(status.Listen, err)
			return false
		}
		if err := openBrowser(url); err != nil {
			reportBrowserFailure(url, err)
		}
		return false
	case serviceChoiceStart:
		return startRegisteredServiceFromSurface(instanceMutex)
	case serviceChoiceUpdate:
		return updateRegisteredServiceFromSurface(status, instanceMutex)
	default:
		desktopLog.Info("the tray changed nothing for the registered service",
			"state", string(status.State))
		return false
	}
}

// startRegisteredServiceFromSurface performs the start of a registered
// service beside the desktop that runs this tray — the answer of the
// registered-service dialog and the start item of the menu run this very
// function, so the two surfaces can never disagree.
//
// A desktop of this session that already serves the address hands the start
// over instead of being asked elevation for a refusal the helper would only
// repeat: the confirmation states that this window closes into the tray, the
// host then leaves desktop mode, the helper starts the service beside a host
// that is gone, and the host comes back as a tray companion. The start keeps
// its confirmation-free behavior everywhere it does NOT hand over — the tray
// companion beside a service it never competes with reaches the elevated
// helper directly, where the guard of the helper decides again,
// authoritatively.
//
// The result reports whether a helper was launched, with the same meaning
// offerRegisteredServiceFromTray documents.
func startRegisteredServiceFromSurface(instanceMutex string) bool {
	if !servingDesktopRefusesMutation(instanceMutex) {
		return launchTrayServiceAction(serviceActionStart, instanceMutex)
	}
	// The handover has to be establishable before the user is asked to
	// confirm a window that would close: a surface without a host to hand
	// over to refuses right here instead of asking first.
	if !serviceActionCanHandOver() {
		reportServingDesktopRefusal(serviceActionStart)
		return false
	}
	if !confirmServiceAction(startServiceConfirmCaption, startServiceConfirmText()) {
		desktopLog.Info("the service start was not confirmed")
		return false
	}
	return serviceActionHandover(serviceActionStart, instanceMutex)
}

// updateRegisteredServiceFromSurface performs the update of a registered
// service beside the desktop that runs this tray — the answer of the
// registered-service dialog and the update item of the menu run this very
// function, so the two surfaces can never disagree.
//
// The update re-points the registration at the console program of THIS
// folder, and only the states that would move the service decide on the
// guard beside this desktop — the very standard the helper applies to the
// same question. An update over a POSITIVELY confirmed stopped registration
// starts nothing: it is a pure rewrite that succeeds beside this desktop
// exactly as it always did, so it reaches the elevated helper directly, with
// no confirmation and without closing this window — no refusal ever existed
// for it to replace. Every other state beside a desktop that serves the
// address hands over instead: the console program is settled first — like
// every other update entry, and always before anything asks the user — then
// the confirmation states that this window closes into the tray, and only
// then does the host leave desktop mode. Everywhere else the update reaches
// the elevated helper directly, where the guard of the helper decides again.
//
// The result reports whether a helper was launched, with the same meaning
// offerRegisteredServiceFromTray documents.
func updateRegisteredServiceFromSurface(status service.Status, instanceMutex string) bool {
	positivelyStopped := status.State == service.StateStopped && !serviceStatusRunning(status)
	if positivelyStopped {
		return launchTrayServiceAction(serviceActionUpdate, instanceMutex)
	}
	if !servingDesktopRefusesMutation(instanceMutex) {
		return launchTrayServiceAction(serviceActionUpdate, instanceMutex)
	}
	if !serviceActionCanHandOver() {
		reportServingDesktopRefusal(serviceActionUpdate)
		return false
	}
	executable, err := desktopExecutable()
	if err != nil {
		reportServiceLaunchFailure(serviceActionUpdate, err)
		return false
	}
	if _, ok := offerMissingConsoleProgram(serviceActionUpdate, executable); !ok {
		return false
	}
	if !confirmServiceAction(updateServiceConfirmCaption, updateServiceConfirmText()) {
		desktopLog.Info("the service update was not confirmed")
		return false
	}
	return serviceActionHandover(serviceActionUpdate, instanceMutex)
}

// readServiceStateForAction reads the registration under the action policy
// for a tray surface and reports the established notices when the state
// cannot be decided on.
//
// false means the surface acts on nothing: a pending transition and a
// failed read each show their established notice of the gate, and a
// registration that is gone changes nothing — the menu line catches up
// with the refresh that follows the request instead of an action on a
// machine this click no longer found. Every caller re-reads through this
// one function, so the update item and the registered-service dialog of
// the install recheck decide on the same readings and report the same
// states.
func readServiceStateForAction(surface string) (service.Status, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), serviceStatusTimeout)
	defer cancel()
	status, err := serviceState.readForAction(ctx)
	if err != nil {
		if errors.Is(err, errServiceGatePending) {
			desktopLog.Info("the tray offers no service action for a state that is still a transition",
				"surface", surface,
				"state", string(status.State))
			showMessage(serviceGatePendingCaption,
				serviceGatePendingText(status.State), windows.MB_ICONINFORMATION)
			return status, false
		}
		desktopLog.Error("the tray could not read the Windows service registration for a service action",
			"surface", surface,
			"error", err)
		showMessage(serviceGateUnreadableCaption,
			serviceGateUnreadableText(err), windows.MB_ICONERROR)
		return status, false
	}
	if status.State == service.StateNotInstalled {
		desktopLog.Info("the tray found no registration for a service action: the menu line catches up",
			"surface", surface,
			"state", string(status.State))
		return status, false
	}
	return status, true
}

// serviceActionSurface names the lifecycle item of the tray menu for the
// record of a refused concurrent request.
func serviceActionSurface(action serviceAction) string {
	switch action {
	case serviceActionStart:
		return "Start service item"
	case serviceActionStop:
		return "Stop service item"
	case serviceActionUpdate:
		return "Update service item"
	default:
		return "Delete service item"
	}
}

// performServiceActionFromTray runs one lifecycle item of the tray menu —
// start, stop, update or removal — behind the single service-action guard.
//
// The flow of every item is the established one: take the guard (a second
// request during any of these steps is refused with its own report), ask the
// confirmation the machine-wide change needs before the administrator prompt
// follows (the stop and the removal always; the start and the update only
// where they hand this host over, because there the confirmation states the
// fact that this window closes), decide whether this desktop serves
// the address, then act: a serving desktop hands the action over through the
// established handover — this host leaves desktop mode and the elevated
// helper acts beside a host that is already gone — and every other surface
// launches the elevated helper directly. The helper
// reports the outcome of the action itself, in a native message box.
//
// The guard covers the whole action: a launched helper inherits it through
// the waiter and releases it when it ends, together with the menu refresh —
// a helper without a process record through the bounded hold that stands in
// for its exit (see launchTrayServiceAction) — and a committed handover keeps
// it until this host exits, exactly like the installation route. Every other
// end path — a declined confirmation, a handover that could not be
// established, a refused elevation, a failed launch, an unknown
// state — releases it here and asks for the menu catch-up, so the status is
// re-read after every request that acted.
func performServiceActionFromTray(action serviceAction, instanceMutex string) {
	if !beginServiceAction(serviceActionSurface(action)) {
		return
	}
	launched := false
	defer func() {
		if launched {
			// The helper inherited the guard; it releases the guard
			// and asks for the menu catch-up when it ends — or this
			// host exits on the handover it committed, exactly like
			// an installation handover.
			return
		}
		serviceActionRunning.Store(false)
		requestServiceMenuCatchUp()
	}()

	switch action {
	case serviceActionStop:
		if !confirmServiceAction(stopServiceConfirmCaption, stopServiceConfirmText()) {
			desktopLog.Info("the service stop was not confirmed")
			return
		}
	case serviceActionUninstall:
		// One probe decides both the confirmation the user reads and the
		// route the action takes, so the two can never disagree about
		// whether this host leaves desktop mode: beside a desktop that
		// serves the address the confirmation states that fact, and the
		// removal then hands over and comes back in desktop mode.
		handover := servingDesktopRefusesMutation(instanceMutex)
		if handover && !serviceActionCanHandOver() {
			// The refusal is the last resort of a handover that cannot
			// be established, and it is read before the confirmation:
			// this surface has no host to hand over to, so agreeing to
			// a closing window would have nothing to close.
			reportServingDesktopRefusal(serviceActionUninstall)
			return
		}
		if !confirmServiceAction(removeServiceConfirmCaption, removeServiceConfirmText(handover)) {
			desktopLog.Info("the removal of the service registration was not confirmed")
			return
		}
		if handover {
			launched = serviceActionHandover(serviceActionUninstall, instanceMutex)
			return
		}
	case serviceActionStart:
		// The start beside this serving desktop does not reach a doomed
		// elevation: the shared function below confirms the handover and
		// performs it, and the refusal of the guard stays reachable only
		// where no handover can be established at all
		// (serviceActionHandover).
		launched = startRegisteredServiceFromSurface(instanceMutex)
		return
	case serviceActionUpdate:
		// The update decides beside the state, so it re-reads the
		// registration under the action policy — the menu line may have
		// been drawn seconds ago — and a state that cannot be decided on
		// reaches the established notice instead of an elevation. The
		// state itself also decides the route: a positively confirmed
		// stopped registration is the pure rewrite that never had a
		// refusal to replace. The rest is the same function the dialog
		// runs.
		status, ok := readServiceStateForAction("Update service item")
		if !ok {
			return
		}
		launched = updateRegisteredServiceFromSurface(status, instanceMutex)
		return
	default:
		desktopLog.Warn("the tray menu performed an unknown service action", "action", string(action))
		return
	}
	launched = launchTrayServiceAction(action, instanceMutex)
}

// launchTrayServiceAction starts the elevated helper for a lifecycle action
// of the tray and hands the single service-action guard to a goroutine that
// keeps it until that helper ends — or, for a helper the shell named no
// process for, until the bounded hold of its action budget ends
// (untrackedHelperHoldBudget), because nothing can wait for the exit of such
// a helper while it may still perform its mutation.
//
// A start and an update carry the desktop singleton of this host — they hold
// it through their mutation, exactly like the launches of the service gate.
// A stop and a removal are launched without one: they hold no singleton, and
// a helper that received one would refuse its own command line instead of
// stopping or removing anything (see validateServiceHandoverOptions).
//
// The result reports whether a helper was launched: true means the waiter
// owns the guard now and releases it, with the menu catch-up, when the
// helper ends or when the bounded hold of an untracked helper ends. false
// means nothing runs and the caller keeps the guard — either the
// console-program notice of the update was declined (no report is due: the
// notice flow already showed its own) or the launch failed, which this
// function reports like every other launch of these actions.
func launchTrayServiceAction(action serviceAction, instanceMutex string) bool {
	mutex := instanceMutex
	if !serviceActionHoldsSingleton(action) {
		mutex = ""
	}
	launch, err := launchServiceHelper(action, 0, mutex)
	if err != nil {
		reportServiceLaunchFailure(action, err)
		return false
	}
	if !launch.started {
		return false
	}
	// Both end-path functions and the hold budget are read HERE, on the
	// goroutine that launched the helper: the waiter outlives this call,
	// and values it read itself could be replaced by a later caller while
	// the waiter is still unscheduled. The captured refresh is the host's
	// own signal — assigned before any tray item exists — or the stub of a
	// test that launched this helper; the captured wait is the same seam
	// the test replaced before this launch.
	if launch.pid == 0 {
		// The shell started the helper but named no process for it, so
		// no one can observe its exit: the guard is held for the
		// bounded hold instead of being released at the launch — the
		// helper may still move the registration, and a second action
		// beside it is refused with the busy report until the hold
		// ends like the run of a waited-for helper.
		go keepServiceActionGuardForUntrackedHelper(untrackedHelperHoldBudget, serviceMenuRefresh)
		return true
	}
	go keepServiceActionGuardUntilHelperEnds(launch.pid, waitForServiceActionHelper, serviceMenuRefresh)
	return true
}

// waitForServiceActionHelper blocks until the elevated helper process ends.
//
// It is a package level variable so the tests can end the wait
// deterministically: the launch of a test names a process id of its own, and
// the wait of the real implementation would open a handle to whichever
// process happens to carry that id. The launching goroutine reads this
// variable before it starts the waiter (launchTrayServiceAction), so a test
// replaces it around a launch without racing the waiter itself.
var waitForServiceActionHelper = waitForServiceActionProcess

// waitForServiceActionProcess waits for the exit of the helper process.
//
// The shell of the launch closes its own handle after reading the process id,
// so the wait opens the process again with the only right it needs — the
// synchronization right, which a caller may hold on a higher-integrity
// process. A process that cannot be opened is already gone (it ended before
// this open): its action is over, and the wait ends without blocking. A zero
// id never reaches this wait: a helper without a process record keeps the
// guard through the bounded hold instead
// (keepServiceActionGuardForUntrackedHelper).
func waitForServiceActionProcess(pid uint32) {
	if pid == 0 {
		return
	}
	// The exit code needs the query right beside the synchronization right.
	// Both are asked for together; a process that grants only the wait —
	// the right the wait always had — still gets its wait, without the
	// code, so adding the record can never cost the guard its evidence.
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	queried := err == nil
	if err != nil {
		handle, err = windows.OpenProcess(windows.SYNCHRONIZE, false, pid)
		if err != nil {
			return
		}
	}
	defer windows.CloseHandle(handle)
	if _, err := windows.WaitForSingleObject(handle, windows.INFINITE); err != nil {
		desktopLog.Warn("the wait for the elevated service helper ended without its exit",
			"pid", pid, "error", err)
		return
	}
	// The host owns the launch and outlives it for every action that runs
	// beside the desktop, so the log of this launch carries the exit of
	// the helper beside the record of the action it was launched for —
	// the evidence the report windows of the helper name their path to.
	if !queried {
		desktopLog.Info("the elevated service helper ended", "pid", pid, "exitCode", "unreadable")
		return
	}
	var exitCode uint32
	if err := windows.GetExitCodeProcess(handle, &exitCode); err != nil {
		desktopLog.Info("the elevated service helper ended", "pid", pid, "exitCode", "unreadable", "error", err)
		return
	}
	desktopLog.Info("the elevated service helper ended", "pid", pid, "exitCode", exitCode)
}

// keepServiceActionGuardUntilHelperEnds keeps the single service-action
// guard for the whole run of the helper and asks the menu to catch up right
// after it ended.
//
// The helper performs the action while this goroutine waits, so no second
// action may start beside it; the catch-up follows every outcome of the
// helper — the successful action re-read the state it settled into, and a
// failed one re-read whatever it left behind. wait and refresh are the
// functions the launching goroutine captured, so this waiter never reads a
// seam another caller could replace while it is unscheduled.
func keepServiceActionGuardUntilHelperEnds(pid uint32, wait func(uint32), refresh func()) {
	wait(pid)
	endServiceActionGuard(refresh)
}

// untrackedHelperWarmUpMargin is the part of that hold that covers the window
// between the launch of the helper and the start of its action budget:
// process creation, the runtime start, the option parse and the singleton
// hold that precede the action. The ordinary warm-up is well under a second;
// thirty seconds is a wide margin for the same window on a loaded machine or
// one whose antivirus scan delays the process start. The margin cannot make
// the coverage complete — see the honest residual on untrackedHelperHoldBudget.
const untrackedHelperWarmUpMargin = 30 * time.Second

// untrackedHelperHoldBudget bounds the guard hold of a helper that runs
// without a process record.
//
// The shell started such a helper, so it may perform its mutation while no
// caller can wait for its exit: releasing the single service-action guard at
// the launch would admit a second action beside a possibly live helper, and
// two helpers would move the same registration next to each other. The hold
// is bounded on purpose, and its budget covers the mutation window measured
// from the launch this caller returned from: the action budget of the helper
// itself (serviceActionBudget — the installation, the update, the start, the
// stop and the removal alike, measured from the start of the action) plus
// the warm-up margin above for the window before that action starts. A helper that reached its action is
// inside the covered window: its mutation ends with its action budget, and
// only the final report window of the helper can outlive that budget, which
// performs no further change; the hold then ends like the run of a
// waited-for helper: the guard is released and the menu catches up. What the
// hold does NOT cover is a warm-up that outlasts the margin — a helper whose
// start is delayed past it (a deep antivirus scan, a heavily paging machine)
// can still begin its mutation after the hold ended. That residual stays
// open on purpose: nothing can observe the progress of a helper the shell
// named no process for, and closing it would need a process record or a
// freshness protocol of its own.
//
// It is a variable so the tests can shorten the hold.
var untrackedHelperHoldBudget = serviceActionBudget + untrackedHelperWarmUpMargin

// keepServiceActionGuardForUntrackedHelper keeps the single service-action
// guard for the bounded hold of a helper that runs without a process record
// and asks the menu to catch up right after the hold ended.
//
// It is the end path of a launch nothing can wait for (see
// untrackedHelperHoldBudget): the budget stands in for the exit of the
// helper, and the release and the catch-up are the very ones the waited-for
// path takes. hold and refresh are the values the launching goroutine
// captured, so this waiter never reads a seam another caller could replace
// while it is unscheduled.
func keepServiceActionGuardForUntrackedHelper(hold time.Duration, refresh func()) {
	time.Sleep(hold)
	endServiceActionGuard(refresh)
}

// endServiceActionGuard releases the single service-action guard and asks
// the menu to catch up: the end every service action shares, after the exit
// of a waited-for helper and after the bounded hold of an untracked one.
func endServiceActionGuard(refresh func()) {
	serviceActionRunning.Store(false)
	if refresh != nil {
		refresh()
	}
}

// serviceMenuRefresh is the host's non-blocking request to re-read the
// registration for the tray menu — the very signal the display reads post
// after they ran. The host assigns it once, before any tray item exists;
// nil (a test, a host without a menu) means there is no menu to catch up.
//
// It is a package level variable, not a parameter, because the goroutine
// that waits for a helper outlives the call that started it and belongs to
// no single flow.
var serviceMenuRefresh func()

// requestServiceMenuCatchUp asks the tray menu to re-read the state: the
// request is the host's non-blocking signal, dropped when one is already
// pending, so an end path can ask without waiting for the menu.
func requestServiceMenuCatchUp() {
	if serviceMenuRefresh != nil {
		serviceMenuRefresh()
	}
}

// serviceHandoverCommit and serviceHandoverQuit are the two endpoints a
// lifecycle action needs from the host it runs on when its click found that
// host serving its own local server: commit hands the ready handover to the
// shutdown coordinator of THIS host, quit ends the host through the very path
// the menu's Quit item takes.
//
// They are package level variables — like serviceMenuRefresh beside them —
// because they belong to the host and not to one click: both hosts assign
// them before any tray item exists, and every surface (the lifecycle items
// and the registered-service dialog of the install recheck) reads the same
// pair. A surface that finds them unset has no host to hand over to, so it
// takes the honest refusal of the serving desktop instead of performing a
// mutation beside it (serviceActionHandover).
var (
	serviceHandoverCommit func(*desktopHandover, serviceAction, handoverRelaunch) bool
	serviceHandoverQuit   func()
)

// handoverRelaunchName is the record of which launch the host continues in
// after a committed handover.
func handoverRelaunchName(relaunch handoverRelaunch) string {
	if relaunch == handoverRelaunchDesktop {
		return "desktop"
	}
	return "tray companion"
}

// serviceActionCanHandOver reports whether this surface has the two host
// endpoints a handover needs — the commit into the shutdown coordinator and
// the quit of the host. Both hosts publish them before any tray item exists;
// a surface without them has no host to hand over to, so it refuses instead
// of asking a confirmation it could not act on (see serviceActionHandover,
// where the same fact is the authoritative last resort).
func serviceActionCanHandOver() bool {
	return serviceHandoverCommit != nil && serviceHandoverQuit != nil
}

// serviceActionHandover runs one service action of a desktop host that serves
// its own local server through the established handover — the very mechanism
// the installation takes: the elevated helper opens its wait handles and
// becomes ready, the host commits that handover to its own shutdown and quits,
// and the helper then acts beside a host that is already gone.
//
// That is what makes the action really perform instead of being refused: the
// refusal of the guard exists because a desktop of this session serves the
// address while the mutation runs, and the handover ends exactly that window
// — the host stops its server before the helper's first service step, so two
// instances never compete for one address.
//
// The launch the host continues in follows the action: the start and the
// update end beside the service, so the host comes back as a tray companion
// that binds no address, while the removal frees the address again and its
// child takes the desktop mode through its own service gate.
//
// The result reports whether a helper was launched AND its handover was
// committed: true means this host quits on that handover, so the caller keeps
// the single service-action guard with it (exactly like the installation
// route). false means nothing runs — every path already reported its own
// reason — and the caller keeps the guard and asks for the menu catch-up.
//
// The honest refusal of the serving desktop stays reachable here as the LAST
// resort: a surface with no host endpoints to hand over to, and a shutdown
// that already began when the commit arrived, are the two ways a handover
// cannot be established at all, and performing the mutation beside the desktop
// that serves the address is exactly what the guard refuses.
func serviceActionHandover(action serviceAction, instanceMutex string) bool {
	if !serviceActionCanHandOver() {
		desktopLog.Error("this surface has no host to hand the service action over to",
			"action", string(action))
		reportServingDesktopRefusal(action)
		return false
	}
	commit, quit := serviceHandoverCommit, serviceHandoverQuit
	relaunch := handoverRelaunchCompanion
	if action == serviceActionUninstall {
		relaunch = handoverRelaunchDesktop
	}
	handover, err := launchServiceHandover(action, os.Getpid(), instanceMutex, relaunch)
	if err != nil {
		reportServiceLaunchFailure(action, err)
		return false
	}
	if !commit(handover, action, relaunch) {
		handover.close()
		desktopLog.Info("the service action handover was not committed because desktop shutdown already started",
			"action", string(action))
		reportServingDesktopRefusal(action)
		return false
	}
	desktopLog.Info("the service action handover was committed: the desktop shuts down so the elevated helper can act",
		"action", string(action), "relaunch", handoverRelaunchName(relaunch))
	quit()
	return true
}

// servingDesktopRefusesMutation reports whether the guard of the elevated
// helper would refuse a mutation beside the desktop that runs this tray:
// the same two facts the helper decides from — another process of this
// session held the singleton when it was probed around its hold, and the
// published claim it reads is live.
//
// A probe that fails is classified fail-closed, exactly like the helper and
// the command family: an unproven freshness is never proof that no desktop
// of this session is alive.
func servingDesktopRefusesMutation(instanceMutex string) bool {
	release, preExisting, err := service.ProbeDesktopSingleton(instanceMutex)
	if release != nil {
		defer release()
	}
	if err != nil {
		preExisting = true
		desktopLog.Warn("the desktop singleton probe failed beside the tray",
			"error", err)
	}
	return service.LiveServingDesktop(preExisting)
}

// reportServingDesktopRefusal shows the refusal of a mutation the serving
// desktop of this session does not allow — the very report the elevated
// helper shows, with the result, the cause and the one action that ends it.
// The tray shows it directly so a mutation whose refusal is already known
// never costs the user an elevation prompt.
func reportServingDesktopRefusal(action serviceAction) {
	desktopLog.Error("the tray did not ask for elevation beside a serving desktop instance",
		"action", string(action))
	showMessage(serviceFailureCaption(action),
		serviceServingDesktopRefusedText(action), windows.MB_ICONERROR)
}

// serviceDataDir returns the data directory the service owns.
//
// An existing registration decides, because the confirmation must name the
// directory the service on this machine really owns and not a default; a
// machine without a registration gets the default state directory of the scope.
// It is a machine-wide directory, never the profile of the user who installs
// it, so the confirmation names the directory the service will really own.
func serviceDataDir() string {
	// The budget is named by this caller: the single read without settle
	// takes serviceStatusTimeout — the 10 seconds this read had before it
	// joined the one owner — so a service control manager slower than the
	// 2s display budget still yields the registered directory for the
	// confirmation text. The read is presentation for the confirmation,
	// not a decision on the state, so it never waits for a transition, and
	// a read that fails keeps the fallback to the default of the scope.
	if status, err := serviceState.readSingle(serviceStatusTimeout); err == nil && status.Paths.StateDir != "" {
		return status.Paths.StateDir
	}

	paths, err := service.DefaultPaths(service.ScopeSystem)
	if err != nil {
		desktopLog.Warn("could not resolve the data directory of the service", "error", err)
		return ""
	}
	return paths.StateDir
}

// programDriveType returns the type of the drive the given program path lies
// on.
//
// It is a package level variable so the tests decide the drive type of a
// path themselves: a test machine has no removable or network drive to put
// the program on.
var programDriveType = driveTypeOfProgramPath

// driveTypeOfProgramPath asks Windows for the drive type of the drive that
// holds the given program path.
//
// The root of the path is what the query takes: the drive letter root for a
// path on a local volume, and the share root for a UNC path — the root of a
// mapped network drive is its drive letter, and Windows reports it as a
// remote drive by itself. A path whose volume cannot be read reports the
// unknown type, which the decision below treats like an ordinary drive: an
// unknown root is never a reason to warn.
func driveTypeOfProgramPath(path string) uint32 {
	root := programDriveRoot(path)
	if root == "" {
		return windows.DRIVE_UNKNOWN
	}
	rootPtr, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return windows.DRIVE_UNKNOWN
	}
	return windows.GetDriveType(rootPtr)
}

// programDriveRoot returns the root the drive-type query takes for the given
// program path: "C:\" for a path on the volume C:, the share root for a UNC
// path, and "" for a path without a readable volume.
func programDriveRoot(path string) string {
	volume := filepath.VolumeName(path)
	if volume == "" {
		return ""
	}
	if strings.HasPrefix(volume, `\\`) {
		// A UNC path: the root of the share, with the trailing
		// separator the query names it by.
		return volume + `\`
	}
	return volume + `\`
}

// programOnRemovableOrNetworkDrive reports whether the program path lies on
// a removable or a network drive — the fact the B2 warning of the
// installation confirmation is shown for. Every other drive type (fixed,
// optical, RAM, unknown) is an ordinary path and changes nothing about the
// confirmation.
func programOnRemovableOrNetworkDrive(path string) bool {
	switch programDriveType(path) {
	case windows.DRIVE_REMOVABLE, windows.DRIVE_REMOTE:
		return true
	default:
		return false
	}
}

// desktopExecutable returns the absolute path of the running desktop program.
func desktopExecutable() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("the path of the desktop program cannot be read: %w", err)
	}
	return path, nil
}

// serviceHelperLaunch is the outcome of one launch of the elevated helper.
//
// It keeps the two outcomes apart that the guard policy of every caller
// decides on differently — a launch that started NO helper, and a helper
// that runs — instead of hiding both behind a zero process id. A started
// helper carries its process id when the shell named one, so a caller can
// wait for its exit; a started helper the shell named no process for carries
// pid 0: it performs the same mutation, but no caller can observe its exit
// (see untrackedHelperHoldBudget).
type serviceHelperLaunch struct {
	// started reports whether the shell started a helper process. false
	// means nothing runs: the console-program notice of an update was
	// declined or its download failed, or the launch failed with its own
	// error.
	started bool
	// pid is the process id of the started helper. Zero while started is
	// true means the launch carries no process record to wait for.
	pid uint32
}

// hostLaunchSurface is the surface THIS host runs as: the launch surface the
// reports of the helpers this host launches must send the user back to.
//
// It is launch-scoped state in the sense of runLogPath: the desktop host keeps
// the default and the tray companion assigns its own surface before the first
// service action of its menu can be wired (runCompanion). The value travels to
// the helper as a command line marker beside the log path
// (serviceHelperArguments), because the elevated helper cannot see the mode of
// the process that started it.
var hostLaunchSurface = launchSurfaceDesktop

// launchServiceHelper starts the elevated helper of the desktop host.
//
// The helper is this very program, started with administrator rights: the
// desktop never implements a second service client, and the helper runs the
// same actions the user would run from a console. The outcome names both the
// fact that a helper runs and its process id (serviceHelperLaunch), so the
// caller can record which helper performs the action and how to end its
// guard.
//
// An update registers from the console program of this folder, so the
// missing program is settled here — the one place every update entry passes
// and always before the elevation prompt — through offerMissingConsoleProgram.
// A declined notice, or a failed download whose report that flow already
// showed, returns the empty result: no helper was started, and there is no
// launch failure left for the caller to report. Every launch the shell
// performed returns started — with the process id when the shell named one,
// and zero when it did not. A start runs the executable the registration
// already names and never looks for a console program.
//
// instanceMutex is handed to an update or a start, which hold that desktop
// singleton through their mutation; every other action carries no wait state.
// The claim path of this desktop travels with it, so the guard of the action
// reads this desktop's claim under any approving account.
func launchServiceHelper(action serviceAction, waitPID int, instanceMutex string) (serviceHelperLaunch, error) {
	executable, err := desktopExecutable()
	if err != nil {
		return serviceHelperLaunch{}, err
	}
	if action == serviceActionUpdate {
		if _, ok := offerMissingConsoleProgram(action, executable); !ok {
			return serviceHelperLaunch{}, nil
		}
	}
	pid, err := launchElevated(elevatedLaunch{
		Executable:       executable,
		Action:           action,
		WaitPID:          waitPID,
		InstanceMutex:    instanceMutex,
		DesktopState:     service.PublishedStatePath(),
		LogFile:          runLogPath,
		CompanionSurface: hostLaunchSurface == launchSurfaceCompanion,
	})
	if err != nil {
		return serviceHelperLaunch{}, err
	}
	desktopLog.Info("the elevated service helper started",
		"action", string(action), "pid", pid)
	return serviceHelperLaunch{started: true, pid: pid}, nil
}

// offerMissingConsoleProgram settles the console program the service action
// will run, before that action asks for anything else: it looks the program
// up in the folder of the desktop program and, when it is missing, shows the
// notice — the verified download of this release's own pair on the user's
// explicit request, the manual route, or, when no download can be offered,
// the reason for that and a way to ask the release again.
//
// It reports the program the action continues with and whether it may
// continue at all. false means the program is still missing and the action
// must change nothing: a declined notice ends without any further report, a
// press of the manual route opens the release page of this build and ends the
// same way — it decides nothing about the action — and a failed download was
// reported by this flow itself — the caller adds no second report beside it.
// The install precheck and both update entries run through here, so the
// notice, the fail-closed download and the rule that keeps a decline quiet
// are one implementation instead of three.
func offerMissingConsoleProgram(action serviceAction, executable string) (string, bool) {
	console, err := findSiblingExecutable(filepath.Dir(executable), executable)
	if err == nil {
		return console, true
	}
	desktopLog.Warn("the console program of the service is missing",
		"action", string(action),
		"folder", filepath.Dir(executable), "error", err)
	// The notice is built from the lookup's folder and error, and the
	// offer is resolved before it: the download link only exists when
	// this release publishes the exact paired asset with an authoritative
	// checksum. A resolve that fails is not dropped — it becomes the
	// reason line of the notice, so a button never disappears in silence.
	offer, offerErr := consoleDownloadOfferFor(executable)
	if offerErr != nil {
		desktopLog.Warn("no verified download is offered for the missing console program", "error", offerErr)
	}
	notice := newSiblingMissingNotice(action, filepath.Dir(executable), executable, err, offer, offerErr)

	answer := showSiblingMissingNotice(notice)
	for answer == siblingMissingRetry {
		// "Try again" reads the release once more: the connection may
		// be back, or the release may publish the file by now. The
		// notice is rebuilt from what was just read, so its reason line
		// and its links always describe the release as it stands, and
		// the loop only ever runs while the user asks for another read.
		desktopLog.Info("the release is read again for the missing console program", "action", string(action))
		offer, offerErr = consoleDownloadOfferFor(executable)
		if offerErr != nil {
			desktopLog.Warn("no verified download is offered for the missing console program", "error", offerErr)
		}
		notice = newSiblingMissingNotice(action, filepath.Dir(executable), executable, err, offer, offerErr)
		answer = showSiblingMissingNotice(notice)
	}
	if answer == siblingMissingManual && notice.ReleaseURL != "" {
		// The manual route opens the release page of this build — the
		// very URL the manual instruction text names — through the one
		// opener every browser call of this host uses, in the user's
		// session, with the one report every failed open ends in. The
		// press decides nothing about the action: the program is still
		// missing, so the decline below still ends it.
		if openErr := openBrowser(notice.ReleaseURL); openErr != nil {
			reportBrowserFailure(notice.ReleaseURL, openErr)
		}
	}
	if answer != siblingMissingDownload {
		desktopLog.Info("the console program stays missing: the service action changes nothing",
			"action", string(action))
		return "", false
	}
	console, err = runConsoleDownload(notice)
	if err != nil {
		desktopLog.Error("the console program could not be downloaded: this download left no file of its own in the folder",
			"action", string(action),
			"folder", filepath.Dir(executable), "error", err)
		showMessage(consoleDownloadFailedCaption, consoleDownloadFailedText(notice, err),
			windows.MB_ICONERROR)
		return "", false
	}
	desktopLog.Info("the console program was downloaded and verified",
		"action", string(action), "console", console)
	return console, true
}

func reportServiceHandoverFailure(action serviceAction, err error) {
	if err == nil {
		return
	}
	desktopLog.Error("the desktop could not finish the service action", "action", string(action), "error", err)
	showMessage(serviceFailureCaption(action),
		serviceHandoverFailureText(action, err),
		windows.MB_ICONERROR)
}

// launchServiceHandover starts the elevated helper of the desktop host for a
// full handover and waits until that helper became ready.
//
// relaunch is the launch THIS host continues in once the handover settled; it
// decides the launch surface the helper's reports address, because those
// reports send the user back to the surface they will really find after the
// handover — the tray companion beside the service for a start and an update,
// and the ordinary desktop launch for a removal that frees the address again.
func launchServiceHandover(action serviceAction, waitPID int, instanceMutex string,
	relaunch handoverRelaunch) (*desktopHandover, error) {
	executable, err := desktopExecutable()
	if err != nil {
		return nil, err
	}
	events, err := newHandoverEvents()
	if err != nil {
		return nil, err
	}
	helper, err := launchElevatedProcess(elevatedLaunch{
		Executable:       executable,
		Action:           action,
		WaitPID:          waitPID,
		ReadyEvent:       events.readyName,
		SuccessEvent:     events.successName,
		InstanceMutex:    instanceMutex,
		DesktopState:     service.PublishedStatePath(),
		LogFile:          runLogPath,
		CompanionSurface: relaunch == handoverRelaunchCompanion,
	})
	if err != nil {
		if helper != nil {
			helper.Close()
		}
		events.close()
		return nil, err
	}
	if helper == nil || helper.handle == 0 {
		if helper != nil {
			helper.Close()
		}
		events.close()
		return nil, errors.New("the elevated service helper started without a process handle")
	}
	if err := waitForHelperReady(helper.handle, events.ready, serviceWaitBudget); err != nil {
		helper.Close()
		events.close()
		// The helper was started: its report is the report of a helper
		// that never became ready, not of a launch that never ran. The
		// desktop never committed the shutdown before this evidence, so
		// no service action was performed — the helper waits for the
		// success signal that only a ready handover authorizes.
		return nil, &helperNotReadyError{cause: err}
	}
	return &desktopHandover{events: events, helper: helper}, nil
}

// confirmServiceInstall asks the user whether the Windows service is
// installed.
//
// It is a package level variable so the tests can answer the confirmation: the
// real message box blocks a process without an interactive window station.
var confirmServiceInstall = confirmNativeServiceInstall

// confirmServiceAction asks the user whether a tray action that changes the
// machine besides the installation runs — today the stop of the service and
// the removal of its registration — before the administrator prompt of that
// action follows.
//
// It is a package level variable for the same reason confirmServiceInstall
// is: the tests answer it, and a declined answer must leave the helper
// unlaunched. The installation keeps its own seam, so its established flow
// stays byte for byte what it is.
var confirmServiceAction = confirmNativeServiceAction

// The answers a message box reports.
const (
	// messageBoxYes is the id of the Yes button.
	messageBoxYes = 6
	// messageBoxNo is the id of the No button.
	messageBoxNo = 7
	// messageBoxCancel is the id of the Cancel button, and the id a task
	// dialog reports when it was closed with Esc or with the close button.
	messageBoxCancel = 2
)

// confirmNativeServiceInstall shows the confirmation of the service
// installation in a native message box.
//
// No is the default button: the registration is a machine-wide change that
// survives the desktop app, so the answer that changes nothing is the one a
// stray Enter selects.
func confirmNativeServiceInstall(text string) bool {
	return confirmNativeServiceAction(installServiceCaption, text)
}

// serviceConfirmMessageBoxFlags are the flags every Yes/No confirmation of
// a service action shows with — the installation included: the two named
// buttons, the question icon and the frame of the tray dialogs, plus
// MB_DEFBUTTON2, so No is the button a stray Enter selects: each of these
// confirmations guards a machine-wide change, and the answer that changes
// nothing has to be the default. The composition is pinned by a test, so a
// flag can neither drop out of it nor join it unnoticed.
const serviceConfirmMessageBoxFlags = windows.MB_YESNO | windows.MB_ICONQUESTION | windows.MB_DEFBUTTON2 |
	windows.MB_SETFOREGROUND | windows.MB_TOPMOST

// confirmNativeServiceAction shows a Yes/No confirmation of a service action
// in a native message box, under the caption its action carries.
//
// No is the default button for every confirmation this function shows: each
// of them guards a machine-wide change, so the answer that changes nothing
// is the one a stray Enter selects — the rule the installation confirmation
// already follows.
func confirmNativeServiceAction(caption, text string) bool {
	textPtr, err := windows.UTF16PtrFromString(text)
	if err != nil {
		return false
	}
	captionPtr, err := windows.UTF16PtrFromString(caption)
	if err != nil {
		return false
	}
	result, _ := windows.MessageBox(0, textPtr, captionPtr, serviceConfirmMessageBoxFlags)
	return result == messageBoxYes
}
