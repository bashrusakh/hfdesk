// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package desktop

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

	assertNames(t, text, "the confirmation", []string{
		"starts at boot",
		"keeps running after this window closes",
		"administrator rights",
		"Windows prompt",
		"The one file HFDesk can download",
		"only after its SHA-256 is verified",
		"nothing else is downloaded or copied",
		"Its data is separate",
		"settings, jobs and models",
		dataDir,
		"are not there",
	})
	// The narrowed rule must not read as the old blanket claim: the
	// desktop app CAN download one file now, so "nothing is downloaded or
	// copied" without the qualifier is the wording that must be gone.
	if strings.Contains(text, ": nothing is downloaded or copied") ||
		strings.Contains(text, "HFDesk stays portable: nothing is downloaded or copied") {
		t.Errorf("the confirmation %q still carries the old blanket claim", text)
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
	assertNames(t, text, "the dialog", []string{
		"The HFDesk service is running.",
		"Web UI: http://127.0.0.1:8080",
		"Its data is separate",
		prompt.DataDir,
	})
	// The plan's target for this layout: the state, the address and the one
	// surprising fact — no mechanism, no justification beside them.
	if strings.Contains(text, "Work in desktop mode") {
		t.Errorf("the dialog %q offers the desktop mode next to a running service", text)
	}
	assertNamesWithout(t, text, "the running dialog", nil, []string{
		"two instances never compete",
		"Update re-registers",
	})

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
// states what that answer does: it starts the server of this app and leaves
// the service stopped. The dialog never claims an address for a service that
// does not serve, and it never carries the sentence of the dialogs that
// always end the launch, because this one can continue it.
func TestServiceGateTextOffersTheDesktopModeForAStoppedService(t *testing.T) {
	prompt := serviceChoicePrompt{
		State:   service.StateStopped,
		Listen:  "127.0.0.1:8080",
		DataDir: `C:\ProgramData\HFDesk`,
	}

	text := serviceGateText(prompt)
	assertNames(t, text, "the dialog", []string{
		"installed, but it is not running",
		"Work in desktop mode starts this app's own server",
		"the service stays stopped",
		"Its data is separate",
		prompt.DataDir,
	})
	assertNamesWithout(t, text, "the dialog", nil, []string{
		// The service does not serve, so no address is offered.
		"The web UI of the service is at",
		// This dialog can continue the launch, so the sentence of the
		// dialogs that always end it does not belong to it, and the
		// text argues with no mechanism beside the one continuing
		// answer.
		"started no server",
		"two instances never compete",
		"Update re-registers",
	})
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
	assertNames(t, running, "the fallback", []string{
		"Choose Yes to open HFDesk in your default browser.",
		"Choose No to update the service.",
		"Choose Cancel to change nothing.",
	})
	if strings.Contains(running, "desktop mode") {
		t.Errorf("the fallback %q offers the desktop mode next to a running service", running)
	}

	stopped := serviceGateFallbackChoices(serviceChoicePrompt{State: service.StateStopped})
	assertNames(t, stopped, "the first box of the stopped fallback", []string{
		"Choose Yes to work in desktop mode.",
		"Choose No to start or update the service.",
		"Choose Cancel to change nothing.",
	})

	// The second box of the stopped fallback keeps the two service actions
	// reachable with the very mapping the ordinary fallback of a service
	// that does not run asks about.
	action := serviceGateStartUpdateChoices()
	assertNames(t, action, "the second box of the stopped fallback", []string{
		"Choose Yes to start the service.",
		"Choose No to update the service.",
		"Choose Cancel to change nothing.",
	})
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
		for _, want := range []string{tc.want, "changes nothing", "starts no server", "Start the desktop app again when the service has settled"} {
			if !strings.Contains(text, want) {
				t.Errorf("the notice of %q is %q, want it to mention %q", tc.state, text, want)
			}
		}
		// The notice carries one next action: the relaunch of the
		// desktop app, never an alternative way to read the state.
		for _, unwanted := range []string{" or ", "hfdesk service status"} {
			if strings.Contains(text, unwanted) {
				t.Errorf("the notice of %q is %q, want no alternative action %q", tc.state, text, unwanted)
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

	assertNames(t, text, "the report", []string{
		"HFDesk 1.3.0",
		`C:\portable\hfdesk.exe`,
		"stays stopped",
		"Nothing serves HFDesk right now",
		"Start the desktop app again and choose Start service",
	})
	// One report, one route: the relaunch of the desktop app is the single
	// named action, and no elevated command line is offered next to it.
	assertNamesWithout(t, text, "the report", nil, []string{"hfdesk service start", "elevated shell", " or "})
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

	assertNames(t, text, "the report", []string{address, "not needed to use the service", `C:\portable\hfdesk.exe`, "keeps the settings"})
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

			assertNames(t, text, "the report", []string{
				tc.quote,
				"is not running after the update",
				"stays stopped",
				"Nothing serves HFDesk right now",
				"Start the desktop app again and choose Start service",
			})
			// One report, one route: no elevated command line and no
			// alternative beside the relaunch of the desktop app.
			assertNamesWithout(t, text, "the report", nil, []string{"hfdesk service start", "elevated shell", " or "})
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

	assertNames(t, text, "the report", []string{
		"HFDesk 1.3.0",
		console,
		"starts at boot",
		address,
		"not needed to use the service",
		dataDir,
		"do not appear in the service",
	})

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
		assertNames(t, text, "the report", []string{
			message,
			"The service registration already exists, and this attempt did not create, change or start it.",
			"Start HFDesk again",
			"startup gate offers to open or start the service and to update it",
		})
		assertNamesWithout(t, text, "the report", nil, []string{
			"A registration may remain",
			`"hfdesk service uninstall"`,
			"do not want to keep the registration",
			// The startup gate is the single next action: the elevated
			// installation command beside it would be a second route.
			`"hfdesk service install"`,
			"elevated shell",
			// The folder guidance is the guidance of a failure whose cause is
			// unknown: the refusal states its own cause.
			program,
			`NT AUTHORITY\LocalService`,
		})
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
	assertNames(t, rewordedText, "the report", []string{
		reworded.Error(),
		"did not create, change or start it",
		"startup gate",
	})
	assertNamesWithout(t, rewordedText, "the report", nil, []string{
		"A registration may remain",
		`"hfdesk service uninstall"`,
		`"hfdesk service install"`,
		"elevated shell",
	})

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
	assertNames(t, generic, "the generic report", []string{"A registration may remain.", `"hfdesk service status"`})
	if strings.Contains(generic, `"hfdesk service uninstall"`) {
		t.Errorf("the generic report %q carries the removed conditional action", generic)
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
// after it was written. Every action names the command that reads the state it
// could not observe, and no report carries a second command or a conditional
// one beside it.
//
// The separate data of the desktop app is part of the confirmation and of the
// gate, not of a failure report: the report is result, cause and one action.
func TestServiceActionFailureTextIsTheResultTheCauseAndOneAction(t *testing.T) {
	reason := errors.New("the service control manager refused the registration")
	const program = `C:\portable\hfdesk.exe`

	install := serviceActionFailureText(serviceActionInstall, reason, program)
	assertNames(t, install, "the report", []string{
		reason.Error(),
		"A registration may remain.",
		`"hfdesk service status"`,
		"elevated shell",
	})
	// The report states a possibility, never a fact: a failed installation
	// may have written a registration, and it may not have.
	if strings.Contains(install, "A registration is installed") || strings.Contains(install, "a registration remains") {
		t.Errorf("the report %q claims that a registration exists", install)
	}
	// One report carries one command: reading the state resolves the
	// uncertainty the report states, so the conditional removal must not
	// sit beside it.
	assertNamesWithout(t, install, "the report", nil, []string{`"hfdesk service uninstall"`, "do not want to keep the registration"})
	if got := strings.Count(install, "hfdesk service"); got != 1 {
		t.Errorf("the report %q names %d commands, want exactly one", install, got)
	}
	if strings.Contains(install, "were not touched") {
		t.Errorf("the report %q repeats the separate data of the desktop app", install)
	}

	update := serviceActionFailureText(serviceActionUpdate, reason, program)
	assertNames(t, update, "the report", []string{reason.Error(), "hfdesk service status"})
	assertNamesWithout(t, update, "the report of the update", nil, []string{"registration may remain", "hfdesk service uninstall", "were not touched"})

	start := serviceActionFailureText(serviceActionStart, reason, program)
	assertNames(t, start, "the report", []string{reason.Error(), "The start was not confirmed.", "hfdesk service status"})
	assertNamesWithout(t, start, "the report of the start", nil, []string{"registration may remain", "hfdesk service uninstall", "were not touched"})
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
	assertNames(t, text, "the report of the installation is", []string{program, `NT AUTHORITY\LocalService`, "must be able to execute", "traverse the folder"})

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
	assertNames(t, removed, "the removed-install report", []string{reason.Error(), "registration created by this attempt was removed", "start the desktop app normally again"})
	assertNamesWithout(t, removed, "the removed-install report", nil, []string{"hfdesk service uninstall", program, `NT AUTHORITY\LocalService`})

	cleanupErr := errors.New("the service process identity changed")
	cleanup := &service.InstallStartError{Cause: reason, CleanupError: cleanupErr}
	kept := serviceInstallStartFailureText(cleanup, program, cleanup)
	assertNames(t, kept, "the kept-install report", []string{reason.Error(), cleanupErr.Error(), "registration was kept", "could not prove", "hfdesk service status", program})
	if strings.Contains(kept, "the service does not run") {
		t.Errorf("the kept-install report %q claims more than the cleanup established", kept)
	}
	// One report, one action: reading the state resolves the uncertainty
	// the report states, so neither a removal menu nor a second imperative
	// may sit beside the state command.
	assertNamesWithout(t, kept, "the kept-install report", nil, []string{
		`"hfdesk service uninstall"`,
		"do not want to keep the registration",
		"Read the service state before taking another action",
	})
	if got := strings.Count(kept, "hfdesk service"); got != 1 {
		t.Errorf("the kept-install report %q names %d commands, want exactly one", kept, got)
	}

	removeErr := errors.New("SCM denied deletion")
	stoppedButKept := &service.InstallStartError{
		Cause:             reason,
		RegistrationOwned: true,
		ProcessStopped:    true,
		CleanupError:      removeErr,
	}
	removeText := serviceInstallStartFailureText(stoppedButKept, program, stoppedButKept)
	assertNames(t, removeText, "the stopped-but-kept report", []string{removeErr.Error(), "process was proven stopped", "could not be removed", "hfdesk service status"})
	if strings.Contains(removeText, "ownership could not be proven") {
		t.Errorf("the stopped-but-kept report %q misstates registration ownership", removeText)
	}
	assertNamesWithout(t, removeText, "the stopped-but-kept report", nil, []string{`"hfdesk service uninstall"`, "do not want to keep the registration"})
	if got := strings.Count(removeText, "hfdesk service"); got != 1 {
		t.Errorf("the stopped-but-kept report %q names %d commands, want exactly one", removeText, got)
	}
}

// TestServiceInstallStartFailureTextSplitsTheStartOutcomeClasses proves that
// the report of a failed first start names the class of the start outcome
// instead of a generic "could not confirm", and that the not-ready class
// names the budget that actually elapsed.
//
// A start that was not performed states that in its headline and never claims
// a start could not be confirmed; a start that ran carries the one observed
// line — the process ended before it came up, the service did not come up
// within the applied budget, or the started service could not be verified.
// The budget shown is the value the failing wait applied, so a different
// applied budget must show a different number. An error classified by no site
// keeps the older cause-only report.
func TestServiceInstallStartFailureTextSplitsTheStartOutcomeClasses(t *testing.T) {
	program := `C:\portable\hfdesk.exe`
	cause := errors.New("the first installation did not put the new executable into service")
	kept := func(outcome service.StartOutcome, budget time.Duration) *service.InstallStartError {
		return &service.InstallStartError{
			Cause:        cause,
			Outcome:      outcome,
			Budget:       budget,
			CleanupError: errors.New("the registration was kept"),
		}
	}

	notPerformed := serviceInstallStartFailureText(cause, program, kept(service.StartOutcomeNotPerformed, 0))
	assertNamesWithout(t, notPerformed, "the not-performed report",
		[]string{"HFDesk registered the service but did not start it.", cause.Error(), "registration was kept"},
		[]string{"could not confirm"})

	ended := serviceInstallStartFailureText(cause, program, kept(service.StartOutcomeEnded, 0))
	assertNames(t, ended, "the ended report",
		[]string{"could not confirm its start", "ended before it came up", "registration was kept"})

	notReady := serviceInstallStartFailureText(cause, program, kept(service.StartOutcomeNotReady, 45*time.Second))
	assertNames(t, notReady, "the not-ready report", []string{"did not come up within 45s"})
	otherBudget := serviceInstallStartFailureText(cause, program, kept(service.StartOutcomeNotReady, 48*time.Second))
	assertNamesWithout(t, otherBudget, "the not-ready report with another applied budget",
		[]string{"did not come up within 48s"}, []string{"45s"})

	// Even a mis-stamped budget never renders for another class: only the
	// not-ready line may name a budget, so an interrupted or unverifiable
	// wait can never claim "within <budget>" at the text layer.
	unverified := serviceInstallStartFailureText(cause, program, kept(service.StartOutcomeUnverified, 45*time.Second))
	assertNamesWithout(t, unverified, "the unverified report",
		[]string{"could not verify the started service"}, []string{"did not come up within", "within 45s"})

	legacyErr := &service.InstallStartError{Cause: cause, CleanupError: errors.New("ownership unproven")}
	legacy := serviceInstallStartFailureText(legacyErr, program, legacyErr)
	assertNamesWithout(t, legacy, "the unclassified report", []string{cause.Error(), "ownership unproven"},
		[]string{"ended before", "did not come up", "could not verify the started service", "did not start it"})
}

// TestServiceInstallStartFailureTextDistinguishesRemovedFromNeverInstalled
// proves that a registration this attempt removed after a failed start is
// never worded like a machine that never had one: the removed report says
// what the attempt removed and names its one action, while the never-installed
// report is the only one that claims HFDesk is not installed at all.
func TestServiceInstallStartFailureTextDistinguishesRemovedFromNeverInstalled(t *testing.T) {
	program := `C:\portable\hfdesk.exe`
	cause := errors.New("the service control manager refused the start")

	removed := serviceInstallStartFailureText(cause, program, &service.InstallStartError{
		Cause:               cause,
		RegistrationRemoved: true,
		RegistrationOwned:   true,
		ProcessStopped:      true,
		Outcome:             service.StartOutcomeNotPerformed,
	})
	assertNamesWithout(t, removed, "the removed-after-failure report",
		[]string{"could not start it", "registration created by this attempt was removed",
			"left no registration behind", "start the desktop app normally again"},
		[]string{"HFDesk is not installed as a Windows service", "There is no service"})

	never := serviceNotInstalledText(serviceActionStart)
	assertNames(t, never, "the never-installed report",
		[]string{"HFDesk is not installed as a Windows service", "Install it from the tray of the desktop app first"})

	// The removed branch carries the start class too: a registration this
	// attempt removed after a service that never came up within its budget
	// states both facts, and still never claims the machine never had one.
	removedNotReady := serviceInstallStartFailureText(cause, program, &service.InstallStartError{
		Cause:               cause,
		RegistrationRemoved: true,
		Outcome:             service.StartOutcomeNotReady,
		Budget:              45 * time.Second,
	})
	assertNamesWithout(t, removedNotReady, "the removed not-ready report",
		[]string{"could not start it", "did not come up within 45s", "registration created by this attempt was removed",
			"start the desktop app normally again"},
		[]string{"HFDesk is not installed as a Windows service"})
}

// TestServiceGateUnreadableTextStatesWhatTheLaunchDid proves that the notice of
// an unreadable registration names the risk, the launch that started no server
// and the one action that resolves the uncertainty: the read of the state. It
// never promises a relaunch — when the read itself fails, for example because a
// Windows policy denies it, the notice states that the state stays unknown
// instead of sending the user into the same notice again.
func TestServiceGateUnreadableTextStatesWhatTheLaunchDid(t *testing.T) {
	reason := errors.New("the service control manager did not answer")
	text := serviceGateUnreadableText(reason)
	assertNames(t, text, "the notice", []string{
		reason.Error(),
		"may be installed and running",
		"started no server",
		"hfdesk service status",
		"elevated shell",
		"can then be started again",
		"leaves the state unknown",
		"will not guess it",
	})
	// The diagnostic is the single named action: no imperative relaunch may
	// follow it as a promise, and no alternative route sits beside it.
	assertNamesWithout(t, text, "the notice", nil, []string{"then start the desktop app again", " or "})
	if got := strings.Count(text, "hfdesk service"); got != 1 {
		t.Errorf("the notice %q names %d commands, want exactly one", text, got)
	}
}

// TestServiceActionFailureTextKeepsTheStateCommandOfTheManagerOnce pins the
// dedup of the state command: a manager text that already carries the one
// action (the composite rollback outcomes name it now) passes it through as
// the single action of the report, and the desktop surface adds no second
// copy. The perturbation is the detail: without the dedup every branch below
// would name the command twice.
func TestServiceActionFailureTextKeepsTheStateCommandOfTheManagerOnce(t *testing.T) {
	program := `C:\portable\hfdesk.exe`
	managerDetail := errors.New(`service: the restart was not confirmed (the previous registration was restored); run "hfdesk service status" to read the current state of the service`)

	for _, tc := range []struct {
		name  string
		build func() string
	}{
		{"an update failure", func() string {
			return serviceActionFailureText(serviceActionUpdate, managerDetail, program)
		}},
		{"a start failure", func() string {
			return serviceActionFailureText(serviceActionStart, managerDetail, program)
		}},
		{"an installation failure", func() string {
			return serviceActionFailureText(serviceActionInstall, managerDetail, program)
		}},
		{"a kept registration of a failed first start", func() string {
			return serviceInstallStartFailureText(managerDetail, program,
				&service.InstallStartError{Cause: managerDetail})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := tc.build()
			if !strings.Contains(text, managerDetail.Error()) {
				t.Errorf("the report %q does not carry the cause of the manager", text)
			}
			if got := strings.Count(text, "hfdesk service status"); got != 1 {
				t.Errorf("the report %q names the state command %d times, want exactly one", text, got)
			}
		})
	}

	// A detail that already names its own action keeps it as the single
	// route: the state command is not served beside it. The perturbation is
	// the detail — a status-only rule appended a second command here.
	actionDetail := errors.New(`service: executable "C:\portable\hfdesk.exe" is not usable: grant the account execute access, then run "hfdesk service install" from an elevated shell again`)
	text := serviceActionFailureText(serviceActionUpdate, actionDetail, program)
	if got := strings.Count(text, "hfdesk service status"); got != 0 {
		t.Errorf("the report %q names the state command %d times beside the manager's own action, want none", text, got)
	}
	if got := strings.Count(text, `"hfdesk service install"`); got != 1 {
		t.Errorf("the report %q carries the manager's own action %d times, want exactly one", text, got)
	}
}

// TestServiceGateChangedTextNamesTheDiagnostic proves the notice of a state
// that changed while the dialog was open follows the same single-action rule
// as the unreadable notice: the read of the state is the one named action, the
// relaunch is its stated consequence and never an imperative promise, and a
// failed read leaves the state unknown instead of looping the notice.
func TestServiceGateChangedTextNamesTheDiagnostic(t *testing.T) {
	text := serviceGateChangedText(serviceChoicePrompt{State: service.StateRunning})
	assertNames(t, text, "the notice", []string{
		"changed while the dialog was open",
		"running now",
		"started no server",
		"hfdesk service status",
		"elevated shell",
		"can then be started again",
		"leaves the state unknown",
		"will not guess it",
	})
	assertNamesWithout(t, text, "the notice", nil, []string{"then start the desktop app again", " or "})
	if got := strings.Count(text, "hfdesk service"); got != 1 {
		t.Errorf("the notice %q names %d commands, want exactly one", text, got)
	}
}

// TestServiceLaunchFailedTextSaysWhatDidNotHappen proves that a launch the user
// refused is reported as an action that changed nothing.
func TestServiceLaunchFailedTextSaysWhatDidNotHappen(t *testing.T) {
	refused := errors.New(elevationDeclined)

	text := serviceLaunchFailedText(serviceActionInstall, refused)
	assertNames(t, text, "the report", []string{elevationDeclined, "The service was not installed."})
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

// TestNewSiblingMissingNoticeNamesFileFolderAndOneAction proves the copy of
// the redesigned notice of a missing console program: the bold instruction
// names the file, the first-glance content names what the program is for and
// the folder it is not in, the offer is attached with the facts of the
// command link, and the long pairing explanation stays out of the first
// glance — it belongs to the manual expander.
//
// The notice is built without a dialog, so this pins the wording every
// renderer shows.
func TestNewSiblingMissingNoticeNamesFileFolderAndOneAction(t *testing.T) {
	const folder = `C:\portable`
	// The path is joined platform-natively: the derivation reads the file
	// name with filepath.Base, so the separator has to be the local one.
	desktop := filepath.Join(folder, testReleaseDesktop)
	asset := "hfdesk_windows_amd64_v1.3.0-beta.7.exe"
	offer := &consoleDownloadOffer{
		Release: consoleRelease{Tag: "v1.3.0-beta.7", Asset: asset},
		Size:    12772352,
	}

	// The lookup's own failure for this fixture: an empty folder next
	// to a version-stamped desktop program fails the pairing rule, and
	// that failure's explanation stays out of the first glance below.
	lookup := t.TempDir()
	_, findErr := findConsoleExecutable(lookup, filepath.Join(lookup, testReleaseDesktop))
	if !errors.Is(findErr, errConsolePairMissing) {
		t.Fatalf("the empty folder of a version-stamped program failed with %v, want the pairing failure", findErr)
	}

	notice := newSiblingMissingNotice(serviceActionInstall, folder, desktop, findErr, offer)

	assertNames(t, notice.Instruction, "the instruction", []string{asset, "is missing"})
	assertNames(t, notice.Content, "the content", []string{
		"The Windows service runs this program",
		folder,
	})
	if strings.Contains(notice.Content, "only runs the console program") {
		t.Errorf("the content %q carries the pairing explanation", notice.Content)
	}
	if notice.Title != "HFDesk" {
		t.Errorf("the title is %q, want %q", notice.Title, "HFDesk")
	}
	if notice.Download != offer {
		t.Errorf("the notice carries %v, want the resolved offer", notice.Download)
	}
	if notice.Folder != folder {
		t.Errorf("the notice carries folder %q, want %q", notice.Folder, folder)
	}

	// The pairing explanation is long and belongs to the expander, not
	// to the first glance; the manual text carries the get-put-retry steps.
	if strings.Contains(notice.Content, "release") {
		t.Errorf("the content %q carries the pairing explanation", notice.Content)
	}
	assertNames(t, notice.Manual, "the manual text", []string{
		"Get release v1.3.0-beta.7",
		"releases/tag/v1.3.0-beta.7",
		"put " + asset + " next to hfdesk-desktop.exe",
		folder,
		"run the action again",
		"only runs the console program of that same release",
	})

	// The labels of the two buttons and the note of the command link are
	// the approved copy; the note states size, release, the check and
	// what follows it.
	assertNames(t, siblingMissingDownloadLabel, "the primary label", []string{"Download it and continue"})
	assertNames(t, siblingMissingDismissLabel, "the dismiss label", []string{"Not now"})
	assertNames(t, siblingMissingManualLabel, "the expander label", []string{"Install it manually instead"})
	assertNames(t, siblingMissingDownloadNote(notice), "the note line", []string{
		"12.2 MB",
		"from the official release v1.3.0-beta.7",
		"SHA-256 checked",
		"then the usual install steps",
	})

	// The claim the feature falsified must be gone from every surface of
	// the notice.
	for name, text := range map[string]string{
		"instruction": notice.Instruction,
		"content":     notice.Content,
		"manual":      notice.Manual,
	} {
		if strings.Contains(text, "never downloads") || strings.Contains(text, "nothing is downloaded or copied") {
			t.Errorf("the %s %q carries the falsified download claim", name, text)
		}
	}
}

// TestNewSiblingMissingNoticeWithoutAReleaseKeepsManualInstructionsOnly
// proves that a local build — a desktop program whose name carries no
// release version — gets no download offer: the notice names hfdesk.exe,
// keeps the cause of the failed lookup beside the first sentence and offers
// the manual steps alone.
func TestNewSiblingMissingNoticeWithoutAReleaseKeepsManualInstructionsOnly(t *testing.T) {
	const folder = `C:\portable`
	notice := newSiblingMissingNotice(serviceActionInstall, folder, filepath.Join(folder, "hfdesk-desktop.exe"),
		errors.New("there is no hfdesk.exe in "+folder), nil)

	if notice.Download != nil {
		t.Errorf("the notice offers the download %v for a local build, want no offer", notice.Download)
	}
	assertNames(t, notice.Instruction, "the instruction", []string{"hfdesk.exe", "is missing"})
	assertNames(t, notice.Content, "the content", []string{
		"The Windows service runs this program",
		folder,
		"there is no hfdesk.exe",
	})
	assertNames(t, notice.Manual, "the manual text", []string{
		"Put hfdesk.exe next to hfdesk-desktop.exe",
		folder,
		"run the action again",
	})
	if strings.Contains(notice.Manual, "Get release") {
		t.Errorf("the manual text %q names a release for a local build", notice.Manual)
	}
}

// TestNewSiblingMissingNoticeKeepsTheCauseOfANonPairingLookupFailure proves
// the cause rule of the notice for a version-stamped desktop program: a
// folder that cannot be read is not the pairing rule, so its cause sits
// beside the first sentence of the content — the version stamp alone never
// silences it.
func TestNewSiblingMissingNoticeKeepsTheCauseOfANonPairingLookupFailure(t *testing.T) {
	// A folder that does not exist: the lookup fails at the read, long
	// before it could apply the pairing rule.
	folder := filepath.Join(t.TempDir(), "gone")
	desktop := filepath.Join(folder, testReleaseDesktop)
	_, findErr := findConsoleExecutable(folder, desktop)
	if findErr == nil {
		t.Fatal("the lookup of a missing folder succeeded")
	}
	if errors.Is(findErr, errConsolePairMissing) {
		t.Fatalf("the failed read %v is marked as the pairing failure", findErr)
	}

	notice := newSiblingMissingNotice(serviceActionInstall, folder, desktop, findErr, nil)
	assertNames(t, notice.Content, "the content", []string{
		"The Windows service runs this program",
		folder,
		"cannot be read",
	})
}

// TestSiblingMissingChooseTextMapsYesNo proves the Yes/No text of an
// offer-bearing notice maps its two answers: Yes takes the verified
// download and continues, No changes nothing and carries the manual path.
// The notice flow's own fallback reaches the box only manual-only, so this
// is the rule of the box itself, which an offer-bearing notice still gets
// when it reaches the box.
func TestSiblingMissingChooseTextMapsYesNo(t *testing.T) {
	const folder = `C:\portable`
	notice := newSiblingMissingNotice(serviceActionInstall, folder, filepath.Join(folder, testReleaseDesktop),
		errors.New("pairing"), &consoleDownloadOffer{
			Release: consoleRelease{Tag: testReleaseTag, Asset: testReleaseAsset},
			Size:    1024,
		})

	text := siblingMissingChooseText(notice)
	assertNames(t, text, "the fallback", []string{
		testReleaseAsset + " is missing",
		"The Windows service runs this program, and it is not in " + folder,
		"Choose Yes to download " + testReleaseAsset,
		"official release " + testReleaseTag,
		"SHA-256",
		"continue with the installation",
		"Choose No to change nothing",
		notice.Manual,
	})
}

// TestSiblingMissingNoticeForFallbackSuppressesAnOfferThatCannotRun proves
// the message box fallback never offers what the transfer behind it would
// refuse: the offer survives only an attempt that did not fail, while the
// failure the fallback exists for drops it — whatever made the task dialog
// fail, and independently of the machine's progress-dialog gate — and the
// box keeps the file, the folder and the manual path.
func TestSiblingMissingNoticeForFallbackSuppressesAnOfferThatCannotRun(t *testing.T) {
	const folder = `C:\portable`
	offered := newSiblingMissingNotice(serviceActionInstall, folder, filepath.Join(folder, testReleaseDesktop),
		errors.New("pairing"), &consoleDownloadOffer{
			Release: consoleRelease{Tag: testReleaseTag, Asset: testReleaseAsset},
			Size:    1024,
		})

	kept := siblingMissingNoticeForFallback(offered, nil)
	if kept != offered {
		t.Errorf("an attempt that did not fail changed the notice: %+v, want the offer kept", kept)
	}

	// The failure of the task dialog attempt itself is the case a gate
	// read alone did not close: the progress-dialog gate of the machine
	// can stand while the notice's own dialog call failed, and the
	// transfer behind this offer shows its progress and its Cancel in
	// exactly that task dialog machinery a moment later. The offer is
	// dropped, so the box never asks a Yes whose transfer could only end
	// in the "nothing was downloaded" report.
	manual := siblingMissingNoticeForFallback(offered,
		errors.New("the dialog could not be shown (HRESULT 0x80070057)"))
	if manual.Download != nil {
		t.Errorf("the fallback keeps the offer %v although the transfer cannot run, want no offer", manual.Download)
	}
	if manual.Instruction != offered.Instruction || manual.Content != offered.Content ||
		manual.Folder != offered.Folder || manual.Manual != offered.Manual {
		t.Errorf("dropping the offer changed the rest of the notice:\ngot  %+v\nwant %+v", manual, offered)
	}

	text := siblingMissingManualNoticeText(manual)
	assertNames(t, text, "the manual-only box", []string{
		offered.Instruction,
		folder,
		offered.Manual,
	})
	assertNamesWithout(t, text, "the manual-only box", nil, []string{"Choose Yes"})
}

// TestConsoleDownloadFailedTextReportsTheReasonAndTheManualPath proves the
// report of a failed download states the result, the cause and the one next
// action — and claims only what the mechanism guarantees: that this download
// left no file of its own behind when no file of it remains, and the path of
// a file whose removal failed instead.
func TestConsoleDownloadFailedTextReportsTheReasonAndTheManualPath(t *testing.T) {
	const folder = `C:\portable`
	notice := newSiblingMissingNotice(serviceActionInstall, folder, filepath.Join(folder, testReleaseDesktop),
		errors.New("pairing"), &consoleDownloadOffer{
			Release: consoleRelease{Tag: testReleaseTag, Asset: testReleaseAsset},
			Size:    1024,
		})

	cause := errors.New("the SHA-256 of the downloaded file is abcd, but release " + testReleaseTag + " publishes efgh for it")
	text := consoleDownloadFailedText(notice, cause)
	assertNames(t, text, "the report", []string{
		"HFDesk could not download " + testReleaseAsset,
		cause.Error(),
		"This download left no file behind in " + folder,
		notice.Manual,
	})
	if strings.Contains(text, "A file remains at") {
		t.Errorf("the report %q claims a leftover for a clean cleanup", text)
	}

	// A staging file that could not be removed is named with its path
	// instead of being covered by the "no file" claim — and it is only
	// called what it is: a file that remains, not a partial one, because
	// the leftover of the cancel path is the complete placed program.
	leftover := folder + `\.hfdesk-console-42.tmp`
	text = consoleDownloadFailedText(notice, &consoleDownloadError{
		cause:    errors.Join(cause, errors.New("the staging file could not be removed: access is denied")),
		leftover: leftover,
	})
	assertNames(t, text, "the report with a leftover", []string{
		cause.Error(),
		"A file remains at " + leftover,
		"could not be removed",
		notice.Manual,
	})
	if strings.Contains(text, "left no file behind") {
		t.Errorf("the report %q claims a clean download although a file remains", text)
	}

	// The cancelled transfer reports its own cause beside the manual
	// path: this is the report the flow shows when the user cancels the
	// progress dialog, so the cause names the cancellation, the download
	// left nothing of its own behind and the manual path stays reachable.
	text = consoleDownloadFailedText(notice, errConsoleDownloadCanceled)
	assertNames(t, text, "the cancellation report", []string{
		"HFDesk could not download " + testReleaseAsset,
		errConsoleDownloadCanceled.Error(),
		"This download left no file behind in " + folder,
		notice.Manual,
	})
}

// TestConsoleDownloadProgressTextNamesTheTransfer proves the progress dialog
// names what is fetched, where from and the check that runs before the file
// is used.
func TestConsoleDownloadProgressTextNamesTheTransfer(t *testing.T) {
	instruction, content := consoleDownloadProgressText(&consoleDownloadOffer{
		Release: consoleRelease{Tag: "v1.3.0-beta.7", Asset: "hfdesk_windows_amd64_v1.3.0-beta.7.exe"},
		Size:    12772352,
	})
	assertNames(t, instruction, "the instruction", []string{"Downloading hfdesk_windows_amd64_v1.3.0-beta.7.exe"})
	assertNames(t, content, "the content", []string{
		"12.2 MB",
		"official release v1.3.0-beta.7",
		"SHA-256 is checked before the file is used",
	})
}

// TestSiblingMissingNoticeCopyNamesTheActionItContinues proves the copy that
// runs after a verified download names the service action the user actually
// invoked: the note line of an update promises the update steps and its
// fallback continues with the update, while the install notice keeps its
// approved wording exactly. The manual path stays the approved,
// action-neutral sentence, which is true for both actions.
func TestSiblingMissingNoticeCopyNamesTheActionItContinues(t *testing.T) {
	const folder = `C:\portable`
	noticeFor := func(action serviceAction) siblingMissingNotice {
		return newSiblingMissingNotice(action, folder, filepath.Join(folder, testReleaseDesktop),
			errors.New("pairing"), &consoleDownloadOffer{
				Release: consoleRelease{Tag: testReleaseTag, Asset: testReleaseAsset},
				Size:    12772352,
			})
	}

	install := noticeFor(serviceActionInstall)
	installNote := siblingMissingDownloadNote(install)
	installFallback := siblingMissingChooseText(install)
	assertNames(t, installNote, "the install note line", []string{
		"from the official release " + testReleaseTag,
		"SHA-256 checked",
		"then the usual install steps",
	})
	assertNames(t, installFallback, "the install fallback", []string{"continue with the installation"})
	assertNamesWithout(t, installNote+"\n"+installFallback, "the install copy", nil, []string{"update"})

	update := noticeFor(serviceActionUpdate)
	updateNote := siblingMissingDownloadNote(update)
	updateFallback := siblingMissingChooseText(update)
	assertNames(t, updateNote, "the update note line", []string{
		"from the official release " + testReleaseTag,
		"SHA-256 checked",
		"then the usual update steps",
	})
	assertNames(t, updateFallback, "the update fallback", []string{"continue with the update"})
	assertNamesWithout(t, updateNote+"\n"+updateFallback, "the update copy", nil,
		[]string{"install steps", "the installation"})

	if update.Action != serviceActionUpdate {
		t.Errorf("the notice carries the action %q, want %q", update.Action, serviceActionUpdate)
	}
	if update.Manual != install.Manual {
		t.Errorf("the manual text differs per action:\ninstall: %q\nupdate: %q",
			install.Manual, update.Manual)
	}
	assertNames(t, update.Manual, "the manual text", []string{"run the action again"})
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
		assertNames(t, text, "the report of", []string{
			serviceActionFailureHeadline(action),
			"No service action was performed",
			cause.Error(),
			`"hfdesk service status"`,
		})
	}

	denied := fmt.Errorf(
		"the desktop singleton %q exists but this Windows identity cannot access it: another HFDesk process under a different Windows account created it; end that process and try again%w",
		`Local\HFDesk.Desktop.test.1`, service.ErrSingletonAccessDenied)
	text := serviceSingletonHoldRefusedText(serviceActionUpdate, denied)
	assertNames(t, text, "the identity refusal", []string{
		serviceActionFailureHeadline(serviceActionUpdate),
		"No service action was performed",
		"this Windows identity cannot access it",
		"end that process and try again",
	})
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
	assertNames(t, text, "the report", []string{
		serviceActionFailureHeadline(serviceActionUpdate),
		`runs as "CONTOSO\svc-other"`,
		"nothing was changed and the service was not stopped",
		`sc config hfdesk obj= "NT AUTHORITY\LocalService" password= ""`,
	})
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
		assertNames(t, text, "the report of", []string{
			serviceActionUnchangedText(action),
			"No service action was performed",
			"desktop: the service helper exited before it became ready",
			"Read the message the administrator helper showed",
			"names the reason and the step that follows",
		})
		// One report, one unconditional action: the helper's own message
		// is the single place that carries the real cause, so no
		// conditional phrasing and no second command may sit beside it.
		for _, unwanted := range []string{"If that message appeared", "then repeat the action", "hfdesk service status", " or "} {
			if strings.Contains(text, unwanted) {
				t.Errorf("the report of %q %q carries %q beside its one action", action, text, unwanted)
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
// helper that never ran: the head and the observed cause stay exactly as they
// are — a declined user account control prompt and every other failed launch
// stay distinguishable by their text — and the report names the one action
// both states share, with no second command beside it.
func TestServiceLaunchFailedTextKeepsTheNeverRanReport(t *testing.T) {
	text := serviceLaunchFailedText(serviceActionInstall, errors.New(elevationDeclined))
	if !strings.HasPrefix(text, "HFDesk could not start its administrator helper.") {
		t.Errorf("the report %q does not keep the head of a launch that never ran", text)
	}
	assertNames(t, text, "the report", []string{
		elevationDeclined,
		"The service was not installed.",
		"Repeat the action and approve the administrator prompt when Windows shows it",
	})
	assertNamesWithout(t, text, "the report", nil, []string{"hfdesk service status", "hfdesk service install", "hfdesk service uninstall", " or "})
}

// TestUnknownServiceActionTextNamesOneAction proves the report of an unknown
// helper action states the result, the observed cause and the one action that
// leaves the next launch with arguments the helper knows.
func TestUnknownServiceActionTextNamesOneAction(t *testing.T) {
	cause := errors.New("unknown service action \"instal\"")
	text := unknownServiceActionText(cause)
	assertNames(t, text, "the report", []string{
		"service action it does not know",
		cause.Error(),
		"Nothing was installed, updated or started.",
		"Start the desktop app again.",
	})
	assertNamesWithout(t, text, "the report", nil, []string{"hfdesk service status", "hfdesk service install", "elevated shell"})
}

// deletionClaimForms are the deletion-claim shapes the confirmations and
// the reports of the removal must never carry beside the data the user
// keeps: the plural and the singular, the past and the perfect — "was
// removed" beside "has been removed". A stop list that pinned only the
// plural let the singular regression through unnoticed, so every surface
// that forbids a deletion claim consults this one list; the positive pins
// ("stay untouched", the registration that "is removed") are phrased so
// none of these forms touches them.
var deletionClaimForms = []string{
	"deleted", "erased", "cleared", "wiped",
	"was removed", "were removed",
	"has been removed", "have been removed",
	"has been deleted", "has been erased", "has been cleared", "has been wiped",
	"is deleted", "is erased", "is cleared",
}

// TestStopAndRemoveConfirmationsStateTheActionAndTheOneFact proves that the
// two confirmations that guard the new machine-wide changes follow the shape
// of the installation confirmation and the text rule of the reports: what the
// action does, the administrator rights it needs, the one fact the user has
// to see before agreeing, and the question — with no internal mechanism and
// no claim about a state the confirmation did not read.
func TestStopAndRemoveConfirmationsStateTheActionAndTheOneFact(t *testing.T) {
	stop := stopServiceConfirmText()
	assertNames(t, stop, "the stop confirmation", []string{
		"This stops the HFDesk service.",
		"administrator rights",
		"Windows prompt follows",
		"Nothing serves the address of the service until you start it again.",
		"Do you want to stop the service?",
	})
	// The confirmation names no state of its own: the state the menu item
	// was enabled by can be stale by the time the box is read, and no
	// mechanism of the stop is the user's business.
	assertNamesWithout(t, stop, "the stop confirmation", nil, []string{
		"service control manager", "SCM", "registry", "is running", "was running",
	})

	remove := removeServiceConfirmText()
	assertNames(t, remove, "the removal confirmation", []string{
		"removes the Windows service registration of HFDesk",
		"administrator rights",
		"Windows prompt follows",
		"settings, jobs, models, cache, history and logs stay untouched",
		"Do you want to remove the registration?",
	})
	// The invariant of the removal is stated as what stays, and the
	// confirmation never claims that any data is deleted — in any of the
	// singular or plural, past or perfect shapes of such a claim.
	forbidden := append(append([]string{}, deletionClaimForms...), "removed your", "will be removed")
	assertNamesWithout(t, remove, "the removal confirmation", nil, forbidden)

	if stopServiceConfirmCaption == "" || removeServiceConfirmCaption == "" {
		t.Error("a confirmation of a new service action has no caption")
	}
	if stopServiceConfirmCaption == removeServiceConfirmCaption ||
		stopServiceConfirmCaption == installServiceCaption || removeServiceConfirmCaption == installServiceCaption {
		t.Errorf("the confirmations share a caption: %q, %q, %q",
			stopServiceConfirmCaption, removeServiceConfirmCaption, installServiceCaption)
	}
}

// TestServiceStoppedTextNamesTheStateTheFactAndOneWayBack proves that the
// report of a stopped service is the state, the one fact the user did not
// ask for — nothing serves the address of the service right now — and
// exactly one way back. The way back goes through the quit and the startup
// gate, because a start beside the desktop that runs this very tray is
// refused by the established guard of the helper: a report that pointed at
// that start would send the user into the refusal in a circle.
func TestServiceStoppedTextNamesTheStateTheFactAndOneWayBack(t *testing.T) {
	text := serviceStoppedText()
	assertNames(t, text, "the report", []string{
		"The HFDesk service is stopped.",
		"Nothing serves the address of the service right now.",
		"Quit HFDesk from the notification-area icon",
		"choose Start service",
	})
	// One report, one route: no elevated command line and no alternative
	// beside the established way.
	assertNamesWithout(t, text, "the report", nil, []string{`"hfdesk service`, " or ", "elevated shell"})
	if serviceStoppedCaption == "" {
		t.Error("the report of a stopped service has no caption")
	}
}

// TestServiceRegistrationRemovedTextNeverClaimsDataDeletion proves the
// invariant of the removal report: it names the state (the registration is
// gone), states that every data the user keeps stays untouched, and offers
// the one way back — while it never claims, hints or implies that any data
// was deleted, because the removal writes no file and deletes no directory.
func TestServiceRegistrationRemovedTextNeverClaimsDataDeletion(t *testing.T) {
	text := serviceRegistrationRemovedText()
	assertNames(t, text, "the report", []string{
		"The Windows service registration of HFDesk is removed.",
		"settings, jobs, models, cache, history and logs stay untouched",
		"Install the service again from the tray of the desktop app",
	})
	// Every data noun appears only in the "stay untouched" sentence: the
	// report never pairs any of them with a deletion — in any of the
	// singular or plural, past or perfect shapes of such a claim.
	assertNamesWithout(t, text, "the report", nil, deletionClaimForms)
	// One report, one route: the installation is the single named action.
	if got := strings.Count(text, "Install the service again"); got != 1 {
		t.Errorf("the report names the way back %d times, want exactly one", got)
	}
	if serviceRegistrationRemovedCaption == "" {
		t.Error("the report of a removed registration has no caption")
	}
}

// TestServiceNoRegistrationTextStatesTheCleanOutcome proves that a removal
// that found no registration reports the same clean outcome the command
// line reports — without claiming a removal that never happened.
func TestServiceNoRegistrationTextStatesTheCleanOutcome(t *testing.T) {
	text := serviceNoRegistrationText()
	assertNames(t, text, "the report", []string{
		"HFDesk is not registered as a Windows service.",
		"There was no registration to remove, so nothing was changed.",
	})
	assertNamesWithout(t, text, "the report", nil, []string{
		"registration of HFDesk is removed", "stay untouched",
	})
	if serviceNoRegistrationCaption == serviceRegistrationRemovedCaption {
		t.Errorf("the report of a machine without a registration shares the caption of a real removal: %q",
			serviceNoRegistrationCaption)
	}
}

// TestNewServiceActionReportClasses pins the report family of the two new
// actions beside the classes the reports of the established actions already
// have: a caption per failed action, the headline of a failure, the result
// sentence of a report whose helper never ran, and the settled state of an
// action that found no registration. None of them claims a state the action
// did not observe.
func TestNewServiceActionReportClasses(t *testing.T) {
	if got, want := serviceFailureCaption(serviceActionStop), "HFDesk - the service did not stop"; got != want {
		t.Errorf("the failure caption of the stop is %q, want %q", got, want)
	}
	if got, want := serviceFailureCaption(serviceActionUninstall), "HFDesk - the service registration was not removed"; got != want {
		t.Errorf("the failure caption of the removal is %q, want %q", got, want)
	}
	if got, want := serviceActionFailureHeadline(serviceActionStop), "HFDesk could not stop the Windows service."; got != want {
		t.Errorf("the failure headline of the stop is %q, want %q", got, want)
	}
	if got, want := serviceActionFailureHeadline(serviceActionUninstall), "HFDesk could not remove the Windows service registration."; got != want {
		t.Errorf("the failure headline of the removal is %q, want %q", got, want)
	}
	if got, want := serviceActionUnchangedText(serviceActionStop), "The service was not stopped."; got != want {
		t.Errorf("the unchanged result of the stop is %q, want %q", got, want)
	}
	if got, want := serviceActionUnchangedText(serviceActionUninstall), "The registration was not removed."; got != want {
		t.Errorf("the unchanged result of the removal is %q, want %q", got, want)
	}

	// A machine without a registration: the stop names the settled state
	// alone — installing a service the user asked to stop would answer a
	// question they did not ask — while the established actions keep the
	// guidance they have.
	stop := serviceNotInstalledText(serviceActionStop)
	assertNames(t, stop, "the not-installed report of the stop", []string{
		"HFDesk is not installed as a Windows service.",
		"There is no service to stop.",
	})
	assertNamesWithout(t, stop, "the not-installed report of the stop", nil,
		[]string{"Install it from the tray", "There is no service to start"})
	start := serviceNotInstalledText(serviceActionStart)
	assertNames(t, start, "the not-installed report of the start", []string{
		"There is no service to start.", "Install it from the tray of the desktop app first.",
	})

	// The failure of each new action is the headline, the cause of the
	// manager on its line and the single state command — it never claims
	// an outcome the action did not observe.
	reason := errors.New("the service control manager refused the operation")
	for _, action := range []serviceAction{serviceActionStop, serviceActionUninstall} {
		text := serviceActionFailureText(action, reason, "")
		assertNames(t, text, "the failure report", []string{
			serviceActionFailureHeadline(action), reason.Error(), `"hfdesk service status"`,
		})
		if got := strings.Count(text, "hfdesk service"); got != 1 {
			t.Errorf("the failure report of %q names %d commands, want exactly one", action, got)
		}
		assertNamesWithout(t, text, "the failure report", nil, []string{
			"The service was stopped", "is stopped.", "was removed", "is removed.",
		})
	}
}

// TestServiceActionBusyTextIsResultCauseAndOneAction proves that the refusal
// of a second service action while one runs reports the result (this action
// was not started), the cause on its line and the one action that ends it —
// and names no state, because the running action is the one that moves it.
func TestServiceActionBusyTextIsResultCauseAndOneAction(t *testing.T) {
	text := serviceActionBusyText()
	assertNames(t, text, "the refusal", []string{
		"Another HFDesk service action is already running",
		"this action was not started",
		"Wait for the running action to finish, then try again.",
	})
	assertNamesWithout(t, text, "the refusal", nil, []string{
		"hfdesk service", "is running.", "is stopped.",
	})
	if serviceActionBusyCaption == "" {
		t.Error("the refusal of a concurrent service action has no caption")
	}
}

// useLogPath points the log-path seam of the failure windows at path for the
// duration of the test and restores it afterwards, so the windows of another
// test never inherit a path.
func useLogPath(t *testing.T, path string) {
	t.Helper()
	previous := runLogPath
	runLogPath = path
	t.Cleanup(func() { runLogPath = previous })
}

// TestFailureWindowsNameTheLogFileOfThisLaunch proves the rule of the fifth
// plan item for the whole family: every window that reports a failure of
// this launch carries the one standard line with the path of the log file of
// this launch, so the log is reachable from outside the window — named
// exactly once, and never with the path baked in twice.
//
// The path is the seam runLogPath: the host sets it when its logging
// pipeline is installed and the elevated helper sets it from the path the
// launching host passed, so the line names the file this run really writes
// (the configured one, or the default %LocalAppData%\HFDesk\logs\hfdesk.log
// of an unconfigured host) instead of a hard-coded guess.
//
// The exception is pinned explicitly with the empty path: a launch that
// installed no logging pipeline names no file, because there is none — the
// single instance refusal before logging starts, the log directory or file
// that could not be opened, a command line error, and a second launch, whose
// log belongs to the running instance. logFileLine("") returns no line, and
// the windows whose path is passed by the box that shows them
// (reportStartupFailure with an explicit path) never bake one in beside it.
func TestFailureWindowsNameTheLogFileOfThisLaunch(t *testing.T) {
	const path = `C:\Users\ada\AppData\Local\HFDesk\logs\hfdesk.log`
	useLogPath(t, path)
	wantLine := "The log file is at " + path + "."

	cause := errors.New("the service control manager did not answer")
	program := `C:\portable\hfdesk.exe`
	kept := &service.InstallStartError{Cause: cause, CleanupError: errors.New("the process identity changed")}
	removed := &service.InstallStartError{Cause: cause, RegistrationRemoved: true}
	notice := newSiblingMissingNotice(serviceActionInstall, `C:\portable`,
		filepath.Join(`C:\portable`, "hfdesk-desktop.exe"), cause, nil)
	downloadNotice := newSiblingMissingNotice(serviceActionInstall, `C:\portable`,
		filepath.Join(`C:\portable`, "hfdesk-desktop.exe"), cause, &consoleDownloadOffer{
			Release: consoleRelease{Tag: testReleaseTag, Asset: testReleaseAsset},
			Size:    1024,
		})

	windows := map[string]string{
		"the installation failure": serviceActionFailureText(serviceActionInstall, cause, program),
		"the update failure":       serviceActionFailureText(serviceActionUpdate, cause, program),
		"the start failure":        serviceActionFailureText(serviceActionStart, cause, program),
		"the stop failure":         serviceActionFailureText(serviceActionStop, cause, program),
		"the removal failure":      serviceActionFailureText(serviceActionUninstall, cause, program),
		"the already-registered refusal": serviceActionFailureText(serviceActionInstall,
			fmt.Errorf("service: hfdesk is already registered%w", service.ErrAlreadyRegistered), program),
		"the kept-start failure":      serviceInstallStartFailureText(kept, program, kept),
		"the removed-start failure":   serviceInstallStartFailureText(removed, program, removed),
		"the singleton refusal":       serviceSingletonHoldRefusedText(serviceActionUpdate, cause),
		"the serving-desktop refusal": serviceServingDesktopRefusedText(serviceActionStart),
		"the unreadable-state refusal": serviceUnreadableClaimRefusedText(serviceActionInstall,
			`C:\Users\ada\AppData\Roaming\HFDesk\desktop-state.json`),
		"the not-installed report": serviceNotInstalledText(serviceActionStart),
		"the busy refusal":         serviceActionBusyText(),
		"the not-ready report": serviceHelperNotReadyText(serviceActionInstall,
			&helperNotReadyError{cause: errors.New("the service helper exited before it became ready")}),
		"the launch-failure report":     serviceLaunchFailedText(serviceActionInstall, cause),
		"the unknown-action report":     unknownServiceActionText(errors.New("unknown service action")),
		"the preparation refusal":       serviceHandoverRefusedText(cause),
		"the unfinished-install report": serviceHandoverFailureText(cause),
		"the unreadable gate notice":    serviceGateUnreadableText(cause),
		"the changed gate notice":       serviceGateChangedText(serviceChoicePrompt{State: service.StateRunning}),
		"the download failure":          consoleDownloadFailedText(downloadNotice, cause),
		"the missing-program notice":    siblingMissingManualNoticeText(notice),
		"the browser failure":           browserOpenFailedText("http://127.0.0.1:8080", cause),
		"the tray-startup failure":      trayStartupFailureText(),
		"the runtime-window failure":    runtimeWindowShowFailedText(errors.New("the window could not be shown")),
	}
	for name, text := range windows {
		if !strings.Contains(text, wantLine) {
			t.Errorf("the %s does not name the log file of this launch:\n%s", name, text)
		}
		if got := strings.Count(text, "The log file is at"); got != 1 {
			t.Errorf("the %s names the log file %d times, want exactly once:\n%s", name, got, text)
		}
	}

	// A launch without a log file names none: logFileLine is the whole
	// pre-logging rule, and every pre-logging window reaches the box
	// through it (reportStartupFailure with an empty path, the singleton
	// refusal, the command line error, the log that could not be opened,
	// the second launch).
	if line := logFileLine(""); line != "" {
		t.Errorf("logFileLine of an empty path = %q, want no line for a launch without a log file", line)
	}
	if line := logFileLine("   "); line != "" {
		t.Errorf("logFileLine of a blank path = %q, want no line", line)
	}

	// The windows whose path is passed by the box that shows them never
	// bake a line in beside that explicit argument — otherwise a box that
	// appends the real path would show it twice, and a pre-logging box
	// that passes "" would still name a file this run does not write.
	for name, text := range map[string]string{
		"the unpublished-URL report":   publishStateStartupText(cause),
		"the unpublished-claim report": publishClaimStartupText(cause),
	} {
		if strings.Contains(text, "The log file is at") {
			t.Errorf("the %s bakes in a log path beside the path its box passes:\n%s", name, text)
		}
	}

	// The choice dialog and the success reports are not failure windows:
	// they carry state and actions, never the log line.
	notFailure := map[string]string{
		"the gate of a running service": serviceGateText(serviceChoicePrompt{
			State: service.StateRunning, URL: "http://127.0.0.1:8080", DataDir: `C:\ProgramData\HFDesk`}),
		"the installation confirmation":   installServiceText(`C:\ProgramData\HFDesk`),
		"the report of an installation":   serviceInstalledText("1.3.0", program, `C:\ProgramData\HFDesk`, "http://127.0.0.1:8080"),
		"the report of a stopped service": serviceStoppedText(),
	}
	for name, text := range notFailure {
		if strings.Contains(text, "The log file is at") {
			t.Errorf("the %s carries the failure-only log line:\n%s", name, text)
		}
	}
}

// TestGateAndReportsCarryNoInternalMechanism pins the rule of the sixth plan
// item for the gate family and the reports: state plus at most one action,
// and no internal mechanism in the text the window itself writes — no
// guards, admissions, cadences, algorithms or flags, no coordination
// vocabulary, no argument about why the buttons are right. The causes a
// report carries verbatim are outside this rule (they are the one fact line
// of the window), so every text below is built with a neutral cause.
func TestGateAndReportsCarryNoInternalMechanism(t *testing.T) {
	banned := []string{
		"guard", "admission", "cadence", "algorithm", "flag",
		"singleton", "mutex", "claim", "handover", "rotation",
		// The two justifications the gate used to argue with.
		"two instances never compete", "Update re-registers",
	}
	cause := errors.New("the state of the service could not be settled")
	program := `C:\portable\hfdesk.exe`
	dataDir := `C:\ProgramData\HFDesk`
	notice := newSiblingMissingNotice(serviceActionInstall, `C:\portable`,
		filepath.Join(`C:\portable`, "hfdesk-desktop.exe"), cause, nil)

	texts := map[string]string{
		"the running gate": serviceGateText(serviceChoicePrompt{
			State: service.StateRunning, URL: "http://127.0.0.1:8080", DataDir: dataDir}),
		"the stopped gate": serviceGateText(serviceChoicePrompt{
			State: service.StateStopped, DataDir: dataDir}),
		"the failed gate": serviceGateText(serviceChoicePrompt{
			State: service.StateFailed, Detail: "the instance exited with the code 1", DataDir: dataDir}),
		"the broken gate with a running instance": serviceGateText(serviceChoicePrompt{
			State: service.StateBroken, Running: true, DataDir: dataDir}),
		"the pending notice":            serviceGatePendingText(service.StateStarting),
		"the unreadable notice":         serviceGateUnreadableText(cause),
		"the changed notice":            serviceGateChangedText(serviceChoicePrompt{State: service.StateRunning}),
		"the installation confirmation": installServiceText(dataDir),
		"the stop confirmation":         stopServiceConfirmText(),
		"the removal confirmation":      removeServiceConfirmText(),
		"the installation failure":      serviceActionFailureText(serviceActionInstall, cause, program),
		"the kept-start failure": serviceInstallStartFailureText(
			&service.InstallStartError{Cause: cause, CleanupError: errors.New("the process identity changed")}, program,
			&service.InstallStartError{Cause: cause, CleanupError: errors.New("the process identity changed")}),
		"the already-registered refusal": serviceActionFailureText(serviceActionInstall,
			fmt.Errorf("service: hfdesk is already registered%w", service.ErrAlreadyRegistered), program),
		"the not-ready report": serviceHelperNotReadyText(serviceActionStart,
			&helperNotReadyError{cause: cause}),
		"the launch-failure report":     serviceLaunchFailedText(serviceActionStart, cause),
		"the preparation refusal":       serviceHandoverRefusedText(cause),
		"the unfinished-install report": serviceHandoverFailureText(cause),
		"the serving-desktop refusal":   serviceServingDesktopRefusedText(serviceActionStart),
		"the unreadable-state refusal": serviceUnreadableClaimRefusedText(serviceActionStart,
			`C:\Users\ada\AppData\Roaming\HFDesk\desktop-state.json`),
		"the not-installed report":   serviceNotInstalledText(serviceActionStart),
		"the busy refusal":           serviceActionBusyText(),
		"the unknown-action report":  unknownServiceActionText(cause),
		"the stopped report":         serviceStoppedText(),
		"the removed report":         serviceRegistrationRemovedText(),
		"the no-registration report": serviceNoRegistrationText(),
		"the missing-program notice": siblingMissingManualNoticeText(notice),
	}
	for name, text := range texts {
		lower := strings.ToLower(text)
		for _, word := range banned {
			if strings.Contains(lower, word) {
				t.Errorf("the %s carries the internal word %q:\n%s", name, word, text)
			}
		}
	}
}

// TestInstallStartFailureStatesTheCleanupFactOnce closes F2: the report of a
// failed first start states the cleanup fact exactly once. The service layer
// states it in the message of its error ("(the registration was kept…)",
// "(the failed instance was settled…)"), and the report states it in its own
// outcome sentence — two formulations of one fact in one window. The report
// now takes only the cause and the cleanup reason from the error and keeps
// its own sentence as the single formulation, while the cause, the reason and
// every class of the start outcome stay what they were.
func TestInstallStartFailureStatesTheCleanupFactOnce(t *testing.T) {
	program := `C:\portable\hfdesk.exe`
	cause := errors.New("the first installation did not put the new executable into service")
	reason := errors.New("the service process identity changed")

	// The production shape: the error the helper receives IS the cleanup
	// record, so its message carries the gloss the report must not repeat.
	kept := &service.InstallStartError{Cause: cause, CleanupError: reason}
	keptText := serviceInstallStartFailureText(kept, program, kept)
	assertNames(t, keptText, "the kept report", []string{
		cause.Error(), reason.Error(), "registration was kept",
	})
	if got := strings.Count(keptText, "kept"); got != 1 {
		t.Errorf("the kept report states the cleanup fact %d times, want once:\n%s", got, keptText)
	}
	for _, gloss := range []string{
		"(the registration was kept", "(the failed instance was settled",
		"(the failed process was stopped", "(the failed process was proven",
	} {
		if strings.Contains(keptText, gloss) {
			t.Errorf("the kept report repeats the service layer's gloss %q:\n%s", gloss, keptText)
		}
	}

	removed := &service.InstallStartError{Cause: cause, RegistrationRemoved: true}
	removedText := serviceInstallStartFailureText(removed, program, removed)
	assertNames(t, removedText, "the removed report", []string{
		cause.Error(), "registration created by this attempt was removed",
		"start the desktop app normally again",
	})
	assertNamesWithout(t, removedText, "the removed report", nil,
		[]string{"HFDesk is not installed as a Windows service", "(the failed instance was settled"})
	if got := strings.Count(removedText, "removed"); got != 1 {
		t.Errorf("the removed report states the cleanup fact %d times, want once:\n%s", got, removedText)
	}
}
