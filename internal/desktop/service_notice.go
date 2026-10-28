// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package desktop

import (
	"errors"
	"fmt"
	"strings"

	"github.com/bashrusakh/hfdesk/internal/service"
)

// errServiceRemovalSkipped reports that the registration of a failed
// installation was left in place because the service may still be running,
// starting or stopping, or because its state could not be read. A registration
// is only removed once the instance is provably settled and not running.
var errServiceRemovalSkipped = errors.New(
	"the registration was not removed: the service may still be running, starting or stopping, or its state cannot be read")

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
	if serviceStateRunning(prompt.State) {
		return "The HFDesk service is running."
	}
	return "The HFDesk service is installed, but it is not running."
}

// serviceGateContent returns the content of the choice dialog of the service
// gate.
//
// The content names what the running service serves, what an update does and
// that the service keeps its own data. The task dialog and the message box
// fallback share it, so the two cannot tell the user different things.
func serviceGateContent(prompt serviceChoicePrompt) string {
	var text string
	if serviceStateRunning(prompt.State) && prompt.URL != "" {
		text += "The web UI of the service is at " + prompt.URL + ".\n\n"
	}
	if prompt.Detail != "" {
		text += prompt.Detail + "\n\n"
	}

	text += "This launch of the desktop app started no server of its own, so two instances never compete for the same address.\n\n" +
		"Update re-registers the service from the hfdesk program next to this app. It keeps the settings of the service and its running state: a running service keeps running, and a stopped service stays stopped.\n\n" +
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
		"Start the desktop app again when the service has settled, or read its state with \"hfdesk service status\"."
}

// serviceGateUnreadableText returns the text of the notice of a launch that
// could not read the registration of the Windows service.
//
// The launch cannot tell a machine without a service from one whose service is
// installed, so it decides on neither: the notice says that a service may be
// installed and running, that this launch started no server, and how to read
// the state and to try again.
func serviceGateUnreadableText(err error) string {
	text := "HFDesk could not read the state of the Windows service.\n\n"
	if err != nil {
		text += err.Error() + "\n\n"
	}
	return text + "An HFDesk service may be installed and running on this machine. " +
		"This launch started no server of its own, so it cannot compete with that instance for the same address.\n\n" +
		"Check the service state with \"hfdesk service status\" from an elevated shell, then start the desktop app again."
}

// serviceGateOpenLabel returns the label of the first button of the choice
// dialog.
func serviceGateOpenLabel(prompt serviceChoicePrompt) string {
	if serviceStateRunning(prompt.State) {
		return "Open HFDesk"
	}
	return "Start service"
}

// The labels of the other two buttons of the choice dialog.
const (
	serviceGateUpdateLabel = "Update service"
	serviceGateCancelLabel = "Cancel"
)

// serviceGateFallbackChoices returns the mapping of the answers of the
// message box fallback onto the actions.
//
// A message box has no named buttons, so the text has to say what Yes, No and
// Cancel do. Without the mapping the user would have to guess which answer
// starts the service and which one updates it.
func serviceGateFallbackChoices(prompt serviceChoicePrompt) string {
	open := "open HFDesk in your default browser"
	if !serviceStateRunning(prompt.State) {
		open = "start the service"
	}
	return "Choose Yes to " + open + ".\n" +
		"Choose No to update the service.\n" +
		"Choose Cancel to change nothing."
}

