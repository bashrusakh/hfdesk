// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package desktop

import (
	"errors"
	"fmt"
	"strings"

	"github.com/bashrusakh/hfdesk/internal/service"
)

// runLogPath is the path of the log file THIS launch writes.
//
// The host assigns it when its logging pipeline is installed, so a window of
// a launch that has no log file of its own — the single instance refusal, a
// command line error, a log that could not be opened, a second launch, and a
// helper run of a hand-typed command line — names no file, because there is
// none to name: such a run writes nothing to any log. The elevated helper
// assigns exactly the path the launching host passed to it
// (ServiceActionOptions.LogFile) and none when the command line carries
// none, so a report the helper shows names the log the host of this session
// writes: the host records every service action it launches and the exit of
// its helper there.
var runLogPath string

// logFileLine returns the one standard line every failure window carries:
// the path of the log file of this launch, so the log is reachable from
// outside the window. A launch without a log file of its own names none —
// the line is empty instead of pointing at a file this run does not write.
func logFileLine(path string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	return "\n\nThe log file is at " + path + "."
}

// failureWindowText appends that standard line to the text of a failure
// window. Windows whose path is passed by the box that shows them
// (reportStartupFailure, the panic guard) do not use it: their path may be
// empty for a failure before the logging started, and the box appends the
// line itself from the path it was given — a constructor must never bake in
// a path beside that explicit one.
func failureWindowText(text string) string {
	return text + logFileLine(runLogPath)
}

// The captions of the message boxes of the service actions.
//
// Every box of the service flow is captioned here, so the tray, the service
// gate and the elevated helper cannot show two different titles for the same
// action.
const (
	// installServiceCaption is the caption of the confirmation that asks
	// the user whether the desktop host registers the Windows service.
	installServiceCaption = "HFDesk - install the Windows service"

	// serviceGateCaption is the caption of the choice dialog of a launch
	// that found an installed service.
	serviceGateCaption = "HFDesk is installed as a Windows service"

	// serviceGatePendingCaption is the caption of the notice of a launch
	// that found the service in the middle of a transition.
	serviceGatePendingCaption = "HFDesk - the service is starting or stopping"

	// serviceGateUnreadableCaption is the caption of the notice of a
	// launch that could not read the registration of the service.
	serviceGateUnreadableCaption = "HFDesk - the service state cannot be read"

	// serviceGateChangedCaption is the caption of the notice of a launch
	// that chose the desktop mode for a stopped service and then re-read a
	// state that is no longer positively stopped.
	serviceGateChangedCaption = "HFDesk - the service state changed"

	// serviceSiblingMissingCaption is the window title of the notice that
	// the console program of the service is missing. The redesigned notice
	// titles its task dialog and its message box fallback alike with the
	// product name, so the two never show different titles for the same
	// question.
	serviceSiblingMissingCaption = "HFDesk"

	// consoleDownloadFailedCaption is the caption of the report of a
	// download of the console program that did not finish or did not
	// verify.
	consoleDownloadFailedCaption = "HFDesk - the console program could not be downloaded"

	// serviceInstalledCaption is the caption of the report of a finished
	// installation.
	serviceInstalledCaption = "HFDesk - service installed"

	// serviceUpdatedCaption is the caption of the report of a finished
	// update.
	serviceUpdatedCaption = "HFDesk - service updated"

	// serviceStartedCaption is the caption of the report of a started
	// service.
	serviceStartedCaption = "HFDesk - service started"

	// serviceHelperNotReadyCaption is the caption of the report of an
	// elevated helper that started but never became ready. It is distinct
	// from the report of a helper that never ran — a declined user account
	// control prompt and a shell that could not start the program keep the
	// caption and the wording they have today — because the two states have
	// different causes and different next steps.
	serviceHelperNotReadyCaption = "HFDesk - the administrator helper did not become ready"
)

// The captions of the confirmations and the reports of the two service
// actions the tray performs beside the installation: the stop of the
// service and the removal of its registration. They sit beside the
// installation family above, whose captions and classes stay what they are.
const (
	// stopServiceConfirmCaption is the caption of the confirmation that
	// asks the user whether the running service is stopped before the
	// administrator prompt follows.
	stopServiceConfirmCaption = "HFDesk - stop the Windows service"

	// removeServiceConfirmCaption is the caption of the confirmation that
	// asks the user whether the service registration is removed before
	// the administrator prompt follows.
	removeServiceConfirmCaption = "HFDesk - remove the Windows service registration"

	// serviceStoppedCaption is the caption of the report of a stopped
	// service.
	serviceStoppedCaption = "HFDesk - service stopped"

	// serviceRegistrationRemovedCaption is the caption of the report of a
	// removed service registration.
	serviceRegistrationRemovedCaption = "HFDesk - service registration removed"

	// serviceNoRegistrationCaption is the caption of the report of a
	// removal that found no registration to remove: the same clean
	// outcome the command line reports, in its own words instead of the
	// caption that claims a removal.
	serviceNoRegistrationCaption = "HFDesk - no Windows service registration"

	// serviceActionBusyCaption is the caption of the refusal of a second
	// service action while one is already running. It names the cause
	// alone — no action is blamed, because the refused click is not the
	// one at fault.
	serviceActionBusyCaption = "HFDesk - another service action is already running"
)

// serviceFailureCaption returns the caption of a failed service action.
//
// The captions are per action, so the user reads which action failed instead
// of a generic failure. None of them claims that the machine was left as it
// was: a failure of the service control manager can happen after the
// registration was written, and a caption that cannot be true is worse than a
// careful one.
func serviceFailureCaption(action serviceAction) string {
	switch action {
	case serviceActionInstall:
		return "HFDesk - the service installation failed"
	case serviceActionUpdate:
		return "HFDesk - the service update failed"
	case serviceActionStart:
		return "HFDesk - the service did not start"
	case serviceActionStop:
		return "HFDesk - the service did not stop"
	case serviceActionUninstall:
		return "HFDesk - the service registration was not removed"
	default:
		return "HFDesk - the service action failed"
	}
}

// dataSeparationLine returns the one line that states the surprising fact of
// the service family: the data of the service is not the data of this app.
// It is shared by the installation confirmation and the service gate, so the
// two windows never state the separation in different words.
func dataSeparationLine(dataDir string) string {
	if dataDir != "" {
		return "Its data is separate: " + dataDir + " — this app's settings, jobs and models are not there."
	}
	return "Its data is separate — this app's settings, jobs and models do not appear in the service."
}

// serviceProgramDriveWarningLine is the warning the installation
// confirmation shows when the program the registration will run lies on a
// removable or network drive (the B2 decision).
//
// It follows the rule of this text family: one short line, in human words,
// that states the consequence for the user — Windows may bring the service
// up before such a drive is there — and no mechanism behind that fact. The
// caller decides WHEN the line appears (the drive type of the program
// path), so this package states the line and nothing else.
const serviceProgramDriveWarningLine = "HFDesk is on a removable or network drive: Windows may start the service before this drive is ready."

