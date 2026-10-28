// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package desktop

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bashrusakh/hfdesk/internal/runtimepaths"
	"github.com/bashrusakh/hfdesk/internal/service"
	"golang.org/x/sys/windows"
)

// replaceSeam points a package level seam at a value of the test and restores
// the production value when the test ends.
//
// The seams of the service flow exist because a test cannot register a
// machine-wide service, answer a prompt of the user account control or show a
// dialog without an interactive window station. The helper keeps the restore in
// one place, so a test cannot leave a seam of its own behind for the next one.
func replaceSeam[T any](t *testing.T, seam *T, value T) {
	t.Helper()
	previous := *seam
	*seam = value
	t.Cleanup(func() { *seam = previous })
}

// fakeServiceManager records the calls of the service helper.
//
// It implements the manager of the service layer, so a test can drive the
// helper without a registration: the service control manager accepts a
// machine-wide registration from an administrator shell only.
type fakeServiceManager struct {
	// statuses are the states reported in order. The last one repeats, so a
	// wait that reads the state more than once ends in a stable value.
	statuses []service.Status
	// installResult is the installation the manager reports. It is the
	// registration as written, which is what the helper reports.
	installResult service.InstallOptions
	// installErr is the failure of an installation.
	installErr error
	// startErr is the failure of a start.
	startErr error
	// uninstallErr is the failure of a removal of the registration.
	uninstallErr error
	// statusErr is the failure of every read of the state.
	statusErr error
	// statusFailures are the failures of the reads of the state, in order: a
	// nil entry is a read that succeeds, and a read beyond the list succeeds
	// as well. It lets a test fail one read without failing the read before
	// it, which is the state an installation reads first.
	statusFailures []error
	// steps records the calls in order, so a test can prove what the helper
	// did before it touched the service.
	steps []string
	// installs are the options of every installation.
	installs []service.InstallOptions
	// starts counts the starts.
	starts int
	// reads counts the reads of the state.
	reads int
}

// Install records the installation and reports the configured outcome.
func (m *fakeServiceManager) Install(_ context.Context, opts service.InstallOptions) (service.InstallOptions, error) {
	m.steps = append(m.steps, "install")
	m.installs = append(m.installs, opts)
	if m.installErr != nil {
		return opts, m.installErr
	}
	if m.installResult.Executable != "" {
		return m.installResult, nil
	}
	return opts, nil
}

// Uninstall records the call and reports the configured outcome.
func (m *fakeServiceManager) Uninstall(context.Context) error {
	m.steps = append(m.steps, "uninstall")
	return m.uninstallErr
}

// Start records the start and reports the configured outcome.
func (m *fakeServiceManager) Start(context.Context) error {
	m.steps = append(m.steps, "start")
	m.starts++
	return m.startErr
}

// Stop records the call.
func (m *fakeServiceManager) Stop(context.Context) error {
	m.steps = append(m.steps, "stop")
	return nil
}

// Restart records the call.
func (m *fakeServiceManager) Restart(context.Context) error {
	m.steps = append(m.steps, "restart")
	return nil
}

// Status reports the next state of the test.
func (m *fakeServiceManager) Status(context.Context) (service.Status, error) {
	m.steps = append(m.steps, "status")
	if m.statusErr != nil {
		return service.Status{}, m.statusErr
	}
	if len(m.statuses) == 0 {
		return service.Status{}, errors.New("the test configured no state")
	}
	index := m.reads
	m.reads++
	if index < len(m.statusFailures) && m.statusFailures[index] != nil {
		return service.Status{}, m.statusFailures[index]
	}
	if index >= len(m.statuses) {
		index = len(m.statuses) - 1
	}
	return m.statuses[index], nil
}

// stepIndex returns the position of the first step with the given name, or -1.
func stepIndex(steps []string, name string) int {
	for index, step := range steps {
		if step == name {
			return index
		}
	}
	return -1
}

// recordMessages replaces the message box with a recorder.
func recordMessages(t *testing.T) *[]shownMessage {
	t.Helper()
	messages := &[]shownMessage{}
	replaceSeam(t, &showMessage, func(caption, text string, icon uint32) {
		*messages = append(*messages, shownMessage{caption: caption, text: text, icon: icon})
	})
	return messages
}

// stubNoServiceRegistration points the service status at a machine without a
// registration.
//
// The tray action names the data directory of an existing registration in its
// confirmation, so it reads the registration. A test that does not stub the
// status would read the real service control manager of the machine.
func stubNoServiceRegistration(t *testing.T) {
	t.Helper()
	replaceSeam(t, &serviceStatus, func(context.Context) (service.Status, error) {
		return service.Status{State: service.StateNotInstalled}, nil
	})
}

// freshServiceInstallGuard gives a test a guard of its own.
//
// The guard outlives one call: an action that handed the installation over keeps
// it, because the desktop host quits after the hand-over. A test that proved that
// hand-over would therefore leave a guard behind that makes the click of the next
// test look like the second click of the same action.
func freshServiceInstallGuard(t *testing.T) {
	t.Helper()
	serviceInstallRunning.Store(false)
	t.Cleanup(func() { serviceInstallRunning.Store(false) })
}

// runningServiceStatus is the status of a running service of a test.
func runningServiceStatus(state service.State, listen string) service.Status {
	return service.Status{
		State:  state,
		Listen: listen,
		Paths:  runtimepaths.Paths{StateDir: `C:\ProgramData\HFDesk`},
	}
}

// TestLaunchWithARunningServiceNeverStartsItsOwnServer proves that a launch
// that finds a running service opens the web UI of the service and ends before
// it creates a server of its own.
//
// The launch is driven through the production startup, so the order of the
// steps cannot be swapped again: a service gate behind app.New starts a second
// server and the test ends in its timeout. The launch writes no published state
// either: the state file belongs to a launch that really serves.
func TestLaunchWithARunningServiceNeverStartsItsOwnServer(t *testing.T) {
	useTemporaryStateDir(t)

	replaceSeam(t, &serviceStatus, func(context.Context) (service.Status, error) {
		return runningServiceStatus(service.StateRunning, "0.0.0.0:18080"), nil
	})
	replaceSeam(t, &showServiceChoice, func(prompt serviceChoicePrompt) serviceChoice {
		if prompt.State != service.StateRunning {
			t.Errorf("the dialog asked about the state %q, want the running service", prompt.State)
		}
		if prompt.URL != "http://127.0.0.1:18080" {
			t.Errorf("the dialog named the address %q, want the loopback address of the wildcard bind", prompt.URL)
		}
		return serviceChoiceOpen
	})

	var opened []string
	replaceSeam(t, &openBrowser, func(url string) error {
		opened = append(opened, url)
		return nil
	})
	messages := recordMessages(t)

	// The launching goroutine starts a logging pipeline, so the logger of the
	// process is put back when the test ends.
	previousLogger := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	logFile := filepath.Join(t.TempDir(), "hfdesk.log")
	done := make(chan error, 1)
	go func() { done <- run(Options{}, logFile) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the launch with an installed service failed: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the launch with an installed service did not end: it started a server of its own")
	}

	if len(opened) != 1 || opened[0] != "http://127.0.0.1:18080" {
		t.Errorf("the launch opened %v, want the web UI of the service", opened)
	}
	if _, ok := readPublishedState(publishedStatePath()); ok {
		t.Error("the launch published a state file although it started no server")
	}
	for _, message := range *messages {
		switch message.caption {
		case serviceGateCaption, browserOpenFailedCaption, serviceFailureCaption(serviceActionStart):
			t.Errorf("the launch showed the message %q: %q", message.caption, message.text)
		}
	}
}

// TestServiceGateContinuesWhenNoServiceIsInstalled proves that a machine
// without a registration starts the desktop host exactly as it always did.
func TestServiceGateContinuesWhenNoServiceIsInstalled(t *testing.T) {
	replaceSeam(t, &serviceStatus, func(context.Context) (service.Status, error) {
		return service.Status{State: service.StateNotInstalled}, nil
	})
	replaceSeam(t, &showServiceChoice, func(serviceChoicePrompt) serviceChoice {
		t.Fatal("the service gate asked the user about a machine without a registration")
		return serviceChoiceCancel
	})

	if !serviceGate() {
		t.Fatal("the service gate stopped a launch on a machine without a service")
	}
}

