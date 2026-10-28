// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package desktop

import (
	"errors"
	"strings"
	"testing"

	"github.com/bashrusakh/hfdesk/internal/service"
)

// TestInstallServiceTextStatesWhatTheRegistrationChanges proves that the
// confirmation of the installation carries the facts the user has to know
// before a machine-wide registration is written.
//
// The registration survives the desktop app, it needs administrator rights, and
// its data is not the data of the desktop. A confirmation that left one of them
// out would let the user agree to something they did not read.
func TestInstallServiceTextStatesWhatTheRegistrationChanges(t *testing.T) {
	const dataDir = `C:\ProgramData\HFDesk`
	text := installServiceText(dataDir)

	for _, want := range []string{
		"starts at boot",
		"keeps running after this window closes",
		"administrator rights",
		"Windows prompt",
		"nothing is downloaded or copied",
		"settings, jobs and models",
		dataDir,
		"do not appear in the service",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the confirmation %q does not mention %q", text, want)
		}
	}
}

// TestInstallServiceTextWithoutADataDirectoryStaysTrue proves that the
// confirmation never names a directory it could not resolve.
func TestInstallServiceTextWithoutADataDirectoryStaysTrue(t *testing.T) {
	text := installServiceText("")
	if strings.Contains(text, "  ") {
		t.Errorf("the confirmation %q carries an empty directory", text)
	}
	if !strings.Contains(text, "do not appear in the service") {
		t.Errorf("the confirmation %q does not state the separation of the data", text)
	}
}

// TestServiceGateTextAsksToOpenARunningService proves that a running service is
// offered as the web UI it already serves, and that the dialog names the three
// actions.
//
// The user started HFDesk again, so the running instance is what they were
// looking for: the first button opens it instead of starting a second instance.
func TestServiceGateTextAsksToOpenARunningService(t *testing.T) {
	prompt := serviceChoicePrompt{
		State:   service.StateRunning,
		Listen:  "127.0.0.1:8080",
		URL:     "http://127.0.0.1:8080",
		DataDir: `C:\ProgramData\HFDesk`,
	}

	text := serviceGateText(prompt)
	for _, want := range []string{
		"The HFDesk service is running.",
		"http://127.0.0.1:8080",
		"started no server of its own",
		"stopped service stays stopped",
		prompt.DataDir,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the dialog %q does not mention %q", text, want)
		}
	}

	if got, want := serviceGateOpenLabel(prompt), "Open HFDesk"; got != want {
		t.Errorf("the first button is %q, want %q", got, want)
	}
	if got, want := serviceGateUpdateLabel, "Update service"; got != want {
		t.Errorf("the update button is %q, want %q", got, want)
	}
	if got, want := serviceGateCancelLabel, "Cancel"; got != want {
		t.Errorf("the cancel button is %q, want %q", got, want)
	}
}

// TestServiceGateTextAsksToStartAStoppedService proves that a service that does
// not run is offered as a start and never claims to serve an address.
func TestServiceGateTextAsksToStartAStoppedService(t *testing.T) {
	prompt := serviceChoicePrompt{
		State:  service.StateStopped,
		Listen: "127.0.0.1:8080",
	}

	text := serviceGateText(prompt)
	if !strings.Contains(text, "installed, but it is not running") {
		t.Errorf("the dialog %q does not say that the service is stopped", text)
	}
	if strings.Contains(text, "The web UI of the service is at") {
		t.Errorf("the dialog %q claims an address of a service that is stopped", text)
	}
	if got, want := serviceGateOpenLabel(prompt), "Start service"; got != want {
		t.Errorf("the first button is %q, want %q", got, want)
	}
}