// installServiceText returns the text of the confirmation that registers the
// Windows service.
//
// The confirmation states the facts the user has to know before a
// machine-wide registration is written — the service starts at boot and
// survives this window, the registration needs administrator rights, the
// narrowed download rule, and the one surprising fact (the separate data) as
// its single line — then asks the one question. dataDir is the state
// directory of the service, so the text names the directory it will really
// own.
//
// unusualDrive adds the B2 warning as its own single line right before that
// surprising fact: it is present exactly when the program the registration
// runs sits on a removable or network drive, and a program on an ordinary
// drive changes nothing — with the flag false the text is byte for byte the
// confirmation this family has always shown.
func installServiceText(dataDir string, unusualDrive bool) string {
	text := "This registers HFDesk as a Windows service.\n\n" +
		"The service starts at boot and keeps running after this window closes.\n" +
		"Installing it needs administrator rights, so a Windows prompt follows.\n\n" +
		"HFDesk stays portable: the service runs the hfdesk program that sits next to this desktop app. The one file HFDesk can download is that missing program from its own release — only when you ask for it, and only after its SHA-256 is verified; nothing else is downloaded or copied.\n\n"
	if unusualDrive {
		text += serviceProgramDriveWarningLine + "\n\n"
	}
	return text + dataSeparationLine(dataDir) + "\n\n" +
		"Do you want to install the service?"
}

// stopServiceConfirmText returns the text of the confirmation that stops the
// Windows service before the administrator prompt follows.
//
// The confirmation follows the shape of the installation confirmation: what
// the action does, the administrator rights it needs, the one fact the user
// has to see before agreeing — nothing serves the address of the service
// afterwards — and the question. It names no state of its own: the state the
// menu item was enabled by can be stale by the time the box is read, and a
// confirmation that claimed a state the machine no longer has would ask the
// user to agree to something that is not there. The helper reads the state
// again and reports what it really found.
func stopServiceConfirmText() string {
	return "This stops the HFDesk service.\n\n" +
		"Stopping it needs administrator rights, so a Windows prompt follows.\n" +
		"Nothing serves the address of the service until you start it again.\n\n" +
		"Do you want to stop the service?"
}

// removeServiceConfirmText returns the text of the confirmation that removes
// the Windows service registration before the administrator prompt follows.
//
// The confirmation states the one invariant the removal has: only the
// registration goes, and the data stays. That is the surprising fact of this
// action — the fact a user has to read before agreeing — so it is the one
// line beside the administrator-rights sentence and the question. The
// confirmation claims nothing about the state of the instance: a running one
// is stopped inside the removal, which the user does not have to orchestrate.
func removeServiceConfirmText() string {
	return "This removes the Windows service registration of HFDesk.\n\n" +
		"Removing it needs administrator rights, so a Windows prompt follows.\n" +
		"Your settings, jobs, models, cache, history and logs stay untouched.\n\n" +
		"Do you want to remove the registration?"
}

// serviceGateInstruction returns the main instruction of the choice dialog of
// the service gate.
func serviceGateInstruction(prompt serviceChoicePrompt) string {
	if servicePromptRunning(prompt) {
		return "The HFDesk service is running."
	}
	return "The HFDesk service is installed, but it is not running."
}

// serviceGateContent returns the content of the choice dialog of the service
// gate.
//
// The dialog follows the rule of the family: the state (the instruction
// above), at most one line beside it — what the one continuing answer does
// for the positively confirmed stopped service, and that this launch starts
// no server for every other state — and the one surprising fact, the
// separate data, as its single line. The task dialog and the message box
// fallback share the content, so the two cannot tell the user different
// things. The buttons carry the actions; the text never explains why they
// are right.
func serviceGateContent(prompt serviceChoicePrompt) string {
	var text string
	if prompt.Detail != "" {
		text += prompt.Detail + "\n\n"
	}
	switch {
	case servicePromptRunning(prompt):
		if prompt.URL != "" {
			text += "Web UI: " + prompt.URL + "\n\n"
		}
	case servicePromptStopped(prompt):
		text += "Work in desktop mode starts this app's own server; the service stays stopped.\n\n"
	default:
		text += "This launch starts no server.\n\n"
	}
	return text + dataSeparationLine(prompt.DataDir)
}

// serviceGateText returns the whole text of the choice dialog of the service
// gate.
func serviceGateText(prompt serviceChoicePrompt) string {
	return serviceGateInstruction(prompt) + "\n\n" + serviceGateContent(prompt)
}

// serviceGatePendingText returns the text of the notice of a launch that found
// the service while it was starting or stopping.
//
// A pending state is not a state to decide on, so the launch offers neither
// Open nor Start. The notice names the transition and that the launch changed
// nothing — with the standard log-path line of every failure window of a
// launch that has a log, like the two notices beside it.
func serviceGatePendingText(state service.State) string {
	transition := "starting"
	if state == service.StateStopping {
		transition = "stopping"
	}
	return failureWindowText("The HFDesk service is still " + transition + " and did not reach a settled state in time.\n\n" +
		"This launch changes nothing: it starts no server.\n\n" +
		"Start the desktop app again when the service has settled.")
}

// serviceGateDiagnosticAction is the one action of the two notices that
// could not settle the state: the read of the state from an elevated shell,
// with its honesty boundary in the same sentence — a read that fails leaves
// the state unknown and this launch never guesses it, so the notice promises
// no relaunch that could show it again.
const serviceGateDiagnosticAction = `Run "hfdesk service status" from an elevated shell: ` +
	`a successful read settles the state and the desktop app can then be started again; ` +
	`a failed read leaves the state unknown and HFDesk will not guess it.`

// serviceGateUnreadableText returns the text of the notice of a launch that
// could not read the registration of the Windows service.
//
// The launch cannot tell a machine without a service from one whose service
// is installed, so it decides on neither: the notice is the state (the read
// failed), the one risk the user has to see (a service may be running, so
// this launch started no server), the cause on its own line and the one
// action that resolves the uncertainty — the read of the state — with the
// boundary of that promise in the same sentence.
func serviceGateUnreadableText(err error) string {
	text := "HFDesk could not read the state of the Windows service.\n\n"
	if err != nil {
		text += err.Error() + "\n\n"
	}
	return failureWindowText(text +
		"An HFDesk service may be installed and running on this machine, so this launch started no server.\n\n" +
		serviceGateDiagnosticAction)
}

