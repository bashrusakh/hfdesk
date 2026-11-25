// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package desktop

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

// logFileLineMarker is the standard line every failure window carries about
// the log file of this launch. It is a marker as well as a sentence: the
// renderer that turns the line into an action (splitLogFileLine) reads the
// path back out of it, so the window and the action always name one file.
const logFileLineMarker = "\n\nThe log file is at "

// logFileOpenLabel is the command link that opens that file, in the notice of
// the missing console program and in every failure window that names a log.
const logFileOpenLabel = "Open the log file"

// logFileLine returns the one standard line every failure window carries:
// the path of the log file of this launch, so the log is reachable from
// outside the window. A launch without a log file of its own names none —
// the line is empty instead of pointing at a file this run does not write.
func logFileLine(path string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	return logFileLineMarker + path + "."
}

// splitLogFileLine is the exact inverse of logFileLine: it splits the standard
// log-path line off the text of a failure window and returns the body of the
// window together with the path the line names — empty when the text carries
// no complete line, which is every window of a launch without a log file of
// its own. A text that merely mentions the marker without ending in a path
// keeps its whole body, so nothing is ever opened on a guess.
func splitLogFileLine(text string) (body, path string) {
	index := strings.LastIndex(text, logFileLineMarker)
	if index < 0 {
		return text, ""
	}
	tail := text[index+len(logFileLineMarker):]
	if !strings.HasSuffix(tail, ".") {
		return text, ""
	}
	path = strings.TrimSuffix(tail, ".")
	if strings.TrimSpace(path) == "" {
		return text, ""
	}
	return text[:index], path
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

// serviceCauseLine returns the one factual line a failure window states as the
// cause of a service-level failure: the message of the service layer with its
// internal markers cut off (see serviceCauseFirstLine), reduced to its first
// line and cut before the first clause that appends an instruction, an
// argument or an internal command. The whole message stays in the log the
// window names.
//
// This is the primary-fact form: it keeps the fact the report class states
// about itself (the singleton cannot be named, the registration runs as another
// account) and drops everything the service layer appends to explain or repair
// it. A report whose cause IS the observed reason uses serviceReasonLine, which
// keeps the colon that names that reason.
func serviceCauseLine(detail string) string {
	detail = serviceCauseFirstLine(detail)
	if detail == "" {
		return ""
	}
	for _, separator := range []string{"; ", ". ", ": "} {
		if index := strings.Index(detail, separator); index >= 0 {
			detail = detail[:index]
		}
	}
	return strings.TrimSpace(strings.TrimRight(detail, " \t.,;:"))
}

// serviceReasonLine returns the cause line of a report whose one cause IS the
// observed reason the service layer wrapped: an image the host cannot execute,
// the console program that is missing, the wait that ended.
//
// It strips the same internal prefixes and keeps the message to one line, but
// it drops only a second sentence and a clause that appends an instruction —
// the service layer's repair step, often introduced by a colon — so the
// observed reason itself survives. A colon is kept: in these messages it
// introduces that reason — "is not a Windows executable: no MZ signature",
// "the console program ... is missing: there is no hfdesk.exe in ...",
// "shutdown HTTP server: context deadline exceeded" — and that reason is the
// fact the window exists for. The whole message stays in the log the window
// names.
func serviceReasonLine(detail string) string {
	detail = serviceCauseFirstLine(detail)
	if detail == "" {
		return ""
	}
	for _, separator := range []string{
		"; ", ". ",
		// Instruction clauses the service layer appends with a colon
		// instead of a sentence: "…: grant the account execute access,
		// then run …". The reason before the clause stays.
		": grant the account ", ": run ", ": use ", ": move ", ": restore ",
		", then run ", ", run ",
	} {
		if index := strings.Index(detail, separator); index >= 0 {
			detail = detail[:index]
		}
	}
	return strings.TrimSpace(strings.TrimRight(detail, " \t.,;:"))
}

// serviceCauseFirstLine reduces a service-layer message to its first line with
// the internal prefixes of the layer removed. Both prefixes are structural
// markers ("service: " of the manager, "desktop: " of the host's own errors),
// never user-facing text, so every cause form drops them before it decides
// where the fact ends.
func serviceCauseFirstLine(detail string) string {
	detail = strings.TrimSpace(detail)
	if detail == "" {
		return ""
	}
	if index := strings.IndexByte(detail, '\n'); index >= 0 {
		detail = detail[:index]
	}
	for {
		previous := detail
		for _, prefix := range []string{"service: ", "desktop: "} {
			detail = strings.TrimPrefix(detail, prefix)
		}
		if detail == previous {
			return detail
		}
	}
}

// serviceFailureReport joins the parts of a failure report — the headline, the
// cause, an outcome line and at most one action — and drops the empty ones, so
// a report without a cause never prints a blank paragraph.
func serviceFailureReport(parts ...string) string {
	kept := parts[:0]
	for _, part := range parts {
		if strings.TrimSpace(part) != "" {
			kept = append(kept, part)
		}
	}
	return strings.Join(kept, "\n\n")
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

	// startServiceConfirmCaption is the caption of the confirmation of a
	// start that hands this host over: the one start that closes the
	// window the user clicked in, so the fact is read before the
	// elevation prompt follows. A start that does not hand over has
	// no confirmation and never shows this caption.
	startServiceConfirmCaption = "HFDesk - start the Windows service"

	// updateServiceConfirmCaption is the caption of the confirmation of
	// an update: the handover form — the counterpart of the start
	// caption above, for the same reason — and the form of an update
	// that stops and starts a running service. An update over a settled
	// registration that does not run has no confirmation and never
	// shows this caption.
	updateServiceConfirmCaption = "HFDesk - update the Windows service"

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
// The confirmation is the question as its headline and exactly two fact
// lines (decision of 2026-11-24: the text is cut to this and nothing else) —
// the lifetime of the service (starts at boot, survives this window) and the
// one surprising fact, the separate data, named by its directory alone. The
// portability, one-file download and SHA-256 facts the earlier text carried
// are released from this window (decision of 2026-11-24): they live in the
// download dialog, where they are needed, and this window states none of
// them. It carries no administrator-rights or prompt sentence: the Windows
// prompt that follows states that itself (decision of 2026-11-24). dataDir is
// the state directory of the service, so the text names the directory it
// will really own; an empty one keeps the fallback line that states the
// separation with no directory to name.
//
// unusualDrive adds the B2 warning as its own single line between the
// lifetime line and the data line: it is present exactly when the program
// the registration runs sits on a removable or network drive, and a program
// on an ordinary drive changes nothing.
func installServiceText(dataDir string, unusualDrive bool) string {
	text := "Install HFDesk as a Windows service?\n\n" +
		"It starts at boot and keeps running after this window closes.\n"
	if unusualDrive {
		text += serviceProgramDriveWarningLine + "\n"
	}
	if dataDir != "" {
		return text + "Its data is separate: " + dataDir + "."
	}
	return text + dataSeparationLine(dataDir)
}

// stopServiceConfirmText returns the text of the confirmation that stops the
// Windows service before the administrator prompt follows.
//
// The confirmation follows the shape of the installation confirmation: what
// the action does, the one fact the user has to see before agreeing —
// nothing serves the address of the service afterwards — and the question,
// with no administrator-rights or prompt sentence (decision of 2026-11-24:
// the Windows prompt that follows states that itself). It names no state of
// its own: the state the menu item was enabled by can be stale by the time
// the box is read, and a confirmation that claimed a state the machine no
// longer has would ask the user to agree to something that is not there. The
// helper reads the state again and reports what it really found.
func stopServiceConfirmText() string {
	return "This stops the HFDesk service.\n\n" +
		"Nothing serves the address of the service until you start it again.\n\n" +
		"Do you want to stop the service?"
}

// startServiceConfirmText returns the text of the confirmation of a start
// that hands this host over — the start of a desktop host that serves its own
// local server, which has to close before the service can take the address.
//
// The text is the question and the one fact the user has to read before
// agreeing — this window closes into the tray and stays there — and nothing
// else: it repeats no administrator-rights or prompt sentence, because the
// Windows prompt that follows states that itself (decision of
// 2026-11-24). It names no state of the service for the same reason the
// stop confirmation names none: the state the menu item was enabled by can be
// stale by the time the box is read, and the helper reads it again.
func startServiceConfirmText() string {
	return "Start the HFDesk service?\n\n" +
		"This window closes into the tray and stays there."
}

// updateServiceConfirmText returns the text of the confirmation of an update
// that hands this host over. Its shape and its honesty boundary are the ones
// startServiceConfirmText documents.
func updateServiceConfirmText() string {
	return "Update the HFDesk service?\n\n" +
		"This window closes into the tray and stays there."
}

// updateRunningServiceConfirmText returns the text of the confirmation of an
// update that does not hand this host over but moves a running instance: the
// helper stops the service and starts it again, so the user reads that
// interruption before agreeing. It follows the shape of the family — the
// question, then the one line of facts and nothing else — and it repeats no
// administrator-rights or prompt sentence, because the Windows prompt that
// follows states that itself (decision of 2026-11-24).
func updateRunningServiceConfirmText() string {
	return "Update the Windows service?\n\n" +
		"The service is stopped and started again. Running downloads are interrupted."
}

// removeServiceConfirmText returns the text of the confirmation that removes
// the Windows service registration before the administrator prompt follows.
//
// The text carries no administrator-rights or prompt sentence — the Windows
// prompt that follows states that itself (decision of 2026-11-24) — and the
// confirmation claims nothing about the state of the instance: a running one
// is stopped inside the removal, which the user does not have to orchestrate.
// The one line that states that action says it as the fact it is — a running
// service is stopped first — without claiming the instance runs now: the
// state the menu item was enabled by can be stale by the time the box is read.
//
// handover picks between the two forms of that decision: the handover form is
// the question plus the facts of this action — the running service is stopped
// first, the data stays, and this window closes and comes back in desktop mode
// once the registration is gone — while every surface that does not hand over
// (the stop's sibling, and a removal beside a host that serves no address)
// reads the shorter question of the family, with the same fact lines and no
// rights sentence.
func removeServiceConfirmText(handover bool) string {
	if handover {
		return "Remove the HFDesk service registration?\n\n" +
			"A running service is stopped first.\n" +
			"Your data stays untouched. This window closes, and HFDesk starts again in desktop mode."
	}
	return "This removes the Windows service registration of HFDesk.\n\n" +
		"A running service is stopped first.\n" +
		"Your data stays untouched.\n\n" +
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
		"This launch starts no server.\n\n" +
		"Start the desktop app again when the service has settled.")
}

// serviceGateDiagnosticAction is the one action of the two notices that
// could not settle the state: the read of the state from an elevated shell.
const serviceGateDiagnosticAction = `Run "hfdesk service status" from an elevated shell.`

// serviceGateUnreadableText returns the text of the notice of a launch that
// could not read the registration of the Windows service.
//
// The launch cannot tell a machine without a service from one whose service
// is installed, so it decides on neither: the notice is the state (the read
// failed), the result (this launch started no server), the cause on its own
// line and the one action that resolves the uncertainty — the read of the
// state.
func serviceGateUnreadableText(err error) string {
	text := "HFDesk could not read the state of the Windows service.\n\n"
	if err != nil {
		text += err.Error() + "\n\n"
	}
	return failureWindowText(text +
		"This launch starts no server.\n\n" +
		serviceGateDiagnosticAction)
}

// serviceGateChangedText returns the text of the notice of a launch that
// chose the desktop mode for a positively confirmed stopped service and then
// re-read a state that is no longer that.
//
// The dialog is modal and can outlive the snapshot it was built from, so the
// launch does not continue. The text is the state (the change and the state
// the re-read reported), the result (no server was started) and the same one
// action the unreadable notice carries.
func serviceGateChangedText(prompt serviceChoicePrompt) string {
	return failureWindowText(
		"The state of the HFDesk service changed while the dialog was open.\n\n" +
			"It is " + serviceGateStatePhrase(prompt) + " now. This launch starts no server.\n\n" +
			serviceGateDiagnosticAction)
}

// serviceGateStatePhrase describes the state of a prompt for a notice of the
// gate. A pending state is named by the transition it is in — a window that
// shows it beside an address that did not answer must not claim it runs —
// and every other prompt is named by the fact that decides it: the running
// fact first, then the diagnosed state.
func serviceGateStatePhrase(prompt serviceChoicePrompt) string {
	switch {
	case prompt.State == service.StateStarting:
		return "starting"
	case prompt.State == service.StateStopping:
		return "stopping"
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

// serviceInstalledText returns the report of a finished installation.
//
// The report is three short blocks and no argument: what was installed and
// what it runs, the address it serves, and the directory the service keeps its
// own data in. It carries no instruction: a success report states the result
// and its facts and names nothing to do next (decision of 2026-11-25). The
// lifetime of the service is stated once, in the confirmation that asked for
// the installation; this report does not repeat it. The helper runs only after
// the desktop process ended, so the report never tells the user to quit an app
// that is gone.
func serviceInstalledText(version, console, dataDir, address string) string {
	text := serviceVersionName(version) + " is installed as a Windows service.\n" +
		"The service runs " + console + " and starts at boot.\n\n" +
		serviceAddressText(address)
	if dataDir != "" {
		text += "\n\nThe service keeps its own data in " + dataDir + "."
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
// the address of the service after such an update, and a message that claimed
// otherwise would send the user to a dead address. The report names no way back
// to a serving service: a success report carries the result and its facts and
// no instruction (decision of 2026-11-25).
//
// surface is the launch surface the helper was started from. It stays in the
// signature with the launch-surface plumbing that carries it to the helper and
// no longer changes this report, which names no surface-dependent route.
//
// oldFile is the program the registration ran BEFORE this update, and only
// when that file still exists somewhere else: an update that moved the
// registration to another file then states the fact right after the program
// the registration runs now — the old file is still on the disk, stated as
// the single fact line of serviceUpdateOldFileLines and nothing behind it,
// with no assumption about the file — and its window carries the one command
// that deletes exactly that file on a deliberate press (deleteOldFileLabel).
// An update that kept the same path, one whose old file is already gone and
// one that failed (a rollback puts the registration back on the previous
// program) pass the empty string, and their report is byte for byte the
// report this function always built.
func serviceUpdatedText(version, console, dataDir, address string, running bool, before service.State,
	surface launchSurface, oldFile string) string {
	text := serviceVersionName(version) + " is updated as a Windows service.\n" +
		"The registration now runs " + console + " — settings and data kept.\n"
	if oldFile != "" {
		text += serviceUpdateOldFileLines(oldFile) + "\n"
	}
	if running {
		text += "\n" + serviceAddressText(address)
	} else {
		text += "\n" + serviceUpdateNotRunningReport(before)
	}
	if dataDir != "" {
		text += "\n\nThe service keeps its own data in " + dataDir + "."
	}
	return text
}

// serviceUpdateOldFileLines returns the one line the report of a finished
// update states beside the program the registration runs now when the
// registration MOVED to another file and the old one is still there: exactly
// that file and the fact that it is still on the disk.
//
// The line states the fact and nothing behind it: no assumption about the
// file, no mechanism, no instruction — the command that acts on the fact is
// the button of the window, not a sentence in this text (decisions of
// 2026-11-24: reports carry facts, never guesses).
func serviceUpdateOldFileLines(oldFile string) string {
	return "Old file still on disk: " + oldFile + "."
}

// serviceUpdateOldFile decides what the report of a finished update says
// about the program the registration ran BEFORE it.
//
// It returns that path only when the registration really moved to ANOTHER
// file and the old one is still on the disk — the two conditions the report's
// command needs to name a real object: the same path (an update that
// re-registered in place, and every rollback, which puts the registration
// back on the previous program) names nothing, and a path that cannot be
// read any more is not an object a command could delete. Every other answer
// is the empty string, and the report carries no mention of an old file at
// all.
func serviceUpdateOldFile(previous, current string) string {
	previous = strings.TrimSpace(previous)
	if previous == "" {
		return ""
	}
	if strings.EqualFold(filepath.Clean(previous), filepath.Clean(strings.TrimSpace(current))) {
		return ""
	}
	if _, err := os.Stat(previous); err != nil {
		return ""
	}
	return previous
}

// deleteOldFileLabel is the command the report of a finished update carries
// when the registration moved to another file and the old one is still
// there. It is the label of the command link of that window; the message box
// fallback of a machine without a task dialog maps the same command onto Yes
// in its own words (deleteOldFileChoiceText), so one wording names the one
// action everywhere it can be pressed.
const deleteOldFileLabel = "Delete the old file"

// removeOldServiceProgram deletes the old registered program.
//
// It is a package level variable so the tests can prove WHICH path the
// command of the report hands over — and that no outcome of an update ever
// reaches it by itself: only the press of the report's command calls it
// (showReportWithOldFileCommand), and no other flow of this package deletes
// a program file.
var removeOldServiceProgram = os.Remove

// The captions of the two outcomes of the report's command.
const (
	// oldFileDeletedCaption is the caption of the fact that the old
	// registered program is gone.
	oldFileDeletedCaption = "HFDesk - the old file was deleted"
	// oldFileDeleteFailedCaption is the caption of the honest failure of
	// that command — a file another program holds, and a file this
	// identity may not write.
	oldFileDeleteFailedCaption = "HFDesk - the old file could not be deleted"
)

// runDeleteOldFileCommand performs the explicit command of the report of a
// finished update and returns the window it shows: the fact of the deletion
// with the path it deleted on the same line, and — when Windows refuses the
// deletion — the honest failure with the cause on its own line beside that
// same path, which is the fact line a busy or unwritable file gives. failed
// picks the icon of that window.
//
// target is exactly the path the report named: the program the registration
// ran before this update. The function holds no path of its own and adds no
// second object — nothing beside that one file is ever touched — and it
// records nothing: the dialog that owns the press records the outcome beside
// the window it then shows.
func runDeleteOldFileCommand(target string) (caption, text string, failed bool) {
	if err := removeOldServiceProgram(target); err != nil {
		return oldFileDeleteFailedCaption,
			failureWindowText("The old file could not be deleted.\n\n" + err.Error() +
				"\n\nThe file is at " + target + "."),
			true
	}
	return oldFileDeletedCaption, "The old file was deleted: " + target + ".", false
}

// deleteOldFileChoiceText maps the answers of the message box fallback onto
// the one command of the report: the deletion of exactly the file the report
// named, and keeping it. No is the default button, so a stray Enter keeps the
// file — the same rule every confirmation of this family follows, and the
// reason the command really needs a deliberate press on a machine whose
// common controls cannot show the command link.
func deleteOldFileChoiceText(target string) string {
	return "Choose Yes to delete the old file:\n" + target + "\n" +
		"Choose No to keep it."
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
		return "The service stays stopped.\nNothing serves the address of the service right now."
	}
	return fmt.Sprintf("The service is not running after the update: it was %q before the update and stays stopped.\nNothing serves the address of the service right now.", before)
}

// serviceStartedText returns the report of a started service.
//
// Two short blocks: the state and the program it runs, then the address it
// serves. It carries no instruction (decision of 2026-11-25).
func serviceStartedText(console, address string) string {
	text := "The HFDesk service is running.\n"
	if console != "" {
		text += "It runs " + console + ".\n"
	}
	return text + "\n" + serviceAddressText(address)
}

// serviceStoppedText returns the report of a stopped service.
//
// The report is the state (stopped) and the one fact the user did not ask for
// and has to see — nothing serves the address of the service right now. It
// names no way back: a success report carries the result and its facts and no
// instruction (decision of 2026-11-25). surface is the launch surface the
// helper was started from; it stays in the signature with the launch-surface
// plumbing and no longer changes this report.
func serviceStoppedText(surface launchSurface) string {
	return "The HFDesk service is stopped.\n" +
		"Nothing serves the address of the service right now."
}

// serviceRegistrationRemovedText returns the report of a removed service
// registration.
//
// The report is the state (the registration is gone) and the invariant of the
// removal as its one line — the data stays, which is the surprising fact of
// this action. It names no way back: a success report carries the result and
// its facts and no instruction (decision of 2026-11-25). It never claims,
// hints or implies that any data was deleted: the removal writes no file,
// deletes no directory and reads no configuration beyond the registration
// itself. The data fact is stated without a list of the directories and files
// it covers: the list named more than the window needed and is not what the
// user acts on.
func serviceRegistrationRemovedText() string {
	return "The Windows service registration of HFDesk is removed.\n" +
		"Your data stays untouched."
}

// serviceNoRegistrationText returns the report of a removal that found no
// registration.
//
// The command line calls the same outcome a clean success, and this report
// keeps it a success without claiming a removal that never happened: the
// state (no registration) and the result of this attempt (there was nothing
// to remove) is all it says.
func serviceNoRegistrationText() string {
	return "HFDesk is not registered as a Windows service.\n" +
		"There was no registration to remove."
}

// serviceAddressText returns the address block of a report: the address as one
// fact.
//
// A registration whose listen address cannot be read as a URL still has an
// address, and naming it is better than leaving the report without one. A
// registration without any address is reported as such: the report never
// invites the user to open an address it just called unknown.
func serviceAddressText(address string) string {
	if address == "" {
		return "The web UI address is not known: the registration names no listen address."
	}
	return "The web UI is at " + address + "."
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
	serviceStatusCommandText = `Run "hfdesk service status" from an elevated shell.`
)

// serviceActionBusyText returns the refusal of a second service action while
// one is already running.
//
// The report is the result (this action was not started), the cause on its
// single line (another action runs) and the one action that ends it (wait for
// that action and repeat this one). It names no state of the service: the
// running action is the one that moves it, and a state read beside it would
// describe a machine that is already changing.
func serviceActionBusyText() string {
	return failureWindowText("Another HFDesk service action is already running.\n\n" +
		"This action was not started.\n\n" +
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
// a service the user did not ask to remove here. The next action is the launch
// of the desktop app, whose startup gate offers to open, start or update the
// service.
func serviceAlreadyRegisteredText() string {
	return serviceFailureReport(
		serviceActionFailureHeadline(serviceActionInstall),
		"The service registration already exists, and this attempt did not create, change or start it.",
		"Start HFDesk again.")
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
// before the first change, so a failed hold touched nothing — and the one
// factual cause line of the failed hold. A refusal because another Windows
// identity owns the object is stated as that fact, with the one step that
// ends the hold (End that process and try again.); every other cause ends
// with the command that reads the state, and no report carries both
// actions.
func serviceSingletonHoldRefusedText(action serviceAction, err error) string {
	detail := ""
	if err != nil {
		detail = err.Error()
	}
	if errors.Is(err, service.ErrSingletonAccessDenied) {
		return failureWindowText(serviceFailureReport(serviceActionFailureHeadline(action),
			"No service action was performed: another Windows account holds the HFDesk desktop session.",
			"End that process and try again."))
	}
	cause := serviceCauseLine(detail)
	result := "No service action was performed."
	if cause != "" {
		result = "No service action was performed: " + cause + "."
	}
	nextAction := ""
	if !service.HasNamedAction(cause) {
		nextAction = serviceStatusCommandText
	}
	return failureWindowText(serviceFailureReport(serviceActionFailureHeadline(action), result, nextAction))
}

// serviceUnreadableClaimRefusedText returns the report of a mutation that
// refused over a published claim it could not read: the result (no service
// action was performed) and the cause (the state file that would tell whether
// a desktop serves cannot be read). That the question stays undetermined is
// the fact that makes the refusal terminal: no single step clears a file the
// guard cannot inspect, so the report names no next action.
func serviceUnreadableClaimRefusedText(action serviceAction, claim string) string {
	return failureWindowText(serviceFailureReport(serviceActionFailureHeadline(action),
		`No service action was performed: the state file of the desktop at "`+claim+
			`" cannot be read, so whether a desktop of this session serves its local server cannot be determined.`))
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
// action that failed, the one factual cause line of the service control
// manager and the one next action of the state the attempt left.
//
// It never claims that a registration was restored: a failure can happen after
// the registration was written, so an installation report states that the
// installation did not confirm a registration. An installation whose
// service-owned first start failed is reported from the cleanup facts returned
// by the service manager instead. A first installation that refused an
// existing registration is reported as the unchanged state it left instead:
// that registration was never written by this attempt, so neither the
// registration line nor the removal belongs to it. An installation that could
// not give the account of the instance access to its program is reported by
// serviceImageAccessFailedText, which states the path and the one action and
// never a registration: that refusal happens before any registration is
// written.
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
	cause := serviceCauseLine(detail)
	nextAction := ""
	if cause != "" && !service.HasNamedAction(cause) {
		nextAction = serviceStatusCommandText
	}

	// The refusal that could not give the account of the instance access to
	// its program happens before any registration is written, so it states
	// the path and the one action alone: a registration report would be false
	// here.
	if errors.Is(err, service.ErrServiceImageAccessUnrepaired) {
		return serviceImageAccessFailedText(action, program)
	}

	switch action {
	case serviceActionInstall:
		if isAlreadyRegisteredRefusal(err) {
			return serviceAlreadyRegisteredText()
		}
		// An installation reports the reason the attempt observed — the
		// image the host cannot execute, the console program that is
		// missing — rather than the verdict alone, because that reason is
		// the fact the user acts on (serviceReasonLine).
		cause = serviceReasonLine(detail)
		if cause != "" && !service.HasNamedAction(cause) {
			nextAction = serviceStatusCommandText
		}
		// The registration is asked for one program and one account, and
		// the window names both: the user can then check the folder and the
		// identity the failed attempt tried to register. The line states
		// the request, never a registration the attempt may not have
		// written; a caller that knows no program states none.
		request := ""
		if program != "" {
			request = "The installation was asked to run " + program + " as " + serviceInstanceAccountName + "."
		}
		// The installation may have written a registration or not, and
		// the report does not know which: reading the state resolves
		// that uncertainty, so the state command is the one next action.
		// A failure the helper reports without a program happened before
		// the registration was attempted, so the window states none.
		outcome := "The installation did not confirm whether it created a registration."
		if program == "" {
			outcome = ""
		}
		return serviceFailureReport(serviceActionFailureHeadline(action), cause, request, outcome, nextAction)
	case serviceActionUpdate:
		// The refusal of a changed account states the fact it observed:
		// the account the registration runs as. It names no next action
		// in this window — the route that restores the supported
		// identity stays in the log.
		if isServiceAccountRefusal(err) {
			return serviceFailureReport(serviceActionFailureHeadline(action), cause)
		}
		return serviceFailureReport(serviceActionFailureHeadline(action), cause, nextAction)
	default:
		return serviceFailureReport(serviceActionFailureHeadline(action), cause, nextAction)
	}
}

// serviceInstanceAccountName is the account a machine-wide HFDesk service runs
// as, as Windows spells it. The service layer registers the instance under
// this built-in identity; the installation window names it beside the program
// the attempt was asked to run, so the user can check both facts of the
// request instead of guessing which identity the registration would use.
const serviceInstanceAccountName = `NT AUTHORITY\LocalService`

// serviceImageAccessFailedText returns the window of an installation that
// could not give the account of the instance access to its program: the
// action that failed, the one fact — the account cannot read the registered
// program — and the one action that resolves it. The refusal happens before
// any registration is written, so the window states no registration at all.
//
// The path is the program the caller passed, never a path read back out of the
// refusal message: the refusal of a directory of the program's route names
// that directory, and a text parse would then call it the program (and break
// silently on any rewording of the service layer). A caller that knows no
// program states the fact without a path.
func serviceImageAccessFailedText(action serviceAction, program string) string {
	cause := "The service account cannot read the HFDesk program."
	if program != "" {
		cause = "The service account cannot read the program at " + program + "."
	}
	retry := "install the service again"
	if action != serviceActionInstall {
		retry = "run the action again"
	}
	return serviceFailureReport(serviceActionFailureHeadline(action), cause,
		"Move the HFDesk pair into a folder the service account can read, then "+retry+".")
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
// ONE line below carries only the cause facts: when the readiness wait
// recorded its clean observation beside its deadline wrapper, that
// observation is the line (InstallStartError.Observation), so the service
// wrapper that would restate the program and the budget never reaches the
// report; every other cause is reduced to its one factual line
// (serviceCauseLine), so the service layer's argument and command never
// become window text. The class is never repeated under its headline, so the
// class and the budget each occur exactly once. The kept-registration branch
// states that the registration stayed and names the one action that reads its
// state. The removed branch never shares its wording with a machine that
// never had a registration: it says what this attempt removed, not that
// HFDesk was never installed.
//
// The cleanup fact itself is stated exactly once: the detail carries the
// cause and the one line the cleanup observed as a reason, never the
// service layer's own gloss of the cleanup outcome — that gloss and the
// outcome sentence below are two formulations of the same fact, and a
// report states it once.
func serviceInstallStartFailureText(err error, program string, cleanup *service.InstallStartError) string {
	detail := installStartDetail(err, cleanup)
	if cleanup == nil || strings.TrimSpace(cleanup.Observation) == "" {
		detail = serviceCauseLine(detail)
	}
	removed := cleanup != nil && cleanup.RegistrationRemoved
	headline := serviceInstallStartHeadline(cleanup)
	if removed {
		return failureWindowText(serviceFailureReport(headline, detail,
			"The registration created by this attempt was removed.",
			"Start the desktop app normally again."))
	}
	outcome := "The registration was kept: cleanup could not prove that this attempt still owned it."
	if cleanup != nil && cleanup.RegistrationOwned {
		if cleanup.ProcessStopped {
			outcome = "The failed process was proven stopped, but the registration created by this attempt could not be removed."
		} else {
			outcome = "The registration created by this attempt was kept: its process was not proven stopped."
		}
	} else if cleanup != nil && cleanup.ProcessStopped {
		outcome = "The failed process was proven stopped, but the registration changed and was kept."
	}
	nextAction := ""
	if !service.HasNamedAction(detail) {
		nextAction = serviceStatusCommandText
	}
	return failureWindowText(serviceFailureReport(headline, detail, outcome, nextAction))
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
// two paragraphs the reader has to join. When the readiness wait recorded its
// clean observation beside its deadline wrapper (InstallStartError.
// Observation), that observation IS the cause line: the state word, the exit
// code, the probe's answer — without the wrapper, which would restate the
// program and the budget the headline of this report already speaks.
// InstallStartError.Error states the cleanup fact in its own words
// ("(the registration was kept…)"); the report states the fact once, in its
// own outcome sentence, so the two formulations can no longer appear beside
// each other. An error the cleanup struct does not describe keeps its whole
// message as the detail it always was.
func installStartDetail(err error, cleanup *service.InstallStartError) string {
	var detail string
	switch {
	case cleanup != nil && strings.TrimSpace(cleanup.Observation) != "":
		detail = cleanup.Observation
	case cleanup != nil && cleanup.Cause != nil:
		detail = cleanup.Cause.Error()
	default:
		if err == nil {
			return ""
		}
		return err.Error()
	}
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
	// link that fetches the paired console program, puts it in place
	// and continues the service action the user invoked.
	siblingMissingDownloadLabel = "Download and install now"
	// siblingMissingDismissLabel dismisses the notice and changes
	// nothing.
	siblingMissingDismissLabel = "Not now"
	// siblingMissingManualLabel is the visible item of the manual route
	// for a desktop program that names its release: the press opens
	// that release page in the default browser — the way to get the
	// file by hand — instead of a fold.
	siblingMissingManualLabel = "Open the release page"
	// siblingMissingNoReleaseManualLabel is the label of the same route
	// for a local build, whose name carries no release version: there
	// is no release page to open, so the item keeps the instructions
	// themselves instead of promising a page that does not exist.
	siblingMissingNoReleaseManualLabel = "Install it manually instead"
	// siblingMissingManualNote is the note under siblingMissingManualLabel:
	// what to do on the page the press opens — download the file there,
	// put it beside hfdesk-desktop.exe, run the action again. The URL
	// itself never becomes text here: the press opens it.
	siblingMissingManualNote = "Download the file there, put it next to hfdesk-desktop.exe, and run the action again."
	// siblingMissingRetryLabel re-reads the release: it is the item that
	// replaces the download button whenever no download can be offered,
	// so the button never disappears without a way back.
	siblingMissingRetryLabel = "Try again"
	// siblingMissingDetailsLabel is the fold that carries what the first
	// glance of the notice deliberately leaves out: the pairing rule and
	// the raw cause of a failed lookup.
	siblingMissingDetailsLabel = "Details"
)

// The reasons a notice without a download states, one line each, in human
// words: what stopped the offer, without the failure behind it, which stays in
// the folded details.
const (
	// consoleOfferNoVersionReason names the state of a build whose file
	// name carries no release version: such a build names no release, so
	// there is nothing to download its pair from. The product rule of that
	// state is the honest reason alone — it is never turned into an offer.
	consoleOfferNoVersionReason = "This copy of HFDesk carries no release version, so there is no release to download this file from."
	// consoleOfferUnreachableReason names a release that could not be
	// read: no network, a timeout or a broken answer, with the one thing
	// the user can act on beside it.
	consoleOfferUnreachableReason = "The release page of HFDesk could not be reached. Check your connection and try again."
	// consoleOfferUnpublishedReason names a release that was read but
	// publishes neither this file nor a checksum for it — the two cases
	// one line covers, because both mean there is nothing to fetch and
	// verify.
	consoleOfferUnpublishedReason = "The release does not publish this file with a checksum to verify it against."
	// consoleOfferUnknownReason is the honest sentence of a failure no
	// class names: no cause is invented for it.
	consoleOfferUnknownReason = "HFDesk could not offer a verified download of this file."
)

// siblingMissingNotice is the copy of the notice a service action shows when
// its console program is missing.
//
// The fields are the parts of the redesigned TaskDialog layout: the bold
// instruction names the file and why the service needs it, the first-glance
// content states what the program is for and the folder it is expected in,
// the download offer — nil when no verified download can be offered — carries
// the release facts the command link shows, and the reason line stands in for
// that button when there is none. The manual text is the route without a
// download, shown as its own visible item; the details hold what the first
// glance leaves out (the pairing rule and the raw cause of a failed lookup);
// and the log file is the file this launch writes, which the dialog opens with
// one action instead of naming it as dead text. Action is the service action
// the user invoked, so the copy that continues after a verified download names
// that action and never another one. The copy is built without any dialog or
// network in it, so a test can pin the wording.
type siblingMissingNotice struct {
	// Action is the service action that continues after a verified
	// download: install or update.
	Action serviceAction
	// Title is the window title of the notice.
	Title string
	// Instruction is the bold main line: the missing file, and why it is
	// needed.
	Instruction string
	// Content is the first-glance facts under it: what the program is for
	// and the folder it is expected in.
	Content string
	// Folder is the folder the lookup looked in and the manual steps
	// point at.
	Folder string
	// Download is the resolved offer of the paired release asset, or nil
	// when only the manual route is shown.
	Download *consoleDownloadOffer
	// Manual is the text of the manual path: how to get the file
	// without a download, ending in the URL of the release page for a
	// desktop program that names its release. It is the text the manual
	// link carried before that link opened the page itself, and it stays
	// the instruction of the message box fallback and of the report of a
	// failed download.
	Manual string
	// ReleaseURL is the release page of this build — the very URL the
	// manual text names (both derive from consoleReleaseFor) — the page
	// the manual route opens. Empty for a local build, whose name
	// carries no release version and therefore names no page.
	ReleaseURL string
	// Reason is the one line that states why no download is offered, and
	// empty when one is.
	Reason string
	// Details is what the folded part of the dialog carries: the pairing
	// rule and the raw cause of the failed lookups.
	Details string
	// LogFile is the log file of this launch, empty when this launch
	// writes none.
	LogFile string
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
// version-stamped program, or hfdesk.exe for a local build — beside the fact
// that the service needs it, and the content states the folder the file is
// expected in. Nothing raw enters that first glance: the pairing rule, the
// cause of a failed lookup and the cause of a failed release read all live in
// the folded details (siblingMissingDetails), and the reason line stands where
// the download button would be whenever there is no button to show
// (siblingMissingOfferReason). The log file of this launch travels as its own
// field, so the dialog opens it with an action instead of printing its path as
// dead text beside the facts.
func newSiblingMissingNotice(action serviceAction, folder, desktop string, findErr error, offer *consoleDownloadOffer, offerErr error) siblingMissingNotice {
	missing := pairedConsoleExecutableName(desktop)
	if missing == "" {
		missing = consoleExecutableName + executableSuffix
	}
	releaseURL := ""
	if release, namesRelease := consoleReleaseFor(desktop); namesRelease {
		releaseURL = releaseTagPagePrefix + release.Tag
	}
	return siblingMissingNotice{
		Action:      action,
		Title:       serviceSiblingMissingCaption,
		Instruction: missing + " is missing, and the Windows service needs it.",
		Content:     missingProgramContent(folder),
		Folder:      folder,
		Download:    offer,
		Manual:      siblingMissingManualText(folder, desktop, missing),
		ReleaseURL:  releaseURL,
		Reason:      siblingMissingOfferReason(offer, offerErr),
		Details:     siblingMissingDetails(desktop, missing, findErr, offerErr),
		LogFile:     runLogPath,
	}
}

// missingProgramContent returns the first-glance facts of the notice: what
// the program is for, and the folder it is expected in — one fact per line,
// so the two read at a glance.
func missingProgramContent(folder string) string {
	return "The Windows service runs this program.\nIt is expected in " + missingProgramFolder(folder) + "."
}

// missingProgramFolder names the folder the lookup looked in, or the folder
// of the desktop app when the caller names none.
func missingProgramFolder(folder string) string {
	if folder == "" {
		return "the folder of this desktop app"
	}
	return folder
}

// siblingMissingOfferReason returns the one line that stands in for the
// download button when no download can be offered, and an empty string when
// one can.
//
// The reason names the state and nothing behind it: a build whose name carries
// no release version names no release to fetch from, an unreadable release is
// a connection the user can check, and a release that was read but publishes
// nothing verifiable says exactly that. A failure no class names gets an
// honest sentence that claims no cause.
func siblingMissingOfferReason(offer *consoleDownloadOffer, offerErr error) string {
	switch {
	case offer != nil:
		return ""
	case offerErr == nil:
		// No offer and no failure is the state of a name without a
		// release version: the resolve reads that state from the name
		// alone and reaches no server for it.
		return consoleOfferNoVersionReason
	}
	if reason, classified := consoleOfferReasonOf(offerErr); classified {
		switch reason {
		case consoleOfferUnreachable:
			return consoleOfferUnreachableReason
		case consoleOfferUnpublished:
			return consoleOfferUnpublishedReason
		}
	}
	return consoleOfferUnknownReason
}

// siblingMissingDetails returns the folded part of the notice: the pairing
// rule, the raw cause of a lookup that failed for a reason of its own, and
// the raw cause of a release read that failed.
//
// The pairing failure is left out of the raw list because its message states
// that very rule, whose human sentence is already the first part here — one
// rule, stated once, in the words a user reads.
func siblingMissingDetails(desktop, missing string, findErr, offerErr error) string {
	var parts []string
	if rule := siblingMissingPairRule(desktop, missing); rule != "" {
		parts = append(parts, rule)
	}
	if findErr != nil && !errors.Is(findErr, errConsolePairMissing) {
		parts = append(parts, findErr.Error())
	}
	if offerErr != nil {
		parts = append(parts, offerErr.Error())
	}
	return strings.Join(parts, "\n\n")
}

// siblingMissingPairRule returns the one sentence that states why the two
// programs belong together: a desktop program of a release only runs the
// console program of that same release, and both ship in one folder. A local
// build names no release, so it gets the short form of the same fact.
func siblingMissingPairRule(desktop, missing string) string {
	if _, namesRelease := consoleReleaseFor(desktop); namesRelease {
		return "A desktop program of a release only runs the console program of that same release, " +
			"so the release ships " + missing + " beside hfdesk-desktop.exe and both belong in one folder."
	}
	return "A release of HFDesk ships both programs in one folder, so the console program belongs beside the desktop app."
}

// siblingMissingBody returns the first-glance text of the notice: the
// instruction, the facts, and — when no download is offered — the one reason
// line where its button would be.
func siblingMissingBody(notice siblingMissingNotice) string {
	body := notice.Instruction + "\n\n" + notice.Content
	if notice.Reason != "" {
		body += "\n\n" + notice.Reason
	}
	return body
}

// siblingMissingManualText returns the text of the manual path: how to get
// the file without a download and where to put it.
//
// A desktop program that names its release gets the release to fetch, with
// the URL of its release page — the very page the manual route of the dialog
// opens (notice.ReleaseURL); a local build names no release, so its text only
// states where the file goes. Both end with the one action that follows: run
// the action again. The pairing rule beside these steps is not part of them —
// it lives in the folded details of the dialog (siblingMissingDetails), where
// it belongs with the other cause a first glance leaves out.
func siblingMissingManualText(folder, desktop, missing string) string {
	where := missingProgramFolder(folder)
	release, namesRelease := consoleReleaseFor(desktop)
	if namesRelease {
		return "Get release " + release.Tag + " from " + releaseTagPagePrefix + release.Tag +
			", put " + missing + " next to hfdesk-desktop.exe in " + where +
			", and run the action again."
	}
	return "Put " + missing + " next to hfdesk-desktop.exe in " + where +
		", and run the action again."
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
// box still names the file, the folder and the way without a download — and
// a notice that carried an offer gains the reason line, so the box says that
// no download is offered instead of hiding a button it never shows.
func siblingMissingNoticeForFallback(notice siblingMissingNotice, dialogErr error) siblingMissingNotice {
	if dialogErr == nil {
		return notice
	}
	notice.Download = nil
	if notice.Reason == "" {
		// The box never shows the download button the dialog was going
		// to show, so it states that plainly instead of leaving a
		// vanished button unexplained.
		notice.Reason = consoleOfferUnknownReason
	}
	return notice
}

// siblingMissingManualNoticeText returns the whole text of the manual-only
// notice for the message box fallback: instruction, facts, the reason line
// when there is no download to offer, the manual path, the folded details this
// box has no fold for, and the standard log-path line — in one box, so the
// degraded window keeps everything the task dialog would have shown.
func siblingMissingManualNoticeText(notice siblingMissingNotice) string {
	text := siblingMissingBody(notice) + "\n\n" + notice.Manual
	if notice.Details != "" {
		text += "\n\n" + notice.Details
	}
	return text + logFileLine(notice.LogFile)
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
	return siblingMissingBody(notice) + "\n\n" +
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
// one command that reads the state. The standard log-path line of every
// failure window closes it.
func serviceHandoverRefusedText(err error) string {
	detail := ""
	if err != nil {
		detail = err.Error()
	}
	cause := serviceReasonLine(detail)
	nextAction := ""
	if !service.HasNamedAction(cause) {
		nextAction = serviceStatusCommandText
	}
	return failureWindowText(serviceFailureReport("HFDesk could not prepare the service action.",
		"No service action was performed.", cause, nextAction))
}

// serviceHandoverFailureHeadline returns the first line of the report of a
// handover that could not finish: the action that did not finish, in the
// words the reports of that action already use. Only an action that can take
// a handover ever reaches it (serviceActionTakesHandover).
func serviceHandoverFailureHeadline(action serviceAction) string {
	switch action {
	case serviceActionUpdate:
		return "HFDesk could not finish the service update."
	case serviceActionStart:
		return "HFDesk could not finish the service start."
	case serviceActionUninstall:
		return "HFDesk could not finish the removal of the service registration."
	default:
		return "HFDesk could not finish the service installation."
	}
}

// serviceHandoverFailureText returns the report of the desktop host whose
// handover could not be finished after its shutdown: the result (the action
// did not finish), the cause on its own line, the one command
// that reads the state the failure left, and the standard log-path line —
// with no description of the mechanism the wait used.
func serviceHandoverFailureText(action serviceAction, err error) string {
	detail := ""
	if err != nil {
		detail = err.Error()
	}
	cause := serviceReasonLine(detail)
	nextAction := ""
	if !service.HasNamedAction(cause) {
		nextAction = serviceStatusCommandText
	}
	return failureWindowText(serviceFailureReport(serviceHandoverFailureHeadline(action), cause, nextAction))
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