// TestServiceGateFallbackChoicesMapTheAnswers proves that the message box
// fallback names the actions its unnamed buttons perform.
//
// A message box has no named buttons: without the mapping the user would have
// to guess which answer starts the service and which one updates it.
func TestServiceGateFallbackChoicesMapTheAnswers(t *testing.T) {
	running := serviceGateFallbackChoices(serviceChoicePrompt{State: service.StateRunning})
	for _, want := range []string{
		"Choose Yes to open HFDesk in your default browser.",
		"Choose No to update the service.",
		"Choose Cancel to change nothing.",
	} {
		if !strings.Contains(running, want) {
			t.Errorf("the fallback %q does not mention %q", running, want)
		}
	}

	stopped := serviceGateFallbackChoices(serviceChoicePrompt{State: service.StateStopped})
	if !strings.Contains(stopped, "Choose Yes to start the service.") {
		t.Errorf("the fallback %q does not offer to start the service", stopped)
	}

	// The fallback carries the whole question, so a user who reads only the
	// message box has the same facts as a user of the task dialog.
	choice := serviceGateChooseText(serviceChoicePrompt{State: service.StateStopped, URL: ""})
	if !strings.Contains(choice, serviceGateText(serviceChoicePrompt{State: service.StateStopped, URL: ""})) {
		t.Errorf("the fallback %q does not carry the question", choice)
	}
}

// TestServiceGatePendingTextNamesTheTransitionAndTheUnchangedLaunch proves that
// a launch that found the service in the middle of a transition reports the
// transition it observed and that the launch itself changed nothing.
//
// The notice is not the choice dialog: a service that is starting or stopping
// has no state a button could act on, so the text carries no button and no
// action of its own.
func TestServiceGatePendingTextNamesTheTransitionAndTheUnchangedLaunch(t *testing.T) {
	for _, tc := range []struct {
		state service.State
		want  string
	}{
		{state: service.StateStarting, want: "still starting"},
		{state: service.StateStopping, want: "still stopping"},
	} {
		text := serviceGatePendingText(tc.state)
		for _, want := range []string{tc.want, "changes nothing", "starts no server"} {
			if !strings.Contains(text, want) {
				t.Errorf("the notice of %q is %q, want it to mention %q", tc.state, text, want)
			}
		}
		if strings.Contains(text, "Open HFDesk") || strings.Contains(text, "Start service") {
			t.Errorf("the notice of %q is %q, want it to offer no action", tc.state, text)
		}
	}
	if serviceGatePendingCaption == "" {
		t.Error("the notice of a pending service has no caption")
	}
}

// TestServiceUpdatedTextKeepsAStoppedServiceStopped proves that the report of an
// update of a stopped service says that nothing serves HFDesk afterwards.
//
// An update never starts a stopped service, so a message that sent the user to
// the address of the service would send them to a dead address. The way to
// start it names the relaunch of the desktop app: the update ran from the
// service gate, whose launch ended the desktop host, so no tray icon is left
// that the report could point at.
func TestServiceUpdatedTextKeepsAStoppedServiceStopped(t *testing.T) {
	const address = "http://127.0.0.1:8080"
	text := serviceUpdatedText("1.3.0", `C:\portable\hfdesk.exe`, `C:\ProgramData\HFDesk`, address, false)

	for _, want := range []string{
		"HFDesk 1.3.0",
		`C:\portable\hfdesk.exe`,
		"stays stopped",
		"never starts a stopped service",
		"Start the desktop app again and choose Start service",
		"hfdesk service start",
		"elevated shell",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the report %q does not mention %q", text, want)
		}
	}
	if strings.Contains(text, address) {
		t.Errorf("the report %q sends the user to the address of a stopped service", text)
	}
	if strings.Contains(text, "tray") {
		t.Errorf("the report %q points at the tray of a desktop app that ended before the report", text)
	}
}

// TestServiceUpdatedTextReportsARunningService proves that the report of an
// update of a running service names the address and tells the user that the
// desktop app is not needed to use the service.
func TestServiceUpdatedTextReportsARunningService(t *testing.T) {
	const address = "http://127.0.0.1:8080"
	text := serviceUpdatedText("1.3.0", `C:\portable\hfdesk.exe`, `C:\ProgramData\HFDesk`, address, true)

	for _, want := range []string{address, "not needed to use the service", `C:\portable\hfdesk.exe`, "keeps the settings"} {
		if !strings.Contains(text, want) {
			t.Errorf("the report %q does not mention %q", text, want)
		}
	}
}

