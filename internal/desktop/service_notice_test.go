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
// The registration survives the desktop app and its data is not the data of
// the desktop. A confirmation that left one of them out would let the user
// agree to something they did not read. No administrator-rights or
// Windows-prompt sentence appears anywhere in it (decision of 2026-11-24):
// the Windows prompt that follows states that itself.
func TestInstallServiceTextStatesWhatTheRegistrationChanges(t *testing.T) {
	const dataDir = `C:\ProgramData\HFDesk`
	text := installServiceText(dataDir, false)

	assertNames(t, text, "the confirmation", []string{
		"starts at boot",
		"keeps running after this window closes",
		"The one file HFDesk can download",
		"only after its SHA-256 is verified",
		"nothing else is downloaded or copied",
		"Its data is separate",
		"settings, jobs and models",
		dataDir,
		"are not there",
	})
	assertNamesWithout(t, text, "the confirmation", nil,
		[]string{"administrator", "Windows prompt"})
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
	text := installServiceText("", false)
	if strings.Contains(text, "  ") {
		t.Errorf("the confirmation %q carries an empty directory", text)
	}
	if !strings.Contains(text, "do not appear in the service") {
		t.Errorf("the confirmation %q does not state the separation of the data", text)
	}
}

// TestInstallServiceTextWarnsOnlyForARemovableOrNetworkProgram pins the B2
// line of the installation confirmation at the text level: the warning
// appears exactly when its flag is set, the ordinary confirmation is
// byte-for-byte the text without it, and the warning itself is one short
// human line that states the consequence for the user and names no
// mechanism behind it. The decision WHEN the flag is set (the drive type of
// the program path) is pinned by the Windows test of the drive seam.
func TestInstallServiceTextWarnsOnlyForARemovableOrNetworkProgram(t *testing.T) {
	const dataDir = `C:\ProgramData\HFDesk`
	ordinary := installServiceText(dataDir, false)
	warned := installServiceText(dataDir, true)

	if strings.Contains(ordinary, serviceProgramDriveWarningLine) {
		t.Errorf("the confirmation of an ordinary drive carries the warning:\n%s", ordinary)
	}
	if !strings.Contains(warned, serviceProgramDriveWarningLine) {
		t.Errorf("the confirmation of a removable or network drive carries no warning:\n%s", warned)
	}
	if count := strings.Count(warned, serviceProgramDriveWarningLine); count != 1 {
		t.Errorf("the warning appears %d times, want exactly once", count)
	}
	// With the warning added, everything else stays: the ordinary text
	// is this very text minus its one line, so a warning cannot quietly
	// rewrite the rest of the confirmation.
	if strings.Replace(warned, serviceProgramDriveWarningLine+"\n\n", "", 1) != ordinary {
		t.Errorf("the warned confirmation differs from the ordinary one by more than its warning line")
	}
	// The warning is the single line of the family's rule: human words,
	// the consequence for the user, and no internal mechanism.
	for _, mechanism := range []string{
		"GetDriveType", "DRIVE_", "kernel32", "drive type", "API",
		"mount", "letter", "volume", "bus",
	} {
		if strings.Contains(serviceProgramDriveWarningLine, mechanism) {
			t.Errorf("the warning names the mechanism %q: %q", mechanism, serviceProgramDriveWarningLine)
		}
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

// TestServiceUpdatedTextKeepsAStoppedServiceStopped proves that the report of
// an update of a stopped service says that nothing serves HFDesk afterwards.
//
// An update never starts a stopped service, so a message that sent the user to
// the address of the service would send them to a dead address. The desktop
// surface keeps its established way back — the relaunch of the desktop app and
// its Start service — byte for byte (the decision of 2026-11-23 kept that
// route), and the companion surface names its own Start service item instead
// (TestServiceUpdatedTextNamesTheCompanionsOwnStartItem). An update is
// reachable from the menu, the dialog, the companion and the launch gate; a
// menu, dialog or companion update leaves the desktop host running and a
// gate update runs after its launch already ended, so each surface gets the
// route its user can actually reach.
func TestServiceUpdatedTextKeepsAStoppedServiceStopped(t *testing.T) {
	const address = "http://127.0.0.1:8080"
	text := serviceUpdatedText("1.3.0", `C:\portable\hfdesk.exe`, `C:\ProgramData\HFDesk`, address, false, service.StateStopped, launchSurfaceDesktop, "")

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
		t.Errorf("the report %q carries the companion route on the desktop surface", text)
	}
}

// TestServiceUpdatedTextNamesTheCompanionsOwnStartItem proves the companion
// half of the surface-dependent way back: the report of a not-running update
// on the companion surface names that companion's own Start service item —
// exactly one step — and never the desktop route of a launch that never ran
// here.
func TestServiceUpdatedTextNamesTheCompanionsOwnStartItem(t *testing.T) {
	const address = "http://127.0.0.1:8080"
	for _, before := range []service.State{service.StateStopped, service.StateFailed} {
		text := serviceUpdatedText("1.3.0", `C:\portable\hfdesk.exe`, `C:\ProgramData\HFDesk`, address, false, before, launchSurfaceCompanion, "")

		assertNames(t, text, "the companion report", []string{
			"Nothing serves HFDesk right now",
			"Choose Start service from the tray menu.",
		})
		// The way back is ONE step, the current menu item of this tray.
		if got := strings.Count(text, "Start service"); got != 1 {
			t.Errorf("the companion report names Start service %d times, want exactly one:\n%s", got, text)
		}
		assertNamesWithout(t, text, "the companion report", nil, []string{
			"Quit HFDesk", "Start the desktop app again", "hfdesk service", "elevated shell", " or ",
		})
	}
}

// TestServiceUpdatedTextReportsARunningService proves that the report of an
// update of a running service names the address and tells the user that the
// desktop app is not needed to use the service.
func TestServiceUpdatedTextReportsARunningService(t *testing.T) {
	const address = "http://127.0.0.1:8080"
	text := serviceUpdatedText("1.3.0", `C:\portable\hfdesk.exe`, `C:\ProgramData\HFDesk`, address, true, service.StateRunning, launchSurfaceDesktop, "")

	// The settings fact stays pinned in the wording of the prescribed
	// report line — "The registration now runs … — settings and data
	// kept." (decision of 2026-11-24).
	assertNames(t, text, "the report", []string{address, "not needed to use the service", `C:\portable\hfdesk.exe`, "settings and data kept"})
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
			text := serviceUpdatedText("1.3.0", `C:\portable\hfdesk.exe`, `C:\ProgramData\HFDesk`, address, false, tc.before, launchSurfaceDesktop, "")

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

// TestServiceInstalledTextCarriesTheAuthorizedTrayFallbackOnce pins the one
// conditional instruction the user authorized for the report of a finished
// installation (decision of 2026-11-23): the line appears exactly once in this
// report, it states the state plus one human action and names no mechanism,
// and it belongs to this report alone — the update and the start reports never
// carry it.
//
// The report's rule of at most one next step stays intact with the line in
// place: the address instruction remains the only unconditional action, no
// other route sits beside it, and the tray line is not counted as a second
// step because it is conditional — it speaks only for the rare restart that
// left no tray icon. That conditional form is the acknowledged exception this
// report carries by the same authorization, so a second UNCONDITIONAL step
// still fails here while the conditional line itself must stay exactly once.
func TestServiceInstalledTextCarriesTheAuthorizedTrayFallbackOnce(t *testing.T) {
	const (
		console = `C:\portable\hfdesk.exe`
		dataDir = `C:\ProgramData\HFDesk`
		address = "http://127.0.0.1:8080"
	)
	install := serviceInstalledText("1.3.0", console, dataDir, address)

	// The line is present exactly once — a duplicated fallback would read
	// as a second instruction of the same report.
	if got := strings.Count(install, serviceInstalledTrayFallbackLine); got != 1 {
		t.Errorf("the report carries the tray fallback %d times, want exactly once:\n%s", got, install)
	}
	// The report keeps its shape: the state comes first, the conditional
	// line sits right after the consequence it qualifies and before the
	// closing fact about the data of the service.
	if !strings.HasPrefix(install, "HFDesk 1.3.0 is installed as a Windows service.") {
		t.Errorf("the report %q does not open with its state", install)
	}
	if !strings.Contains(install, "open the address above.\n\n"+serviceInstalledTrayFallbackLine) {
		t.Errorf("the report %q does not carry the fallback right after the address consequence:\n%s", install, install)
	}
	if !strings.HasSuffix(install, "do not appear in the service.") {
		t.Errorf("the report %q does not end with the data fact:\n%s", install, install)
	}

	// The conditional form IS the exception: the line opens with its own
	// condition, so it prescribes an action only for the rare failure it
	// names and never counts as a second next step of the report.
	if !strings.HasPrefix(serviceInstalledTrayFallbackLine, "If the tray icon does not appear") {
		t.Errorf("the tray line %q does not open with its condition", serviceInstalledTrayFallbackLine)
	}
	// The one next step stays exactly one: the address instruction is the
	// only unconditional action, and no alternative route sits beside it.
	if got := strings.Count(install, "open the address above"); got != 1 {
		t.Errorf("the report carries the address instruction %d times, want exactly once:\n%s", got, install)
	}
	for _, route := range []string{
		"hfdesk service", "Start the desktop app again", "Quit HFDesk",
		"choose Start service", "Install the service again", "Run \"",
	} {
		if strings.Contains(install, route) {
			t.Errorf("the report carries a second route %q:\n%s", route, install)
		}
	}

	// The line names no mechanism: no account, no right, no code path.
	lower := strings.ToLower(serviceInstalledTrayFallbackLine)
	for _, mechanism := range []string{
		"nt authority", "localservice", "localsystem", "account", "permission", "administrator",
		"elevated", "execute", "traverse", "read access", "write access",
		`c:\`, ".exe", "hfdesk service", "path", "%",
		"flag", "guard", "mutex", "singleton", "claim", "handover",
		"registry", "service control manager", "scm",
	} {
		if strings.Contains(lower, mechanism) {
			t.Errorf("the tray line names the mechanism %q: %q", mechanism, serviceInstalledTrayFallbackLine)
		}
	}

	// The line belongs to this report alone: the update (in both of its
	// states) and the start report carry neither it nor any tray wording.
	for name, text := range map[string]string{
		"the update of a running service":                          serviceUpdatedText("1.3.0", console, dataDir, address, true, service.StateRunning, launchSurfaceDesktop, ""),
		"the update of a stopped service":                          serviceUpdatedText("1.3.0", console, dataDir, address, false, service.StateStopped, launchSurfaceDesktop, ""),
		"the update of a stopped service on the companion surface": serviceUpdatedText("1.3.0", console, dataDir, address, false, service.StateStopped, launchSurfaceCompanion, ""),
		"the start report":                                         serviceStartedText(console, address),
	} {
		if strings.Contains(text, serviceInstalledTrayFallbackLine) || strings.Contains(text, "tray icon") {
			t.Errorf("the %s carries the tray fallback:\n%s", name, text)
		}
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
			// No account or folder mechanism beside the report: the
			// refusal states its own cause.
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

// TestServiceActionFailureTextNamesNoFolderAccessMechanism proves the
// folder guidance stays out of every failure report: the sentence that named
// the service account and its execute/traverse/read rights over the folder of
// the program was internal mechanism, a justification and a legal hedge
// beside the one action of the report — the rule of the sixth plan item
// allows none of that. A failure report is result, cause and one action, so
// no account identity, no access right and no program path returns to it.
func TestServiceActionFailureTextNamesNoFolderAccessMechanism(t *testing.T) {
	reason := errors.New("the service control manager refused the registration")
	const program = `C:\portable\hfdesk.exe`

	for _, action := range []serviceAction{serviceActionInstall, serviceActionUpdate, serviceActionStart} {
		text := serviceActionFailureText(action, reason, program)
		assertNamesWithout(t, text, fmt.Sprintf("the report of %s", action), nil, []string{
			program, `NT AUTHORITY\LocalService`, "must be able to execute", "traverse the folder", "read access",
		})
	}

	// A caller that does not know the program carries no general form of
	// the retired guidance either.
	unknown := serviceActionFailureText(serviceActionInstall, reason, "")
	assertNamesWithout(t, unknown, "the report without a program", nil,
		[]string{"must be able to execute the program and to traverse the folder that holds it"})
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
	assertNamesWithout(t, removed, "the removed-install report", nil, []string{"hfdesk service uninstall", program, `NT AUTHORITY\LocalService`, "could not confirm"})

	cleanupErr := errors.New("the service process identity changed")
	cleanup := &service.InstallStartError{Cause: reason, CleanupError: cleanupErr}
	kept := serviceInstallStartFailureText(cleanup, program, cleanup)
	assertNamesWithout(t, kept, "the kept-install report",
		[]string{reason.Error(), cleanupErr.Error(), "registration was kept", "could not prove", "hfdesk service status"},
		[]string{program, `NT AUTHORITY\LocalService`, "could not confirm"})
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
	assertNamesWithout(t, removeText, "the stopped-but-kept report", nil, []string{`"hfdesk service uninstall"`, "do not want to keep the registration", "could not confirm"})
	if got := strings.Count(removeText, "hfdesk service"); got != 1 {
		t.Errorf("the stopped-but-kept report %q names %d commands, want exactly one", removeText, got)
	}
}

// TestServiceInstallStartFailureTextSplitsTheStartOutcomeClasses proves the
// compressed form decided on 2026-11-23: every classified class of a failed
// first start carries its OWN headline instead of a generic "could not
// confirm", the class and the budget are spoken exactly once — in that
// headline — and the ONE line below carries only the cause facts of the
// detail chain, never a repeat of the class.
//
// The not-ready headline names the budget the failing wait actually applied,
// so a different applied budget must show a different number, and no class
// but the not-ready one may name a budget at all. An error classified by no
// site gets the honest state of a start that did not finish plus its cause.
// The vague phrase "could not confirm" is forbidden in EVERY report of the
// family, the legacy shape included, and so is the old observation line: a
// duplicate of the class under its headline fails the prefix shape check and
// the "The start ran, but" ban at once — the mutation discrimination of
// this form.
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
	causeLine := cause.Error() + " — the registration was kept"

	// assertCompressedForm pins the form itself: headline, then ONE line
	// of cause facts, then the outcome. Re-adding the old observation
	// line breaks the prefix shape, reappears in the class-repeat ban and
	// doubles the headline count.
	assertCompressedForm := func(t *testing.T, text, headline, line string) {
		t.Helper()
		if !strings.HasPrefix(text, headline+"\n\n"+line+"\n\n") {
			t.Errorf("the report is not headline + one cause line + outcome:\n%s", text)
		}
		if strings.Contains(text, "The start ran, but") {
			t.Errorf("the report repeats the class in an observation line:\n%s", text)
		}
		if got := strings.Count(text, headline); got != 1 {
			t.Errorf("the class headline appears %d times, want exactly once:\n%s", got, text)
		}
	}

	notPerformedHeadline := "HFDesk registered the service but did not start it."
	notPerformed := serviceInstallStartFailureText(cause, program, kept(service.StartOutcomeNotPerformed, 0))
	assertCompressedForm(t, notPerformed, notPerformedHeadline, causeLine)
	assertNamesWithout(t, notPerformed, "the not-performed report",
		[]string{cause.Error(), "registration was kept"},
		[]string{"could not confirm"})

	endedHeadline := "HFDesk registered the service, but the service program ended before it was ready."
	ended := serviceInstallStartFailureText(cause, program, kept(service.StartOutcomeEnded, 0))
	assertCompressedForm(t, ended, endedHeadline, causeLine)
	assertNamesWithout(t, ended, "the ended report",
		[]string{endedHeadline, "registration was kept"},
		[]string{"could not confirm", "The start ran, but", "ended before it came up"})

	notReadyHeadline := "HFDesk registered the service, but the service did not come up within 45s."
	notReady := serviceInstallStartFailureText(cause, program, kept(service.StartOutcomeNotReady, 45*time.Second))
	assertCompressedForm(t, notReady, notReadyHeadline, causeLine)
	// The budget is spoken exactly once — in the headline.
	if got := strings.Count(notReady, "45s"); got != 1 {
		t.Errorf("the budget appears %d times in the not-ready report, want exactly once:\n%s", got, notReady)
	}
	assertNamesWithout(t, notReady, "the not-ready report", nil,
		[]string{"could not confirm", "The start ran, but"})

	otherBudgetHeadline := "HFDesk registered the service, but the service did not come up within 48s."
	otherBudget := serviceInstallStartFailureText(cause, program, kept(service.StartOutcomeNotReady, 48*time.Second))
	assertCompressedForm(t, otherBudget, otherBudgetHeadline, causeLine)
	if got := strings.Count(otherBudget, "48s"); got != 1 {
		t.Errorf("the applied budget appears %d times, want exactly once:\n%s", got, otherBudget)
	}
	assertNamesWithout(t, otherBudget, "the not-ready report with another applied budget",
		nil, []string{"45s"})

	// Even a mis-stamped budget never renders for another class: only the
	// not-ready report may name a budget, so an interrupted or unverifiable
	// wait can never claim a budget at the text layer.
	unverifiedHeadline := "HFDesk registered the service, but the started service could not be verified."
	unverified := serviceInstallStartFailureText(cause, program, kept(service.StartOutcomeUnverified, 45*time.Second))
	assertCompressedForm(t, unverified, unverifiedHeadline, causeLine)
	assertNamesWithout(t, unverified, "the unverified report",
		[]string{unverifiedHeadline},
		[]string{"did not come up within", "45s", "could not confirm", "The start ran, but"})

	legacyHeadline := "HFDesk registered the service, but its start did not finish."
	legacyErr := &service.InstallStartError{Cause: cause, CleanupError: errors.New("ownership unproven")}
	legacy := serviceInstallStartFailureText(legacyErr, program, legacyErr)
	assertCompressedForm(t, legacy, legacyHeadline, cause.Error()+" — ownership unproven")
	assertNamesWithout(t, legacy, "the unclassified report",
		[]string{cause.Error(), "ownership unproven"},
		[]string{"ended before", "did not come up", "could not verify the started service", "did not start it",
			"could not confirm", "The start ran"})

	// The cause line carries the reason facts as the detail chain reports
	// them — the state word and the exit code of the control manager —
	// and nothing of the class: the class lives in the headline alone.
	stateCause := errors.New(`the service control manager reports the state "stopped" with the exit code 1`)
	stateReport := serviceInstallStartFailureText(stateCause, program, &service.InstallStartError{
		Cause: stateCause, Outcome: service.StartOutcomeEnded, CleanupError: errors.New("the registration was kept"),
	})
	assertCompressedForm(t, stateReport, endedHeadline, stateCause.Error()+" — the registration was kept")
	if strings.Contains(stateReport, "The start ran, but") || strings.Count(stateReport, stateCause.Error()) != 1 {
		t.Errorf("the state-fact report does not carry its cause exactly once without a class repeat:\n%s", stateReport)
	}

	// The vague phrase and the duplicated class line are gone from the
	// WHOLE family — every class, the removed shape and the legacy shape
	// included. Without this sweep a single report could carry either
	// while the per-class pins above pass.
	for name, text := range map[string]string{
		"the not-performed report": notPerformed,
		"the ended report":         ended,
		"the not-ready report":     notReady,
		"the unverified report":    unverified,
		"the unclassified report":  legacy,
		"the state-fact report":    stateReport,
		"the removed report": serviceInstallStartFailureText(cause, program,
			&service.InstallStartError{Cause: cause, RegistrationRemoved: true, Outcome: service.StartOutcomeEnded}),
	} {
		if strings.Contains(text, "could not confirm") {
			t.Errorf("the %s carries the forbidden vague phrase:\n%s", name, text)
		}
		if strings.Contains(text, "The start ran, but") {
			t.Errorf("the %s repeats the class below its headline:\n%s", name, text)
		}
	}
}

// TestInstallStartFailureCauseLineCarriesTheObservationNotTheWrapper pins
// the REAL cause chain a failed first start builds and its distillation in
// the report: the readiness wait returns a wrapper that names the program
// and the budget again ("the instance does not run … within 45s: …") and the
// cause wraps that once more. Under a headline that already speaks the class
// — and for not-ready the budget — that echo must not survive: the report's
// cause line carries the clean observation facts the wait recorded
// (InstallStartError.Observation), as one line and without the wrapper.
//
// Each class carries the fact style its wait really observed: the state word
// for not-ready, the state word with the exit code for ended, the
// unreadable-state reason when no state could ever be read (unverified). A
// record whose Outcome classifies nothing keeps the same distillation, so
// the budget never appears under a legacy headline either.
func TestInstallStartFailureCauseLineCarriesTheObservationNotTheWrapper(t *testing.T) {
	program := `C:\portable\hfdesk.exe`
	reason := "the registration was kept"
	wrapped := func(budget, observation string) error {
		return fmt.Errorf("service: the first installation did not put the new executable into service: %w",
			fmt.Errorf("the instance does not run %q within %s: %s", program, budget, observation))
	}
	// The cause paragraph is EXACTLY the observation (plus the cleanup
	// reason on the same line); the wrapper and its budget echo never
	// reach the report, and the class headline occurs once.
	assertCleanCauseLine := func(t *testing.T, text, headline, observation string) {
		t.Helper()
		want := headline + "\n\n" + observation + " — " + reason + "\n\n"
		if !strings.HasPrefix(text, want) {
			t.Errorf("the report is not headline + the clean cause line:\n%s", text)
		}
		causeLine := strings.Split(text, "\n\n")[1]
		if strings.Contains(causeLine, "within") || strings.Contains(causeLine, "does not run") {
			t.Errorf("the cause line %q carries the service wrapper or its budget echo", causeLine)
		}
		if got := strings.Count(text, headline); got != 1 {
			t.Errorf("the class headline appears %d times, want exactly once:\n%s", got, text)
		}
	}

	// Not-ready: the budget is spoken exactly once — in the headline.
	notReadyObservation := `the service control manager reports the state "start-pending"`
	notReady := &service.InstallStartError{
		Cause:        wrapped("45s", notReadyObservation),
		Observation:  notReadyObservation,
		Outcome:      service.StartOutcomeNotReady,
		Budget:       45 * time.Second,
		CleanupError: errors.New(reason),
	}
	text := serviceInstallStartFailureText(notReady, program, notReady)
	assertCleanCauseLine(t, text,
		"HFDesk registered the service, but the service did not come up within 45s.", notReadyObservation)
	if got := strings.Count(text, "45s"); got != 1 {
		t.Errorf("the budget appears %d times, want exactly once — in the headline:\n%s", got, text)
	}
	if strings.Contains(text, "the instance does not run") {
		t.Errorf("the report carries the service wrapper of the wait:\n%s", text)
	}

	// Ended: the observation carries the state word and the exit code,
	// and no budget appears in the report at all.
	endedObservation := `the service control manager reports the state "stopped" with the exit code 1`
	ended := &service.InstallStartError{
		Cause:        wrapped("2m0s", endedObservation),
		Observation:  endedObservation,
		Outcome:      service.StartOutcomeEnded,
		CleanupError: errors.New(reason),
	}
	text = serviceInstallStartFailureText(ended, program, ended)
	assertCleanCauseLine(t, text,
		"HFDesk registered the service, but the service program ended before it was ready.", endedObservation)
	for _, banned := range []string{"within", "2m0s", "does not run"} {
		if strings.Contains(text, banned) {
			t.Errorf("the ended report carries %q although no budget belongs to it:\n%s", banned, text)
		}
	}

	// Unverified: no state could ever be read — that reason is the fact,
	// and the budget of the wrapper stays out of the report.
	unverifiedObservation := `the state of HFDesk cannot be read: access is denied`
	unverified := &service.InstallStartError{
		Cause:        wrapped("2m0s", unverifiedObservation),
		Observation:  unverifiedObservation,
		Outcome:      service.StartOutcomeUnverified,
		CleanupError: errors.New(reason),
	}
	text = serviceInstallStartFailureText(unverified, program, unverified)
	assertCleanCauseLine(t, text,
		"HFDesk registered the service, but the started service could not be verified.", unverifiedObservation)
	for _, banned := range []string{"within", "2m0s", "does not run"} {
		if strings.Contains(text, banned) {
			t.Errorf("the unverified report carries %q although no budget belongs to it:\n%s", banned, text)
		}
	}

	// A record that classifies nothing keeps the distillation: the legacy
	// headline never inherits a budget from the cause either.
	legacy := &service.InstallStartError{
		Cause:        wrapped("2m0s", endedObservation),
		Observation:  endedObservation,
		CleanupError: errors.New(reason),
	}
	text = serviceInstallStartFailureText(legacy, program, legacy)
	assertCleanCauseLine(t, text,
		"HFDesk registered the service, but its start did not finish.", endedObservation)
	for _, banned := range []string{"within", "2m0s", "does not run"} {
		if strings.Contains(text, banned) {
			t.Errorf("the legacy report carries %q although it names no budget:\n%s", banned, text)
		}
	}

	// Without the recorded observation the cause chain stays exactly what
	// it was — the distillation never invents facts of its own.
	noObservation := &service.InstallStartError{
		Cause:        errors.New("the first installation did not put the new executable into service"),
		Outcome:      service.StartOutcomeEnded,
		CleanupError: errors.New(reason),
	}
	if detail := installStartDetail(noObservation, noObservation); detail != "the first installation did not put the new executable into service — "+reason {
		t.Errorf("the cause line without an observation = %q, want the untouched cause chain", detail)
	}
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
		[]string{"HFDesk registered the service but did not start it.", "registration created by this attempt was removed",
			"left no registration behind", "start the desktop app normally again"},
		[]string{"HFDesk is not installed as a Windows service", "There is no service", "could not confirm",
			"The start ran, but"})
	// The compressed form holds on the removed branch too: headline, one
	// cause line, then the removal sentence — the class is never repeated.
	if !strings.HasPrefix(removed, "HFDesk registered the service but did not start it.\n\n"+cause.Error()+"\n\n") {
		t.Errorf("the removed report is not headline + one cause line + outcome:\n%s", removed)
	}

	never := serviceNotInstalledText(serviceActionStart)
	assertNames(t, never, "the never-installed report",
		[]string{"HFDesk is not installed as a Windows service", "Install it from the tray of the desktop app first"})

	// The removed report carries the start class in its own headline too:
	// a registration this attempt removed after a service that never came
	// up within its budget states both facts, and still never claims the
	// machine never had one.
	removedNotReady := serviceInstallStartFailureText(cause, program, &service.InstallStartError{
		Cause:               cause,
		RegistrationRemoved: true,
		Outcome:             service.StartOutcomeNotReady,
		Budget:              45 * time.Second,
	})
	assertNamesWithout(t, removedNotReady, "the removed not-ready report",
		[]string{"HFDesk registered the service, but the service did not come up within 45s.",
			"did not come up within 45s", "registration created by this attempt was removed",
			"start the desktop app normally again"},
		[]string{"HFDesk is not installed as a Windows service", "could not confirm", "The start ran, but"})
	// Headline with the applied budget, one cause line, removal sentence:
	// the budget is spoken exactly once, in the headline.
	if !strings.HasPrefix(removedNotReady,
		"HFDesk registered the service, but the service did not come up within 45s.\n\n"+cause.Error()+"\n\n") {
		t.Errorf("the removed not-ready report is not headline + one cause line + outcome:\n%s", removedNotReady)
	}
	if got := strings.Count(removedNotReady, "45s"); got != 1 {
		t.Errorf("the budget appears %d times in the removed not-ready report, want exactly once:\n%s",
			got, removedNotReady)
	}
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
// names the file and why the service needs it, the first-glance content is
// the two facts (what the program is for, the folder it is expected in), the
// offer is attached with the facts of its command link, the manual route
// carries only its steps, and the pairing explanation stays out of the first
// glance — it belongs to the folded details.
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

	notice := newSiblingMissingNotice(serviceActionInstall, folder, desktop, findErr, offer, nil)

	assertNames(t, notice.Instruction, "the instruction", []string{asset, "is missing", "the Windows service needs it"})
	assertNames(t, notice.Content, "the content", []string{
		"The Windows service runs this program",
		"It is expected in " + folder,
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
	// A notice that offers a download states no reason for the absence of
	// one, and the log path travels as a field the dialog turns into an
	// action — never as dead text beside the facts.
	if notice.Reason != "" {
		t.Errorf("the reason line is %q although a download is offered, want none", notice.Reason)
	}
	if strings.Contains(notice.Content, "The log file is at") {
		t.Errorf("the content %q prints the log path as text", notice.Content)
	}

	// The pairing explanation is long and belongs to the folded details,
	// not to the first glance; the manual text carries the get-put-retry
	// steps alone.
	if strings.Contains(notice.Content, "release") {
		t.Errorf("the content %q carries the pairing explanation", notice.Content)
	}
	assertNames(t, notice.Details, "the folded details", []string{
		"only runs the console program of that same release",
		"both belong in one folder",
	})
	assertNames(t, notice.Manual, "the manual text", []string{
		"Get release v1.3.0-beta.7",
		"releases/tag/v1.3.0-beta.7",
		"put " + asset + " next to hfdesk-desktop.exe",
		folder,
		"run the action again",
	})
	if strings.Contains(notice.Manual, "only runs the console program") {
		t.Errorf("the manual text %q carries the pairing explanation", notice.Manual)
	}

	// The labels of the links and the note of the download command link
	// are the approved copy; the note states size, release, the check and
	// what follows it, and the manual route opens the release page its
	// instruction text names.
	assertNames(t, siblingMissingDownloadLabel, "the primary label", []string{"Download and install now"})
	assertNames(t, siblingMissingDismissLabel, "the dismiss label", []string{"Not now"})
	assertNames(t, siblingMissingManualLabel, "the manual label", []string{"Open the release page"})
	assertNames(t, siblingMissingManualNote, "the manual note", []string{
		"Download the file there",
		"put it next to hfdesk-desktop.exe",
		"run the action again",
	})
	assertNames(t, siblingMissingRetryLabel, "the retry label", []string{"Try again"})
	assertNames(t, siblingMissingDetailsLabel, "the details label", []string{"Details"})
	assertNames(t, logFileOpenLabel, "the log label", []string{"Open the log file"})
	assertNames(t, siblingMissingDownloadNote(notice), "the note line", []string{
		"12.2 MB",
		"from the official release v1.3.0-beta.7",
		"SHA-256 checked",
		"then the usual install steps",
	})
	// The page the manual route opens is ONE URL: the same string the
	// manual instruction text names, both derived from the release of
	// this build's own name.
	if notice.ReleaseURL != releaseTagPagePrefix+testReleaseTag {
		t.Errorf("the release page is %q, want the URL of this build's release %q",
			notice.ReleaseURL, releaseTagPagePrefix+testReleaseTag)
	}
	if !strings.Contains(notice.Manual, notice.ReleaseURL) {
		t.Errorf("the manual text %q does not carry the release page %q the route opens",
			notice.Manual, notice.ReleaseURL)
	}

	// The claim the feature falsified must be gone from every surface of
	// the notice.
	for name, text := range map[string]string{
		"instruction": notice.Instruction,
		"content":     notice.Content,
		"manual":      notice.Manual,
		"details":     notice.Details,
	} {
		if strings.Contains(text, "never downloads") || strings.Contains(text, "nothing is downloaded or copied") {
			t.Errorf("the %s %q carries the falsified download claim", name, text)
		}
	}
}

// TestNewSiblingMissingNoticeWithoutAReleaseKeepsManualInstructionsOnly
// proves that a local build — a desktop program whose name carries no
// release version — gets no download offer and says why: the notice names
// hfdesk.exe, states the honest reason where the button would be, keeps the
// cause of the failed lookup and the pairing rule in the folded details, and
// offers the manual steps alone.
func TestNewSiblingMissingNoticeWithoutAReleaseKeepsManualInstructionsOnly(t *testing.T) {
	const folder = `C:\portable`
	notice := newSiblingMissingNotice(serviceActionInstall, folder, filepath.Join(folder, "hfdesk-desktop.exe"),
		errors.New("there is no hfdesk.exe in "+folder), nil, nil)

	if notice.Download != nil {
		t.Errorf("the notice offers the download %v for a local build, want no offer", notice.Download)
	}
	// No offer and no failure to classify is exactly the state of a name
	// without a release version: the reason line stands where the button
	// would be, so nothing disappears in silence.
	assertNames(t, notice.Reason, "the reason line", []string{"no release version"})
	assertNames(t, notice.Instruction, "the instruction", []string{"hfdesk.exe", "is missing"})
	assertNames(t, notice.Content, "the content", []string{
		"The Windows service runs this program",
		folder,
	})
	if strings.Contains(notice.Content, "there is no hfdesk.exe") {
		t.Errorf("the content %q carries the raw cause of the lookup", notice.Content)
	}
	assertNames(t, notice.Details, "the folded details", []string{
		"there is no hfdesk.exe",
		"ships both programs in one folder",
	})
	assertNames(t, notice.Manual, "the manual text", []string{
		"Put hfdesk.exe next to hfdesk-desktop.exe",
		folder,
		"run the action again",
	})
	if strings.Contains(notice.Manual, "Get release") {
		t.Errorf("the manual text %q names a release for a local build", notice.Manual)
	}
	// A local build names no release page: there is nothing the manual
	// route could open, so the notice carries no URL beside its
	// instructions.
	if notice.ReleaseURL != "" {
		t.Errorf("the notice of a local build carries the release page %q, want none", notice.ReleaseURL)
	}
}

// TestNewSiblingMissingNoticeKeepsTheCauseOfANonPairingLookupFailure proves
// the cause rule of the notice for a version-stamped desktop program: a
// folder that cannot be read is not the pairing rule, so its raw cause is
// kept — in the folded details, where every raw cause lives, never in the
// first glance the user reads before choosing anything.
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

	notice := newSiblingMissingNotice(serviceActionInstall, folder, desktop, findErr, nil, nil)
	assertNames(t, notice.Content, "the content", []string{
		"The Windows service runs this program",
		folder,
	})
	assertNames(t, notice.Details, "the folded details", []string{"cannot be read"})
	if strings.Contains(notice.Content, "cannot be read") {
		t.Errorf("the content %q carries the raw cause of the lookup", notice.Content)
	}
}

// TestSiblingMissingNoticeStatesTheReasonOfAnUnavailableDownload proves the
// rule of the notice when no download can be offered: one honest line stands
// where the button would be — the build whose name carries no release
// version, a release that could not be reached, a release that publishes
// nothing verifiable, and a failure no class names — while the raw cause
// stays in the folded details and never enters the first glance. With an
// offer there is no reason line at all.
func TestSiblingMissingNoticeStatesTheReasonOfAnUnavailableDownload(t *testing.T) {
	const folder = `C:\portable`
	desktop := filepath.Join(folder, testReleaseDesktop)

	for _, tc := range []struct {
		name       string
		offer      *consoleDownloadOffer
		offerErr   error
		wantReason string
	}{
		{
			name: "an offer exists",
			offer: &consoleDownloadOffer{
				Release: consoleRelease{Tag: testReleaseTag, Asset: testReleaseAsset},
				Size:    1024,
			},
		},
		{name: "the build carries no release version", wantReason: "no release version"},
		{
			name:     "the release could not be reached",
			offerErr: offerUnreachable(errors.New("dial tcp: no route to host")),
			// The connection is the one thing the user can act on
			// here, so the line names it and asks for exactly that.
			wantReason: "could not be reached",
		},
		{
			name:       "the release publishes nothing verifiable",
			offerErr:   offerUnpublished(errors.New("the release " + testReleaseTag + " publishes no SHA-256 checksum for " + testReleaseAsset)),
			wantReason: "does not publish this file with a checksum",
		},
		{
			name:       "a failure no class names",
			offerErr:   errors.New("something the resolve did not classify"),
			wantReason: "could not offer a verified download",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			notice := newSiblingMissingNotice(serviceActionInstall, folder, desktop,
				errors.New("there is no program in "+folder), tc.offer, tc.offerErr)

			if tc.wantReason == "" {
				if notice.Reason != "" {
					t.Errorf("the reason line is %q although a download is offered, want none", notice.Reason)
				}
				return
			}
			if notice.Download != nil {
				t.Errorf("the notice offers %v although no download can be offered", notice.Download)
			}
			assertNames(t, notice.Reason, "the reason line", []string{tc.wantReason})
			// The raw cause belongs to the folded details only: the
			// first glance states the fact, never the failure behind
			// it.
			if tc.offerErr != nil {
				assertNames(t, notice.Details, "the folded details", []string{tc.offerErr.Error()})
				if strings.Contains(notice.Reason, tc.offerErr.Error()) {
					t.Errorf("the reason line %q carries the raw cause %q", notice.Reason, tc.offerErr)
				}
				if strings.Contains(notice.Content, tc.offerErr.Error()) {
					t.Errorf("the content %q carries the raw cause %q", notice.Content, tc.offerErr)
				}
			}
			// The reason never leaves the notice silent: the manual
			// route and the way to ask the release again are the
			// approved labels the links carry — for these notices the
			// desktop name carries a release, so the manual route is
			// the page-opening item.
			assertNames(t, siblingMissingManualLabel, "the manual label", []string{"Open the release page"})
			assertNames(t, siblingMissingRetryLabel, "the retry label", []string{"Try again"})
		})
	}
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
		}, nil)

	text := siblingMissingChooseText(notice)
	assertNames(t, text, "the fallback", []string{
		testReleaseAsset + " is missing",
		"The Windows service runs this program",
		"It is expected in " + folder,
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
// fail, and independently of the machine's progress-dialog gate — and the box
// then states that no download is offered instead of hiding a vanished
// button, beside the file, the folder, the manual path and the details this
// box has no fold for.
func TestSiblingMissingNoticeForFallbackSuppressesAnOfferThatCannotRun(t *testing.T) {
	const folder = `C:\portable`
	offered := newSiblingMissingNotice(serviceActionInstall, folder, filepath.Join(folder, testReleaseDesktop),
		errors.New("pairing"), &consoleDownloadOffer{
			Release: consoleRelease{Tag: testReleaseTag, Asset: testReleaseAsset},
			Size:    1024,
		}, nil)

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
	// in the "nothing was downloaded" report — and the reason line stands
	// where the button was, so the box explains itself.
	manual := siblingMissingNoticeForFallback(offered,
		errors.New("the dialog could not be shown (HRESULT 0x80070057)"))
	if manual.Download != nil {
		t.Errorf("the fallback keeps the offer %v although the transfer cannot run, want no offer", manual.Download)
	}
	if manual.Reason == "" {
		t.Error("the fallback drops the offer without stating why no download is offered")
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
		manual.Reason,
		manual.Details,
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
		}, nil)

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
			}, nil)
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
// action does, the one fact the user has to see before agreeing, and the
// question — with no internal mechanism and no claim about a state the
// confirmation did not read. Neither of them carries the rights sentence any
// more (decision of 2026-11-24), pinned for every action confirmation in
// TestServiceActionConfirmationsCarryNoAdministratorPhrase.
func TestStopAndRemoveConfirmationsStateTheActionAndTheOneFact(t *testing.T) {
	stop := stopServiceConfirmText()
	assertNames(t, stop, "the stop confirmation", []string{
		"This stops the HFDesk service.",
		"Nothing serves the address of the service until you start it again.",
		"Do you want to stop the service?",
	})
	// The confirmation names no state of its own: the state the menu item
	// was enabled by can be stale by the time the box is read, and no
	// mechanism of the stop is the user's business — and it repeats no
	// rights sentence.
	assertNamesWithout(t, stop, "the stop confirmation", nil, []string{
		"service control manager", "SCM", "registry", "is running", "was running",
		"administrator", "Windows prompt",
	})

	remove := removeServiceConfirmText(false)
	assertNames(t, remove, "the removal confirmation", []string{
		"removes the Windows service registration of HFDesk",
		"settings, jobs, models, cache, history and logs stay untouched",
		"Do you want to remove the registration?",
	})
	// The rights sentence is gone from this confirmation entirely — the
	// Windows prompt that follows states it itself (decision of
	// 2026-11-24).
	assertNamesWithout(t, remove, "the removal confirmation", nil,
		[]string{"administrator", "Windows prompt"})
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

// TestServiceActionConfirmationsCarryNoAdministratorPhrase pins the
// decision of 2026-11-24 over EVERY confirmation of a service action: the
// install (in both of its shapes), the stop, the start, the update and the
// removal (in both of its variants) never repeat the administrator-rights or
// Windows-prompt sentence — the Windows prompt that follows states that
// itself — and each keeps its exact prescribed form.
func TestServiceActionConfirmationsCarryNoAdministratorPhrase(t *testing.T) {
	const dataDir = `C:\ProgramData\HFDesk`
	for name, text := range map[string]string{
		"the installation confirmation":                installServiceText(dataDir, false),
		"the installation confirmation on a bad drive": installServiceText(dataDir, true),
		"the stop confirmation":                        stopServiceConfirmText(),
		"the start confirmation":                       startServiceConfirmText(),
		"the update confirmation":                      updateServiceConfirmText(),
		"the removal confirmation with a handover":     removeServiceConfirmText(true),
		"the removal confirmation without a handover":  removeServiceConfirmText(false),
	} {
		lower := strings.ToLower(text)
		for _, phrase := range []string{"administrator", "windows prompt"} {
			if strings.Contains(lower, phrase) {
				t.Errorf("the %s carries %q, want no rights sentence:\n%s", name, phrase, text)
			}
		}
	}

	// The exact forms of the decision: the question, then the one line
	// of facts — nothing else.
	assertNames(t, startServiceConfirmText(), "the start confirmation", []string{
		"Start the HFDesk service?",
		"This window closes into the tray and stays there.",
	})
	assertNames(t, updateServiceConfirmText(), "the update confirmation", []string{
		"Update the HFDesk service?",
		"This window closes into the tray and stays there.",
	})
	assertNames(t, removeServiceConfirmText(true), "the removal confirmation with a handover", []string{
		"Remove the HFDesk service registration?",
		"Your data stays untouched.",
		"HFDesk starts again in desktop mode.",
	})
	assertNames(t, removeServiceConfirmText(false), "the removal confirmation without a handover", []string{
		"This removes the Windows service registration of HFDesk.",
		"Your settings, jobs, models, cache, history and logs stay untouched.",
		"Do you want to remove the registration?",
	})
}

// TestServiceStoppedTextNamesTheStateTheFactAndOneWayBack proves that the
// report of a stopped service is the state, the one fact the user did not
// ask for — nothing serves the address of the service right now — and
// exactly one way back. On the desktop surface that way back goes through
// the quit and the startup gate, and it stays byte for byte the established
// route: a start beside the desktop that serves its own address is refused
// by the established guard of the helper, so a report that pointed at that
// start would send the user into the refusal in a circle. The companion
// surface gets its own route instead
// (TestServiceStoppedTextNamesTheCompanionsOwnStartItem).
func TestServiceStoppedTextNamesTheStateTheFactAndOneWayBack(t *testing.T) {
	text := serviceStoppedText(launchSurfaceDesktop)
	assertNames(t, text, "the report", []string{
		"The HFDesk service is stopped.",
		"Nothing serves the address of the service right now.",
		"Quit HFDesk from the notification-area icon",
		"choose Start service",
	})
	// One report, one route: no elevated command line and no alternative
	// beside the established way.
	assertNamesWithout(t, text, "the report", nil, []string{`"hfdesk service`, " or ", "elevated shell", "tray menu"})
	if serviceStoppedCaption == "" {
		t.Error("the report of a stopped service has no caption")
	}
}

// TestServiceStoppedTextNamesTheCompanionsOwnStartItem proves the companion
// half of the surface-dependent way back: the report of a stopped service on
// the companion surface names that companion's own Start service item — the
// current menu item — as exactly one step, because the companion serves no
// address and that item runs the start immediately. The desktop route of a
// launch that did not run here is absent.
func TestServiceStoppedTextNamesTheCompanionsOwnStartItem(t *testing.T) {
	text := serviceStoppedText(launchSurfaceCompanion)
	assertNames(t, text, "the companion report", []string{
		"The HFDesk service is stopped.",
		"Nothing serves the address of the service right now.",
		"Choose Start service from the tray menu.",
	})
	if got := strings.Count(text, "Start service"); got != 1 {
		t.Errorf("the companion report names Start service %d times, want exactly one:\n%s", got, text)
	}
	assertNamesWithout(t, text, "the companion report", nil, []string{
		"Quit HFDesk", "Start the desktop app again", `"hfdesk service`, " or ", "elevated shell",
	})
}

// TestLaunchSurfaceOfKeepsTheDesktopDefaultForAHandTypedHelper pins the
// honest default of a helper command line without the surface marker: the
// desktop route, which needs only the desktop app the user can start. The
// companion route is never the default — it names a menu item only a running
// companion has — so a hand-typed run cannot be sent looking for it.
func TestLaunchSurfaceOfKeepsTheDesktopDefaultForAHandTypedHelper(t *testing.T) {
	if got := launchSurfaceOf(false); got != launchSurfaceDesktop {
		t.Errorf("launchSurfaceOf without the marker = %q, want the desktop default %q", got, launchSurfaceDesktop)
	}
	if got := launchSurfaceOf(true); got != launchSurfaceCompanion {
		t.Errorf("launchSurfaceOf with the marker = %q, want %q", got, launchSurfaceCompanion)
	}
	if got, want := serviceStoppedText(launchSurfaceOf(false)),
		"Quit HFDesk from the notification-area icon and choose Start service when you start the desktop app again."; !strings.HasSuffix(got, want) {
		t.Errorf("the report of a marker-less helper run does not end with the established desktop way back %q:\n%s", want, got)
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

	// The removal has its own honest form: the state plus the absent
	// object, never the "start" of the default branch — the latent
	// branch of this function. Like the stop it stays a settled absence
	// with no next action: sending a user who asked to remove a
	// registration to its installation would answer a different
	// question, and the form never claims a deletion either.
	removal := serviceNotInstalledText(serviceActionUninstall)
	assertNames(t, removal, "the not-installed report of the removal", []string{
		"HFDesk is not installed as a Windows service.",
		"There is no service registration to remove.",
	})
	assertNamesWithout(t, removal, "the not-installed report of the removal", nil,
		[]string{"start", "Install it from the tray", "is removed", "was removed", "stay untouched"})

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
		filepath.Join(`C:\portable`, "hfdesk-desktop.exe"), cause, nil, nil)
	downloadNotice := newSiblingMissingNotice(serviceActionInstall, `C:\portable`,
		filepath.Join(`C:\portable`, "hfdesk-desktop.exe"), cause, &consoleDownloadOffer{
			Release: consoleRelease{Tag: testReleaseTag, Asset: testReleaseAsset},
			Size:    1024,
		}, nil)

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
		"the launch-failure report":             serviceLaunchFailedText(serviceActionInstall, cause),
		"the unknown-action report":             unknownServiceActionText(errors.New("unknown service action")),
		"the preparation refusal":               serviceHandoverRefusedText(cause),
		"the unfinished-install report":         serviceHandoverFailureText(serviceActionInstall, cause),
		"the unreadable gate notice":            serviceGateUnreadableText(cause),
		"the changed gate notice":               serviceGateChangedText(serviceChoicePrompt{State: service.StateRunning}),
		"the download failure":                  consoleDownloadFailedText(downloadNotice, cause),
		"the missing-program notice":            siblingMissingManualNoticeText(notice),
		"the browser failure":                   browserOpenFailedText("http://127.0.0.1:8080", cause),
		"the tray-startup failure":              trayStartupFailureText(),
		"the runtime-window failure":            runtimeWindowShowFailedText(errors.New("the window could not be shown")),
		"the service-log open failure":          serviceLogOpenFailedText(`C:\ProgramData\HFDesk\logs\hfdesk.log`, cause),
		"the service-log missing report":        serviceLogMissingText(`C:\ProgramData\HFDesk\logs\hfdesk.log`),
		"the pending gate notice":               serviceGatePendingText(service.StateStarting),
		"the runtime-window unavailable report": runtimeWindowUnavailableText("http://127.0.0.1:8080"),
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
		"the installation confirmation":                            installServiceText(`C:\ProgramData\HFDesk`, true),
		"the report of an installation":                            serviceInstalledText("1.3.0", program, `C:\ProgramData\HFDesk`, "http://127.0.0.1:8080"),
		"the report of a stopped service":                          serviceStoppedText(launchSurfaceDesktop),
		"the report of a stopped service on the companion surface": serviceStoppedText(launchSurfaceCompanion),
	}
	for name, text := range notFailure {
		if strings.Contains(text, "The log file is at") {
			t.Errorf("the %s carries the failure-only log line:\n%s", name, text)
		}
	}
}

// TestSplitLogFileLineReadsBackWhatLogFileLinePrinted proves the inverse of
// the standard log-path line: the renderer that turns that line into the
// action "Open the log file" gets the body of the window and the path the
// line really named — so the window and the action always name one file. A
// text that carries no line, or one that merely mentions the marker without
// ending in a path, keeps its whole body: nothing is ever opened on a guess.
func TestSplitLogFileLineReadsBackWhatLogFileLinePrinted(t *testing.T) {
	const path = `C:\Users\ada\AppData\Local\HFDesk\logs\hfdesk.log`
	body := "HFDesk could not install the Windows service.\n\nthe cause of the failure"

	gotBody, gotPath := splitLogFileLine(body + logFileLine(path))
	if gotBody != body {
		t.Errorf("the body is %q, want the window without its log line %q", gotBody, body)
	}
	if gotPath != path {
		t.Errorf("the path is %q, want %q", gotPath, path)
	}

	// A launch without a log file prints no line at all, so there is
	// nothing to split off and no file to open.
	if gotBody, gotPath = splitLogFileLine(body); gotBody != body || gotPath != "" {
		t.Errorf("splitLogFileLine of a window without a log line gave (%q, %q), want the whole body and no path",
			gotBody, gotPath)
	}

	// The marker without a path behind it is not that line.
	if gotBody, gotPath = splitLogFileLine(body + logFileLineMarker); gotBody != body+logFileLineMarker || gotPath != "" {
		t.Errorf("splitLogFileLine of a dangling marker gave (%q, %q), want the whole text and no path",
			gotBody, gotPath)
	}
	// Nor is a line followed by more text: only the closing line of the
	// window is the line.
	followed := body + logFileLine(path) + "\n\none more sentence"
	if _, middlePath := splitLogFileLine(followed); middlePath != "" {
		t.Errorf("splitLogFileLine of a line in the middle gave the path %q, want none", middlePath)
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
		// The account/access mechanism class: a service account or an
		// execute/traverse/read right named as the mechanism of a
		// failure — the folder guidance these reports used to carry.
		"nt authority", "localservice", "account", "permission",
		"must be able to", "execute", "traverse", "read access", "write access",
	}
	cause := errors.New("the state of the service could not be settled")
	program := `C:\portable\hfdesk.exe`
	dataDir := `C:\ProgramData\HFDesk`
	notice := newSiblingMissingNotice(serviceActionInstall, `C:\portable`,
		filepath.Join(`C:\portable`, "hfdesk-desktop.exe"), cause, nil, nil)

	texts := map[string]string{
		"the running gate": serviceGateText(serviceChoicePrompt{
			State: service.StateRunning, URL: "http://127.0.0.1:8080", DataDir: dataDir}),
		"the stopped gate": serviceGateText(serviceChoicePrompt{
			State: service.StateStopped, DataDir: dataDir}),
		"the failed gate": serviceGateText(serviceChoicePrompt{
			State: service.StateFailed, Detail: "the instance exited with the code 1", DataDir: dataDir}),
		"the broken gate with a running instance": serviceGateText(serviceChoicePrompt{
			State: service.StateBroken, Running: true, DataDir: dataDir}),
		"the pending notice":                                 serviceGatePendingText(service.StateStarting),
		"the unreadable notice":                              serviceGateUnreadableText(cause),
		"the changed notice":                                 serviceGateChangedText(serviceChoicePrompt{State: service.StateRunning}),
		"the installation confirmation":                      installServiceText(dataDir, false),
		"the installation confirmation on a removable drive": installServiceText(dataDir, true),
		"the stop confirmation":                              stopServiceConfirmText(),
		"the removal confirmation":                           removeServiceConfirmText(false),
		"the start confirmation with a handover":             startServiceConfirmText(),
		"the update confirmation with a handover":            updateServiceConfirmText(),
		"the removal confirmation with a handover":           removeServiceConfirmText(true),
		"the service-log open failure":                       serviceLogOpenFailedText(`C:\ProgramData\HFDesk\logs\hfdesk.log`, cause),
		"the service-log missing report":                     serviceLogMissingText(`C:\ProgramData\HFDesk\logs\hfdesk.log`),
		"the installation failure":                           serviceActionFailureText(serviceActionInstall, cause, program),
		"the kept-start failure": serviceInstallStartFailureText(
			&service.InstallStartError{Cause: cause, CleanupError: errors.New("the process identity changed")}, program,
			&service.InstallStartError{Cause: cause, CleanupError: errors.New("the process identity changed")}),
		"the removed-start failure": serviceInstallStartFailureText(
			&service.InstallStartError{Cause: cause, RegistrationRemoved: true}, program,
			&service.InstallStartError{Cause: cause, RegistrationRemoved: true}),
		"the already-registered refusal": serviceActionFailureText(serviceActionInstall,
			fmt.Errorf("service: hfdesk is already registered%w", service.ErrAlreadyRegistered), program),
		"the not-ready report": serviceHelperNotReadyText(serviceActionStart,
			&helperNotReadyError{cause: cause}),
		"the launch-failure report":     serviceLaunchFailedText(serviceActionStart, cause),
		"the preparation refusal":       serviceHandoverRefusedText(cause),
		"the unfinished-install report": serviceHandoverFailureText(serviceActionInstall, cause),
		"the serving-desktop refusal":   serviceServingDesktopRefusedText(serviceActionStart),
		"the unreadable-state refusal": serviceUnreadableClaimRefusedText(serviceActionStart,
			`C:\Users\ada\AppData\Roaming\HFDesk\desktop-state.json`),
		"the not-installed report":                               serviceNotInstalledText(serviceActionStart),
		"the not-installed report of the removal":                serviceNotInstalledText(serviceActionUninstall),
		"the busy refusal":                                       serviceActionBusyText(),
		"the unknown-action report":                              unknownServiceActionText(cause),
		"the stopped report":                                     serviceStoppedText(launchSurfaceDesktop),
		"the stopped report of the companion surface":            serviceStoppedText(launchSurfaceCompanion),
		"the not-running update report":                          serviceUpdatedText("1.3.0", program, dataDir, "", false, service.StateStopped, launchSurfaceDesktop, ""),
		"the not-running update report of the companion surface": serviceUpdatedText("1.3.0", program, dataDir, "", false, service.StateStopped, launchSurfaceCompanion, ""),
		"the removed report":                                     serviceRegistrationRemovedText(),
		"the no-registration report":                             serviceNoRegistrationText(),
		"the missing-program notice":                             siblingMissingManualNoticeText(notice),
	}
	for name, text := range texts {
		lower := strings.ToLower(text)
		for _, word := range banned {
			if strings.Contains(lower, word) {
				t.Errorf("the %s carries the internal word %q:\n%s", name, word, text)
			}
		}
	}

	// The stop-list really catches the sentence class it was extended
	// for: the account/access sentence of the retired folder guidance.
	// Without this control the class words could be dropped again and
	// every text above would still pass.
	mechanism := strings.ToLower(`The Windows service runs as NT AUTHORITY\LocalService, so that account ` +
		`must be able to execute the program and to traverse the folder that holds it; ` +
		`it needs read access to the program only where a handler genuinely needs it.`)
	caught := false
	for _, word := range banned {
		if strings.Contains(mechanism, word) {
			caught = true
			break
		}
	}
	if !caught {
		t.Error("the stop-list does not catch the account/access mechanism sentence")
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

// TestInstallFailureReportsStateTheCauseInOneLineAndOneAction proves the
// shape rule of the sixth plan item for the reports of a failed
// installation beside their classes: the cause of the error is ONE line —
// the start cause and the one line its cleanup observed are one line of the
// report, never two paragraphs the reader has to join — and the report
// carries exactly one action.
//
// The count is taken over the action vocabulary of these reports (the state
// command and the relaunch of the desktop app), so "exactly one" is a count
// and not a hope: a second command or a second route fails here. The
// account/access mechanism sentence cannot slip back beside the count —
// that class fails the stop-list of
// TestGateAndReportsCarryNoInternalMechanism.
func TestInstallFailureReportsStateTheCauseInOneLineAndOneAction(t *testing.T) {
	cause := errors.New("the first installation did not put the new executable into service")
	reason := errors.New("the service process identity changed")
	kept := &service.InstallStartError{Cause: cause, CleanupError: reason}
	causeOnly := &service.InstallStartError{Cause: cause}
	removed := &service.InstallStartError{Cause: cause, CleanupError: reason, RegistrationRemoved: true}
	const program = `C:\portable\hfdesk.exe`

	for name, detail := range map[string]string{
		"cause and cleanup reason": installStartDetail(kept, kept),
		"cause alone":              installStartDetail(causeOnly, causeOnly),
	} {
		if strings.ContainsAny(detail, "\r\n") {
			t.Errorf("the %s is %q, want the whole cause on one line", name, detail)
		}
	}

	const (
		statusCommand = "hfdesk service"
		relaunchAgain = "start the desktop app"
	)
	for name, text := range map[string]string{
		"the installation failure":  serviceActionFailureText(serviceActionInstall, cause, program),
		"the kept-start failure":    serviceInstallStartFailureText(kept, program, kept),
		"the removed-start failure": serviceInstallStartFailureText(removed, program, removed),
	} {
		if got := strings.Count(text, statusCommand) + strings.Count(text, relaunchAgain); got != 1 {
			t.Errorf("the %s carries %d actions, want exactly one:\n%s", name, got, text)
		}
	}
}