// TestLaunchWithAnUnreadableServiceStartsNoServer proves that the production
// startup ends at the service gate when the registration cannot be read.
//
// The launch is driven through run(), so the order of the steps cannot be
// swapped: a gate that continues creates a server of its own next to the service
// the machine may be running. The launch publishes no state either, because the
// state file belongs to a launch that really serves.
func TestLaunchWithAnUnreadableServiceStartsNoServer(t *testing.T) {
	useTemporaryStateDir(t)

	replaceSeam(t, &serviceStatus, func(context.Context) (service.Status, error) {
		return service.Status{}, errors.New("the service control manager did not answer")
	})
	replaceSeam(t, &showServiceChoice, func(serviceChoicePrompt) serviceChoice {
		t.Fatal("the launch asked the user although it could not read the registration")
		return serviceChoiceCancel
	})
	messages := recordMessages(t)

	// The launching goroutine starts a logging pipeline, so the logger of the
	// process is put back when the test ends.
	previousLogger := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	logFile := filepath.Join(t.TempDir(), "hfdesk.log")
	done := make(chan error, 1)
	go func() { done <- run(Options{}, logFile) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the launch with an unreadable service failed: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the launch with an unreadable service did not end: it started a server of its own")
	}

	if _, ok := readPublishedState(publishedStatePath()); ok {
		t.Error("the launch published a state file although it started no server")
	}

	// The test binary is a console subsystem program, so this launch also
	// reports its build: the notice of the gate is searched among the
	// messages instead of counting all of them.
	var notices []shownMessage
	for _, message := range *messages {
		if message.caption == serviceGateUnreadableCaption {
			notices = append(notices, message)
		}
		if message.caption == serviceGateCaption {
			t.Errorf("the launch asked the user to choose although it could not read the registration: %q",
				message.text)
		}
	}
	if len(notices) != 1 {
		t.Fatalf("the launch showed %v, want the notice of the unreadable registration", *messages)
	}
	if !strings.Contains(notices[0].text, "started no server") {
		t.Errorf("the notice %q does not say that the launch started no server", notices[0].text)
	}
}

// TestServiceGateEndsTheLaunchWhenTheRegistrationCannotBeRead proves that a
// registration the host cannot read ends the launch with a notice.
//
// A service that is installed may be running and bind the address the desktop
// server would bind, so a launch that cannot read the state starts no server of
// its own: it reports what it could not read, that a service may be running, and
// how to read the state and to try again.
func TestServiceGateEndsTheLaunchWhenTheRegistrationCannotBeRead(t *testing.T) {
	replaceSeam(t, &serviceStatus, func(context.Context) (service.Status, error) {
		return service.Status{}, errors.New("the service control manager did not answer")
	})
	replaceSeam(t, &showServiceChoice, func(serviceChoicePrompt) serviceChoice {
		t.Fatal("the service gate asked the user although it could not read the registration")
		return serviceChoiceCancel
	})
	replaceSeam(t, &launchElevated, func(elevatedLaunch) (uint32, error) {
		t.Fatal("the service gate launched a helper although it could not read the registration")
		return 0, nil
	})
	replaceSeam(t, &openBrowser, func(string) error {
		t.Fatal("the service gate opened a browser although it could not read the registration")
		return nil
	})
	messages := recordMessages(t)
	records := &recordedMessages{}
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(records))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	if serviceGate() {
		t.Fatal("the service gate let a launch whose registration could not be read start its own server")
	}
	if len(*messages) != 1 {
		t.Fatalf("the gate showed %v, want the notice of the unreadable registration", *messages)
	}
	if caption := serviceGateUnreadableCaption; (*messages)[0].caption != caption {
		t.Errorf("the caption is %q, want %q", (*messages)[0].caption, caption)
	}
	if icon := uint32(windows.MB_ICONERROR); (*messages)[0].icon != icon {
		t.Errorf("the icon is %d, want the error icon %d", (*messages)[0].icon, icon)
	}
	for _, want := range []string{
		"the service control manager did not answer",
		"may be installed and running",
		"started no server",
		"hfdesk service status",
		"elevated shell",
		"start the desktop app again",
	} {
		if !strings.Contains((*messages)[0].text, want) {
			t.Errorf("the notice %q does not mention %q", (*messages)[0].text, want)
		}
	}
	// The launch writes the record as well: the helper installs no logging
	// pipeline, and the record of a launch that ended at the gate is the
	// material a later support request has.
	if !records.contains("could not read the Windows service registration: this launch starts no server") {
		t.Errorf("the records of the launch are %v, want the record of the unreadable registration",
			records.messages)
	}
}

// TestServiceGateStartsAStoppedServiceWithTheElevatedHelper proves that the
// start of a stopped service is handed to the elevated helper, and that the
// launch itself starts no server.
func TestServiceGateStartsAStoppedServiceWithTheElevatedHelper(t *testing.T) {
	replaceSeam(t, &serviceStatus, func(context.Context) (service.Status, error) {
		return runningServiceStatus(service.StateStopped, "127.0.0.1:18080"), nil
	})
	replaceSeam(t, &showServiceChoice, func(prompt serviceChoicePrompt) serviceChoice {
		if got, want := serviceGateOpenLabel(prompt), "Start service"; got != want {
			t.Errorf("the first button is %q, want %q", got, want)
		}
		return serviceChoiceStart
	})

	var launches []elevatedLaunch
	replaceSeam(t, &launchElevated, func(l elevatedLaunch) (uint32, error) {
		launches = append(launches, l)
		return 4242, nil
	})
	messages := recordMessages(t)

	if serviceGate() {
		t.Fatal("the service gate let a launch with an installed service start its own server")
	}
	if len(launches) != 1 {
		t.Fatalf("the gate launched %d helpers, want the start of the service", len(launches))
	}
	if launches[0].Action != serviceActionStart {
		t.Errorf("the helper performs %q, want %q", launches[0].Action, serviceActionStart)
	}
	if launches[0].WaitPID != 0 {
		t.Errorf("the helper waits for the process %d, want no wait: the gate started no server", launches[0].WaitPID)
	}
	if self, err := os.Executable(); err != nil {
		t.Fatalf("read the path of the desktop executable: %v", err)
	} else if launches[0].Executable != self {
		t.Errorf("the helper is %q, want the desktop executable %q", launches[0].Executable, self)
	}
	if len(*messages) != 0 {
		t.Errorf("the gate showed %v, want the helper to report the outcome", *messages)
	}
}

// TestServiceGateOpensAStartingServiceAfterItSettled proves that a launch that
// finds the service starting waits for it to settle and offers the running
// service instead of starting it a second time.
func TestServiceGateOpensAStartingServiceAfterItSettled(t *testing.T) {
	reads := 0
	replaceSeam(t, &serviceStatus, func(context.Context) (service.Status, error) {
		reads++
		if reads == 1 {
			return runningServiceStatus(service.StateStarting, "127.0.0.1:18080"), nil
		}
		return runningServiceStatus(service.StateRunning, "127.0.0.1:18080"), nil
	})
	replaceSeam(t, &showServiceChoice, func(prompt serviceChoicePrompt) serviceChoice {
		if prompt.State != service.StateRunning {
			t.Errorf("the dialog asked about the state %q, want the settled running service", prompt.State)
		}
		if got, want := serviceGateOpenLabel(prompt), "Open HFDesk"; got != want {
			t.Errorf("the first button is %q, want %q", got, want)
		}
		return serviceChoiceOpen
	})
	var opened []string
	replaceSeam(t, &openBrowser, func(url string) error {
		opened = append(opened, url)
		return nil
	})
	replaceSeam(t, &launchElevated, func(elevatedLaunch) (uint32, error) {
		t.Fatal("the gate started a service that was already starting")
		return 0, nil
	})
	messages := recordMessages(t)

	if serviceGate() {
		t.Fatal("the service gate let a launch with an installed service start its own server")
	}
	if len(opened) != 1 || opened[0] != "http://127.0.0.1:18080" {
		t.Errorf("the gate opened %v, want the web UI of the running service", opened)
	}
	if len(*messages) != 0 {
		t.Errorf("the gate showed %v, want no notice", *messages)
	}
}

