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
// starting or stopping, or because its state could not be read.
//
// The registration of a first installation is only removed once the instance is
// provably settled and not running: a running instance serves, and a pending one
// may still reach the running state. Removing either would end an instance that
// is coming up instead of cleaning up an attempt that failed.
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
// The confirmation states the four facts the user has to know before a
// machine-wide registration is written: the service starts at boot and keeps
// running after the window closes, the registration needs administrator
// rights, nothing is downloaded or copied, and the desktop and the service
// keep separate data. The last one is the surprising one: the settings, jobs
// and models of the desktop app are not the data of the service.
//
// dataDir is the state directory of the service, so the text names the
// directory the service will really own instead of repeating a path from the
// documentation.
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
// that the service keeps its own data. It is the same text for the task dialog
// and for the message box fallback, so the two can never tell the user
// different things.
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
// The notice is not the choice dialog: a pending state is not a state to decide
// on, so the launch offers neither Open nor Start for it. It names the
// transition it observed and that the launch changed nothing, so the user knows
// that the state of the service is not the result of this launch.
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
// The launch cannot tell a machine without a service from a machine whose
// service is installed, so it decides on neither: a service that is installed
// may be running, and it binds the address the desktop server would bind. The
// notice says what that means for this launch, that a service may be installed
// and running, that this launch started no server, and how to read the state and
// to try again.
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

// serviceActionFailureText returns the report of a failed service action.
//
// The report carries the underlying error, so the user can see what the
// service control manager refused, and what the attempt did not confirm about
// the state afterwards. It never claims that a registration was restored: a
// failure can happen after the registration was written, and the state of the
// service has to be read before it is described.
//
// The report also names what to check when the service account cannot use the
// program: the helper installs no logging pipeline, so the failure report is
// the only surface this reason has. program is the program of the action, empty
// when the caller does not know it.
//
// An installation that wrote a registration and could not start it does not end
// here: the registration may have been removed again, and
// serviceInstallFailureText reports that outcome instead.
func serviceActionFailureText(action serviceAction, err error, program string) string {
	detail := ""
	if err != nil {
		detail = err.Error()
	}
	access := serviceAccountAccessText(program)

	switch action {
	case serviceActionInstall:
		return "HFDesk could not install the Windows service.\n\n" + detail + "\n\n" +
			"This attempt did not confirm the service as running. The settings, jobs and models of the desktop app were not touched. " +
			access + "\n\n" +
			"A registration may remain: a previous one, or the one this attempt wrote while its instance could not be proven stopped. " +
			"While a registration exists, every launch of the desktop app ends at the service gate and starts no server of its own. " +
			"If you do not want to keep it, remove it from an elevated shell with \"hfdesk service uninstall\".\n\n" +
			"Run \"hfdesk service status\" to read the current state of the service."
	case serviceActionUpdate:
		return "HFDesk could not update the Windows service.\n\n" + detail + "\n\n" +
			"The desktop app and the service keep separate data: the settings, jobs and models of the desktop app were not touched. " +
			access + "\n\n" +
			"Run \"hfdesk service status\" to read the current state of the service."
	default:
		return "HFDesk could not start the Windows service.\n\n" + detail + "\n\n" +
			"The start attempt was not confirmed. " +
			access + "\n\n" +
			"Run \"hfdesk service status\" to read the current state of the service."
	}
}

// serviceInstallFailureText returns the report of an installation that wrote the
// registration and did not confirm the service as running.
//
// The report depends on what the failed installation left on the machine, and it
// says so: a registration that this action wrote is removed again, so the user
// reads that HFDesk is not installed and that the desktop app starts normally
// again. A registration that was there before the action, or one whose state
// before the action could not be read, stays in place, and the report names what
// remains and the elevated command that removes it. The same holds for a
// registration that was not removed because its instance still runs or starts, or
// because its state could not be read; the report says what remains and never
// claims that the instance stopped. A removal that failed is reported with its
// reason and with the same command, because the registration is still there.
//
// The account and program guidance of a failed installation is part of every
// outcome: it is the reason a first installation commonly fails, and it is what
// makes a second attempt work.
func serviceInstallFailureText(err error, program string, aftermath serviceInstallAftermath, removalErr error) string {
	detail := ""
	if err != nil {
		detail = err.Error()
	}
	access := serviceAccountAccessText(program)

	var outcome string
	switch {
	case errors.Is(removalErr, errServiceRemovalSkipped):
		outcome = "The registration of this attempt was not removed, because the service may still be running, starting or stopping, or its state could not be read. " +
			"The registration stays in place.\n\n" +
			"While that registration exists, every launch of the desktop app ends at the service gate and starts no server of its own. " +
			"Remove it from an elevated shell with \"hfdesk service uninstall\" if you do not want to keep it."
	case aftermath == serviceInstallRemovalFailed:
		outcome = "The registration of this attempt could not be removed again: HFDesk stays installed as a Windows service although it does not run."
		if removalErr != nil {
			outcome += "\n\nThe removal failed: " + removalErr.Error()
		}
		outcome += "\n\nWhile that registration exists, every launch of the desktop app ends at the service gate and starts no server of its own. " +
			"Remove it from an elevated shell with \"hfdesk service uninstall\"."
	case aftermath == serviceInstallRemoved:
		outcome = "The registration of this attempt was removed again: HFDesk is not installed as a Windows service, and you can start the desktop app normally again.\n\n" +
			"Only the registration was removed. The data directory of the service, if this attempt created one, stays where it is."
	default:
		outcome = "The registration was left in place: it was installed before this attempt, or its state before the attempt could not be read. " +
			"The service stays installed, but it does not run.\n\n" +
			"While that registration exists, every launch of the desktop app ends at the service gate and starts no server of its own. " +
			"Remove it from an elevated shell with \"hfdesk service uninstall\" if you do not want to keep it."
	}

	return "HFDesk could not install the Windows service.\n\n" + detail + "\n\n" + outcome + "\n\n" +
		"The settings, jobs and models of the desktop app were not touched. " +
		access + "\n\n" +
		"Run \"hfdesk service status\" to read the current state of the service."
}

// serviceAccountAccessText returns the sentence that names what to check when
// the service account cannot use the program of the registration.
//
// The machine-wide service runs as NT AUTHORITY\LocalService, so a program in a
// folder of the user who installed it is not reachable for the instance: the
// account has to be able to read the program and to traverse its folder. The
// sentence is guidance and not a check of its own: the action reports the
// failure it got and names the reason the user can repair.
func serviceAccountAccessText(program string) string {
	text := `The Windows service runs as NT AUTHORITY\LocalService, so that account must be able to read`
	if program != "" {
		text += " " + program
	} else {
		text += " the program"
	}
	return text + " and to traverse its folder. If the program sits in a folder only you can read, " +
		"move it to a folder the service can read, for example under %ProgramFiles%, and install again."
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