// TestServiceInstalledTextNamesTheProgramTheAddressAndTheData proves that the
// report of an installation names what the service runs, where it serves and
// where its data lives.
//
// The program is named, so the portability of the installation is visible: the
// service runs the program of the folder of the desktop host and not a copy
// somewhere on the machine.
func TestServiceInstalledTextNamesTheProgramTheAddressAndTheData(t *testing.T) {
	const (
		console = `C:\portable\hfdesk.exe`
		dataDir = `C:\ProgramData\HFDesk`
		address = "http://127.0.0.1:8080"
	)
	text := serviceInstalledText("1.3.0", console, dataDir, address)

	for _, want := range []string{
		"HFDesk 1.3.0",
		console,
		"starts at boot",
		address,
		"not needed to use the service",
		dataDir,
		"do not appear in the service",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the report %q does not mention %q", text, want)
		}
	}

	// A local build carries no release version: it is named without one
	// instead of reporting the build placeholder to the user.
	if text := serviceInstalledText("dev", console, "", ""); strings.Contains(text, "dev") {
		t.Errorf("the report %q reports the build placeholder as a version", text)
	}
	// A registration without a readable address is reported as such, and the
	// report never leaves a sentence without an address.
	if text := serviceInstalledText("1.3.0", console, "", ""); !strings.Contains(text, "names no listen address") {
		t.Errorf("the report %q does not report the missing address", text)
	}
}

// TestServiceActionFailureTextIsTheResultTheCauseAndOneAction proves that a
// failed service action reports the underlying error, the state the attempt left
// and exactly one next action, instead of asserting a state it may not have
// observed.
//
// The report never claims that a registration was restored: a failure can happen
// after it was written. An installation names the removal of a registration that
// may remain, an update and a start name the command that reads the state they
// could not observe, and no report carries both commands.
//
// The separate data of the desktop app is part of the confirmation and of the
// gate, not of a failure report: the report is result, cause and one action.
func TestServiceActionFailureTextIsTheResultTheCauseAndOneAction(t *testing.T) {
	reason := errors.New("the service control manager refused the registration")
	const program = `C:\portable\hfdesk.exe`

	install := serviceActionFailureText(serviceActionInstall, reason, program)
	for _, want := range []string{
		reason.Error(),
		"A registration may remain.",
		`"hfdesk service uninstall"`,
		"elevated shell",
	} {
		if !strings.Contains(install, want) {
			t.Errorf("the report %q does not mention %q", install, want)
		}
	}
	// The report states a possibility, never a fact: a failed installation
	// may have written a registration, and it may not have.
	if strings.Contains(install, "A registration is installed") || strings.Contains(install, "a registration remains") {
		t.Errorf("the report %q claims that a registration exists", install)
	}
	// One report carries one command: the state of a failed installation is
	// described by the report itself, so the removal is its only next action.
	if strings.Contains(install, "hfdesk service status") {
		t.Errorf("the report %q names a second next action next to the removal", install)
	}
	if strings.Contains(install, "were not touched") {
		t.Errorf("the report %q repeats the separate data of the desktop app", install)
	}

	update := serviceActionFailureText(serviceActionUpdate, reason, program)
	for _, want := range []string{reason.Error(), "hfdesk service status"} {
		if !strings.Contains(update, want) {
			t.Errorf("the report %q does not mention %q", update, want)
		}
	}
	for _, unwanted := range []string{"registration may remain", "hfdesk service uninstall", "were not touched"} {
		if strings.Contains(update, unwanted) {
			t.Errorf("the report of the update %q carries %q, want the state it left and one command", update, unwanted)
		}
	}

	start := serviceActionFailureText(serviceActionStart, reason, program)
	for _, want := range []string{reason.Error(), "The start was not confirmed.", "hfdesk service status"} {
		if !strings.Contains(start, want) {
			t.Errorf("the report %q does not mention %q", start, want)
		}
	}
	for _, unwanted := range []string{"registration may remain", "hfdesk service uninstall", "were not touched"} {
		if strings.Contains(start, unwanted) {
			t.Errorf("the report of the start %q carries %q, want the state it left and one command", start, unwanted)
		}
	}
}