// serviceGateChooseText returns the text of the message box fallback of the
// choice dialog.
func serviceGateChooseText(prompt serviceChoicePrompt) string {
	return serviceGateText(prompt) + "\n\n" + serviceGateFallbackChoices(prompt)
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
// that was running is still running, and a service that was stopped stays
// stopped. The stopped case says so explicitly: the update never starts a
// service that was stopped, so nothing serves HFDesk after it, and a message
// that claims otherwise would send the user to a dead address.
//
// The stopped case names the relaunch of the desktop app and not the tray: an
// update is launched only from the service gate of a launch, and that launch
// ends the desktop host, so the tray is gone by the time this report is read. A
// sentence that pointed at a tray icon that is not there would send the user
// looking for an action they cannot reach.
func serviceUpdatedText(version, console, dataDir, address string, running bool) string {
	text := serviceVersionName(version) + " is updated as a Windows service.\n\n" +
		"The registration now runs " + console + " and keeps the settings of the service and its data.\n\n"
	if running {
		text += serviceAddressText(address)
		text += "The desktop app is not needed to use the service: open the address above."
	} else {
		text += "The service stays stopped: it was stopped before the update, and an update never starts a stopped service. Nothing serves HFDesk right now.\n\n" +
			"Start the desktop app again and choose Start service, or run \"hfdesk service start\" from an elevated shell."
	}
	if dataDir != "" {
		text += "\n\nThe service keeps its own data in " + dataDir + ". The settings, jobs and models of the desktop app do not appear in the service."
	}
	return text
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

// The commands that a failure report of a service action ends with: a report is
// the result, the cause and one next action, so each command is written once
// here instead of being repeated by every report.
const (
	// serviceStatusCommandText is the next action of a report that cannot
	// say which state the action left: the state of the service has to be
	// read before anything else is decided.
	serviceStatusCommandText = `Run "hfdesk service status" from an elevated shell to read the current state of the service.`

	// serviceUninstallCommandText is the next action of a report that left a
	// registration behind: whether the registration is kept is the decision
	// of the user.
	serviceUninstallCommandText = `If you do not want to keep the registration, remove it from an elevated shell with "hfdesk service uninstall".`
)

// serviceActionFailureText returns the report of a failed service action: the
// action that failed, the error of the service control manager and the one next
// action of the state the attempt left.
//
// It never claims that a registration was restored: a failure can happen after
// the registration was written, so an installation report names a registration
// that may remain. An installation report carries the folder of its program as
// well, because a machine-wide service runs under its own account and a program
// only the installing user can read is the common reason a first installation
// fails. An installation that wrote a registration and could not start it is
// reported by serviceInstallFailureText instead.
func serviceActionFailureText(action serviceAction, err error, program string) string {
	detail := ""
	if err != nil {
		detail = err.Error()
	}

	switch action {
	case serviceActionInstall:
		return "HFDesk could not install the Windows service.\n\n" + detail + "\n\n" +
			"A registration may remain.\n\n" +
			serviceFolderAccessText(program) + "\n\n" +
			serviceUninstallCommandText
	case serviceActionUpdate:
		return "HFDesk could not update the Windows service.\n\n" + detail + "\n\n" +
			serviceStatusCommandText
	default:
		return "HFDesk could not start the Windows service.\n\n" + detail + "\n\n" +
			"The start was not confirmed.\n\n" +
			serviceStatusCommandText
	}
}

// serviceInstallFailureText returns the report of an installation that wrote the
// registration and did not start it.
//
// The outcome is stated in what the registration may do next: a registration
// this action wrote is removed again, so the user starts the desktop app
// normally again; a registration that was there before, one whose state could
// not be read, one whose removal was skipped and one whose removal failed all
// stay, so the user decides whether to keep them. The folder of the program is
// part of the outcomes where the registration stays.
func serviceInstallFailureText(err error, program string, aftermath serviceInstallAftermath, removalErr error) string {
	detail := ""
	if err != nil {
		detail = err.Error()
	}

	stays := true
	var outcome string
	switch {
	case errors.Is(removalErr, errServiceRemovalSkipped):
		outcome = "The registration of this attempt was not removed, because the service may still be running, starting or stopping, or its state could not be read. " +
			"The registration stays in place.\n\n" +
			serviceUninstallCommandText
	case aftermath == serviceInstallRemovalFailed:
		outcome = "The registration of this attempt could not be removed again: HFDesk stays installed as a Windows service although it does not run."
		if removalErr != nil {
			outcome += "\n\nThe removal failed: " + removalErr.Error()
		}
		outcome += "\n\n" + serviceUninstallCommandText
	case aftermath == serviceInstallRemoved:
		stays = false
		outcome = "The registration of this attempt was removed again: HFDesk is not installed as a Windows service, so start the desktop app normally again."
	default:
		outcome = "The registration was left in place: it was installed before this attempt, or its state before the attempt could not be read. " +
			"The service stays installed, but it does not run.\n\n" +
			serviceUninstallCommandText
	}

	text := "HFDesk registered the service but could not start it.\n\n" + detail + "\n\n" + outcome
	if stays {
		text += "\n\n" + serviceFolderAccessText(program)
	}
	return text
}

// serviceFolderAccessText returns the sentence that helps when the failed
// installation could not run the program because of its folder.
//
// A machine-wide service runs under its own account, so a program in a folder
// only the installing user can read is not reachable for the instance. The
// sentence names the program the action tried to put into service and the one
// repair the user can make. It belongs to the reports of an installation only: an
// update and a start describe a registration that already ran.
func serviceFolderAccessText(program string) string {
	subject := "the program"
	if program != "" {
		subject = program
	}
	return "If " + subject + " sits in a folder only you can read, move it to %ProgramFiles% and try again."
}

// serviceLaunchFailedText returns the report of a failed launch of the elevated
// helper.
//
// The desktop host reports the failure itself, because the helper never ran:
// the helper is the only surface that reports the outcome of an action it
// performed. A refused request for administrator rights and a helper that
// could not be started both end here, before anything was changed.
func serviceLaunchFailedText(action serviceAction, err error) string {
	detail := ""
	if err != nil {
		detail = err.Error()
	}

	var unchanged string
	switch action {
	case serviceActionInstall:
		unchanged = "The service was not installed."
	case serviceActionUpdate:
		unchanged = "The service was not updated."
	default:
		unchanged = "The service was not started."
	}
	return "HFDesk could not start its administrator helper.\n\n" + detail + "\n\n" + unchanged
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
// know.
func unknownServiceActionText(err error) string {
	detail := ""
	if err != nil {
		detail = err.Error()
	}
	return fmt.Sprintf("HFDesk was started with a service action it does not know.\n\n%s\n\n"+
		"Nothing was installed, updated or started.", detail)
}
