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
//   - the display surfaces (the tray line, the Runtime / Logs window) name
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

// readSingle reads the registration exactly once under the budget its
// caller names: the one mechanism of the owner for every read that does not
// wait for a transition. The callers differ in the budget they name, never
// in the mechanism.
//
// Every read and its result are recorded: the log of the host is where a
// user checks what the status surfaces saw, and the record is written as the
// read it was — never as the outcome of an action this host did not perform.
func (serviceStateReader) readSingle(budget time.Duration) (service.Status, error) {
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	status, err := serviceStatus(ctx)
	if err != nil {
		desktopLog.Warn("read the Windows service state", "error", err)
		return status, err
	}
	desktopLog.Info("read the Windows service state",
		"state", string(status.State),
		"running", status.Running)
	return status, nil
}

// readRuntimeServiceState answers the state read of the Runtime / Logs
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
// install button of the Runtime / Logs window.
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
// guard spans the mutation and not only the click that asked for it. An
// installation handover keeps it until the host exits.
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

	if !confirmServiceInstall(installServiceText(serviceDataDir())) {
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

	handover, err := launchServiceHandover(serviceActionInstall, os.Getpid(), instanceMutex)
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
// apart. No answer starts a handover — this host keeps running: the desktop
// mode and Cancel change nothing, the browser action opens the URL of the
// registration, and the start and the update go to the elevated helper the
// way the gate launches them, where the helper's own guards stay
// authoritative.
//
// The invariant at this surface is that no answer asks for elevation for a
// mutation that is already decided against: a start beside this serving
// desktop, and an update that is not a pure rewrite of a positively
// confirmed stopped registration, are refused HERE with the helper's own
// report — result, cause and the one action that ends it — instead of
// costing the user a user account control prompt for that same refusal.
// The decision reuses the guard of the helper itself (the singleton probe
// beside the live published claim), so the two surfaces — this dialog and
// the lifecycle items of the menu, which run the very same functions — can
// never disagree about which mutations the serving desktop allows; the
// helper decides again, authoritatively, when the elevation actually
// happens.
//
// The second result reports whether a helper was launched for the answer:
// such a helper inherits the single service-action guard its caller holds
// and releases it when it ends, so the caller must not release the guard
// itself. false means nothing was launched and the caller keeps the
// ownership of the guard.
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
// This host runs the address a started service would take, so the guard of
// the helper refuses every start beside it: the answer is reported here
// instead of being sent through elevation to its own refusal. The guard
// decides again, authoritatively, when a start that passed this check
// reaches the elevation.
//
// The result reports whether a helper was launched, with the same meaning
// offerRegisteredServiceFromTray documents.
func startRegisteredServiceFromSurface(instanceMutex string) bool {
	if servingDesktopRefusesMutation(instanceMutex) {
		reportServingDesktopRefusal(serviceActionStart)
		return false
	}
	return launchTrayServiceAction(serviceActionStart, instanceMutex)
}

// updateRegisteredServiceFromSurface performs the update of a registered
// service beside the desktop that runs this tray — the answer of the
// registered-service dialog and the update item of the menu run this very
// function, so the two surfaces can never disagree.
//
// Only the update of a positively confirmed stopped registration (the same
// standard the gate's dialog and the helper use) is a pure rewrite that can
// succeed beside this desktop; every other state restarts the instance and
// is refused beside the desktop that serves the address. Both answers are
// decided with the guard of the helper, so a doomed update never reaches
// the elevation. The result carries the same meaning
// offerRegisteredServiceFromTray documents.
func updateRegisteredServiceFromSurface(status service.Status, instanceMutex string) bool {
	positivelyStopped := status.State == service.StateStopped && !serviceStatusRunning(status)
	if !positivelyStopped {
		if servingDesktopRefusesMutation(instanceMutex) {
			reportServingDesktopRefusal(serviceActionUpdate)
			return false
		}
	}
	return launchTrayServiceAction(serviceActionUpdate, instanceMutex)
}

// openRegisteredServiceFromTray offers the choice of an existing
// registration to the Service... item of the tray.
//
// The item exists only for a registration the menu read, but the machine
// can change between that read and the click, so the state is read again
// before the dialog is built — with the settle wait of the gate, because
// the dialog decides on the state just like the gate's dialog does. The
// dialog itself is reached through offerRegisteredServiceFromTray, the very
// function the install item reaches when its own recheck finds a
// registration, so the two items can never ask different questions or act
// on different readings. A state that reads as no registration changes
// nothing: the menu line catches up with its next refresh instead of
// offering an installation the user did not ask for with this click.
//
// The item takes the single service-action guard for the whole dialog: a
// click on any other service surface while the dialog is open is refused
// with its own report instead of asking a second question beside the first
// one. An answer that launches a helper inherits the guard through
// offerRegisteredServiceFromTray's result and releases it when the helper
// ends; every other answer releases it before this function returns.
func openRegisteredServiceFromTray(instanceMutex string) {
	if !beginServiceAction("Service... item") {
		return
	}
	handedToHelper := false
	defer func() {
		if !handedToHelper {
			serviceActionRunning.Store(false)
		}
	}()

	status, ok := readServiceStateForAction("Service... item")
	if !ok {
		return
	}
	handedToHelper = offerRegisteredServiceFromTray(status, instanceMutex)
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
// one function, so the Service... dialog and the lifecycle items decide on
// the same readings and report the same states.
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
// follows (the stop and the removal; a start and an update keep the
// confirmation-free behavior they have), decide the pre-elevation refusals
// with the guard of the helper, then launch the elevated helper. The helper
// reports the outcome of the action itself, in a native message box.
//
// The guard covers the whole action: a launched helper inherits it through
// the waiter and releases it when it ends, together with the menu refresh;
// every other end path — a declined confirmation, a refused mutation, a
// failed launch, an unknown state — releases it here and asks for the menu
// catch-up, so the status is re-read after every request that acted, and
// after the helper ends for the ones that launched it.
func performServiceActionFromTray(action serviceAction, instanceMutex string) {
	if !beginServiceAction(serviceActionSurface(action)) {
		return
	}
	launched := false
	defer func() {
		if launched {
			// The helper inherited the guard; it releases the guard
			// and asks for the menu catch-up when it ends.
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
		if !confirmServiceAction(removeServiceConfirmCaption, removeServiceConfirmText()) {
			desktopLog.Info("the removal of the service registration was not confirmed")
			return
		}
	case serviceActionStart:
		// The start answer of the registered-service dialog runs this
		// very precheck: a start beside this serving desktop is refused
		// here with the helper's own report instead of costing a user
		// account control prompt for that same refusal.
		if servingDesktopRefusesMutation(instanceMutex) {
			reportServingDesktopRefusal(serviceActionStart)
			return
		}
	case serviceActionUpdate:
		// The update decides beside the state, so it re-reads the
		// registration under the action policy — the menu line may have
		// been drawn seconds ago — and a state that cannot be decided on
		// reaches the established notice instead of an elevation. The
		// rest is the same function the dialog runs.
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
// keeps it until that helper ends.
//
// A start and an update carry the desktop singleton of this host — they hold
// it through their mutation, exactly like the launches of the service gate.
// A stop and a removal are launched without one: they hold no singleton, and
// a helper that received one would refuse its own command line instead of
// stopping or removing anything (see validateServiceHandoverOptions).
//
// The result reports whether a helper was launched: true means the waiter
// owns the guard now and releases it, with the menu catch-up, when the
// helper ends. false means nothing runs and the caller keeps the guard —
// either the console-program notice of the update was declined (no report is
// due: the notice flow already showed its own) or the launch failed, which
// this function reports like every other launch of these actions.
func launchTrayServiceAction(action serviceAction, instanceMutex string) bool {
	mutex := instanceMutex
	if !serviceActionHoldsSingleton(action) {
		mutex = ""
	}
	pid, err := launchServiceHelper(action, 0, mutex)
	if err != nil {
		reportServiceLaunchFailure(action, err)
		return false
	}
	if pid == 0 {
		return false
	}
	// Both end-path functions are read HERE, on the goroutine that launched
	// the helper: the waiter outlives this call, and values it read itself
	// could be replaced by a later caller while the waiter is still
	// unscheduled. The captured refresh is the host's own signal — assigned
	// before any tray item exists — or the stub of a test that launched
	// this helper; the captured wait is the same seam the test replaced
	// before this launch.
	go keepServiceActionGuardUntilHelperEnds(pid, waitForServiceActionHelper, serviceMenuRefresh)
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
// this open, or its id was never readable): its action is over, and the wait
// ends without blocking.
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

// desktopExecutable returns the absolute path of the running desktop program.
func desktopExecutable() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("the path of the desktop program cannot be read: %w", err)
	}
	return path, nil
}

// launchServiceHelper starts the elevated helper of the desktop host.
//
// The helper is this very program, started with administrator rights: the
// desktop never implements a second service client, and the helper runs the
// same actions the user would run from a console. The process id of the helper
// is returned, so the caller can record which helper performs the action.
//
// An update registers from the console program of this folder, so the
// missing program is settled here — the one place every update entry passes
// and always before the elevation prompt — through offerMissingConsoleProgram.
// A declined notice, or a failed download whose report that flow already
// showed, returns (0, nil): no helper was started, and there is no launch
// failure left for the caller to report. A start runs the executable the
// registration already names and never looks for a console program.
//
// instanceMutex is handed to an update or a start, which hold that desktop
// singleton through their mutation; every other action carries no wait state.
// The claim path of this desktop travels with it, so the guard of the action
// reads this desktop's claim under any approving account.
func launchServiceHelper(action serviceAction, waitPID int, instanceMutex string) (uint32, error) {
	executable, err := desktopExecutable()
	if err != nil {
		return 0, err
	}
	if action == serviceActionUpdate {
		if _, ok := offerMissingConsoleProgram(action, executable); !ok {
			return 0, nil
		}
	}
	pid, err := launchElevated(elevatedLaunch{
		Executable:    executable,
		Action:        action,
		WaitPID:       waitPID,
		InstanceMutex: instanceMutex,
		DesktopState:  service.PublishedStatePath(),
		LogFile:       runLogPath,
	})
	if err != nil {
		return 0, err
	}
	desktopLog.Info("the elevated service helper started",
		"action", string(action), "pid", pid)
	return pid, nil
}

// offerMissingConsoleProgram settles the console program the service action
// will run, before that action asks for anything else: it looks the program
// up in the folder of the desktop program and, when it is missing, shows the
// notice — the verified download of this release's own pair on the user's
// explicit request, or the manual path.
//
// It reports the program the action continues with and whether it may
// continue at all. false means the program is still missing and the action
// must change nothing: a declined notice ends without any further report, and
// a failed download was reported by this flow itself — the caller adds no
// second report beside it. The install precheck and both update entries run
// through here, so the notice, the fail-closed download and the rule that
// keeps a decline quiet are one implementation instead of three.
func offerMissingConsoleProgram(action serviceAction, executable string) (string, bool) {
	console, err := findSiblingExecutable(filepath.Dir(executable), executable)
	if err == nil {
		return console, true
	}
	desktopLog.Warn("the console program of the service is missing",
		"action", string(action),
		"folder", filepath.Dir(executable), "error", err)
	// The notice is built from the lookup's folder and error, and the
	// offer is resolved before it: the download button only exists when
	// this release publishes the exact paired asset with an authoritative
	// checksum — otherwise the notice keeps its manual instructions alone.
	offer, offerErr := consoleDownloadOfferFor(executable)
	if offerErr != nil {
		desktopLog.Warn("no verified download is offered for the missing console program", "error", offerErr)
	}
	notice := newSiblingMissingNotice(action, filepath.Dir(executable), executable, err, offer)
	if showSiblingMissingNotice(notice) != siblingMissingDownload {
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

func reportServiceHandoverFailure(err error) {
	if err == nil {
		return
	}
	desktopLog.Error("the desktop could not finish the service installation", "error", err)
	showMessage(serviceFailureCaption(serviceActionInstall),
		serviceHandoverFailureText(err),
		windows.MB_ICONERROR)
}

func launchServiceHandover(action serviceAction, waitPID int, instanceMutex string) (*desktopHandover, error) {
	executable, err := desktopExecutable()
	if err != nil {
		return nil, err
	}
	events, err := newHandoverEvents()
	if err != nil {
		return nil, err
	}
	helper, err := launchElevatedProcess(elevatedLaunch{
		Executable:    executable,
		Action:        action,
		WaitPID:       waitPID,
		ReadyEvent:    events.readyName,
		SuccessEvent:  events.successName,
		InstanceMutex: instanceMutex,
		DesktopState:  service.PublishedStatePath(),
		LogFile:       runLogPath,
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