// TestServiceActionFailureTextNamesTheFolderOfTheProgram proves that the report of
// a failed installation names the program the action could not put into service
// and the one repair of a program that only the installing user can read.
//
// An update and a start report leave the folder out: they describe a registration
// that already ran, so the folder of the program is not the likely cause there.
func TestServiceActionFailureTextNamesTheFolderOfTheProgram(t *testing.T) {
	reason := errors.New("the service control manager refused the registration")
	const program = `C:\portable\hfdesk.exe`

	text := serviceActionFailureText(serviceActionInstall, reason, program)
	for _, want := range []string{program, `%ProgramFiles%`, "try again"} {
		if !strings.Contains(text, want) {
			t.Errorf("the report of the installation is %q, want it to mention %q", text, want)
		}
	}

	for _, action := range []serviceAction{serviceActionUpdate, serviceActionStart} {
		text := serviceActionFailureText(action, reason, program)
		for _, unwanted := range []string{program, `%ProgramFiles%`} {
			if strings.Contains(text, unwanted) {
				t.Errorf("the report of %q is %q, want no %q for a registration that already ran", action, text, unwanted)
			}
		}
	}

	// A caller that does not know the program still names the repair: the
	// guidance is not withheld because one path of the action has no program.
	unknown := serviceActionFailureText(serviceActionInstall, reason, "")
	if !strings.Contains(unknown, "If the program sits in a folder only you can read") {
		t.Errorf("the report without a program is %q, want the general form of the guidance", unknown)
	}
}

// TestServiceInstallFailureTextStatesWhatTheFailedInstallationLeft proves that
// the report of an installation whose service did not run states what happened
// to the registration.
//
// A failed first installation removes its own registration again, so the report
// says that HFDesk is not installed and that the desktop app starts normally
// again. A registration that was there before, or whose state before could not be
// read, stays, and the report names what remains and the elevated command that
// removes it. A removal that failed is reported with its reason and with the same
// command: the registration is still there.
func TestServiceInstallFailureTextStatesWhatTheFailedInstallationLeft(t *testing.T) {
	reason := errors.New("the service did not start")
	const (
		program = `C:\portable\hfdesk.exe`
		result  = "registered the service but could not start it"
	)

	removed := serviceInstallFailureText(reason, program, serviceInstallRemoved, nil)
	for _, want := range []string{
		result,
		reason.Error(),
		"not installed as a Windows service",
		"start the desktop app normally again",
	} {
		if !strings.Contains(removed, want) {
			t.Errorf("the report of the removed registration %q does not mention %q", removed, want)
		}
	}
	// A registration that was removed again is no program folder the user has
	// to check, and no registration to remove: the report stays with the
	// desktop app the user starts again.
	for _, unwanted := range []string{"hfdesk service uninstall", "%ProgramFiles%", program} {
		if strings.Contains(removed, unwanted) {
			t.Errorf("the report of the removed registration %q names %q, want neither a removal nor a folder of a registration that is gone", removed, unwanted)
		}
	}

	kept := serviceInstallFailureText(reason, program, serviceInstallKept, nil)
	for _, want := range []string{
		result,
		reason.Error(),
		"left in place",
		"stays installed, but it does not run",
		`"hfdesk service uninstall"`,
		"elevated shell",
		program,
		`%ProgramFiles%`,
	} {
		if !strings.Contains(kept, want) {
			t.Errorf("the report of the kept registration %q does not mention %q", kept, want)
		}
	}

	removalErr := errors.New("the registration is marked for deletion")
	failed := serviceInstallFailureText(reason, program, serviceInstallRemovalFailed, removalErr)
	for _, want := range []string{
		result,
		reason.Error(),
		"could not be removed again",
		removalErr.Error(),
		`"hfdesk service uninstall"`,
		"elevated shell",
		program,
		`%ProgramFiles%`,
	} {
		if !strings.Contains(failed, want) {
			t.Errorf("the report of the failed removal %q does not mention %q", failed, want)
		}
	}

	// A removal that was not attempted because the instance could not be
	// proven stopped says what remains instead of claiming that it stopped:
	// the service may still be running or coming up.
	skipped := serviceInstallFailureText(reason, program, serviceInstallKept, errServiceRemovalSkipped)
	for _, want := range []string{
		result,
		reason.Error(),
		"not removed",
		"may still be running, starting or stopping",
		"state could not be read",
		"stays in place",
		`"hfdesk service uninstall"`,
		"elevated shell",
		program,
		`%ProgramFiles%`,
	} {
		if !strings.Contains(skipped, want) {
			t.Errorf("the report of the skipped removal %q does not mention %q", skipped, want)
		}
	}
	if strings.Contains(skipped, "it does not run") {
		t.Errorf("the report of the skipped removal %q claims that the service stopped", skipped)
	}

	// The outcome of the installation is the whole state of the service, so
	// no report of it asks the user to read a state it just described, and no
	// report of it repeats the separate data of the desktop app.
	for _, outcome := range []struct {
		name string
		text string
	}{
		{"removed", removed},
		{"kept", kept},
		{"failed", failed},
		{"skipped", skipped},
	} {
		for _, unwanted := range []string{"hfdesk service status", "were not touched"} {
			if strings.Contains(outcome.text, unwanted) {
				t.Errorf("the report of the %s registration %q names %q, want the outcome and one action", outcome.name, outcome.text, unwanted)
			}
		}
	}
}