// serviceGateChangedText returns the text of the notice of a launch that
// chose the desktop mode for a positively confirmed stopped service and then
// re-read a state that is no longer that.
//
// The dialog is modal and can outlive the snapshot it was built from, so the
// launch does not continue. The text is the state (the change and the state
// the re-read reported), the result (no server was started) and the same one
// action the unreadable notice carries, with the same boundary.
func serviceGateChangedText(prompt serviceChoicePrompt) string {
	return failureWindowText(
		"The state of the HFDesk service changed while the dialog was open.\n\n" +
			"It is " + serviceGateStatePhrase(prompt) + " now, so this launch did not continue into desktop mode; it started no server.\n\n" +
			serviceGateDiagnosticAction)
}

// serviceGateStatePhrase describes the state of a prompt for a notice of the
// gate. A prompt that is not the positively confirmed stopped one is named by
// the fact that decides it: the running fact first, then the diagnosed state.
func serviceGateStatePhrase(prompt serviceChoicePrompt) string {
	switch {
	case servicePromptRunning(prompt):
		return "running"
	case prompt.State == service.StateNotInstalled:
		return "no longer registered"
	case prompt.State == service.StateFailed:
		return "in a failed state"
	case prompt.State == service.StateBroken:
		return "reported as broken"
	case prompt.State == service.StateUnknown:
		return "in an unknown state"
	default:
		return string(prompt.State)
	}
}

// serviceGateOpenLabel returns the label of the open/start button of the
// choice dialog: the button that opens a running service and starts one that
// does not run. It is the first button of every dialog except the one of a
// positively confirmed stopped service, where the desktop mode comes first and
// this button names the start.
func serviceGateOpenLabel(prompt serviceChoicePrompt) string {
	if servicePromptRunning(prompt) {
		return "Open HFDesk"
	}
	return "Start service"
}

// The labels of the other buttons of the choice dialog.
const (
	// serviceGateDesktopLabel is the label of the button that continues the
	// launch into desktop mode. Only the dialog of a positively confirmed
	// stopped service has it, and it is that dialog's default button.
	serviceGateDesktopLabel = "Work in desktop mode"
	// serviceGateCompanionLabel is the label of the button that continues
	// the launch as a tray companion. Every dialog of the launch gate has
	// it, beside the answers the state offers; it is never the default
	// button, so a stray Enter keeps selecting the answer it selected
	// before. The wording follows serviceGateDesktopLabel: a short,
	// human sentence that names the mode the launch continues in.
	serviceGateCompanionLabel = "Work as tray companion"
	serviceGateUpdateLabel    = "Update service"
	serviceGateCancelLabel    = "Cancel"
)

// serviceGateFallbackChoices returns the mapping of the answers of the
// message box fallback onto the actions.
//
// A message box has no named buttons, so the text has to say what Yes, No and
// Cancel do. Without the mapping the user would have to guess which answer
// starts the service and which one updates it.
//
// The positively confirmed stopped service is asked in two boxes, so this is
// the mapping of its first box: the desktop mode, a service action and no
// action. The second box names the two service actions and is built by
// serviceGateStartUpdateChoices.
func serviceGateFallbackChoices(prompt serviceChoicePrompt) string {
	if servicePromptStopped(prompt) {
		return "Choose Yes to work in desktop mode.\n" +
			"Choose No to start or update the service.\n" +
			"Choose Cancel to change nothing."
	}
	if servicePromptRunning(prompt) {
		return "Choose Yes to open HFDesk in your default browser.\n" +
			"Choose No to update the service.\n" +
			"Choose Cancel to change nothing."
	}
	return serviceGateStartUpdateChoices()
}

// serviceGateStartUpdateChoices returns the mapping of the answers of the box
// that picks the service action: the same two service actions and no action
// the ordinary fallback of a service that does not run asks about. It is the
// second box of the stopped fallback and the whole mapping of every other
// dialog that does not run.
func serviceGateStartUpdateChoices() string {
	return "Choose Yes to start the service.\n" +
		"Choose No to update the service.\n" +
		"Choose Cancel to change nothing."
}

// serviceGateChooseText returns the text of the message box fallback of the
// choice dialog.
func serviceGateChooseText(prompt serviceChoicePrompt) string {
	return serviceGateText(prompt) + "\n\n" + serviceGateFallbackChoices(prompt)
}

// serviceGateServiceActionChooseText returns the text of the second message
// box of the stopped fallback: it names the state again and maps Yes, No and
// Cancel onto the two service actions and no action, so the box reads as a
// question of its own after the first box was dismissed.
func serviceGateServiceActionChooseText(prompt serviceChoicePrompt) string {
	return serviceGateInstruction(prompt) + "\n\n" + serviceGateStartUpdateChoices()
}

// serviceVersionName returns the product name with the build version of the
// desktop host that installed or updated the service.
//
// A local build carries no release version, so it is named without one instead
// of reporting "dev" to the user.
func serviceVersionName(version string) string {
	version = strings.TrimSpace(version)
	if version == "" || version == "dev" {
		return "HFDesk"
	}
	return "HFDesk " + version
}

// serviceInstalledTrayFallbackLine is the one conditional instruction of the
// report of a finished installation: the host of this session restarts as a
// tray companion, and one that leaves no tray icon is answered by this single
// human action. The user authorized exactly this one line (decision of
// 2026-11-23) for this report alone — the update and start reports never carry
// it — as the acknowledged exception to the report's one-next-step rule: it is
// conditional, so it is not a second next step beside the address instruction,
// and it names no mechanism behind the rare failure it covers.
const serviceInstalledTrayFallbackLine = "If the tray icon does not appear, start HFDesk manually."

// serviceInstalledText returns the report of a finished installation.
//
// The report names the program the registration runs, the address the service
// serves and the data directory it owns. The last sentence tells the user the
// consequence they need: the desktop app is not needed to use the service, and
// its own data stays where it is. Right after that consequence follows the one
// authorized conditional instruction (serviceInstalledTrayFallbackLine), which
// belongs to this report alone because only this action restarts the host as a
// tray companion. The helper runs only after the desktop process ended,
// so the report never tells the user to quit an app that is gone.
func serviceInstalledText(version, console, dataDir, address string) string {
	text := serviceVersionName(version) + " is installed as a Windows service.\n\n" +
		"The service runs " + console + ", starts at boot and keeps running after this window closes.\n\n"
	text += serviceAddressText(address)
	text += "The desktop app is not needed to use the service: open the address above.\n\n" +
		serviceInstalledTrayFallbackLine
	if dataDir != "" {
		text += "\n\nThe service keeps its own data in " + dataDir + ". The settings, jobs and models of the desktop app do not appear in the service."
	}
	return text
}