// TestServiceGateStartsAStoppingServiceAfterItSettled proves that a launch that
// finds the service stopping waits for it to settle and only then offers the
// start of the stopped service.
//
// A stopping service is not a running one, so a decision taken while it is still
// on its way down would offer "Start service" for a registration the service
// control manager is still stopping.
func TestServiceGateStartsAStoppingServiceAfterItSettled(t *testing.T) {
	reads := 0
	replaceSeam(t, &serviceStatus, func(context.Context) (service.Status, error) {
		reads++
		if reads == 1 {
			return runningServiceStatus(service.StateStopping, "127.0.0.1:18080"), nil
		}
		return runningServiceStatus(service.StateStopped, "127.0.0.1:18080"), nil
	})
	replaceSeam(t, &showServiceChoice, func(prompt serviceChoicePrompt) serviceChoice {
		if prompt.State != service.StateStopped {
			t.Errorf("the dialog asked about the state %q, want the settled stopped service", prompt.State)
		}
		if got, want := serviceGateOpenLabel(prompt), "Start service"; got != want {
			t.Errorf("the first button is %q, want %q", got, want)
		}
		return serviceChoiceStart
	})
	replaceSeam(t, &openBrowser, func(string) error {
		t.Fatal("the gate opened a browser for a stopped service")
		return nil
	})
	var launches []elevatedLaunch
	replaceSeam(t, &launchElevated, func(l elevatedLaunch) (uint32, error) {
		launches = append(launches, l)
		return 4242, nil
	})
	messages := recordMessages(t)

	if serviceGate() {
		t.Fatal("the service gate let a launch with an installed service start its own server")
	}
	if len(launches) != 1 || launches[0].Action != serviceActionStart {
		t.Fatalf("the gate launched %v, want the start of the settled service", launches)
	}
	if len(*messages) != 0 {
		t.Errorf("the gate showed %v, want the helper to report the outcome", *messages)
	}
}

// TestServiceGateEndsAPendingLaunchWithoutAChoice proves that a service that did
// not settle within the budget ends the launch with a notice: no answer of the
// dialog is right for a service in the middle of a transition, so the gate asks
// no question and starts no server.
func TestServiceGateEndsAPendingLaunchWithoutAChoice(t *testing.T) {
	replaceSeam(t, &serviceStatusTimeout, 50*time.Millisecond)
	replaceSeam(t, &serviceStatus, func(context.Context) (service.Status, error) {
		return runningServiceStatus(service.StateStopping, "127.0.0.1:18080"), nil
	})
	replaceSeam(t, &showServiceChoice, func(serviceChoicePrompt) serviceChoice {
		t.Fatal("the service gate asked the user about a service that is still stopping")
		return serviceChoiceCancel
	})
	replaceSeam(t, &launchElevated, func(elevatedLaunch) (uint32, error) {
		t.Fatal("the gate launched a helper for a service that is still stopping")
		return 0, nil
	})
	replaceSeam(t, &openBrowser, func(string) error {
		t.Fatal("the gate opened a browser for a service that is still stopping")
		return nil
	})
	messages := recordMessages(t)

	if serviceGate() {
		t.Fatal("the service gate let a launch with an installed service start its own server")
	}
	if len(*messages) != 1 {
		t.Fatalf("the gate showed %v, want the notice of the pending service", *messages)
	}
	if caption := serviceGatePendingCaption; (*messages)[0].caption != caption {
		t.Errorf("the caption is %q, want %q", (*messages)[0].caption, caption)
	}
	if !strings.Contains((*messages)[0].text, "still stopping") {
		t.Errorf("the notice %q does not name the transition", (*messages)[0].text)
	}
	if !strings.Contains((*messages)[0].text, "changes nothing") {
		t.Errorf("the notice %q does not say that the launch changed nothing", (*messages)[0].text)
	}
}

// TestServiceGateUpdatesTheServiceWithTheElevatedHelper proves that the update
// of an installed service is handed to the elevated helper.
func TestServiceGateUpdatesTheServiceWithTheElevatedHelper(t *testing.T) {
	replaceSeam(t, &serviceStatus, func(context.Context) (service.Status, error) {
		return runningServiceStatus(service.StateStopped, "127.0.0.1:18080"), nil
	})
	replaceSeam(t, &showServiceChoice, func(serviceChoicePrompt) serviceChoice {
		return serviceChoiceUpdate
	})

	var launches []elevatedLaunch
	replaceSeam(t, &launchElevated, func(l elevatedLaunch) (uint32, error) {
		launches = append(launches, l)
		return 4343, nil
	})
	messages := recordMessages(t)

	if serviceGate() {
		t.Fatal("the service gate let a launch with an installed service start its own server")
	}
	if len(launches) != 1 || launches[0].Action != serviceActionUpdate {
		t.Fatalf("the gate launched %v, want the update of the service", launches)
	}
	if len(*messages) != 0 {
		t.Errorf("the gate showed %v, want the helper to report the outcome", *messages)
	}
}

// TestServiceGateCancelChangesNothing proves that the cancel answer ends the
// launch without touching the browser, the helper or the service.
func TestServiceGateCancelChangesNothing(t *testing.T) {
	replaceSeam(t, &serviceStatus, func(context.Context) (service.Status, error) {
		return runningServiceStatus(service.StateRunning, "127.0.0.1:18080"), nil
	})
	replaceSeam(t, &showServiceChoice, func(serviceChoicePrompt) serviceChoice {
		return serviceChoiceCancel
	})
	replaceSeam(t, &launchElevated, func(elevatedLaunch) (uint32, error) {
		t.Fatal("the gate launched a helper after the user cancelled the dialog")
		return 0, nil
	})
	replaceSeam(t, &openBrowser, func(string) error {
		t.Fatal("the gate opened a browser after the user cancelled the dialog")
		return nil
	})
	messages := recordMessages(t)

	if serviceGate() {
		t.Fatal("the service gate let a cancelled launch start its own server")
	}
	if len(*messages) != 0 {
		t.Errorf("the gate showed %v after the user cancelled the dialog", *messages)
	}
}

// TestServiceGateReportsARefusedHelperLaunch proves that a refused request for
// administrator rights is reported and takes no action.
func TestServiceGateReportsARefusedHelperLaunch(t *testing.T) {
	replaceSeam(t, &serviceStatus, func(context.Context) (service.Status, error) {
		return runningServiceStatus(service.StateStopped, "127.0.0.1:18080"), nil
	})
	replaceSeam(t, &showServiceChoice, func(serviceChoicePrompt) serviceChoice {
		return serviceChoiceStart
	})
	replaceSeam(t, &launchElevated, func(elevatedLaunch) (uint32, error) {
		return 0, errors.New(elevationDeclined)
	})
	messages := recordMessages(t)

	if serviceGate() {
		t.Fatal("the service gate let a launch with an installed service start its own server")
	}
	if len(*messages) != 1 {
		t.Fatalf("the gate showed %d message boxes, want the failure of the launch", len(*messages))
	}
	if caption := serviceFailureCaption(serviceActionStart); (*messages)[0].caption != caption {
		t.Errorf("the caption is %q, want %q", (*messages)[0].caption, caption)
	}
	if !strings.Contains((*messages)[0].text, elevationDeclined) {
		t.Errorf("the message %q does not carry the reason of the failure", (*messages)[0].text)
	}
}

// TestTrayServiceInstallLaunchesTheHelperAfterTheConfirmation proves that the
// tray action verifies the console program, confirms the installation and
// launches the elevated helper with the process id of the desktop host.
//
// The process id is what makes the installation safe: the helper waits for the
// desktop to exit, so the service never starts while the desktop still holds the
// listen address. The check of the console program runs before the question, so
// the user is never asked to confirm an installation that cannot run.
func TestTrayServiceInstallLaunchesTheHelperAfterTheConfirmation(t *testing.T) {
	stubNoServiceRegistration(t)
	freshServiceInstallGuard(t)
	confirmations := 0
	var confirmed string
	replaceSeam(t, &confirmServiceInstall, func(text string) bool {
		confirmations++
		confirmed = text
		return true
	})
	replaceSeam(t, &findSiblingExecutable, func(string, string) (string, error) {
		return `C:\portable\hfdesk.exe`, nil
	})

	var launches []elevatedLaunch
	replaceSeam(t, &launchElevated, func(l elevatedLaunch) (uint32, error) {
		launches = append(launches, l)
		return 5151, nil
	})
	messages := recordMessages(t)

	if !installServiceFromTray() {
		t.Fatal("the tray action did not hand the installation over to the helper")
	}
	for _, want := range []string{
		"starts at boot",
		"administrator rights",
		"its own data in ",
		"do not appear in the service",
	} {
		if !strings.Contains(confirmed, want) {
			t.Errorf("the confirmation %q does not mention %q", confirmed, want)
		}
	}
	if len(launches) != 1 {
		t.Fatalf("the tray action launched %d helpers, want one installation", len(launches))
	}
	if launches[0].Action != serviceActionInstall {
		t.Errorf("the helper performs %q, want %q", launches[0].Action, serviceActionInstall)
	}
	if launches[0].WaitPID != os.Getpid() {
		t.Errorf("the helper waits for the process %d, want this desktop host %d", launches[0].WaitPID, os.Getpid())
	}
	if len(*messages) != 0 {
		t.Errorf("the tray action showed %v, want the helper to report the outcome", *messages)
	}

	// The guard is kept after a hand-over: the host ends on this answer, so a
	// click that still reaches the tray may not start a second installation.
	if installServiceFromTray() {
		t.Error("the tray action handed the installation over a second time")
	}
	if confirmations != 1 {
		t.Errorf("the tray action asked the confirmation %d times, want one question", confirmations)
	}
	if len(launches) != 1 {
		t.Errorf("the tray action launched %d helpers, want one installation", len(launches))
	}
}

