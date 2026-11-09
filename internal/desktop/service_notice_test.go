// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package desktop

import (
	"errors"
	"fmt"
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
// The dialog never offers the desktop mode: that answer belongs to the stopped
// service alone.
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
	if strings.Contains(text, "Work in desktop mode") {
		t.Errorf("the dialog %q offers the desktop mode next to a running service", text)
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
		t.Errorf("the start button is %q, want %q", got, want)
	}
}

// TestServiceGateTextOffersTheDesktopModeForAStoppedService proves that the
// dialog of a positively confirmed stopped service offers the desktop mode and
// states what that answer does: it starts the server of this app, never starts
// the service and leaves the registration alone. The dialog never claims an
// address for a service that does not serve, and it never carries the sentence
// of the dialogs that always end the launch, because this one can continue it.
func TestServiceGateTextOffersTheDesktopModeForAStoppedService(t *testing.T) {
	prompt := serviceChoicePrompt{
		State:   service.StateStopped,
		Listen:  "127.0.0.1:8080",
		DataDir: `C:\ProgramData\HFDesk`,
	}

	text := serviceGateText(prompt)
	for _, want := range []string{
		"installed, but it is not running",
		"Work in desktop mode continues this launch as a normal desktop app",
		"never starts the service",
		"service stays stopped",
		"stopped service stays stopped",
		prompt.DataDir,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the dialog %q does not mention %q", text, want)
		}
	}
	for _, unwanted := range []string{
		// The service does not serve, so no address is offered.
		"The web UI of the service is at",
		// This dialog can continue the launch, so the sentence of the
		// dialogs that always end it does not belong to it.
		"started no server of its own",
	} {
		if strings.Contains(text, unwanted) {
			t.Errorf("the dialog %q carries %q", text, unwanted)
		}
	}
	if got, want := serviceGateDesktopLabel, "Work in desktop mode"; got != want {
		t.Errorf("the desktop button is %q, want %q", got, want)
	}
}

// TestServicePromptStoppedRecognifiesOnlyAConfirmedStoppedService proves the
// classification the desktop mode depends on: the observed stopped state
// without the running fact. Every other state keeps its gate, and a
// registration whose state could not be read never builds a prompt at all —
// the gate reports it before the dialog exists, so an error is never read as a
// stopped service or as a machine without one.
func TestServicePromptStoppedRecognizesOnlyAConfirmedStoppedService(t *testing.T) {
	for _, tc := range []struct {
		name   string
		prompt serviceChoicePrompt
		want   bool
	}{
		{name: "stopped", prompt: serviceChoicePrompt{State: service.StateStopped}, want: true},
		// The supervisor's running fact wins over the diagnosed state, so
		// a stopped state whose instance still serves is never continued.
		{name: "stopped but the running fact is set", prompt: serviceChoicePrompt{State: service.StateStopped, Running: true}},
		{name: "running", prompt: serviceChoicePrompt{State: service.StateRunning}},
		{name: "starting", prompt: serviceChoicePrompt{State: service.StateStarting}},
		{name: "stopping", prompt: serviceChoicePrompt{State: service.StateStopping}},
		{name: "failed", prompt: serviceChoicePrompt{State: service.StateFailed}},
		{name: "broken", prompt: serviceChoicePrompt{State: service.StateBroken}},
		{name: "broken but running", prompt: serviceChoicePrompt{State: service.StateBroken, Running: true}},
		{name: "not installed", prompt: serviceChoicePrompt{State: service.StateNotInstalled}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := servicePromptStopped(tc.prompt); got != tc.want {
				t.Errorf("servicePromptStopped of the %s prompt = %t, want %t", tc.name, got, tc.want)
			}
		})
	}
}