// serviceUpdatedText returns the report of a finished update.
//
// The report depends on the state the service had before the update: a service
// that was running is still running, and a service that did not run stays not
// running. The not-running case states the prior state the update actually
// observed (before): a registration that was stopped says so, and one that was
// failed, broken or in another state is named as what it was instead of being
// restated as stopped — a report claiming a state the machine did not have
// would send the user to a cause that is not there. Either way nothing serves
// HFDesk after such an update, and a message that claimed otherwise would send
// the user to a dead address.
//
// The not-running case names the way back to a serving service, and that way
// back depends on the surface the helper was launched from. An update is
// reachable from the menu and the dialog of a running desktop host, from the
// tray companion and from the launch gate: a menu, dialog or companion update
// leaves that host running, while a gate update runs after the launch that
// carried the gate already ended. Each surface is therefore told the route
// its user can actually reach — the tray companion gets its own Start service
// item (the companion serves no address, so that item starts the service
// immediately), and the desktop surface keeps the established route of this
// report (the relaunch of the desktop app and its Start service) byte for
// byte, as decided on 2026-11-23. A hand-typed helper command line carries
// no surface marker and gets the desktop route (launchSurfaceOf).
func serviceUpdatedText(version, console, dataDir, address string, running bool, before service.State, surface launchSurface) string {
	text := serviceVersionName(version) + " is updated as a Windows service.\n\n" +
		"The registration now runs " + console + " and keeps the settings of the service and its data.\n\n"
	if running {
		text += serviceAddressText(address)
		text += "The desktop app is not needed to use the service: open the address above."
	} else {
		wayBack := "Start the desktop app again and choose Start service."
		if surface == launchSurfaceCompanion {
			wayBack = serviceCompanionStartWayBack
		}
		text += serviceUpdateNotRunningReport(before) + "\n\n" + wayBack
	}
	if dataDir != "" {
		text += "\n\nThe service keeps its own data in " + dataDir + ". The settings, jobs and models of the desktop app do not appear in the service."
	}
	return text
}

// serviceUpdateNotRunningReport returns the result of the report of an
// update that did not leave the service running.
//
// The result is the outcome of the update: the service stays stopped, and
// nothing serves HFDesk. The not-running case also names the prior state the
// update actually observed (before): a registration that was stopped is
// reported by that fact alone, and one that was failed, broken or in another
// state is quoted as what it really was instead of being restated as
// "stopped" — a report claiming a state the machine did not have would send
// the user to a cause that is not there. The report argues with no rule
// beside it: what an update does or never does is not the user's business in
// the window; the state it left is.
func serviceUpdateNotRunningReport(before service.State) string {
	if before == service.StateStopped {
		return "The service stays stopped.\n\nNothing serves HFDesk right now."
	}
	return fmt.Sprintf("The service is not running after the update: it was %q before the update, and it stays stopped.\n\nNothing serves HFDesk right now.", before)
}

// serviceStartedText returns the report of a started service.
func serviceStartedText(console, address string) string {
	text := "The HFDesk service is running.\n\n"
	if console != "" {
		text += "It runs " + console + ".\n\n"
	}
	text += serviceAddressText(address)
	text += "The desktop app is not needed to use the service: open the address above."
	return text
}

// serviceCompanionStartWayBack is the one step back to a serving service
// both not-running reports name on the companion surface: the companion's
// own Start service item — the current menu item of that tray. The claim is
// true for this surface alone: the companion serves no address of its own,
// so the guard of the helper never refuses a start beside it and the item
// runs the start immediately. The sentence names no mechanism behind the
// state, like every way back of these reports.
const serviceCompanionStartWayBack = "Choose Start service from the tray menu."

// serviceStoppedText returns the report of a stopped service.
//
// The report is the state (stopped), the one fact the user did not ask for
// and has to see — nothing serves the address of the service right now — and
// the single way back. The way back depends on the surface the helper was
// launched from. The desktop route names the quit of this desktop and the
// startup gate of its next launch, because a start beside the desktop that
// serves its own address is refused by the established guard of the helper:
// a report that pointed at that start would send the user into that refusal
// in a circle. The companion route names the companion's own Start service
// item directly (serviceCompanionStartWayBack), and that is true for its
// surface: the companion serves no address of its own, so the guard of the
// helper never refuses that item and it starts the service immediately. A
// hand-typed helper command line carries no surface marker and gets the
// desktop route (launchSurfaceOf).
func serviceStoppedText(surface launchSurface) string {
	wayBack := "Quit HFDesk from the notification-area icon and choose Start service when you start the desktop app again."
	if surface == launchSurfaceCompanion {
		wayBack = serviceCompanionStartWayBack
	}
	return "The HFDesk service is stopped.\n\n" +
		"Nothing serves the address of the service right now.\n" + wayBack
}

// serviceRegistrationRemovedText returns the report of a removed service
// registration.
//
// The report is the state (the registration is gone), the invariant of the
// removal as its one line — the data stays, which is the surprising fact of
// this action — and the single way back. It never claims, hints or implies
// that any data was deleted: the removal writes no file, deletes no
// directory and reads no configuration beyond the registration itself.
func serviceRegistrationRemovedText() string {
	return "The Windows service registration of HFDesk is removed.\n\n" +
		"Your settings, jobs, models, cache, history and logs stay untouched.\n" +
		"Install the service again from the tray of the desktop app whenever you want it back."
}

// serviceNoRegistrationText returns the report of a removal that found no
// registration.
//
// The command line calls the same outcome a clean success, and this report
// keeps it a success without claiming a removal that never happened: the
// state (no registration) and the result of this attempt (nothing was
// changed) is all it says.
func serviceNoRegistrationText() string {
	return "HFDesk is not registered as a Windows service.\n\n" +
		"There was no registration to remove, so nothing was changed."
}

// serviceAddressText returns the sentence that names the address of the
// service.
//
// A registration whose listen address cannot be read as a URL still has an
// address, and naming it is better than leaving the report without one. A
// registration without any address is reported as such instead of naming an
// address that does not exist.
func serviceAddressText(address string) string {
	if address == "" {
		return "The registration names no listen address, so the address of the service is not known.\n\n"
	}
	return "The web UI of the service is at " + address + ".\n\n"
}

// serviceNotInstalledText returns the error of an action that needs a
// registration, for a machine without one.
func serviceNotInstalledText(action serviceAction) string {
	var there string
	switch action {
	case serviceActionUpdate:
		there = "There is no service to update."
	case serviceActionStop:
		// A settled absence needs no next action for a stop: there is
		// nothing to stop, and sending the user to the installation of
		// a service they asked to stop would answer a question they did
		// not ask. The state is settled, so the report names it alone.
		return failureWindowText("HFDesk is not installed as a Windows service.\n\n" +
			"There is no service to stop.")
	case serviceActionUninstall:
		// The removal gets its own honest form instead of the start of
		// the default branch: the state plus the absent object — there
		// is no registration to remove — and no next action either,
		// with the same settled-absence reasoning as the stop: sending
		// a user who asked to remove a registration to its installation
		// would answer a different question.
		return failureWindowText("HFDesk is not installed as a Windows service.\n\n" +
			"There is no service registration to remove.")
	default:
		there = "There is no service to start."
	}
	return failureWindowText("HFDesk is not installed as a Windows service.\n\n" +
		there + " Install it from the tray of the desktop app first.")
}