// TestTrayServiceInstallNamesTheDataDirectoryOfTheRegistration proves that the
// confirmation names the directory the service will really own.
//
// An installation merges with an existing registration, so the service keeps the
// state directory it already runs with. A confirmation that named the default of
// the scope instead would promise a directory the service does not use.
func TestTrayServiceInstallNamesTheDataDirectoryOfTheRegistration(t *testing.T) {
	freshServiceInstallGuard(t)
	replaceSeam(t, &serviceStatus, func(context.Context) (service.Status, error) {
		return service.Status{
			State: service.StateStopped,
			Paths: runtimepaths.Paths{StateDir: `D:\hfdesk`},
		}, nil
	})
	replaceSeam(t, &findSiblingExecutable, func(string, string) (string, error) {
		return `C:\portable\hfdesk.exe`, nil
	})
	var confirmed string
	replaceSeam(t, &confirmServiceInstall, func(text string) bool {
		confirmed = text
		return false
	})
	recordMessages(t)

	if installServiceFromTray() {
		t.Fatal("the tray action quit the host although the confirmation was declined")
	}
	if !strings.Contains(confirmed, `D:\hfdesk`) {
		t.Errorf("the confirmation %q does not name the data directory of the registration", confirmed)
	}
}

// TestTrayServiceInstallDoesNothingWhenTheConfirmationIsDeclined proves that the
// tray action changes nothing without the consent of the user.
//
// The console program is looked up before the question, so the action has already
// read that folder; what needs the consent is the launch of the helper, and that
// one never happens.
func TestTrayServiceInstallDoesNothingWhenTheConfirmationIsDeclined(t *testing.T) {
	stubNoServiceRegistration(t)
	freshServiceInstallGuard(t)
	replaceSeam(t, &confirmServiceInstall, func(string) bool { return false })
	replaceSeam(t, &findSiblingExecutable, func(string, string) (string, error) {
		return `C:\portable\hfdesk.exe`, nil
	})
	replaceSeam(t, &launchElevated, func(elevatedLaunch) (uint32, error) {
		t.Fatal("the tray action launched the helper without the consent of the user")
		return 0, nil
	})
	messages := recordMessages(t)

	if installServiceFromTray() {
		t.Fatal("the tray action quit the host without the consent of the user")
	}
	if len(*messages) != 0 {
		t.Errorf("the tray action showed %v after the confirmation was declined", *messages)
	}
}

// TestTrayServiceInstallReportsAMissingConsoleProgram proves that a missing
// console program is reported before the user is asked to confirm anything, and
// that it leaves the desktop host running.
//
// An installation whose program is not there cannot work, so the action reports
// the missing program instead of asking for a confirmation it would have to
// withdraw after the elevation prompt.
func TestTrayServiceInstallReportsAMissingConsoleProgram(t *testing.T) {
	stubNoServiceRegistration(t)
	freshServiceInstallGuard(t)
	replaceSeam(t, &confirmServiceInstall, func(string) bool {
		t.Error("the tray action asked for a confirmation although the console program is missing")
		return true
	})
	replaceSeam(t, &findSiblingExecutable, func(dir, _ string) (string, error) {
		return "", errors.New("there is no hfdesk.exe in " + dir)
	})
	replaceSeam(t, &launchElevated, func(elevatedLaunch) (uint32, error) {
		t.Fatal("the tray action launched the helper without the console program")
		return 0, nil
	})
	messages := recordMessages(t)

	if installServiceFromTray() {
		t.Fatal("the tray action quit the host without the console program")
	}
	if len(*messages) != 1 {
		t.Fatalf("the tray action showed %d message boxes, want one", len(*messages))
	}
	if caption := serviceSiblingMissingCaption; (*messages)[0].caption != caption {
		t.Errorf("the caption is %q, want %q", (*messages)[0].caption, caption)
	}
	if !strings.Contains((*messages)[0].text, "hfdesk.exe") {
		t.Errorf("the message %q does not name the missing program", (*messages)[0].text)
	}
}

// TestTrayServiceInstallReportsARefusedElevation proves that a refused request
// for administrator rights is reported and leaves the desktop host running.
func TestTrayServiceInstallReportsARefusedElevation(t *testing.T) {
	stubNoServiceRegistration(t)
	freshServiceInstallGuard(t)
	replaceSeam(t, &confirmServiceInstall, func(string) bool { return true })
	replaceSeam(t, &findSiblingExecutable, func(string, string) (string, error) {
		return `C:\portable\hfdesk.exe`, nil
	})
	replaceSeam(t, &launchElevated, func(elevatedLaunch) (uint32, error) {
		return 0, errors.New(elevationDeclined)
	})
	messages := recordMessages(t)

	if installServiceFromTray() {
		t.Fatal("the tray action quit the host after the elevation prompt was refused")
	}
	if len(*messages) != 1 {
		t.Fatalf("the tray action showed %d message boxes, want one", len(*messages))
	}
	for _, want := range []string{elevationDeclined, "The service was not installed."} {
		if !strings.Contains((*messages)[0].text, want) {
			t.Errorf("the message %q does not mention %q", (*messages)[0].text, want)
		}
	}
	if icon := (*messages)[0].icon; icon != windows.MB_ICONERROR {
		t.Errorf("the icon of the message is %d, want the error icon %d", icon, windows.MB_ICONERROR)
	}
}

// TestTrayServiceInstallIgnoresAClickWhileAnActionRuns proves that the tray
// action never runs twice at the same time.
//
// The tray keeps delivering clicks while the confirmation of the first click is
// open, so a second click must not ask a second time and must not raise a second
// prompt for administrator rights: two helpers would write the same registration
// and start the service next to each other. A click after a failed action is not
// a concurrent one: the item works again, and the next click asks the
// confirmation anew.
func TestTrayServiceInstallIgnoresAClickWhileAnActionRuns(t *testing.T) {
	stubNoServiceRegistration(t)
	freshServiceInstallGuard(t)
	replaceSeam(t, &findSiblingExecutable, func(string, string) (string, error) {
		return `C:\portable\hfdesk.exe`, nil
	})
	replaceSeam(t, &launchElevated, func(elevatedLaunch) (uint32, error) {
		t.Error("the tray action launched a helper although the confirmation was declined")
		return 0, nil
	})

	entered := make(chan struct{})
	release := make(chan struct{})
	var confirmations int32
	replaceSeam(t, &confirmServiceInstall, func(string) bool {
		if atomic.AddInt32(&confirmations, 1) == 1 {
			close(entered)
			<-release
		}
		return false
	})
	recordMessages(t)

	first := make(chan bool, 1)
	go func() { first <- installServiceFromTray() }()
	<-entered

	// The second click arrives while the first action waits for its
	// confirmation.
	if installServiceFromTray() {
		t.Error("the concurrent click on the tray item quit the host")
	}
	if got := atomic.LoadInt32(&confirmations); got != 1 {
		t.Errorf("the concurrent click asked the confirmation %d times, want the question of the running action only", got)
	}

	close(release)
	if <-first {
		t.Error("the declined confirmation quit the host")
	}

	// The action ended without a hand-over, so the guard is released and the
	// tray item asks again instead of being taken for a concurrent click.
	if installServiceFromTray() {
		t.Error("the retried tray action quit the host although the confirmation was declined")
	}
	if got := atomic.LoadInt32(&confirmations); got != 2 {
		t.Errorf("the tray action asked the confirmation %d times after a failed action, want the retry to ask again", got)
	}
}

