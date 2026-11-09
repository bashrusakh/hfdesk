// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package desktop

import (
	"errors"
	"fmt"
	"strings"

	"github.com/bashrusakh/hfdesk/internal/service"
)

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

	// serviceSiblingMissingCaption is the caption of the notice that the
	// console program of the service is missing.
	serviceSiblingMissingCaption = "HFDesk - the hfdesk program is missing"

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
	default:
		return "HFDesk - the service action failed"
	}
}

// installServiceText returns the text of the confirmation that registers the
// Windows service.
//
// The confirmation states the four facts the user has to know: the service
// starts at boot and keeps running after the window closes, the registration
// needs administrator rights, nothing is downloaded or copied, and the desktop
// and the service keep separate data (the surprising one). dataDir is the state
// directory of the service, so the text names the directory it will really own.
func installServiceText(dataDir string) string {
	text := "This registers HFDesk as a Windows service.\n\n" +
		"The service starts at boot and keeps running after this window closes.\n" +
		"Installing it needs administrator rights, so a Windows prompt follows.\n\n" +
		"HFDesk stays portable: nothing is downloaded or copied. The service runs the hfdesk program that sits next to this desktop app.\n\n" +
		"This desktop app and the service keep separate data:\n" +
		"- the settings, jobs and models of this app stay where they are, and\n" +
		"- the service starts with its own data"
	if dataDir != "" {
		text += " in " + dataDir
	}
	text += ", so the settings, jobs and models of this app do not appear in the service.\n\n" +
		"Do you want to install the service?"
	return text
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
// The content names what the running service serves, what an update does and
// that the service keeps its own data. The positively confirmed stopped
// service gets the sentence of the desktop mode instead of the sentence of a
// launch that ends: it is the one state whose dialog can continue this launch,
// and the sentence states what that answer does and that it never starts the
// service. The task dialog and the message box fallback share the content, so
// the two cannot tell the user different things.
func serviceGateContent(prompt serviceChoicePrompt) string {
	var text string
	if servicePromptRunning(prompt) && prompt.URL != "" {
		text += "The web UI of the service is at " + prompt.URL + ".\n\n"
	}
	if prompt.Detail != "" {
		text += prompt.Detail + "\n\n"
	}

	if servicePromptStopped(prompt) {
		text += "Work in desktop mode continues this launch as a normal desktop app: it starts its own server and never starts the service, so the service stays stopped and two instances never compete for the same address.\n\n"
	} else {
		text += "This launch of the desktop app started no server of its own, so two instances never compete for the same address.\n\n"
	}
	text += "Update re-registers the service from the hfdesk program next to this app. It keeps the settings of the service and its running state: a running service keeps running, and a stopped service stays stopped.\n\n" +
		"The service keeps its own data"
	if prompt.DataDir != "" {
		text += " in " + prompt.DataDir
	}
	text += ", so the settings, jobs and models of this desktop app do not appear in the service."
	return text
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
// nothing.
func serviceGatePendingText(state service.State) string {
	transition := "starting"
	if state == service.StateStopping {
		transition = "stopping"
	}
	return "The HFDesk service is still " + transition + " and did not reach a settled state in time.\n\n" +
		"This launch changes nothing: it starts no server of its own, so two instances never compete for the same address.\n\n" +
		"Start the desktop app again when the service has settled."
}

// serviceGateUnreadableText returns the text of the notice of a launch that
// could not read the registration of the Windows service.
//
// The launch cannot tell a machine without a service from one whose service is
// installed, so it decides on neither: the notice says that a service may be
// installed and running, that this launch started no server, and names the one
// action that resolves the uncertainty — the read of the state. It does not
// promise a relaunch: when that read itself fails, for example because a
// Windows policy denies it to this account, the state stays unknown and the
// notice says so instead of sending the user into a notice loop.
func serviceGateUnreadableText(err error) string {
	text := "HFDesk could not read the state of the Windows service.\n\n"
	if err != nil {
		text += err.Error() + "\n\n"
	}
	return text + "An HFDesk service may be installed and running on this machine. " +
		"This launch started no server of its own, so it cannot compete with that instance for the same address.\n\n" +
		"Check the service state with \"hfdesk service status\" from an elevated shell: a successful read settles what this launch could not — whether a service is installed and running — and the desktop app can then be started again; a read that fails, for example because a Windows policy denies it to your account, leaves the state unknown, and this launch will not guess it."
}

// serviceGateChangedText returns the text of the notice of a launch that
// chose the desktop mode for a positively confirmed stopped service and then
// re-read a state that is no longer that.
//
// The dialog is modal and can outlive the snapshot it was built from: a
// service another actor started in the meantime would serve the address this
// launch binds, so the launch does not continue. The text is the result (no
// server was started), the cause (the state the re-read reported) and one
// next action: the command that reads the state. No relaunch is promised
// beside it — when that read fails, for example because a Windows policy
// denies it to this account, the state stays unknown and the notice says so
// instead of sending the user into the same notice again.
func serviceGateChangedText(prompt serviceChoicePrompt) string {
	return "The state of the HFDesk service changed while the dialog was open.\n\n" +
		"It is " + serviceGateStatePhrase(prompt) + " now, so this launch did not continue into desktop mode: " +
		"it started no server of its own, so two instances never compete for the same address.\n\n" +
		"Check the state with \"hfdesk service status\" from an elevated shell: a successful read settles the state that changed while the dialog was open, and the desktop app can then be started again; a read that fails, for example because a Windows policy denies it to your account, leaves the state unknown, and this launch will not guess it."
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
	serviceGateUpdateLabel  = "Update service"
	serviceGateCancelLabel  = "Cancel"
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
// The report names the program the registration runs, the address the service
// serves and the data directory it owns. The last sentence tells the user the
// consequence they need: the desktop app is not needed to use the service, and
// its own data stays where it is. The helper runs only after the desktop process
// ended, so the report never tells the user to quit an app that is gone.
func serviceInstalledText(version, console, dataDir, address string) string {
	text := serviceVersionName(version) + " is installed as a Windows service.\n\n" +
		"The service runs " + console + ", starts at boot and keeps running after this window closes.\n\n"
	text += serviceAddressText(address)
	text += "The desktop app is not needed to use the service: open the address above."
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
// The not-running case names the relaunch of the desktop app and not the tray:
// an update is launched only from the service gate of a launch, and that launch
// ends the desktop host, so the tray is gone by the time this report is read. A
// sentence that pointed at a tray icon that is not there would send the user
// looking for an action they cannot reach.
func serviceUpdatedText(version, console, dataDir, address string, running bool, before service.State) string {
	text := serviceVersionName(version) + " is updated as a Windows service.\n\n" +
		"The registration now runs " + console + " and keeps the settings of the service and its data.\n\n"
	if running {
		text += serviceAddressText(address)
		text += "The desktop app is not needed to use the service: open the address above."
	} else {
		text += serviceUpdateNotRunningReport(before) + "\n\n" +
			"Start the desktop app again and choose Start service."
	}
	if dataDir != "" {
		text += "\n\nThe service keeps its own data in " + dataDir + ". The settings, jobs and models of the desktop app do not appear in the service."
	}
	return text
}

// serviceUpdateNotRunningReport returns the result and the cause of the report
// of an update that did not leave the service running.
//
// The result is the outcome of the update, the cause is the state the update
// observed before it and the rule that explains why nothing serves now. A
// service that was stopped before the update keeps the sentence it always had.
// Any other observed prior state is quoted as what the supervisor reported
// instead of being restated as "stopped", the way the failure reports of these
// actions already name a prior state. Every not-running report also says that
// the service stays stopped: an update never starts a service that was not
// running, so a report that lost that fact would leave it unstated.
func serviceUpdateNotRunningReport(before service.State) string {
	if before == service.StateStopped {
		return "The service stays stopped: it was stopped before the update, and an update never starts a stopped service. Nothing serves HFDesk right now."
	}
	return fmt.Sprintf("The service is not running after the update: it was %q before the update, and an update never starts a service that was not running, so the service stays stopped. Nothing serves HFDesk right now.", before)
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
	default:
		there = "There is no service to start."
	}
	return "HFDesk is not installed as a Windows service.\n\n" +
		there + " Install it from the tray of the desktop app first."
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
// not name it yet, and nothing when the manager text already carries that
// command: a report states one action, never the same command twice.
func statusIfUnnamed(detail string) string {
	if strings.Contains(detail, "hfdesk service status") {
		return ""
	}
	return "\n\n" + serviceStatusCommandText
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
// registration to the program this app runs from. The folder guidance of a
// failed installation is left out as well: the cause is known, and it is not
// the folder of the program.
func serviceAlreadyRegisteredText(detail string) string {
	return "HFDesk could not install the Windows service.\n\n" + detail + "\n\n" +
		"The service registration already exists, and this attempt did not create, change or start it.\n\n" +
		"Start HFDesk again: with the registration in place, its startup gate offers to open or start the service and to update it."
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
	return text
}

// serviceActionFailureText returns the report of a failed service action: the
// action that failed, the error of the service control manager and the one next
// action of the state the attempt left.
//
// It never claims that a registration was restored: a failure can happen after
// the registration was written, so an installation report names a registration
// that may remain. An installation report carries the folder of its program as
// well, because a machine-wide service runs under its own account and a program
// only the installing user can read is the common reason a first installation
// fails. An installation whose service-owned first start failed is reported
// from the cleanup facts returned by the service manager instead. A first
// installation that refused an existing registration is reported as the
// unchanged state it left instead: that registration was never written by this
// attempt, so neither the may-remain warning nor the removal belongs to it.
func serviceActionFailureText(action serviceAction, err error, program string) string {
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
			"A registration may remain.\n\n" +
			serviceFolderAccessText(program) +
			statusIfUnnamed(detail)
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
	default:
		return serviceActionFailureHeadline(action) + "\n\n" + detail + "\n\n" +
			"The start was not confirmed." + statusIfUnnamed(detail)
	}
}

// serviceInstallStartFailureText reports a first-install start failure from
// the cleanup facts returned by the service manager. Cleanup is performed
// through the creation handle, never by reopening a service name.
//
// The kept-registration branch states the uncertainty the cleanup actually
// left — the registration may remain — and names the one action that
// resolves it: the read of the state. Whether anything is left to remove is
// exactly what that read decides, so no removal is offered next to it.
func serviceInstallStartFailureText(err error, program string, cleanup *service.InstallStartError) string {
	detail := ""
	if err != nil {
		detail = err.Error()
	}
	if cleanup != nil && cleanup.RegistrationRemoved {
		return "HFDesk registered the service but could not start it.\n\n" + detail + "\n\n" +
			"The failed instance was settled and the registration created by this attempt was removed. HFDesk is not installed as a Windows service, so start the desktop app normally again."
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
	text := "HFDesk registered the service but could not confirm its start.\n\n" + detail + "\n\n" +
		outcome
	return text + statusIfUnnamed(detail) + "\n\n" + serviceFolderAccessText(program)
}

// serviceFolderAccessText returns the sentence that names the access the folder
// of the program needs.
//
// A machine-wide service runs under its own account, so a program that
// account cannot execute is not usable for the instance. The sentence names
// the program the action tried to put into service and states the rule the
// preflight checks: that account must be able to execute the program and to
// traverse its folder; read access to the program is demanded only where a
// handler genuinely needs it, never as a blanket requirement — the image is
// executed without being read. It is a statement and not an instruction,
// because the repair belongs to the user. It belongs to the reports of an
// installation only: an update and a start describe a registration that
// already ran.
func serviceFolderAccessText(program string) string {
	subject := "the program"
	if program != "" {
		subject = program
	}
	return `The Windows service runs as NT AUTHORITY\LocalService, so that account must be able to execute ` + subject + ` and to traverse the folder that holds it; it needs read access to ` + subject + ` only where a handler genuinely needs it.`
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
	default:
		return "The service was not started."
	}
}

// serviceHelperNotReadyText returns the report of a helper that started but
// never became ready.
//
// The report is one coherent message for this state: it states the result
// (nothing was performed, because the helper acts only after it becomes
// ready), the observed cause of the failed readiness, and one next action.
// It never claims that the helper could not be started — it was started —
// and it never repeats the reason the helper reports in its own box: it
// references that message, which is where the user reads the actual reason
// and the step that follows from it.
func serviceHelperNotReadyText(action serviceAction, err error) string {
	detail := ""
	if err != nil {
		detail = err.Error()
	}
	return serviceActionUnchangedText(action) +
		" No service action was performed: the helper acts only after it becomes ready, and it never did.\n\n" +
		detail + "\n\n" +
		"The helper runs its checks before it becomes ready and shows its own message with the reason when one of them fails. " +
		"Follow that message: it names the reason and the step that follows from it."
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
// keeps the prompt from ever appearing cannot be approved by that action, so
// the report says so instead of promising the approval will come. A helper
// that was started but never became ready is a different state and has its own
// report (serviceHelperNotReadyText).
func serviceLaunchFailedText(action serviceAction, err error) string {
	detail := ""
	if err != nil {
		detail = err.Error()
	}

	return "HFDesk could not start its administrator helper.\n\n" + detail + "\n\n" +
		serviceActionUnchangedText(action) +
		" Repeat the action and approve the administrator prompt when Windows shows it; a prompt Windows never shows — for example when a policy blocks the elevation — cannot be approved, and that cause stays."
}

// serviceSiblingMissingText returns the report of a missing console program.
//
// The desktop host does not download, copy or replace a binary, so the program
// has to be in the folder of the desktop host already. The text names the
// folder it looked at and how the release is laid out, so the user can put the
// two programs back together.
func serviceSiblingMissingText(lookedIn string, err error) string {
	detail := ""
	if err != nil {
		detail = err.Error()
	}
	text := "HFDesk could not find the hfdesk program that the service runs.\n\n" + detail + "\n\n" +
		"The desktop app never downloads or copies a binary. The release ships hfdesk.exe next to hfdesk-desktop.exe"
	if lookedIn != "" {
		text += ", and this desktop app was started from " + lookedIn
	}
	return text + ". Put both programs in the same folder and try again."
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
	return fmt.Sprintf("HFDesk was started with a service action it does not know.\n\n%s\n\n"+
		"Nothing was installed, updated or started. Start the desktop app again.", detail)
}