// The command a failure report of a service action ends with: a report is
// the result, the cause and one next action, so the command is written once
// here instead of being repeated by every report.
const (
	// serviceStatusCommandText is the next action of a report that cannot
	// say which state the action left: the state of the service has to be
	// read before anything else is decided.
	serviceStatusCommandText = `Run "hfdesk service status" from an elevated shell to read the current state of the service.`
)

// statusIfUnnamed returns the state command for a report whose detail does
// not name an action yet, and nothing when the detail already carries one —
// in whatever shape the service layer printed it: a report states one
// action, never the same route twice. The marker rule itself lives with the
// service layer (service.HasNamedAction), so both surfaces share it.
func statusIfUnnamed(detail string) string {
	if service.HasNamedAction(detail) {
		return ""
	}
	return "\n\n" + serviceStatusCommandText
}

// serviceActionBusyText returns the refusal of a second service action while
// one is already running.
//
// The report is the result (this action was not started), the cause on its
// single line (another action runs) and the one action that ends it (wait for
// that action and repeat this one). It names no state of the service: the
// running action is the one that moves it, and a state read beside it would
// describe a machine that is already changing.
func serviceActionBusyText() string {
	return failureWindowText("Another HFDesk service action is already running, so this action was not started.\n\n" +
		"Wait for the running action to finish, then try again.")
}

// isAlreadyRegisteredRefusal reports whether err is the refusal of a
// first-install attempt that found a registration it did not create.
//
// The service layer marks both refusals (at the read and before the write)
// with its exported error, so the classification is structural and survives a
// rewording of the manager messages.
func isAlreadyRegisteredRefusal(err error) bool {
	return errors.Is(err, service.ErrAlreadyRegistered)
}

// isServiceAccountRefusal reports whether err is the refusal of an update
// over a registration whose account is not the supported identity.
//
// The service layer marks the refusal with its exported error, so the
// classification is structural and survives a rewording of the manager
// message. The message of the refusal carries its whole report — the result,
// the cause and the command that restores the identity — so the report of
// the helper shows it as it is and adds no second next action.
func isServiceAccountRefusal(err error) bool {
	return errors.Is(err, service.ErrServiceAccountMismatch)
}

// serviceAlreadyRegisteredText returns the report of a first-install refusal of
// a registration the attempt did not create.
//
// The refusal happens before this attempt writes, so the report states that
// the registration already exists and that this attempt changed none of it: it
// never says that a registration may remain and never offers the removal of a
// registration this attempt never touched, because removing it would take away
// a service the user did not ask to remove here. The next action is the one
// this state offers: the startup gate of the next launch of the desktop app,
// whose choices cover opening or starting the service and updating the
// registration to the program this app runs from.
func serviceAlreadyRegisteredText(detail string) string {
	return "HFDesk could not install the Windows service.\n\n" + detail + "\n\n" +
		"The service registration already exists, and this attempt did not create, change or start it.\n\n" +
		"Start HFDesk again: its startup gate offers to open or start the service and to update it."
}

// serviceActionFailureHeadline returns the first line of the report of a
// failed service action: the action that failed, in the words every report of
// that action starts with.
func serviceActionFailureHeadline(action serviceAction) string {
	switch action {
	case serviceActionInstall:
		return "HFDesk could not install the Windows service."
	case serviceActionUpdate:
		return "HFDesk could not update the Windows service."
	case serviceActionStop:
		return "HFDesk could not stop the Windows service."
	case serviceActionUninstall:
		return "HFDesk could not remove the Windows service registration."
	default:
		return "HFDesk could not start the Windows service."
	}
}

// serviceSingletonHoldRefusedText returns the report of a mutation that
// refused before it changed anything because the per-session desktop
// singleton could not be held.
//
// The report states that no service action was performed — the hold is taken
// before the first change, so a failed hold touched nothing — and names the
// cause of the failed hold as its next part. A refusal because another
// Windows identity owns the object already names the step that ends that
// hold, so it carries the only next action of this report; every other cause
// ends with the command that reads the state, and no report carries both
// commands.
func serviceSingletonHoldRefusedText(action serviceAction, err error) string {
	detail := ""
	if err != nil {
		detail = err.Error()
	}
	text := serviceActionFailureHeadline(action) + "\n\n" +
		"No service action was performed: " + detail
	if !errors.Is(err, service.ErrSingletonAccessDenied) {
		text += statusIfUnnamed(detail)
	}
	return failureWindowText(text)
}

// serviceUnreadableClaimRefusedText returns the report of a mutation that
// refused over a published claim it could not read: the result (no service
// action was performed) and the cause (the state file that would tell
// whether a desktop serves cannot be read, so that question stays
// undetermined). The report carries no next action: no single step clears a
// file the guard cannot inspect, so it stays an honest terminal.
func serviceUnreadableClaimRefusedText(action serviceAction, claim string) string {
	return failureWindowText(serviceActionFailureHeadline(action) + "\n\n" +
		fmt.Sprintf("No service action was performed: the state file of the desktop at %q cannot be read, so whether a desktop of this session serves its local server cannot be determined.", claim))
}

// serviceServingDesktopRefusedText returns the report of a mutation that
// refused beside a serving desktop instance: the result (no service action
// was performed), the cause (the desktop serves its local server) and the
// one next action (quit the desktop, which ends the cause). The report
// carries no second command: reading the state of the service would not end
// the refusal.
func serviceServingDesktopRefusedText(action serviceAction) string {
	return failureWindowText(serviceActionFailureHeadline(action) + "\n\n" +
		"No service action was performed: a running HFDesk desktop instance of this session serves its local server.\n\n" +
		"Quit HFDesk from the notification-area icon and perform the action again.")
}

// serviceActionFailureText returns the report of a failed service action: the
// action that failed, the error of the service control manager and the one next
// action of the state the attempt left.
//
// It never claims that a registration was restored: a failure can happen after
// the registration was written, so an installation report names a registration
// that may remain. An installation whose service-owned first start failed is
// reported from the cleanup facts returned by the service manager instead. A
// first installation that refused an existing registration is reported as the
// unchanged state it left instead: that registration was never written by this
// attempt, so neither the may-remain warning nor the removal belongs to it.
func serviceActionFailureText(action serviceAction, err error, program string) string {
	return failureWindowText(serviceActionFailureBody(action, err, program))
}