// TestServiceHelperUpdateNeverStartsAStoppedService proves that an update of a
// stopped service leaves it stopped.
//
// The update re-registers the service from the folder of the helper and keeps
// the running state it had. It never starts a stopped service, so the helper
// starts nothing itself and the service reports the stopped state afterwards.
func TestServiceHelperUpdateNeverStartsAStoppedService(t *testing.T) {
	replaceSeam(t, &serviceVerifyBudget, 100*time.Millisecond)
	replaceSeam(t, &findSiblingExecutable, func(string, string) (string, error) {
		return `C:\portable\hfdesk.exe`, nil
	})

	manager := &fakeServiceManager{
		statuses: []service.Status{
			runningServiceStatus(service.StateStopped, "127.0.0.1:18080"),
			runningServiceStatus(service.StateStopped, "127.0.0.1:18080"),
		},
		installResult: service.InstallOptions{
			Scope:      service.ScopeSystem,
			Executable: `C:\portable\hfdesk.exe`,
			Listen:     "127.0.0.1:18080",
			Paths:      runtimepaths.Paths{StateDir: `C:\ProgramData\HFDesk`},
		},
	}
	replaceSeam(t, &newServiceManager, func(service.Scope) (service.Manager, error) {
		return manager, nil
	})
	messages := recordMessages(t)

	if err := RunServiceAction(ServiceActionOptions{Action: "update", Version: "1.3.0"}); err != nil {
		t.Fatalf("the update of a stopped service failed: %v", err)
	}

	if manager.starts != 0 {
		t.Fatalf("the update started the stopped service %d times, want it to stay stopped", manager.starts)
	}
	if len(manager.installs) != 1 {
		t.Fatalf("the update installed %d times, want one registration", len(manager.installs))
	}
	sent := manager.installs[0]
	if sent.Scope != service.ScopeSystem {
		t.Errorf("the scope of the update is %q, want %q", sent.Scope, service.ScopeSystem)
	}
	if sent.Executable != `C:\portable\hfdesk.exe` {
		t.Errorf("the update registers %q, want the console program next to the helper", sent.Executable)
	}
	if sent.Explicit != (service.InstallFieldSet{}) {
		t.Errorf("the update names fields of its own (%+v), want the defaults so the registration keeps its settings", sent.Explicit)
	}

	if len(*messages) != 1 {
		t.Fatalf("the helper showed %d message boxes, want the report of the update", len(*messages))
	}
	if caption := serviceUpdatedCaption; (*messages)[0].caption != caption {
		t.Errorf("the caption is %q, want %q", (*messages)[0].caption, caption)
	}
	if !strings.Contains((*messages)[0].text, "stays stopped") {
		t.Errorf("the report %q does not say that the service stays stopped", (*messages)[0].text)
	}
}

// TestServiceHelperUpdateKeepsARunningServiceRunning proves that an update of a
// running service reports the address it serves afterwards.
func TestServiceHelperUpdateKeepsARunningServiceRunning(t *testing.T) {
	replaceSeam(t, &serviceVerifyBudget, 100*time.Millisecond)
	replaceSeam(t, &findSiblingExecutable, func(string, string) (string, error) {
		return `C:\portable\hfdesk.exe`, nil
	})

	running := runningServiceStatus(service.StateRunning, "127.0.0.1:18080")
	manager := &fakeServiceManager{
		statuses:      []service.Status{running, running},
		installResult: service.InstallOptions{Executable: `C:\portable\hfdesk.exe`, Listen: "127.0.0.1:18080", Paths: running.Paths},
	}
	replaceSeam(t, &newServiceManager, func(service.Scope) (service.Manager, error) {
		return manager, nil
	})
	messages := recordMessages(t)

	if err := RunServiceAction(ServiceActionOptions{Action: "update", Version: "1.3.0"}); err != nil {
		t.Fatalf("the update of a running service failed: %v", err)
	}
	if manager.starts != 0 {
		t.Errorf("the update started the running service itself, want the service layer to restart it")
	}
	if len(*messages) != 1 {
		t.Fatalf("the helper showed %d message boxes, want the report of the update", len(*messages))
	}
	for _, want := range []string{"http://127.0.0.1:18080", "not needed to use the service"} {
		if !strings.Contains((*messages)[0].text, want) {
			t.Errorf("the report %q does not mention %q", (*messages)[0].text, want)
		}
	}
}

// TestServiceHelperUpdateFailsWhenAStoppedServiceRuns proves that the update
// verifies the state it promised: the update of a stopped service may never
// leave a running one behind.
func TestServiceHelperUpdateFailsWhenAStoppedServiceRuns(t *testing.T) {
	replaceSeam(t, &serviceVerifyBudget, 50*time.Millisecond)
	replaceSeam(t, &findSiblingExecutable, func(string, string) (string, error) {
		return `C:\portable\hfdesk.exe`, nil
	})

	manager := &fakeServiceManager{
		statuses: []service.Status{
			runningServiceStatus(service.StateStopped, "127.0.0.1:18080"),
			runningServiceStatus(service.StateRunning, "127.0.0.1:18080"),
		},
	}
	replaceSeam(t, &newServiceManager, func(service.Scope) (service.Manager, error) {
		return manager, nil
	})
	messages := recordMessages(t)

	if err := RunServiceAction(ServiceActionOptions{Action: "update"}); err == nil {
		t.Fatal("the update of a stopped service succeeded although the service runs")
	}
	if manager.starts != 0 {
		t.Errorf("the update started the stopped service %d times", manager.starts)
	}
	if len(*messages) != 1 {
		t.Fatalf("the helper showed %d message boxes, want the failure of the update", len(*messages))
	}
	if caption := serviceFailureCaption(serviceActionUpdate); (*messages)[0].caption != caption {
		t.Errorf("the caption is %q, want %q", (*messages)[0].caption, caption)
	}
	if !strings.Contains((*messages)[0].text, "must leave the service stopped") {
		t.Errorf("the report %q does not name the state the update promised", (*messages)[0].text)
	}
}

// TestServiceHelperInstallStartsTheServiceAndReportsTheAddress proves that an
// installation registers the console program next to the helper, starts the
// service and reports the address it serves.
func TestServiceHelperInstallStartsTheServiceAndReportsTheAddress(t *testing.T) {
	replaceSeam(t, &serviceVerifyBudget, 100*time.Millisecond)
	replaceSeam(t, &findSiblingExecutable, func(string, string) (string, error) {
		return `C:\portable\hfdesk.exe`, nil
	})

	running := runningServiceStatus(service.StateRunning, "0.0.0.0:18080")
	manager := &fakeServiceManager{
		statuses:      []service.Status{running},
		installResult: service.InstallOptions{Executable: `C:\portable\hfdesk.exe`, Listen: running.Listen, Paths: running.Paths},
	}
	replaceSeam(t, &newServiceManager, func(service.Scope) (service.Manager, error) {
		return manager, nil
	})
	messages := recordMessages(t)

	if err := RunServiceAction(ServiceActionOptions{Action: "install", Version: "1.3.0"}); err != nil {
		t.Fatalf("the installation failed: %v", err)
	}
	if manager.starts != 1 {
		t.Fatalf("the installation started the service %d times, want one start", manager.starts)
	}
	if len(*messages) != 1 {
		t.Fatalf("the helper showed %d message boxes, want the report of the installation", len(*messages))
	}
	if caption := serviceInstalledCaption; (*messages)[0].caption != caption {
		t.Errorf("the caption is %q, want %q", (*messages)[0].caption, caption)
	}
	for _, want := range []string{`C:\portable\hfdesk.exe`, "http://127.0.0.1:18080", "not needed to use the service"} {
		if !strings.Contains((*messages)[0].text, want) {
			t.Errorf("the report %q does not mention %q", (*messages)[0].text, want)
		}
	}
}