// TestServiceGateFallbackChoicesMapTheAnswers proves that the message box
// fallback names the actions its unnamed buttons perform.
//
// A message box has no named buttons: without the mapping the user would have
// to guess which answer starts the service and which one updates it. The
// positively confirmed stopped service has four actions but a box has three
// answers, so it is asked in two boxes: the first maps the desktop mode, a
// service action and no action, and the second names the two service actions.
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
	if strings.Contains(running, "desktop mode") {
		t.Errorf("the fallback %q offers the desktop mode next to a running service", running)
	}

	stopped := serviceGateFallbackChoices(serviceChoicePrompt{State: service.StateStopped})
	for _, want := range []string{
		"Choose Yes to work in desktop mode.",
		"Choose No to start or update the service.",
		"Choose Cancel to change nothing.",
	} {
		if !strings.Contains(stopped, want) {
			t.Errorf("the first box of the stopped fallback %q does not mention %q", stopped, want)
		}
	}

	// The second box of the stopped fallback keeps the two service actions
	// reachable with the very mapping the ordinary fallback of a service
	// that does not run asks about.
	action := serviceGateStartUpdateChoices()
	for _, want := range []string{
		"Choose Yes to start the service.",
		"Choose No to update the service.",
		"Choose Cancel to change nothing.",
	} {
		if !strings.Contains(action, want) {
			t.Errorf("the second box of the stopped fallback %q does not mention %q", action, want)
		}
	}
	second := serviceGateServiceActionChooseText(serviceChoicePrompt{State: service.StateStopped})
	if !strings.Contains(second, "installed, but it is not running") {
		t.Errorf("the second box %q does not name the state it acts on", second)
	}
	if !strings.Contains(second, action) {
		t.Errorf("the second box %q does not carry its mapping", second)
	}

	// A failed service is not a positively confirmed stopped one: its
	// dialog ends the launch and never offers the desktop mode.
	failed := serviceGateFallbackChoices(serviceChoicePrompt{State: service.StateFailed})
	if !strings.Contains(failed, "Choose Yes to start the service.") {
		t.Errorf("the fallback %q does not offer to start the service", failed)
	}
	if strings.Contains(failed, "desktop mode") {
		t.Errorf("the fallback %q offers the desktop mode for a failed service", failed)
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
	text := serviceUpdatedText("1.3.0", `C:\portable\hfdesk.exe`, `C:\ProgramData\HFDesk`, address, false, service.StateStopped)

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
	text := serviceUpdatedText("1.3.0", `C:\portable\hfdesk.exe`, `C:\ProgramData\HFDesk`, address, true, service.StateRunning)

	for _, want := range []string{address, "not needed to use the service", `C:\portable\hfdesk.exe`, "keeps the settings"} {
		if !strings.Contains(text, want) {
			t.Errorf("the report %q does not mention %q", text, want)
		}
	}
}