// serviceActionFailureBody builds the report itself; the wrapper above adds
// the standard log-path line of every failure window exactly once.
func serviceActionFailureBody(action serviceAction, err error, program string) string {
	detail := ""
	if err != nil {
		detail = err.Error()
	}

	switch action {
	case serviceActionInstall:
		if isAlreadyRegisteredRefusal(err) {
			return serviceAlreadyRegisteredText(detail)
		}
		// The installation may have written a registration or not, and
		// the report does not know which: reading the state resolves
		// that uncertainty, so the state command is the one next action
		// — no conditional removal sits beside it, because whether there
		// is anything to remove is exactly what the read decides. A
		// manager text that already names the command keeps it as the
		// single action.
		return serviceActionFailureHeadline(action) + "\n\n" + detail + "\n\n" +
			"A registration may remain." + statusIfUnnamed(detail)
	case serviceActionUpdate:
		// The refusal of a changed account states its own report: the
		// result (nothing was changed), the cause (the account the
		// registration runs as) and the command that restores the
		// supported identity. Adding the state command here would give
		// the report a second next action.
		if isServiceAccountRefusal(err) {
			return serviceActionFailureHeadline(action) + "\n\n" + detail
		}
		return serviceActionFailureHeadline(action) + "\n\n" + detail + statusIfUnnamed(detail)
	case serviceActionStop, serviceActionUninstall:
		// The headline is the result, the manager text is the cause on
		// its single line, and the state command resolves what the
		// failure left — the shape of the update report, so no report of
		// these actions ever claims a state its action did not observe.
		return serviceActionFailureHeadline(action) + "\n\n" + detail + statusIfUnnamed(detail)
	default:
		return serviceActionFailureHeadline(action) + "\n\n" + detail + "\n\n" +
			"The start was not confirmed." + statusIfUnnamed(detail)
	}
}

// serviceInstallStartFailureText reports a first-install start failure from
// the cleanup facts returned by the service manager. Cleanup is performed
// through the creation handle, never by reopening a service name.
//
// The headline names the class of the start outcome: a start that was not
// performed, a program that ended before it was ready, a service that did
// not come up within the budget that elapsed, or a started service that
// could not be verified — and an error no site classified gets the honest
// state of a start that did not finish. No headline of this family ever
// claims a "could not confirm" vagueness again, not even the legacy shape.
//
// The report keeps the compressed form decided on 2026-11-23: the headline
// carries the class — and for the not-ready class the budget the failing
// wait actually applied, the only place any budget is spoken — while the
// ONE line below carries only the cause facts of the detail chain (the
// state word, the exit code and the like). The class is never repeated
// under its headline, so the class and the budget each occur exactly once.
// The kept-registration branch states the uncertainty the cleanup actually
// left — the registration may remain — and names the one action that
// resolves it: the read of the state. The removed branch never shares its
// wording with a machine that never had a registration: it says what this
// attempt removed, not that HFDesk was never installed.
//
// The cleanup fact itself is stated exactly once: the detail carries the
// cause and the one line the cleanup observed as a reason, never the
// service layer's own gloss of the cleanup outcome — that gloss and the
// outcome sentence below are two formulations of the same fact, and a
// report states it once.
func serviceInstallStartFailureText(err error, program string, cleanup *service.InstallStartError) string {
	detail := installStartDetail(err, cleanup)
	removed := cleanup != nil && cleanup.RegistrationRemoved
	text := serviceInstallStartHeadline(cleanup) + "\n\n" + detail + "\n\n"
	if removed {
		return failureWindowText(text + "The failed instance was settled and the registration created by this attempt was removed: this installation attempt left no registration behind, so start the desktop app normally again.")
	}
	outcome := "The registration was kept because cleanup could not prove that it was still owned by this attempt."
	if cleanup != nil && cleanup.RegistrationOwned {
		if cleanup.ProcessStopped {
			outcome = "The failed process was proven stopped, but the registration created by this attempt could not be removed."
		} else {
			outcome = "The registration created by this attempt was kept because its process was not proven stopped."
		}
	} else if cleanup != nil && cleanup.ProcessStopped {
		outcome = "The failed process was proven stopped, but the registration changed and was kept."
	}
	text += outcome
	return failureWindowText(text + statusIfUnnamed(detail))
}

// serviceInstallStartHeadline returns the headline of a failed first start:
// every classified class names itself, so the class — and for the not-ready
// class the budget — is spoken exactly once, in this headline. An error that
// no site classified — the empty Outcome of a record built without one, or a
// value no class owns — gets the honest state of a start that did not finish,
// with the cause following as the one line below. The budget is read only for
// the not-ready class and only from the field the failing wait stamped; a
// not-ready record without one names no number.
func serviceInstallStartHeadline(cleanup *service.InstallStartError) string {
	if cleanup != nil {
		switch cleanup.Outcome {
		case service.StartOutcomeNotPerformed:
			return "HFDesk registered the service but did not start it."
		case service.StartOutcomeEnded:
			return "HFDesk registered the service, but the service program ended before it was ready."
		case service.StartOutcomeNotReady:
			if cleanup.Budget > 0 {
				return fmt.Sprintf("HFDesk registered the service, but the service did not come up within %s.", cleanup.Budget)
			}
			return "HFDesk registered the service, but the service did not come up within the readiness budget."
		case service.StartOutcomeUnverified:
			return "HFDesk registered the service, but the started service could not be verified."
		}
	}
	return "HFDesk registered the service, but its start did not finish."
}

// installStartDetail returns the cause of a failed first start and the one
// line its cleanup observed as a reason — never the cleanup fact itself, and
// always as ONE line: the cause of the error is one line of the report, never
// two paragraphs the reader has to join. InstallStartError.Error states the
// cleanup fact in its own words ("(the registration was kept…)"); the report
// states the fact once, in its own outcome sentence, so the two formulations
// can no longer appear beside each other. An error the cleanup struct does
// not describe keeps its whole message as the detail it always was.
func installStartDetail(err error, cleanup *service.InstallStartError) string {
	if cleanup == nil || cleanup.Cause == nil {
		if err == nil {
			return ""
		}
		return err.Error()
	}
	detail := cleanup.Cause.Error()
	if cleanup.CleanupError != nil {
		detail += " — " + cleanup.CleanupError.Error()
	}
	return detail
}

// helperNotReadyError reports that the elevated helper was started but never
// became ready: it ended before its ready signal, or the signal never came
// within the budget, or it was gone right after the signal. The helper acts
// only after it becomes ready — the desktop commits its shutdown and the
// success signal on that evidence alone — so every cause of this error means
// that no service action was performed.
//
// The error is its own report class because the desktop cannot know the
// reason the helper itself failed: the helper shows that reason in its own
// message box before it ends, and a second box must reference that message
// instead of contradicting it or repeating what it says.
type helperNotReadyError struct {
	cause error
}

func (e *helperNotReadyError) Error() string {
	if e == nil || e.cause == nil {
		return "the service helper did not become ready"
	}
	return e.cause.Error()
}