// TestServiceHelperInstallRemovesTheRegistrationOfAFailedFirstInstallation
// proves that a failed first installation does not leave a registration behind.
//
// The service gate of the desktop app ends every launch that finds a
// registration, so a registration that serves nothing would keep the user from
// the desktop app. The state before the installation reported no service, so the
// registration is the result of this action and is removed again.
func TestServiceHelperInstallRemovesTheRegistrationOfAFailedFirstInstallation(t *testing.T) {
	const console = `C:\portable\hfdesk.exe`
	replaceSeam(t, &findSiblingExecutable, func(string, string) (string, error) {
		return console, nil
	})
	manager := &fakeServiceManager{
		statuses: []service.Status{{State: service.StateNotInstalled}},
		startErr: errors.New("the service did not start"),
	}
	replaceSeam(t, &newServiceManager, func(service.Scope) (service.Manager, error) {
		return manager, nil
	})
	messages := recordMessages(t)

	if err := RunServiceAction(ServiceActionOptions{Action: "install", Version: "1.4.0"}); err == nil {
		t.Fatal("the installation succeeded although the service did not start")
	}
	install := stepIndex(manager.steps, "install")
	uninstall := stepIndex(manager.steps, "uninstall")
	if install < 0 || uninstall < install {
		t.Fatalf("the steps of the helper are %v, want the registration removed after the failed start",
			manager.steps)
	}
	if len(*messages) != 1 {
		t.Fatalf("the helper showed %d message boxes, want the failure of the installation", len(*messages))
	}
	if caption := serviceFailureCaption(serviceActionInstall); (*messages)[0].caption != caption {
		t.Errorf("the caption is %q, want %q", (*messages)[0].caption, caption)
	}
	for _, want := range []string{
		"the service did not start",
		"not installed as a Windows service",
		"start the desktop app normally again",
	} {
		if !strings.Contains((*messages)[0].text, want) {
			t.Errorf("the report %q does not mention %q", (*messages)[0].text, want)
		}
	}
	if strings.Contains((*messages)[0].text, "hfdesk service uninstall") {
		t.Errorf("the report %q names a removal although the registration was removed", (*messages)[0].text)
	}
	if strings.Contains((*messages)[0].text, "%ProgramFiles%") {
		t.Errorf("the report %q names a program folder although the registration was removed", (*messages)[0].text)
	}
}

// TestServiceHelperInstallKeepsAnExistingRegistrationWhenTheStartFails proves
// that an installation never removes a registration it did not write.
//
// The service existed before the action, so its settings and its data belong to
// an instance this installation did not create. The report names what remains and
// the elevated command that removes it.
func TestServiceHelperInstallKeepsAnExistingRegistrationWhenTheStartFails(t *testing.T) {
	replaceSeam(t, &findSiblingExecutable, func(string, string) (string, error) {
		return `C:\portable\hfdesk.exe`, nil
	})
	manager := &fakeServiceManager{
		statuses: []service.Status{runningServiceStatus(service.StateStopped, "127.0.0.1:18080")},
		startErr: errors.New("the service did not start"),
	}
	replaceSeam(t, &newServiceManager, func(service.Scope) (service.Manager, error) {
		return manager, nil
	})
	messages := recordMessages(t)

	if err := RunServiceAction(ServiceActionOptions{Action: "install"}); err == nil {
		t.Fatal("the installation succeeded although the service did not start")
	}
	if step := stepIndex(manager.steps, "uninstall"); step >= 0 {
		t.Fatalf("the steps of the helper are %v, want the existing registration left in place", manager.steps)
	}
	if len(*messages) != 1 {
		t.Fatalf("the helper showed %d message boxes, want the failure of the installation", len(*messages))
	}
	for _, want := range []string{
		"left in place",
		"stays installed, but it does not run",
		"elevated shell",
		`"hfdesk service uninstall"`,
		`%ProgramFiles%`,
	} {
		if !strings.Contains((*messages)[0].text, want) {
			t.Errorf("the report %q does not mention %q", (*messages)[0].text, want)
		}
	}
}

// TestServiceHelperInstallKeepsTheRegistrationWhenItsStateBeforeIsUnreadable
// proves that an unreadable state before the installation is not evidence of a
// machine without a service: nothing is removed.
func TestServiceHelperInstallKeepsTheRegistrationWhenItsStateBeforeIsUnreadable(t *testing.T) {
	replaceSeam(t, &findSiblingExecutable, func(string, string) (string, error) {
		return `C:\portable\hfdesk.exe`, nil
	})
	manager := &fakeServiceManager{
		statusErr: errors.New("the registration cannot be read"),
		startErr:  errors.New("the service did not start"),
	}
	replaceSeam(t, &newServiceManager, func(service.Scope) (service.Manager, error) {
		return manager, nil
	})
	messages := recordMessages(t)

	if err := RunServiceAction(ServiceActionOptions{Action: "install"}); err == nil {
		t.Fatal("the installation succeeded although the service did not start")
	}
	if step := stepIndex(manager.steps, "uninstall"); step >= 0 {
		t.Fatalf("the steps of the helper are %v, want the unreadable registration left in place", manager.steps)
	}
	if len(*messages) != 1 {
		t.Fatalf("the helper showed %d message boxes, want the failure of the installation", len(*messages))
	}
	for _, want := range []string{
		"could not be read",
		`"hfdesk service uninstall"`,
	} {
		if !strings.Contains((*messages)[0].text, want) {
			t.Errorf("the report %q does not mention %q", (*messages)[0].text, want)
		}
	}
}

// TestServiceHelperInstallReportsARegistrationItCouldNotRemove proves that a
// removal that fails is reported with its reason and with the command that
// removes the registration.
func TestServiceHelperInstallReportsARegistrationItCouldNotRemove(t *testing.T) {
	replaceSeam(t, &findSiblingExecutable, func(string, string) (string, error) {
		return `C:\portable\hfdesk.exe`, nil
	})
	manager := &fakeServiceManager{
		statuses:     []service.Status{{State: service.StateNotInstalled}},
		startErr:     errors.New("the service did not start"),
		uninstallErr: errors.New("the registration is marked for deletion"),
	}
	replaceSeam(t, &newServiceManager, func(service.Scope) (service.Manager, error) {
		return manager, nil
	})
	messages := recordMessages(t)

	if err := RunServiceAction(ServiceActionOptions{Action: "install"}); err == nil {
		t.Fatal("the installation succeeded although the service did not start")
	}
	if step := stepIndex(manager.steps, "uninstall"); step < 0 {
		t.Fatalf("the steps of the helper are %v, want the removal of the failed registration", manager.steps)
	}
	if len(*messages) != 1 {
		t.Fatalf("the helper showed %d message boxes, want the failure of the installation", len(*messages))
	}
	for _, want := range []string{
		"could not be removed again",
		"stays installed as a Windows service although it does not run",
		"the registration is marked for deletion",
		`"hfdesk service uninstall"`,
	} {
		if !strings.Contains((*messages)[0].text, want) {
			t.Errorf("the report %q does not mention %q", (*messages)[0].text, want)
		}
	}
}

// TestServiceHelperInstallRemovesARegistrationThatNeverRan proves that the
// removal covers the verification of the start as well: a registration whose
// service does not reach the running state serves nothing either.
func TestServiceHelperInstallRemovesARegistrationThatNeverRan(t *testing.T) {
	replaceSeam(t, &serviceVerifyBudget, 100*time.Millisecond)
	replaceSeam(t, &findSiblingExecutable, func(string, string) (string, error) {
		return `C:\portable\hfdesk.exe`, nil
	})
	manager := &fakeServiceManager{statuses: []service.Status{
		{State: service.StateNotInstalled},
		runningServiceStatus(service.StateStopped, "127.0.0.1:18080"),
	}}
	replaceSeam(t, &newServiceManager, func(service.Scope) (service.Manager, error) {
		return manager, nil
	})
	messages := recordMessages(t)

	if err := RunServiceAction(ServiceActionOptions{Action: "install"}); err == nil {
		t.Fatal("the installation succeeded although the service never reached the running state")
	}
	install := stepIndex(manager.steps, "install")
	uninstall := stepIndex(manager.steps, "uninstall")
	if install < 0 || uninstall < install {
		t.Fatalf("the steps of the helper are %v, want the registration removed after the failed verification",
			manager.steps)
	}
	if len(*messages) != 1 {
		t.Fatalf("the helper showed %d message boxes, want the failure of the installation", len(*messages))
	}
	for _, want := range []string{"not installed as a Windows service", "start the desktop app normally again"} {
		if !strings.Contains((*messages)[0].text, want) {
			t.Errorf("the report %q does not mention %q", (*messages)[0].text, want)
		}
	}
}