// TestServiceUpdatedTextNamesTheObservedPriorState proves that the report of a
// successful update that did not leave the service running carries both
// established facts of that state: it names the prior state the update
// actually observed instead of restating it as "stopped", and it says that the
// not-running service stays stopped.
//
// An update over a registration whose state was "failed", or "broken" without a
// running instance, rewrites the registration and starts nothing. The report of
// that update claimed "it was stopped before the update" — a state the machine
// never had — although the helper tracked the observed state. The report keeps
// the shape of these notices (result, cause, one next action), quotes the
// observed state the way the failure reports of these actions already do, and
// keeps both facts of the not-running outcome: the observed prior state and
// that the update leaves a service that was not running not running.
//
// The forbidden claim is the false prior state — that this registration was
// observed stopped — and not the word "stopped": the established Windows
// oracle of these reports requires the sentence that the service stays
// stopped, and a matcher that forbade the word would contradict it.
func TestServiceUpdatedTextNamesTheObservedPriorState(t *testing.T) {
	const address = "http://127.0.0.1:8080"
	for _, tc := range []struct {
		name   string
		before service.State
		quote  string
	}{
		{name: "failed", before: service.StateFailed, quote: `it was "failed" before the update`},
		{name: "broken", before: service.StateBroken, quote: `it was "broken" before the update`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := serviceUpdatedText("1.3.0", `C:\portable\hfdesk.exe`, `C:\ProgramData\HFDesk`, address, false, tc.before)

			for _, want := range []string{
				tc.quote,
				"is not running after the update",
				"never starts a service that was not running",
				"stays stopped",
				"Nothing serves HFDesk right now",
				"Start the desktop app again and choose Start service",
				"hfdesk service start",
				"elevated shell",
			} {
				if !strings.Contains(text, want) {
					t.Errorf("the report %q does not mention %q", text, want)
				}
			}
			// The only false claim this report must never carry is the
			// prior state it did not observe: this registration was
			// %q, never stopped.
			if strings.Contains(text, "it was stopped before the update") {
				t.Errorf("the report %q claims the service was stopped before the update, want the observed prior state %q", text, tc.before)
			}
			if strings.Contains(text, address) {
				t.Errorf("the report %q sends the user to the address of a service that does not run", text)
			}
		})
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

// TestServiceActionFailureTextOfAnAlreadyRegisteredFirstInstall proves that a
// first-install refusal of an existing registration is reported as the
// unchanged state it left: this attempt wrote no registration, so the report
// carries neither the may-remain warning nor the removal of a registration it
// never touched, and it names the next actions the state really offers.
//
// The refusals are wrapped exactly like the manager wraps them, marker
// included, so the report is exercised with the error the helper really
// receives; the rendered detail stays byte-identical to the manager message.
func TestServiceActionFailureTextOfAnAlreadyRegisteredFirstInstall(t *testing.T) {
	const program = `C:\portable\hfdesk.exe`
	refusals := []error{
		fmt.Errorf("service: %s is already registered; the first-install action will not change or start it%w", service.ServiceName, service.ErrAlreadyRegistered),
		fmt.Errorf("service: %s appeared during the first installation; no existing registration was changed%w", service.ServiceName, service.ErrAlreadyRegistered),
	}

	// The marker adds nothing to the message the user reads: the rendered
	// refusal stays byte-identical to the manager message.
	for i, want := range []string{
		"service: %s is already registered; the first-install action will not change or start it",
		"service: %s appeared during the first installation; no existing registration was changed",
	} {
		if got, want := refusals[i].Error(), fmt.Sprintf(want, service.ServiceName); got != want {
			t.Errorf("the rendered refusal %q is not byte-identical to the manager message %q", got, want)
		}
	}

	for _, err := range refusals {
		message := err.Error()
		text := serviceActionFailureText(serviceActionInstall, err, program)
		for _, want := range []string{
			message,
			"The service registration already exists, and this attempt did not create, change or start it.",
			"Start HFDesk again",
			"startup gate offers to open or start the service and to update it",
			`"hfdesk service install"`,
			"elevated shell",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("the report %q does not mention %q", text, want)
			}
		}
		for _, unwanted := range []string{
			"A registration may remain",
			`"hfdesk service uninstall"`,
			"do not want to keep the registration",
			// The folder guidance is the guidance of a failure whose cause is
			// unknown: the refusal states its own cause.
			program,
			`NT AUTHORITY\LocalService`,
		} {
			if strings.Contains(text, unwanted) {
				t.Errorf("the report %q carries %q although this attempt changed no registration", text, unwanted)
			}
		}
		// One report, one place to act from: the state command of an
		// unobservable outcome does not belong to a refusal that states the
		// outcome itself.
		if strings.Contains(text, "hfdesk service status") {
			t.Errorf("the report %q names a state command next to the actions of an unchanged registration", text)
		}
	}

	// The classification is structural: the marker classifies any wording, so
	// a reworded refusal still gets the report of an unchanged registration.
	reworded := fmt.Errorf("the registration of %s already exists%w", service.ServiceName, service.ErrAlreadyRegistered)
	rewordedText := serviceActionFailureText(serviceActionInstall, reworded, program)
	for _, want := range []string{
		reworded.Error(),
		"did not create, change or start it",
		`"hfdesk service install"`,
	} {
		if !strings.Contains(rewordedText, want) {
			t.Errorf("the report %q does not mention %q", rewordedText, want)
		}
	}
	for _, unwanted := range []string{"A registration may remain", `"hfdesk service uninstall"`} {
		if strings.Contains(rewordedText, unwanted) {
			t.Errorf("the report %q of a reworded refusal carries %q although this attempt changed no registration", rewordedText, unwanted)
		}
	}

	// The text markers are gone: the same clause without the marker no longer
	// classifies, so a wording match cannot creep back unnoticed.
	unmarked := serviceActionFailureText(serviceActionInstall,
		errors.New("service: hfdesk is already registered; the first-install action will not change or start it"), program)
	if !strings.Contains(unmarked, "A registration may remain.") {
		t.Errorf("the report %q of an unmarked error swallowed the class of a failure that may have written a registration", unmarked)
	}

	// A failure of the installation that is not this refusal keeps the
	// wording of a report that may have written a registration: the class
	// above must not swallow it.
	generic := serviceActionFailureText(serviceActionInstall,
		errors.New("the service control manager refused the registration"), program)
	for _, want := range []string{"A registration may remain.", `"hfdesk service uninstall"`} {
		if !strings.Contains(generic, want) {
			t.Errorf("the generic report %q does not mention %q", generic, want)
		}
	}

	// A report without an error never classifies as a refusal: errors.Is on a
	// nil error must keep the guard the old matcher had explicitly.
	if isAlreadyRegisteredRefusal(nil) {
		t.Error("a nil error was classified as a first-install refusal")
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
// and the access the folder of that program needs.
//
// The sentence states the requirement and does not tell the user to move the
// program: the Windows service runs as LocalService, so that account must be able
// to execute the program and to traverse its folder; read access is demanded only
// where a handler genuinely needs it. An update and a start report
// leave the folder out: they describe a registration that already ran, so the
// folder of the program is not the likely cause there.
func TestServiceActionFailureTextNamesTheFolderOfTheProgram(t *testing.T) {
	reason := errors.New("the service control manager refused the registration")
	const program = `C:\portable\hfdesk.exe`

	text := serviceActionFailureText(serviceActionInstall, reason, program)
	for _, want := range []string{program, `NT AUTHORITY\LocalService`, "must be able to execute", "traverse the folder"} {
		if !strings.Contains(text, want) {
			t.Errorf("the report of the installation is %q, want it to mention %q", text, want)
		}
	}

	for _, action := range []serviceAction{serviceActionUpdate, serviceActionStart} {
		text := serviceActionFailureText(action, reason, program)
		for _, unwanted := range []string{program, `NT AUTHORITY\LocalService`} {
			if strings.Contains(text, unwanted) {
				t.Errorf("the report of %q is %q, want no %q for a registration that already ran", action, text, unwanted)
			}
		}
	}

	// A caller that does not know the program still names the requirement: the
	// guidance is not withheld because one path of the action has no program.
	unknown := serviceActionFailureText(serviceActionInstall, reason, "")
	if !strings.Contains(unknown, "must be able to execute the program and to traverse the folder that holds it") {
		t.Errorf("the report without a program is %q, want the general form of the guidance", unknown)
	}
}

// TestServiceInstallFailureTextStatesWhatTheFailedInstallationLeft proves that
// the report of an installation whose service did not start states what the
// cleanup observed.
//
// A registration this installation created and removed is gone; one it did not
// create, could not read or could not remove stays and is named with the elevated
// command, without ever claiming that the service does not run.
func TestServiceInstallFailureTextStatesWhatTheFailedInstallationLeft(t *testing.T) {
	reason := errors.New("the health endpoint reports another version")
	const program = `C:\portable\hfdesk.exe`
	removed := serviceInstallStartFailureText(reason, program, &service.InstallStartError{Cause: reason, RegistrationRemoved: true})
	for _, want := range []string{reason.Error(), "registration created by this attempt was removed", "start the desktop app normally again"} {
		if !strings.Contains(removed, want) {
			t.Errorf("the removed-install report %q does not mention %q", removed, want)
		}
	}
	for _, unwanted := range []string{"hfdesk service uninstall", program, `NT AUTHORITY\LocalService`} {
		if strings.Contains(removed, unwanted) {
			t.Errorf("the removed-install report %q names %q although nothing remains to inspect", removed, unwanted)
		}
	}

	cleanupErr := errors.New("the service process identity changed")
	cleanup := &service.InstallStartError{Cause: reason, CleanupError: cleanupErr}
	kept := serviceInstallStartFailureText(cleanup, program, cleanup)
	for _, want := range []string{reason.Error(), cleanupErr.Error(), "registration was kept", "could not be proven", "hfdesk service status", program} {
		if !strings.Contains(kept, want) {
			t.Errorf("the kept-install report %q does not mention %q", kept, want)
		}
	}
	if strings.Contains(kept, "the service does not run") {
		t.Errorf("the kept-install report %q claims more than the cleanup established", kept)
	}

	removeErr := errors.New("SCM denied deletion")
	stoppedButKept := &service.InstallStartError{
		Cause:             reason,
		RegistrationOwned: true,
		ProcessStopped:    true,
		CleanupError:      removeErr,
	}
	removeText := serviceInstallStartFailureText(stoppedButKept, program, stoppedButKept)
	for _, want := range []string{removeErr.Error(), "process was proven stopped", "could not be removed"} {
		if !strings.Contains(removeText, want) {
			t.Errorf("the stopped-but-kept report %q does not mention %q", removeText, want)
		}
	}
	if strings.Contains(removeText, "ownership could not be proven") {
		t.Errorf("the stopped-but-kept report %q misstates registration ownership", removeText)
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

// TestServiceSingletonHoldRefusedTextStatesNoActionAndOneCommand proves the
// report of a mutation that refused before it changed anything because the
// desktop singleton could not be held: it states that no service action was
// performed, carries the cause of the failed hold and exactly one next
// action.
//
// A refusal because another Windows identity owns the object already names
// its own step inside the cause, so it carries no second command; every other
// cause ends with the command that reads the state, and no report carries
// both.
func TestServiceSingletonHoldRefusedTextStatesNoActionAndOneCommand(t *testing.T) {
	cause := errors.New("the per-session desktop singleton cannot be named: the session of the process cannot be read")
	for _, action := range []serviceAction{serviceActionInstall, serviceActionUpdate, serviceActionStart} {
		text := serviceSingletonHoldRefusedText(action, cause)
		for _, want := range []string{
			serviceActionFailureHeadline(action),
			"No service action was performed",
			cause.Error(),
			`"hfdesk service status"`,
		} {
			if !strings.Contains(text, want) {
				t.Errorf("the report of %q %q does not mention %q", action, text, want)
			}
		}
	}

	denied := fmt.Errorf(
		"the desktop singleton %q exists but this Windows identity cannot access it: another HFDesk process under a different Windows account created it; end that process and try again%w",
		`Local\HFDesk.Desktop.test.1`, service.ErrSingletonAccessDenied)
	text := serviceSingletonHoldRefusedText(serviceActionUpdate, denied)
	for _, want := range []string{
		serviceActionFailureHeadline(serviceActionUpdate),
		"No service action was performed",
		"this Windows identity cannot access it",
		"end that process and try again",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the identity refusal %q does not mention %q", text, want)
		}
	}
	if strings.Contains(text, "hfdesk service status") {
		t.Errorf("the identity refusal %q carries a second next action next to its own step", text)
	}
}

// TestServiceActionFailureTextOfAChangedAccountIsOneReport proves the report
// of an update that refused over a changed account: the headline names the
// action, the message of the service layer carries the result, the cause and
// the command that restores the supported identity — and the report adds no
// second next action next to it.
func TestServiceActionFailureTextOfAChangedAccountIsOneReport(t *testing.T) {
	accountErr := fmt.Errorf(
		"service: the registration of hfdesk runs as \"%s\", and an HFDesk update keeps the supported identity %s: "+
			"nothing was changed and the service was not stopped. Restore the supported identity from an elevated "+
			`shell by running: sc config hfdesk obj= "NT AUTHORITY\LocalService" password= "" — then run the installation again%w`,
		`CONTOSO\svc-other`, `NT AUTHORITY\LocalService`, service.ErrServiceAccountMismatch)

	text := serviceActionFailureText(serviceActionUpdate, accountErr, `C:\portable\hfdesk.exe`)
	for _, want := range []string{
		serviceActionFailureHeadline(serviceActionUpdate),
		`runs as "CONTOSO\svc-other"`,
		"nothing was changed and the service was not stopped",
		`sc config hfdesk obj= "NT AUTHORITY\LocalService" password= ""`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the report %q does not mention %q", text, want)
		}
	}
	if strings.Contains(text, "hfdesk service status") {
		t.Errorf("the report %q carries the state command next to the restore command, want one next action", text)
	}
	if strings.Contains(text, "A registration may remain") || strings.Contains(text, "hfdesk service uninstall") {
		t.Errorf("the report %q claims a change the refusal never made", text)
	}

	// An ordinary update failure keeps its state command: only the
	// classified account refusal carries its own action.
	ordinary := serviceActionFailureText(serviceActionUpdate,
		errors.New("the service control manager refused the registration"), `C:\portable\hfdesk.exe`)
	if !strings.Contains(ordinary, "hfdesk service status") {
		t.Errorf("the ordinary update report %q lost its state command", ordinary)
	}
}

// TestServiceHelperNotReadyTextIsOneCoherentReport proves the report of a
// helper that started but never became ready is one truthful message: it
// states the result and why it follows from the readiness rule, carries the
// observed cause verbatim, references the reason the helper reports in its
// own box instead of repeating or contradicting it, and names one next
// action. It never claims the helper could not be started — that is the
// report of a launch that never ran, whose caption it must not share.
func TestServiceHelperNotReadyTextIsOneCoherentReport(t *testing.T) {
	earlyExit := &helperNotReadyError{cause: errors.New("desktop: the service helper exited before it became ready")}
	// The report carries the reason of the helper wait as it is: a renamed
	// or paraphrased cause would be the contradiction this report fixes.
	if got := earlyExit.Error(); got != "desktop: the service helper exited before it became ready" {
		t.Errorf("helperNotReadyError.Error() = %q, want the observed cause verbatim", got)
	}
	if serviceHelperNotReadyCaption == serviceFailureCaption(serviceActionInstall) {
		t.Errorf("the caption of the not-ready report %q is the caption of the never-ran report, want a distinct one",
			serviceHelperNotReadyCaption)
	}

	for _, action := range []serviceAction{serviceActionInstall, serviceActionUpdate, serviceActionStart} {
		text := serviceHelperNotReadyText(action, earlyExit)
		for _, want := range []string{
			serviceActionUnchangedText(action),
			"No service action was performed",
			"the helper acts only after it becomes ready",
			"desktop: the service helper exited before it became ready",
			"shows its own message with the reason",
			"If that message appeared, address what it names, then repeat the action",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("the report of %q %q does not mention %q", action, text, want)
			}
		}
		if strings.Contains(text, "could not start its administrator helper") {
			t.Errorf("the report of %q claims the helper could not be started although it was started", action)
		}
	}

	timeout := serviceHelperNotReadyText(serviceActionUpdate,
		&helperNotReadyError{cause: errors.New("desktop: the service helper did not become ready within 1m0s")})
	if !strings.Contains(timeout, "did not become ready within 1m0s") {
		t.Errorf("the report %q does not carry the observed cause of the timeout", timeout)
	}
}

// TestServiceLaunchFailedTextKeepsTheNeverRanReport pins the report of a
// helper that never ran: the wording a declined user account control prompt
// and every other failed launch produce stays exactly as it is, so the two
// states of the helper remain distinguishable by their text alone.
func TestServiceLaunchFailedTextKeepsTheNeverRanReport(t *testing.T) {
	text := serviceLaunchFailedText(serviceActionInstall, errors.New(elevationDeclined))
	if !strings.HasPrefix(text, "HFDesk could not start its administrator helper.") {
		t.Errorf("the report %q does not keep the head of a launch that never ran", text)
	}
	for _, want := range []string{elevationDeclined, "The service was not installed."} {
		if !strings.Contains(text, want) {
			t.Errorf("the report %q does not mention %q", text, want)
		}
	}
}