func (e *helperNotReadyError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// serviceActionUnchangedText returns the result sentence of a report whose
// action never ran: the state the action did not reach, in the words every
// report of the unchanged service starts with.
func serviceActionUnchangedText(action serviceAction) string {
	switch action {
	case serviceActionInstall:
		return "The service was not installed."
	case serviceActionUpdate:
		return "The service was not updated."
	case serviceActionStop:
		return "The service was not stopped."
	case serviceActionUninstall:
		return "The registration was not removed."
	default:
		return "The service was not started."
	}
}

// serviceHelperNotReadyText returns the report of a helper that started but
// never became ready.
//
// The report is one coherent message for this state: it states the result
// (nothing was performed), carries the observed cause verbatim, and names
// one next action — the message the helper shows itself, which is where the
// user reads the actual reason and the step that follows from it. It never
// claims that the helper could not be started — it was started — and it
// never repeats the reason the helper reports in its own box.
func serviceHelperNotReadyText(action serviceAction, err error) string {
	detail := ""
	if err != nil {
		detail = err.Error()
	}
	return failureWindowText(serviceActionUnchangedText(action) +
		" No service action was performed.\n\n" +
		detail + "\n\n" +
		"Read the message the administrator helper showed: it names the reason and the step that follows.")
}

// serviceLaunchFailedText returns the report of a failed launch of the elevated
// helper.
//
// The desktop host reports the failure itself, because the helper never ran:
// the helper is the only surface that reports the outcome of an action it
// performed. A refused request for administrator rights and a helper that
// could not be started both end here, before anything was changed, and both
// share the one next action: repeat the action and approve the elevation when
// Windows asks for it — the approval is what a declined prompt lacked, and a
// repeat is what a shell that could not start the helper needs. A cause that
// keeps the prompt from ever appearing is stated on its own line by the
// detail; the report argues with no policy examples beside it. A helper that
// was started but never became ready is a different state and has its own
// report (serviceHelperNotReadyText).
func serviceLaunchFailedText(action serviceAction, err error) string {
	detail := ""
	if err != nil {
		detail = err.Error()
	}

	return failureWindowText("HFDesk could not start its administrator helper.\n\n" + detail + "\n\n" +
		serviceActionUnchangedText(action) +
		" Repeat the action and approve the administrator prompt when Windows shows it.")
}

// The labels of the missing-program notice.
const (
	// siblingMissingDownloadLabel is the primary action: the command
	// link that fetches the paired console program and continues the
	// service action the user invoked.
	siblingMissingDownloadLabel = "Download it and continue"
	// siblingMissingDismissLabel dismisses the notice and changes
	// nothing.
	siblingMissingDismissLabel = "Not now"
	// siblingMissingManualLabel is the collapsed expander that carries
	// the instructions for putting the file in place by hand.
	siblingMissingManualLabel = "Install it manually instead"
)

// siblingMissingNotice is the copy of the notice a service action shows when
// its console program is missing.
//
// The fields are the parts of the redesigned TaskDialog layout: the bold
// instruction names the file, the first-glance content states what the
// program is for and the folder it is not in, the download offer — nil when
// no verified download can be offered — carries the release facts the command
// link shows, and the manual text is what the collapsed expander and the
// message box fallback carry as the way without a download. Action is the
// service action the user invoked, so the copy that continues after a
// verified download names that action and never another one. The copy is
// built without any dialog or network in it, so a test can pin the wording.
type siblingMissingNotice struct {
	// Action is the service action that continues after a verified
	// download: install or update.
	Action serviceAction
	// Title is the window title of the notice.
	Title string
	// Instruction is the bold main line: the missing file, named.
	Instruction string
	// Content is the first-glance sentence under it.
	Content string
	// Folder is the folder the lookup looked in and the manual steps
	// point at.
	Folder string
	// Download is the resolved offer of the paired release asset, or nil
	// when only the manual instructions are shown.
	Download *consoleDownloadOffer
	// Manual is the text of the manual path.
	Manual string
}

// serviceActionNoticePhrases returns how the notice names the action that
// continues after a verified download: the modifier before "steps" in the
// note line of the command link, and the noun after "continue with" in the
// message box fallback.
//
// Only the two actions that register from the folder — install and update —
// reach this notice, and each names the action the user actually invoked, in
// the words the reports of that same action already use (the
// serviceActionFailureHeadline family): the note line of an update must not
// promise the install steps. A start resolves no console program and never
// gets here; the default keeps the install spelling instead of inventing a
// phrase for an action that cannot occur.
func serviceActionNoticePhrases(action serviceAction) (steps, continueWith string) {
	switch action {
	case serviceActionUpdate:
		return "update", "the update"
	default:
		return "install", "the installation"
	}
}

// newSiblingMissingNotice builds the notice of a failed lookup of the
// console program for the service action that asked for it.
//
// The instruction always names the file the action expected — the pair of a
// version-stamped program, or hfdesk.exe for a local build — and the content
// states the folder it is not in. The one failure whose cause stays out of
// the first glance is the pairing failure of a version-stamped program
// (marked with errConsolePairMissing): its explanation is the pairing rule
// the collapsed expander already states. Every other failure of the lookup —
// an unreadable folder, no candidate, several candidates — keeps its cause
// beside the first sentence, because that cause is what the manual steps
// have to deal with.
func newSiblingMissingNotice(action serviceAction, folder, desktop string, findErr error, offer *consoleDownloadOffer) siblingMissingNotice {
	missing := pairedConsoleExecutableName(desktop)
	if missing == "" {
		missing = consoleExecutableName + executableSuffix
	}
	notice := siblingMissingNotice{
		Action:      action,
		Title:       serviceSiblingMissingCaption,
		Instruction: missing + " is missing",
		Content:     missingProgramContent(folder),
		Folder:      folder,
		Download:    offer,
		Manual:      siblingMissingManualText(folder, desktop, missing),
	}
	if findErr != nil && !errors.Is(findErr, errConsolePairMissing) {
		notice.Content += "\n\n" + findErr.Error()
	}
	// The missing program is the failure that blocked the action, and the
	// lookup wrote its cause to the log: the notice carries the standard
	// line that makes that log reachable from outside the window.
	notice.Content = failureWindowText(notice.Content)
	return notice
}

// missingProgramContent returns the first-glance sentence of the notice: what
// the program is for and the folder it is not in.
func missingProgramContent(folder string) string {
	where := folder
	if where == "" {
		where = "the folder of this desktop app"
	}
	return "The Windows service runs this program, and it is not in " + where + "."
}

// siblingMissingManualText returns the text of the manual path: how to get
// the file without a download and where to put it.
//
// A desktop program that names its release gets the release to fetch and the
// pairing rule that makes the two programs belong in one folder; a local
// build names no release, so its text only states where the file goes. Both
// end with the one action that follows: run the action again.
func siblingMissingManualText(folder, desktop, missing string) string {
	where := folder
	if where == "" {
		where = "the folder of this desktop app"
	}
	release, namesRelease := consoleReleaseFor(desktop)
	if namesRelease {
		return "Get release " + release.Tag + " from " + releaseTagPagePrefix + release.Tag +
			", put " + missing + " next to hfdesk-desktop.exe in " + where +
			", and run the action again.\n\n" +
			"A desktop program of a release only runs the console program of that same release, " +
			"so the release ships " + missing + " beside hfdesk-desktop.exe and both belong in one folder."
	}
	return "Put " + missing + " next to hfdesk-desktop.exe in " + where +
		" — a release of HFDesk ships both programs in one folder — and run the action again."
}

// siblingMissingDownloadNote returns the note line under the download command
// link: the size the release publishes, the release, and what the click leads
// to — the checksum check and then the ordinary steps of the action that
// asked for the program, named per action.
func siblingMissingDownloadNote(notice siblingMissingNotice) string {
	offer := notice.Download
	steps, _ := serviceActionNoticePhrases(notice.Action)
	return consoleSizeText(offer.Size) + " from the official release " + offer.Release.Tag +
		" · SHA-256 checked · then the usual " + steps + " steps"
}

// siblingMissingNoticeForFallback returns the notice the message box
// fallback shows when the task dialog of the notice could not be shown.
//
// dialogErr is the failure of that attempt. The offer is only kept when the
// transfer behind it can actually run, and the transfer shows its progress
// bar and its Cancel in a task dialog of this same machine moments later: no
// failure of the attempt proves that the next task dialog can be shown —
// whatever made it fail counts exactly like a machine without a progress
// dialog — so every failure drops the offer and the box degrades to
// manual-only instead of asking a Yes that could only end in the "nothing
// was downloaded" report. An action that cannot proceed is never offered,
// the same way a release without an authoritative checksum never offers one.
// Only an attempt that did not fail keeps the notice as it is; the notice
// flow reaches this fallback only when one did fail. The instruction, the
// content and the manual path stay exactly as they are, so the manual-only
// box still names the file, the folder and the way without a download.
func siblingMissingNoticeForFallback(notice siblingMissingNotice, dialogErr error) siblingMissingNotice {
	if dialogErr == nil {
		return notice
	}
	notice.Download = nil
	return notice
}

// siblingMissingManualNoticeText returns the whole text of the manual-only
// notice for the message box fallback: instruction, content and the manual
// path, in one box.
func siblingMissingManualNoticeText(notice siblingMissingNotice) string {
	return notice.Instruction + "\n\n" + notice.Content + "\n\n" + notice.Manual
}

// siblingMissingChooseText returns the Yes/No text of the message box of a
// notice that offers the download. The fallback of the notice flow itself is
// manual-only — siblingMissingNoticeForFallback drops the offer after the
// failed task dialog attempt — so this text belongs to an offer-bearing
// notice reaching the box, which the box and its tests keep.
//
// A message box has no named buttons, so the text maps Yes and No onto the
// two choices: the verified download that continues, and the manual path that
// changes nothing. The continuation is named per action, so the box of an
// update says update and never installation. No is the default button, so a
// stray Enter never starts a download — the same rule the confirmation of
// the installation follows.
func siblingMissingChooseText(notice siblingMissingNotice) string {
	offer := notice.Download
	_, continueWith := serviceActionNoticePhrases(notice.Action)
	return notice.Instruction + "\n\n" + notice.Content + "\n\n" +
		"Choose Yes to download " + offer.Release.Asset + " from the official release " + offer.Release.Tag +
		", check its SHA-256, and continue with " + continueWith + ".\n" +
		"Choose No to change nothing and put the file there yourself:\n\n" +
		notice.Manual
}

// consoleDownloadProgressText returns the instruction and the content of the
// progress dialog of the transfer: what is being fetched, and where from —
// with the check that runs before the file is used.
func consoleDownloadProgressText(offer *consoleDownloadOffer) (instruction, content string) {
	return "Downloading " + offer.Release.Asset + "…",
		consoleSizeText(offer.Size) + " from the official release " + offer.Release.Tag +
			" · SHA-256 is checked before the file is used"
}

// consoleDownloadFailedText returns the report of a download that did not
// finish or did not verify: the result, the cause and the one next action —
// the manual path.
//
// The report states only what the mechanism guarantees: the clean line says
// that this download left no file of its own behind — true whether nothing
// was ever created or cleanup removed what was — and claims nothing about
// files the folder already held or the user placed beside it; a file whose
// removal failed is named with its path instead of being covered by that
// line, because a report that hides a leftover sends the user looking for a
// file the report said was not there. Neither leftover is called "partial":
// the leftover of a cancelled transfer is the complete program the transfer
// placed, so only the guaranteed facts — a file remains there, and it has to
// be deleted — are reported.
func consoleDownloadFailedText(notice siblingMissingNotice, downloadErr error) string {
	offer := notice.Download
	text := "HFDesk could not download " + offer.Release.Asset + ".\n\n" + downloadErr.Error() + "\n\n"
	var failure *consoleDownloadError
	if errors.As(downloadErr, &failure) && failure.leftover != "" {
		text += "A file remains at " + failure.leftover + " and could not be removed: delete it yourself.\n\n"
	} else {
		text += "This download left no file behind in " + notice.Folder + ".\n\n"
	}
	return failureWindowText(text + notice.Manual)
}

// serviceHandoverRefusedText returns the report of a helper that could not
// prepare its coordination with the launching desktop: the result (no
// service action was performed), the observed cause on its own line and the
// one command that reads the state — kept as the single action by the same
// rule every report follows (a cause that already names the command keeps it
// as the only one). The standard log-path line of every failure window closes
// it.
func serviceHandoverRefusedText(err error) string {
	detail := ""
	if err != nil {
		detail = err.Error()
	}
	return failureWindowText("HFDesk could not prepare the service action.\n\n" +
		"No service action was performed.\n\n" + detail + statusIfUnnamed(detail))
}

// serviceHandoverFailureText returns the report of the desktop host whose
// installation could not be finished after its shutdown: the result (the
// installation did not finish), the cause on its own line, the one command
// that reads the state the failure left, and the standard log-path line —
// with no description of the mechanism the wait used.
func serviceHandoverFailureText(err error) string {
	detail := ""
	if err != nil {
		detail = err.Error()
	}
	return failureWindowText("HFDesk could not finish the service installation.\n\n" +
		detail + "\n\n" + serviceStatusCommandText)
}

// unknownServiceActionText returns the report of an action the helper does not
// know: the result, the cause as observed, and the one action that leaves the
// next launch with arguments the helper knows — a normal start of the desktop
// app.
func unknownServiceActionText(err error) string {
	detail := ""
	if err != nil {
		detail = err.Error()
	}
	return failureWindowText(fmt.Sprintf("HFDesk was started with a service action it does not know.\n\n%s\n\n"+
		"Nothing was installed, updated or started. Start the desktop app again.", detail))
}