// TestServiceHelperInstallLeavesARegistrationWhoseServiceStillRuns proves that a
// failed first installation never removes a registration whose instance may
// still serve.
//
// The start that failed may have left an instance that runs or is still coming
// up. The state is read again before the destructive step, and only a settled
// state that does not serve is removed: a running, starting or stopping instance
// is never torn down, and the report says that the registration stays.
func TestServiceHelperInstallLeavesARegistrationWhoseServiceStillRuns(t *testing.T) {
	const console = `C:\portable\hfdesk.exe`
	for _, state := range []service.State{service.StateRunning, service.StateStarting, service.StateStopping} {
		t.Run(string(state), func(t *testing.T) {
			replaceSeam(t, &findSiblingExecutable, func(string, string) (string, error) {
				return console, nil
			})
			manager := &fakeServiceManager{
				statuses: []service.Status{
					{State: service.StateNotInstalled},
					runningServiceStatus(state, "127.0.0.1:18080"),
				},
				startErr: errors.New("the service did not start"),
			}
			replaceSeam(t, &newServiceManager, func(service.Scope) (service.Manager, error) {
				return manager, nil
			})
			messages := recordMessages(t)

			if err := RunServiceAction(ServiceActionOptions{Action: "install"}); err == nil {
				t.Fatal("the installation succeeded although the service did not start")
			}
			if step := stepIndex(manager.steps, "uninstall"); step >= 0 {
				t.Fatalf("the steps of the helper are %v, want the %s registration left in place", manager.steps, state)
			}
			if len(*messages) != 1 {
				t.Fatalf("the helper showed %d message boxes, want the failure of the installation", len(*messages))
			}
			for _, want := range []string{
				"not removed",
				"may still be running, starting or stopping",
				"stays in place",
				`"hfdesk service uninstall"`,
			} {
				if !strings.Contains((*messages)[0].text, want) {
					t.Errorf("the report %q does not mention %q", (*messages)[0].text, want)
				}
			}
		})
	}
}

// TestServiceHelperInstallLeavesARegistrationWhoseStateCannotBeRead proves that
// an unreadable state before the removal is not evidence of a stopped service.
//
// The registration of the failed attempt stays, and the report says so: removing
// it on a guess could end an instance that is still running or starting.
func TestServiceHelperInstallLeavesARegistrationWhoseStateCannotBeRead(t *testing.T) {
	const console = `C:\portable\hfdesk.exe`
	replaceSeam(t, &findSiblingExecutable, func(string, string) (string, error) {
		return console, nil
	})
	manager := &fakeServiceManager{
		statuses:       []service.Status{{State: service.StateNotInstalled}},
		statusFailures: []error{nil, errors.New("the service control manager did not answer")},
		startErr:       errors.New("the service did not start"),
	}
	replaceSeam(t, &newServiceManager, func(service.Scope) (service.Manager, error) {
		return manager, nil
	})
	messages := recordMessages(t)

	if err := RunServiceAction(ServiceActionOptions{Action: "install"}); err == nil {
		t.Fatal("the installation succeeded although the service did not start")
	}
	if step := stepIndex(manager.steps, "uninstall"); step >= 0 {
		t.Fatalf("the steps of the helper are %v, want the unreadable registration left in place", manager.steps)
	}
	if len(*messages) != 1 {
		t.Fatalf("the helper showed %d message boxes, want the failure of the installation", len(*messages))
	}
	for _, want := range []string{
		"not removed",
		"state could not be read",
		"stays in place",
		`"hfdesk service uninstall"`,
	} {
		if !strings.Contains((*messages)[0].text, want) {
			t.Errorf("the report %q does not mention %q", (*messages)[0].text, want)
		}
	}
}

// TestServiceHelperWaitsForTheDesktopBeforeItInstalls proves that the helper
// waits for the desktop host before it changes the service.
//
// The desktop stops its own server through the graceful path and ends; the
// helper starts the service only afterwards, so the service never binds an
// address the desktop still holds.
func TestServiceHelperWaitsForTheDesktopBeforeItInstalls(t *testing.T) {
	replaceSeam(t, &serviceVerifyBudget, 100*time.Millisecond)
	replaceSeam(t, &findSiblingExecutable, func(string, string) (string, error) {
		return `C:\portable\hfdesk.exe`, nil
	})
	manager := &fakeServiceManager{
		statuses: []service.Status{runningServiceStatus(service.StateRunning, "127.0.0.1:18080")},
	}
	replaceSeam(t, &newServiceManager, func(service.Scope) (service.Manager, error) {
		return manager, nil
	})
	waited := false
	replaceSeam(t, &waitForProcess, func(pid int, budget time.Duration) error {
		if pid != 4321 {
			t.Errorf("the helper waited for the process %d, want 4321", pid)
		}
		if budget != serviceWaitBudget {
			t.Errorf("the helper waited for %s, want the bounded budget %s", budget, serviceWaitBudget)
		}
		waited = true
		manager.steps = append(manager.steps, "wait")
		return nil
	})
	recordMessages(t)

	if err := RunServiceAction(ServiceActionOptions{Action: "install", WaitPID: 4321}); err != nil {
		t.Fatalf("the installation failed: %v", err)
	}
	if !waited {
		t.Fatal("the helper did not wait for the desktop host")
	}
	install := stepIndex(manager.steps, "install")
	if wait := stepIndex(manager.steps, "wait"); install < 0 || wait < 0 || wait > install {
		t.Fatalf("the steps of the helper are %v, want the wait before the installation", manager.steps)
	}
}

// TestServiceHelperReportsAWaitThatTimesOut proves that a desktop host that does
// not end is a failure of the action and not a reason to start the service
// anyway.
func TestServiceHelperReportsAWaitThatTimesOut(t *testing.T) {
	replaceSeam(t, &waitForProcess, func(pid int, budget time.Duration) error {
		return errors.New("the HFDesk desktop app (process 4321) did not exit within 1m0s")
	})
	replaceSeam(t, &newServiceManager, func(service.Scope) (service.Manager, error) {
		t.Fatal("the helper touched the service although the desktop host did not exit")
		return nil, nil
	})
	messages := recordMessages(t)

	if err := RunServiceAction(ServiceActionOptions{Action: "install", WaitPID: 4321}); err == nil {
		t.Fatal("the installation succeeded although the desktop host did not exit")
	}
	if len(*messages) != 1 {
		t.Fatalf("the helper showed %d message boxes, want the failure of the wait", len(*messages))
	}
	if !strings.Contains((*messages)[0].text, "did not exit") {
		t.Errorf("the report %q does not carry the reason of the failure", (*messages)[0].text)
	}
	if !strings.Contains((*messages)[0].text, "A registration may remain.") {
		t.Errorf("the report %q does not state the state the attempt may have left", (*messages)[0].text)
	}
}

// TestServiceHelperStartReportsAMissingRegistration proves that the start of a
// service on a machine without a registration is reported with the way to
// install it.
func TestServiceHelperStartReportsAMissingRegistration(t *testing.T) {
	manager := &fakeServiceManager{statuses: []service.Status{{State: service.StateNotInstalled}}}
	replaceSeam(t, &newServiceManager, func(service.Scope) (service.Manager, error) {
		return manager, nil
	})
	messages := recordMessages(t)

	if err := RunServiceAction(ServiceActionOptions{Action: "start"}); err == nil {
		t.Fatal("the start of a service that is not installed succeeded")
	}
	if manager.starts != 0 {
		t.Errorf("the helper started a service that is not installed %d times", manager.starts)
	}
	if len(*messages) != 1 {
		t.Fatalf("the helper showed %d message boxes, want the failure of the start", len(*messages))
	}
	if caption := serviceFailureCaption(serviceActionStart); (*messages)[0].caption != caption {
		t.Errorf("the caption is %q, want %q", (*messages)[0].caption, caption)
	}
	if !strings.Contains((*messages)[0].text, "not installed as a Windows service") {
		t.Errorf("the report %q does not say that the service is not installed", (*messages)[0].text)
	}
}