// TestServiceGateUnreadableTextStatesWhatTheLaunchDid proves that the notice of
// an unreadable registration names the risk, the launch that started no server
// and the way to read the state and to try again.
func TestServiceGateUnreadableTextStatesWhatTheLaunchDid(t *testing.T) {
	reason := errors.New("the service control manager did not answer")
	text := serviceGateUnreadableText(reason)
	for _, want := range []string{
		reason.Error(),
		"may be installed and running",
		"started no server",
		"hfdesk service status",
		"elevated shell",
		"start the desktop app again",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the notice %q does not mention %q", text, want)
		}
	}
}

// TestServiceLaunchFailedTextSaysWhatDidNotHappen proves that a launch the user
// refused is reported as an action that changed nothing.
func TestServiceLaunchFailedTextSaysWhatDidNotHappen(t *testing.T) {
	refused := errors.New(elevationDeclined)

	text := serviceLaunchFailedText(serviceActionInstall, refused)
	for _, want := range []string{elevationDeclined, "The service was not installed."} {
		if !strings.Contains(text, want) {
			t.Errorf("the report %q does not mention %q", text, want)
		}
	}
	if text := serviceLaunchFailedText(serviceActionUpdate, refused); !strings.Contains(text, "The service was not updated.") {
		t.Errorf("the report of the update %q does not say that nothing was updated", text)
	}
	if text := serviceLaunchFailedText(serviceActionStart, refused); !strings.Contains(text, "The service was not started.") {
		t.Errorf("the report of the start %q does not say that nothing was started", text)
	}
}

// TestServiceNotInstalledTextNamesTheWayToInstall proves that an action that
// needs a registration on a machine without one tells the user how to get one.
func TestServiceNotInstalledTextNamesTheWayToInstall(t *testing.T) {
	for _, action := range []serviceAction{serviceActionStart, serviceActionUpdate} {
		text := serviceNotInstalledText(action)
		for _, want := range []string{"not installed as a Windows service", "tray of the desktop app"} {
			if !strings.Contains(text, want) {
				t.Errorf("the report of %q is %q, want it to mention %q", action, text, want)
			}
		}
	}
}

// TestServiceSiblingMissingTextNamesTheFolder proves that a missing console
// program is reported with the folder the installation looked at and with the
// layout of the release, so the user can put the two programs back together.
func TestServiceSiblingMissingTextNamesTheFolder(t *testing.T) {
	const folder = `C:\portable`
	text := serviceSiblingMissingText(folder, errors.New("there is no hfdesk.exe in "+folder))

	for _, want := range []string{folder, "hfdesk.exe", "hfdesk-desktop.exe", "same folder"} {
		if !strings.Contains(text, want) {
			t.Errorf("the report %q does not mention %q", text, want)
		}
	}
}