// TestServiceHelperStartReportsTheRefusedStartAndTheStatusCommand proves that a
// start the service control manager refused is reported as the result, the reason
// and the one command that reads the state of the service.
//
// The helper installs no logging pipeline, so the message box is the only surface
// the reason has. The folder and program guidance belongs to the reports of an
// installation: a start describes a registration that already ran, so the report
// stays with the state it left.
func TestServiceHelperStartReportsTheRefusedStartAndTheStatusCommand(t *testing.T) {
	status := runningServiceStatus(service.StateStopped, "127.0.0.1:18080")
	status.Executable = `C:\Users\ada\HFDesk\hfdesk.exe`
	manager := &fakeServiceManager{
		statuses: []service.Status{status},
		startErr: errors.New("the service control manager refused the start"),
	}
	replaceSeam(t, &newServiceManager, func(service.Scope) (service.Manager, error) {
		return manager, nil
	})
	messages := recordMessages(t)

	if err := RunServiceAction(ServiceActionOptions{Action: "start"}); err == nil {
		t.Fatal("the start succeeded although the service control manager refused it")
	}
	if len(*messages) != 1 {
		t.Fatalf("the helper showed %d message boxes, want the failure of the start", len(*messages))
	}
	for _, want := range []string{"the service control manager refused the start", "The start was not confirmed.", "hfdesk service status"} {
		if !strings.Contains((*messages)[0].text, want) {
			t.Errorf("the report %q does not mention %q", (*messages)[0].text, want)
		}
	}
	for _, unwanted := range []string{status.Executable, `%ProgramFiles%`} {
		if strings.Contains((*messages)[0].text, unwanted) {
			t.Errorf("the report %q names %q, want no folder guidance on a registration that already ran", (*messages)[0].text, unwanted)
		}
	}
}

// TestServiceHelperInstallNamesTheEffectiveProgram proves that a failed
// installation names the program the registration was asked to run, so the user
// can check the folder of that program.
func TestServiceHelperInstallNamesTheEffectiveProgram(t *testing.T) {
	const console = `C:\portable\hfdesk.exe`
	replaceSeam(t, &findSiblingExecutable, func(string, string) (string, error) {
		return console, nil
	})
	manager := &fakeServiceManager{
		statuses:   []service.Status{runningServiceStatus(service.StateStopped, "127.0.0.1:18080")},
		installErr: errors.New("the service control manager refused the registration"),
	}
	replaceSeam(t, &newServiceManager, func(service.Scope) (service.Manager, error) {
		return manager, nil
	})
	messages := recordMessages(t)

	if err := RunServiceAction(ServiceActionOptions{Action: "install"}); err == nil {
		t.Fatal("the installation succeeded although the service control manager refused it")
	}
	if len(*messages) != 1 {
		t.Fatalf("the helper showed %d message boxes, want the failure of the installation", len(*messages))
	}
	for _, want := range []string{console, `%ProgramFiles%`} {
		if !strings.Contains((*messages)[0].text, want) {
			t.Errorf("the report %q does not mention %q", (*messages)[0].text, want)
		}
	}
}

// TestServiceHelperUpdateRefusesAMachineWithoutARegistration proves that an
// update never creates a registration: an installation is the action that
// starts what it registers, and an update of a stopped service would leave the
// user with an instance that serves nothing.
func TestServiceHelperUpdateRefusesAMachineWithoutARegistration(t *testing.T) {
	replaceSeam(t, &findSiblingExecutable, func(string, string) (string, error) {
		return `C:\portable\hfdesk.exe`, nil
	})
	manager := &fakeServiceManager{statuses: []service.Status{{State: service.StateNotInstalled}}}
	replaceSeam(t, &newServiceManager, func(service.Scope) (service.Manager, error) {
		return manager, nil
	})
	messages := recordMessages(t)

	if err := RunServiceAction(ServiceActionOptions{Action: "update"}); err == nil {
		t.Fatal("the update of a service that is not installed succeeded")
	}
	if len(manager.installs) != 0 {
		t.Errorf("the update wrote %d registrations on a machine without one", len(manager.installs))
	}
	if !strings.Contains((*messages)[0].text, "tray of the desktop app") {
		t.Errorf("the report %q does not name the way to install the service", (*messages)[0].text)
	}
}

// TestServiceHelperRejectsAnUnknownAction proves that the helper never guesses
// the action it was asked to perform.
func TestServiceHelperRejectsAnUnknownAction(t *testing.T) {
	replaceSeam(t, &newServiceManager, func(service.Scope) (service.Manager, error) {
		t.Fatal("the helper touched the service for an action it does not know")
		return nil, nil
	})
	messages := recordMessages(t)

	if err := RunServiceAction(ServiceActionOptions{Action: "uninstall"}); err == nil {
		t.Fatal("the helper accepted an action it does not know")
	}
	if len(*messages) != 1 || !strings.Contains((*messages)[0].text, "does not know") {
		t.Fatalf("the helper showed %v, want the report of the unknown action", *messages)
	}
}

// TestServiceHelperArgumentsNameTheActionAndTheProcess proves that the command
// line of the elevated helper carries the action and the process it waits for,
// and that a launch without a process carries no wait flag.
func TestServiceHelperArgumentsNameTheActionAndTheProcess(t *testing.T) {
	install := serviceHelperArguments(elevatedLaunch{Action: serviceActionInstall, WaitPID: 4321})
	if want := "--service-action install --wait-pid 4321"; install != want {
		t.Errorf("the arguments are %q, want %q", install, want)
	}
	update := serviceHelperArguments(elevatedLaunch{Action: serviceActionUpdate})
	if want := "--service-action update"; update != want {
		t.Errorf("the arguments are %q, want %q", update, want)
	}
}

// TestWaitForProcessExitReportsATimeout proves that the wait for the desktop host
// is bounded: the running test process is observed and the wait reports that it
// did not exit within the budget.
func TestWaitForProcessExitReportsATimeout(t *testing.T) {
	err := waitForProcessExit(os.Getpid(), 50*time.Millisecond)
	if err == nil {
		t.Fatal("the wait for the running process succeeded, want a timeout")
	}
	if !strings.Contains(err.Error(), "did not exit within") {
		t.Errorf("the error is %q, want the timeout of the wait", err)
	}
	if err := waitForProcessExit(0, time.Second); err != nil {
		t.Errorf("the wait for no process failed: %v", err)
	}
}

// TestServiceHelperInstallReportsAMissingConsoleProgram proves that the helper
// refuses an installation without the console program of the service, before it
// touches the registration.
//
// The desktop host downloads and copies nothing, so the program has to be in the
// folder of the helper already. The helper reports the folder and leaves the
// machine as it was.
func TestServiceHelperInstallReportsAMissingConsoleProgram(t *testing.T) {
	replaceSeam(t, &findSiblingExecutable, func(dir, _ string) (string, error) {
		return "", errors.New("there is no hfdesk.exe in " + dir)
	})
	replaceSeam(t, &newServiceManager, func(service.Scope) (service.Manager, error) {
		t.Fatal("the helper touched the service without the console program")
		return nil, nil
	})
	messages := recordMessages(t)

	if err := RunServiceAction(ServiceActionOptions{Action: "install"}); err == nil {
		t.Fatal("the installation succeeded without the console program")
	}
	if len(*messages) != 1 {
		t.Fatalf("the helper showed %d message boxes, want the failure of the installation", len(*messages))
	}
	if caption := serviceFailureCaption(serviceActionInstall); (*messages)[0].caption != caption {
		t.Errorf("the caption is %q, want %q", (*messages)[0].caption, caption)
	}
	if !strings.Contains((*messages)[0].text, "hfdesk.exe") {
		t.Errorf("the report %q does not name the missing program", (*messages)[0].text)
	}
}

// TestServiceHelperStartReportsTheAddress proves that a started service is
// reported with the address it serves and with the program it runs.
func TestServiceHelperStartReportsTheAddress(t *testing.T) {
	replaceSeam(t, &serviceVerifyBudget, 100*time.Millisecond)
	running := runningServiceStatus(service.StateRunning, "127.0.0.1:18080")
	running.Executable = `C:\portable\hfdesk.exe`
	manager := &fakeServiceManager{statuses: []service.Status{
		runningServiceStatus(service.StateStopped, running.Listen),
		running,
	}}
	replaceSeam(t, &newServiceManager, func(service.Scope) (service.Manager, error) {
		return manager, nil
	})
	messages := recordMessages(t)

	if err := RunServiceAction(ServiceActionOptions{Action: "start"}); err != nil {
		t.Fatalf("the start of the service failed: %v", err)
	}
	if manager.starts != 1 {
		t.Fatalf("the helper started the service %d times, want one start", manager.starts)
	}
	if len(*messages) != 1 {
		t.Fatalf("the helper showed %d message boxes, want the report of the start", len(*messages))
	}
	if caption := serviceStartedCaption; (*messages)[0].caption != caption {
		t.Errorf("the caption is %q, want %q", (*messages)[0].caption, caption)
	}
	for _, want := range []string{"http://127.0.0.1:18080", `C:\portable\hfdesk.exe`, "not needed to use the service"} {
		if !strings.Contains((*messages)[0].text, want) {
			t.Errorf("the report %q does not mention %q", (*messages)[0].text, want)
		}
	}
}
