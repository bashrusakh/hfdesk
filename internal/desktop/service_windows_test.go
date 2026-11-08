// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package desktop

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
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

// testGateSingleton is the desktop singleton identity the gate tests pass:
// the update choice hands it to the helper, and the tests that never launch a
// helper only carry it through.
const testGateSingleton = `Local\HFDesk.Test.Gate.1`

// requireFreeSingleton returns the name of the per-session singleton and skips
// the test when another owner holds it: both sides of a hold check are only
// provable from a clean start, or a leftover would fail the release check for
// the wrong reason and could make the hold check pass without the action.
func requireFreeSingleton(t *testing.T) string {
	t.Helper()
	name, err := instanceMutexName()
	if err != nil {
		t.Fatalf("read the per-session singleton name: %v", err)
	}
	probe, err := acquireInstanceLock()
	if err != nil {
		t.Fatalf("probe the per-session singleton: %v", err)
	}
	if probe.Exists() {
		probe.Release()
		t.Skip("another desktop test or app owns the per-session singleton")
	}
	probe.Release()
	return name
}

// recordMessagesWithSingletonProbe replaces the message box with a recorder
// that also notes, per message, whether the singleton still existed when the
// box was shown: the hold has to end before the final notice.
func recordMessagesWithSingletonProbe(t *testing.T) (*[]shownMessage, *[]bool) {
	t.Helper()
	messages := &[]shownMessage{}
	existedAtNotice := &[]bool{}
	replaceSeam(t, &showMessage, func(caption, text string, icon uint32) {
		lock, err := acquireInstanceLock()
		*existedAtNotice = append(*existedAtNotice, err == nil && lock.Exists())
		if err == nil {
			lock.Release()
		}
		*messages = append(*messages, shownMessage{caption: caption, text: text, icon: icon})
	})
	return messages, existedAtNotice
}

// requireSingletonReleased fails the test when the singleton survived the end
// of the action: a later launch would be refused as already running.
func requireSingletonReleased(t *testing.T) {
	t.Helper()
	after, err := acquireInstanceLock()
	if err != nil {
		t.Fatalf("probe the per-session singleton after the action: %v", err)
	}
	if after.Exists() {
		after.Release()
		t.Fatal("the singleton survived the end of the action: a later launch would be refused as already running")
	}
	after.Release()
}

// testPEMachineIA64 is a PE machine type no x86-64 Windows executes.
const testPEMachineIA64 = 0x0200

// testPEImage writes a structurally complete minimal PE image with the given
// COFF machine into the temp folder of the test and returns its path: DOS
// header, PE signature + COFF header, a PE32+ optional header without data
// directories and one section header without raw data — the minimum the
// image reader of the service layer accepts before it reports the machine.
func testPEImage(t *testing.T, machine uint16) string {
	t.Helper()
	const (
		peOffset     = 64
		optionalSize = 112
	)
	image := make([]byte, peOffset+24+optionalSize+40)
	image[0], image[1] = 'M', 'Z'
	binary.LittleEndian.PutUint32(image[0x3C:], peOffset)
	copy(image[peOffset:], []byte{'P', 'E', 0, 0})
	binary.LittleEndian.PutUint16(image[peOffset+4:], machine)
	binary.LittleEndian.PutUint16(image[peOffset+6:], 1)
	binary.LittleEndian.PutUint16(image[peOffset+20:], optionalSize)
	binary.LittleEndian.PutUint16(image[peOffset+22:], 0x0002) // executable image
	optional := image[peOffset+24:]
	binary.LittleEndian.PutUint16(optional, 0x20B) // PE32+
	binary.LittleEndian.PutUint32(optional[16:], 1)
	path := filepath.Join(t.TempDir(), "hfdesk.exe")
	if err := os.WriteFile(path, image, 0o644); err != nil {
		t.Fatalf("write the PE fixture: %v", err)
	}
	return path
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
	// installStartResult is the outcome of the service-owned first-install
	// start path.
	installStartResult service.InstallStartResult
	// installStartErr is the failure of the service-owned first-install path.
	installStartErr error
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

func (m *fakeServiceManager) InstallAndStart(_ context.Context, opts service.InstallOptions) (service.InstallStartResult, error) {
	m.steps = append(m.steps, "install-start")
	m.installs = append(m.installs, opts)
	if m.installStartErr != nil {
		return service.InstallStartResult{}, m.installStartErr
	}
	installed := opts
	if m.installStartResult.Installation.Executable != "" {
		installed = m.installStartResult.Installation
	} else if m.installResult.Executable != "" {
		installed = m.installResult
	}
	status := m.installStartResult.Status
	if status.State == "" {
		status = service.Status{
			State:      service.StateRunning,
			Running:    true,
			Scope:      installed.Scope,
			Name:       service.ServiceName,
			Executable: installed.Executable,
			Listen:     installed.Listen,
			Paths:      installed.Paths,
			CacheDir:   installed.CacheDir,
		}
	}
	m.starts++
	return service.InstallStartResult{Installation: installed, Status: status}, nil
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

func runHelperInstallForTest(version string) error {
	return runServiceAction(serviceActionInstall, version, &serviceActionReporter{action: serviceActionInstall})
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

	if !serviceGate(testGateSingleton) {
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

	if serviceGate(testGateSingleton) {
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
			t.Errorf("the start button is %q, want %q", got, want)
		}
		return serviceChoiceStart
	})

	var launches []elevatedLaunch
	replaceSeam(t, &launchElevated, func(l elevatedLaunch) (uint32, error) {
		launches = append(launches, l)
		return 4242, nil
	})
	messages := recordMessages(t)

	if serviceGate(testGateSingleton) {
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
	if launches[0].InstanceMutex != testGateSingleton {
		t.Errorf("the start helper holds the singleton %q, want this launch's singleton %q",
			launches[0].InstanceMutex, testGateSingleton)
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

// TestServiceGateContinuesInDesktopModeForAStoppedService proves that the
// default answer of the stopped dialog continues the launch into desktop mode.
//
// The gate's true answer is the launch a machine without a registration gets,
// so the application is created after it. Nothing starts or changes the
// service on the way: the elevated helper is the only code here that could
// touch the registration, and a stopped service stays stopped. The prompt has
// to be the positively confirmed stopped one — only that state reaches the
// layout whose default button is the desktop mode.
func TestServiceGateContinuesInDesktopModeForAStoppedService(t *testing.T) {
	replaceSeam(t, &serviceStatus, func(context.Context) (service.Status, error) {
		return runningServiceStatus(service.StateStopped, "127.0.0.1:18080"), nil
	})
	replaceSeam(t, &showServiceChoice, func(prompt serviceChoicePrompt) serviceChoice {
		if !servicePromptStopped(prompt) {
			t.Errorf("the dialog was not asked about a positively confirmed stopped service: state %q, running %t",
				prompt.State, prompt.Running)
		}
		if got, want := serviceGateOpenLabel(prompt), "Start service"; got != want {
			t.Errorf("the start button is %q, want %q", got, want)
		}
		return serviceChoiceDesktop
	})
	replaceSeam(t, &launchElevated, func(elevatedLaunch) (uint32, error) {
		t.Fatal("the desktop mode launched an elevated helper: it starts or changes the service")
		return 0, nil
	})
	replaceSeam(t, &openBrowser, func(string) error {
		t.Fatal("the desktop mode opened a browser although no server exists yet")
		return nil
	})
	messages := recordMessages(t)

	if !serviceGate(testGateSingleton) {
		t.Fatal("the service gate ended the launch although the user chose to work in desktop mode")
	}
	if len(*messages) != 0 {
		t.Errorf("the gate showed %v, want no notice", *messages)
	}
}

// TestServiceGateNeverContinuesWithoutAConfirmedStoppedService proves the
// other side of the invariant: the desktop mode is honored only for a
// positively confirmed stopped service. A running service and states that were
// not confirmed stopped never get a server next to themselves, even if an
// answer reached the gate that no dialog of theirs offers.
func TestServiceGateNeverContinuesWithoutAConfirmedStoppedService(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status service.Status
	}{
		{name: "running", status: runningServiceStatus(service.StateRunning, "127.0.0.1:18080")},
		{name: "failed", status: runningServiceStatus(service.StateFailed, "127.0.0.1:18080")},
		{name: "broken without an instance", status: runningServiceStatus(service.StateBroken, "127.0.0.1:18080")},
		// The state reads stopped but the supervisor reports a serving
		// instance: the running fact wins, so this is a running gate.
		{name: "stopped with the running fact", status: func() service.Status {
			status := runningServiceStatus(service.StateStopped, "127.0.0.1:18080")
			status.Running = true
			return status
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			replaceSeam(t, &serviceStatus, func(context.Context) (service.Status, error) {
				return tc.status, nil
			})
			replaceSeam(t, &showServiceChoice, func(serviceChoicePrompt) serviceChoice {
				return serviceChoiceDesktop
			})
			replaceSeam(t, &launchElevated, func(elevatedLaunch) (uint32, error) {
				t.Fatal("the gate launched an elevated helper although the desktop mode was not offered")
				return 0, nil
			})
			replaceSeam(t, &openBrowser, func(string) error {
				t.Fatal("the gate opened a browser although the launch ends without a server")
				return nil
			})
			messages := recordMessages(t)

			if serviceGate(testGateSingleton) {
				t.Fatal("the service gate continued into desktop mode although the service was not confirmed stopped")
			}
			if len(*messages) != 0 {
				t.Errorf("the gate showed %v, want no notice", *messages)
			}
		})
	}
}

// TestServiceGateCancelOfAStoppedServiceChangesNothing proves that Cancel of
// the stopped dialog behaves exactly like the Cancel of every other dialog of
// the gate: the launch ends and touches neither the browser, nor the helper,
// nor the service.
func TestServiceGateCancelOfAStoppedServiceChangesNothing(t *testing.T) {
	replaceSeam(t, &serviceStatus, func(context.Context) (service.Status, error) {
		return runningServiceStatus(service.StateStopped, "127.0.0.1:18080"), nil
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

	if serviceGate(testGateSingleton) {
		t.Fatal("a cancelled launch of a stopped service continued into desktop mode")
	}
	if len(*messages) != 0 {
		t.Errorf("the gate showed %v after the user cancelled the dialog", *messages)
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

	if serviceGate(testGateSingleton) {
		t.Fatal("the service gate let a launch with an installed service start its own server")
	}
	if len(opened) != 1 || opened[0] != "http://127.0.0.1:18080" {
		t.Errorf("the gate opened %v, want the web UI of the running service", opened)
	}
	if len(*messages) != 0 {
		t.Errorf("the gate showed %v, want no notice", *messages)
	}
}

// TestServiceGateOpensABrokenButRunningService proves that a launch offers the
// running service when the registration is broken but the instance still runs.
//
// A registration whose program was deleted is reported as broken, and the
// service control manager keeps the process it started. The launch has to offer
// "Open HFDesk" for it: offering the start of a service that already serves
// would send the user to a second start of the same instance.
func TestServiceGateOpensABrokenButRunningService(t *testing.T) {
	broken := runningServiceStatus(service.StateBroken, "127.0.0.1:18080")
	broken.Running = true
	broken.Detail = "the registered executable C:\\portable\\hfdesk.exe does not exist"

	replaceSeam(t, &serviceStatus, func(context.Context) (service.Status, error) {
		return broken, nil
	})
	replaceSeam(t, &showServiceChoice, func(prompt serviceChoicePrompt) serviceChoice {
		if prompt.State != service.StateBroken {
			t.Errorf("the dialog asked about the state %q, want the broken registration", prompt.State)
		}
		if !prompt.Running {
			t.Error("the prompt does not carry the running fact of the broken registration")
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
		t.Fatal("the gate started a service that is already running")
		return 0, nil
	})
	messages := recordMessages(t)

	if serviceGate(testGateSingleton) {
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
			t.Errorf("the start button is %q, want %q", got, want)
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

	if serviceGate(testGateSingleton) {
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

	if serviceGate(testGateSingleton) {
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

	if serviceGate(testGateSingleton) {
		t.Fatal("the service gate let a launch with an installed service start its own server")
	}
	if len(launches) != 1 || launches[0].Action != serviceActionUpdate {
		t.Fatalf("the gate launched %v, want the update of the service", launches)
	}
	if launches[0].WaitPID != 0 {
		t.Errorf("the update helper waits for the process %d, want no wait: the gate started no server", launches[0].WaitPID)
	}
	if launches[0].InstanceMutex != testGateSingleton {
		t.Errorf("the update helper holds the singleton %q, want this launch's singleton %q",
			launches[0].InstanceMutex, testGateSingleton)
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

	if serviceGate(testGateSingleton) {
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

	if serviceGate(testGateSingleton) {
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
// waits until the elevated helper has opened its handover handles.
//
// Readiness prevents shutdown before the helper has the original process handle
// and singleton. The helper later needs both successful completion and process
// exit; the console program is still checked before confirmation.
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
	replaceSeam(t, &launchElevatedProcess, func(l elevatedLaunch) (*elevatedProcess, error) {
		launches = append(launches, l)
		return readyTestProcess(t, l), nil
	})
	messages := recordMessages(t)

	handover := installServiceFromTray(`Local\HFDesk.Test.1`)
	if handover == nil {
		t.Fatal("the tray action did not hand the installation over to the helper")
	}
	t.Cleanup(handover.close)
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
	if launches[0].ReadyEvent == "" || launches[0].SuccessEvent == "" || launches[0].ReadyEvent == launches[0].SuccessEvent {
		t.Errorf("the helper handover events are %q and %q, want two distinct names", launches[0].ReadyEvent, launches[0].SuccessEvent)
	}
	if launches[0].InstanceMutex != `Local\HFDesk.Test.1` {
		t.Errorf("the helper was given singleton %q, want the existing object", launches[0].InstanceMutex)
	}
	if len(*messages) != 0 {
		t.Errorf("the tray action showed %v, want the helper to report the outcome", *messages)
	}

	// The guard is kept after a hand-over: the host ends on this answer, so a
	// click that still reaches the tray may not start a second installation.
	if installServiceFromTray(`Local\HFDesk.Test.1`) != nil {
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

	if installServiceFromTray(`Local\HFDesk.Test.2`) != nil {
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
	replaceSeam(t, &launchElevatedProcess, func(elevatedLaunch) (*elevatedProcess, error) {
		t.Fatal("the tray action launched the helper without the consent of the user")
		return nil, nil
	})
	messages := recordMessages(t)

	if installServiceFromTray(`Local\HFDesk.Test.3`) != nil {
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
	replaceSeam(t, &launchElevatedProcess, func(elevatedLaunch) (*elevatedProcess, error) {
		t.Fatal("the tray action launched the helper without the console program")
		return nil, nil
	})
	messages := recordMessages(t)

	if installServiceFromTray(`Local\HFDesk.Test.4`) != nil {
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
	replaceSeam(t, &launchElevatedProcess, func(elevatedLaunch) (*elevatedProcess, error) {
		return nil, errors.New(elevationDeclined)
	})
	messages := recordMessages(t)

	if installServiceFromTray(`Local\HFDesk.Test.5`) != nil {
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

// TestServiceHandoverFailureReportNamesTheStateCommand proves that the report
// of an unconfirmed graceful handover ends its next action with the command
// that really reads the state: the earlier wording asked the user to read the
// service state without naming a way to do it and hinted at an unnamed retry.
// The report keeps its result, its cause, the error of the wait and exactly
// one next action.
func TestServiceHandoverFailureReportNamesTheStateCommand(t *testing.T) {
	messages := recordMessages(t)

	reason := errors.New("the completion signal was never confirmed")
	reportServiceHandoverFailure(reason)

	if len(*messages) != 1 {
		t.Fatalf("the handover failure showed %d message boxes, want one", len(*messages))
	}
	if caption := serviceFailureCaption(serviceActionInstall); (*messages)[0].caption != caption {
		t.Errorf("the caption is %q, want %q", (*messages)[0].caption, caption)
	}
	text := (*messages)[0].text
	for _, want := range []string{
		"could not confirm the graceful service handover",
		"a successful completion signal and the desktop exits",
		reason.Error(),
		`"hfdesk service status"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the report %q does not mention %q", text, want)
		}
	}
	for _, unwanted := range []string{"Read the current service state", "before trying again"} {
		if strings.Contains(text, unwanted) {
			t.Errorf("the report %q asks for %q without naming the command", text, unwanted)
		}
	}
}

func TestTrayServiceInstallReadinessFailureKeepsRetryAvailable(t *testing.T) {
	stubNoServiceRegistration(t)
	freshServiceInstallGuard(t)
	var confirmations, launches int
	replaceSeam(t, &confirmServiceInstall, func(string) bool {
		confirmations++
		return true
	})
	replaceSeam(t, &findSiblingExecutable, func(string, string) (string, error) {
		return `C:\portable\hfdesk.exe`, nil
	})
	replaceSeam(t, &launchElevatedProcess, func(l elevatedLaunch) (*elevatedProcess, error) {
		launches++
		if launches == 2 {
			return readyTestProcess(t, l), nil
		}
		handle, err := windows.CreateEvent(nil, 1, 1, nil)
		if err != nil {
			return nil, err
		}
		return &elevatedProcess{handle: handle, pid: 5151}, nil
	})
	messages := recordMessages(t)

	if handover := installServiceFromTray(`Local\HFDesk.Test.Retry`); handover != nil {
		handover.close()
		t.Fatal("the tray action committed after the helper had already exited")
	}
	if serviceInstallRunning.Load() {
		t.Fatal("the failed pre-shutdown attempt kept the tray retry guard")
	}
	handover := installServiceFromTray(`Local\HFDesk.Test.Retry`)
	if handover == nil {
		t.Fatal("a retry could not proceed after the helper readiness failure")
	}
	defer handover.close()
	if launches != 2 || confirmations != 2 {
		t.Errorf("the retries launched %d helpers and asked %d confirmations, want two each", launches, confirmations)
	}
	if len(*messages) != 1 || !strings.Contains((*messages)[0].text, "exited before it became ready") {
		t.Errorf("the first attempt report is %v, want the early helper exit", *messages)
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
	replaceSeam(t, &launchElevatedProcess, func(elevatedLaunch) (*elevatedProcess, error) {
		t.Error("the tray action launched a helper although the confirmation was declined")
		return nil, nil
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
	go func() { first <- installServiceFromTray(`Local\HFDesk.Test.6`) != nil }()
	<-entered

	// The second click arrives while the first action waits for its
	// confirmation.
	if installServiceFromTray(`Local\HFDesk.Test.6`) != nil {
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
	if installServiceFromTray(`Local\HFDesk.Test.6`) != nil {
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

// TestServiceHelperRendersOnlyTheManagerVersionResult checks the desktop
// presentation boundary. It does not prove version verification; the service
// package tests exercise the real health probe and its version oracle.
func TestServiceHelperRendersOnlyTheManagerVersionResult(t *testing.T) {
	cases := []struct {
		name       string
		version    string
		verified   bool
		wantPassed string
		wantReport bool
	}{
		{"a verified release", "1.3.0", true, "1.3.0", true},
		{"a release without version proof", "1.3.0", false, "1.3.0", false},
		{"a local build", "dev", false, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			replaceSeam(t, &serviceVerifyBudget, 100*time.Millisecond)
			replaceSeam(t, &findSiblingExecutable, func(string, string) (string, error) {
				return `C:\portable\hfdesk.exe`, nil
			})

			running := runningServiceStatus(service.StateRunning, "127.0.0.1:18080")
			manager := &fakeServiceManager{
				statuses: []service.Status{running, running},
				installResult: service.InstallOptions{
					Executable:      `C:\portable\hfdesk.exe`,
					Listen:          running.Listen,
					Paths:           running.Paths,
					ExpectedVersion: tc.wantPassed,
					VersionVerified: tc.verified,
				},
			}
			replaceSeam(t, &newServiceManager, func(service.Scope) (service.Manager, error) {
				return manager, nil
			})
			messages := recordMessages(t)

			if err := RunServiceAction(ServiceActionOptions{Action: "update", Version: tc.version}); err != nil {
				t.Fatalf("the update failed: %v", err)
			}
			if len(manager.installs) != 1 {
				t.Fatalf("the update installed %d times, want one registration", len(manager.installs))
			}
			if got := manager.installs[0].ExpectedVersion; got != tc.wantPassed {
				t.Errorf("the update passed the expected version %q, want %q", got, tc.wantPassed)
			}
			if len(*messages) != 1 {
				t.Fatalf("the helper showed %d message boxes, want the report of the update", len(*messages))
			}
			text := (*messages)[0].text
			if got := strings.Contains(text, "1.3.0"); got != tc.wantReport {
				t.Errorf("the report %q names the version = %t, want %t", text, got, tc.wantReport)
			}
		})
	}
}

// TestServiceHelperUpdateKeepsTheRunningFactOfABrokenRegistration proves that
// the running fact of a broken registration decides what the update expects.
//
// A registration whose program was deleted is reported as broken. The service
// control manager keeps the process it started, so the registration can still
// serve: the update of such a service has to expect a running service and must
// not fail although the manager restarted it. A broken registration without a
// running instance stays stopped.
func TestServiceHelperUpdateKeepsTheRunningFactOfABrokenRegistration(t *testing.T) {
	for _, tc := range []struct {
		name    string
		running bool
	}{
		{name: "broken but running", running: true},
		{name: "broken and stopped", running: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			replaceSeam(t, &serviceVerifyBudget, 100*time.Millisecond)
			replaceSeam(t, &findSiblingExecutable, func(string, string) (string, error) {
				return `C:\portable\hfdesk.exe`, nil
			})

			broken := runningServiceStatus(service.StateBroken, "127.0.0.1:18080")
			broken.Running = tc.running
			manager := &fakeServiceManager{
				statuses: []service.Status{broken, broken},
				installResult: service.InstallOptions{
					Executable: `C:\portable\hfdesk.exe`,
					Listen:     broken.Listen,
					Paths:      broken.Paths,
				},
			}
			replaceSeam(t, &newServiceManager, func(service.Scope) (service.Manager, error) {
				return manager, nil
			})
			messages := recordMessages(t)

			if err := RunServiceAction(ServiceActionOptions{Action: "update", Version: "1.3.0"}); err != nil {
				t.Fatalf("the update of the %s service failed: %v", tc.name, err)
			}
			if manager.starts != 0 {
				t.Errorf("the update started the service %d times, want the service layer to decide", manager.starts)
			}
			if len(*messages) != 1 {
				t.Fatalf("the helper showed %d message boxes, want the report of the update", len(*messages))
			}
			text := (*messages)[0].text
			if tc.running {
				if !strings.Contains(text, "http://127.0.0.1:18080") {
					t.Errorf("the report %q does not name the address of the running service", text)
				}
				if strings.Contains(text, "stays stopped") {
					t.Errorf("the report %q claims the running service stays stopped", text)
				}
				return
			}
			if !strings.Contains(text, "stays stopped") {
				t.Errorf("the report %q does not say that the stopped service stays stopped", text)
			}
		})
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

	if err := runHelperInstallForTest("1.3.0"); err != nil {
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
	if len(manager.steps) != 1 || manager.steps[0] != "install-start" {
		t.Errorf("the helper steps are %v, want one service-owned install/start call", manager.steps)
	}
	for _, want := range []string{`C:\portable\hfdesk.exe`, "http://127.0.0.1:18080", "not needed to use the service"} {
		if !strings.Contains((*messages)[0].text, want) {
			t.Errorf("the report %q does not mention %q", (*messages)[0].text, want)
		}
	}
}

func TestServiceHelperInstallReportsTheServiceOwnedCleanupResult(t *testing.T) {
	cases := []struct {
		name    string
		cleanup *service.InstallStartError
		want    []string
	}{
		{"removed", &service.InstallStartError{Cause: errors.New("another version was reported"), RegistrationRemoved: true}, []string{"another version was reported", "registration created by this attempt was removed", "start the desktop app normally again"}},
		{"kept when ownership is unknown", &service.InstallStartError{Cause: errors.New("another version was reported"), CleanupError: errors.New("process identity changed")}, []string{"another version was reported", "registration was kept", "could not prove", "hfdesk service status"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const console = `C:\portable\hfdesk.exe`
			replaceSeam(t, &findSiblingExecutable, func(string, string) (string, error) { return console, nil })
			manager := &fakeServiceManager{installStartErr: tc.cleanup}
			replaceSeam(t, &newServiceManager, func(service.Scope) (service.Manager, error) { return manager, nil })
			messages := recordMessages(t)
			if err := runHelperInstallForTest("1.4.0"); err == nil {
				t.Fatal("the installation succeeded although the verified start failed")
			}
			if len(manager.steps) != 1 || manager.steps[0] != "install-start" || manager.reads != 0 || manager.starts != 0 {
				t.Errorf("the helper used steps %v, read status %d times and started %d times; want one service-owned operation", manager.steps, manager.reads, manager.starts)
			}
			if len(*messages) != 1 {
				t.Fatalf("the helper showed %d message boxes, want one", len(*messages))
			}
			for _, want := range tc.want {
				if !strings.Contains((*messages)[0].text, want) {
					t.Errorf("the report %q does not mention %q", (*messages)[0].text, want)
				}
			}
		})
	}
}

func TestServiceHelperInstallDoesNotCleanAConcurrentRegistrationByName(t *testing.T) {
	const console = `C:\portable\hfdesk.exe`
	replaceSeam(t, &findSiblingExecutable, func(string, string) (string, error) { return console, nil })
	// The refusal of the manager, wrapped exactly like production wraps it: a
	// registration that appears before the write is not this attempt's
	// registration, so the helper reports it without reopening the name to
	// clean it up.
	manager := &fakeServiceManager{installStartErr: fmt.Errorf(
		"service: %s appeared during the first installation; no existing registration was changed%w", service.ServiceName, service.ErrAlreadyRegistered)}
	replaceSeam(t, &newServiceManager, func(service.Scope) (service.Manager, error) { return manager, nil })
	messages := recordMessages(t)
	if err := runHelperInstallForTest(""); err == nil {
		t.Fatal("the install succeeded although a concurrent registration appeared")
	}
	if len(manager.steps) != 1 || manager.steps[0] != "install-start" || manager.reads != 0 {
		t.Errorf("the helper used steps %v and read status %d times, want no by-name cleanup", manager.steps, manager.reads)
	}
	if len(*messages) != 1 || !strings.Contains((*messages)[0].text, "appeared during the first installation") {
		t.Errorf("the helper report is %v, want the concurrent-registration error", *messages)
	}
	// The attempt changed no registration, so the report offers no removal
	// of a registration it never touched.
	for _, unwanted := range []string{"A registration may remain", `"hfdesk service uninstall"`} {
		if strings.Contains((*messages)[0].text, unwanted) {
			t.Errorf("the helper report %v carries %q although this attempt changed no registration", *messages, unwanted)
		}
	}
}

func TestServiceHelperInstallReportsARegistrationConflictWithoutStatusLookup(t *testing.T) {
	const console = `C:\portable\hfdesk.exe`
	replaceSeam(t, &findSiblingExecutable, func(string, string) (string, error) { return console, nil })
	// The refusal of the manager at the first-install boundary, wrapped
	// exactly like production wraps it, so the notice is exercised with the
	// error the helper will really receive.
	manager := &fakeServiceManager{installStartErr: fmt.Errorf(
		"service: %s is already registered; the first-install action will not change or start it%w", service.ServiceName, service.ErrAlreadyRegistered)}
	replaceSeam(t, &newServiceManager, func(service.Scope) (service.Manager, error) { return manager, nil })
	messages := recordMessages(t)

	if err := runHelperInstallForTest(""); err == nil {
		t.Fatal("the first-install action accepted an existing registration")
	}
	if manager.reads != 0 || manager.starts != 0 || stepIndex(manager.steps, "uninstall") >= 0 {
		t.Errorf("the helper read, started or removed a registration: steps=%v reads=%d starts=%d", manager.steps, manager.reads, manager.starts)
	}
	if len(*messages) != 1 {
		t.Fatalf("the helper showed %d message boxes, want the refusal of the existing registration", len(*messages))
	}
	for _, want := range []string{"already registered", "did not create, change or start it", `"hfdesk service install"`} {
		if !strings.Contains((*messages)[0].text, want) {
			t.Errorf("the report %q does not mention %q", (*messages)[0].text, want)
		}
	}
	for _, unwanted := range []string{"A registration may remain", `"hfdesk service uninstall"`} {
		if strings.Contains((*messages)[0].text, unwanted) {
			t.Errorf("the report %q carries %q although this attempt changed no registration", (*messages)[0].text, unwanted)
		}
	}
}

func TestServiceHelperPassesTheVersionExpectationToTheServiceOwner(t *testing.T) {
	const console = `C:\portable\hfdesk.exe`
	replaceSeam(t, &findSiblingExecutable, func(string, string) (string, error) { return console, nil })
	manager := &fakeServiceManager{}
	replaceSeam(t, &newServiceManager, func(service.Scope) (service.Manager, error) { return manager, nil })
	recordMessages(t)

	if err := runHelperInstallForTest("1.4.0"); err != nil {
		t.Fatalf("the first installation failed: %v", err)
	}
	if len(manager.installs) != 1 || manager.installs[0].ExpectedVersion != "1.4.0" {
		t.Fatalf("the service-owned install options are %+v, want expected version 1.4.0", manager.installs)
	}
	if stepIndex(manager.steps, "start") >= 0 || manager.starts != 1 {
		t.Errorf("the helper start path has steps %v and starts=%d, want one service-owned start and no ordinary Start", manager.steps, manager.starts)
	}
}

func TestServiceHelperDoesNotReopenTheServiceAfterUnprovenCleanup(t *testing.T) {
	const console = `C:\portable\hfdesk.exe`
	for _, state := range []service.State{service.StateRunning, service.StateStarting, service.StateStopping} {
		t.Run(string(state), func(t *testing.T) {
			replaceSeam(t, &findSiblingExecutable, func(string, string) (string, error) { return console, nil })
			manager := &fakeServiceManager{
				statuses: []service.Status{runningServiceStatus(state, "127.0.0.1:18080")},
				installStartErr: &service.InstallStartError{
					Cause:        errors.New("the health version did not match"),
					CleanupError: errors.New("the service process was not owned by this attempt"),
				},
			}
			replaceSeam(t, &newServiceManager, func(service.Scope) (service.Manager, error) { return manager, nil })
			messages := recordMessages(t)
			if err := runHelperInstallForTest(""); err == nil {
				t.Fatal("the installation succeeded although the verified start failed")
			}
			if manager.reads != 0 || manager.starts != 0 || stepIndex(manager.steps, "uninstall") >= 0 {
				t.Errorf("the helper independently touched the registration after ownership was unproven: steps=%v reads=%d starts=%d", manager.steps, manager.reads, manager.starts)
			}
			if len(*messages) != 1 || !strings.Contains((*messages)[0].text, "registration was kept") {
				t.Errorf("the helper report is %v, want the service-owned kept outcome", *messages)
			}
		})
	}
}

func TestServiceHelperKeepsAnUnconfirmedCleanupResult(t *testing.T) {
	const console = `C:\portable\hfdesk.exe`
	replaceSeam(t, &findSiblingExecutable, func(string, string) (string, error) { return console, nil })
	manager := &fakeServiceManager{
		statusErr: errors.New("the SCM cannot be read"),
		installStartErr: &service.InstallStartError{
			Cause:        errors.New("the service could not be verified"),
			CleanupError: errors.New("the SCM state could not be read"),
		},
	}
	replaceSeam(t, &newServiceManager, func(service.Scope) (service.Manager, error) { return manager, nil })
	messages := recordMessages(t)
	if err := runHelperInstallForTest(""); err == nil {
		t.Fatal("the installation succeeded although the service could not be verified")
	}
	if manager.reads != 0 || stepIndex(manager.steps, "uninstall") >= 0 {
		t.Errorf("the helper reopened the service after an unconfirmed cleanup: steps=%v reads=%d", manager.steps, manager.reads)
	}
	if len(*messages) != 1 {
		t.Fatalf("the helper showed %d message boxes, want the cleanup outcome", len(*messages))
	}
	for _, want := range []string{"could not be proven", "registration was kept", "service status"} {
		if !strings.Contains((*messages)[0].text, want) {
			t.Errorf("the report %q does not mention %q", (*messages)[0].text, want)
		}
	}
}

// stubConsoleProgram points the console-program lookup at a real executable
// the image preflight accepts: the test binary of this process.
func stubConsoleProgram(t *testing.T) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("read the test executable: %v", err)
	}
	replaceSeam(t, &findSiblingExecutable, func(string, string) (string, error) { return self, nil })
}

func TestServiceHelperRequiresBothSuccessfulShutdownAndDesktopExit(t *testing.T) {
	replaceSeam(t, &serviceVerifyBudget, 100*time.Millisecond)
	stubConsoleProgram(t)
	manager := &fakeServiceManager{}
	replaceSeam(t, &newServiceManager, func(service.Scope) (service.Manager, error) {
		return manager, nil
	})
	handover := testHelperHandover(t)
	replaceSeam(t, &prepareHelperHandover, func(ServiceActionOptions) (*helperHandover, error) { return handover, nil })
	recordMessages(t)

	done := make(chan error, 1)
	go func() {
		done <- RunServiceAction(ServiceActionOptions{
			Action: "install", WaitPID: 4321,
			ReadyEvent: "ready", SuccessEvent: "success", InstanceMutex: "mutex",
		})
	}()
	if result, err := windows.WaitForSingleObject(handover.ready, 2_000); err != nil || result != windows.WAIT_OBJECT_0 {
		t.Fatalf("the helper did not signal ready: result=%#x err=%v", result, err)
	}
	select {
	case err := <-done:
		t.Fatalf("the helper returned before successful completion or desktop exit: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := windows.SetEvent(handover.success); err != nil {
		t.Fatalf("signal successful completion: %v", err)
	}
	select {
	case err := <-done:
		t.Fatalf("the helper returned after success but before the desktop exited: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := windows.SetEvent(handover.parent); err != nil {
		t.Fatalf("signal desktop exit: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("the authorized installation failed: %v", err)
	}
	if len(manager.steps) != 1 || manager.steps[0] != "install-start" {
		t.Fatalf("the helper used service steps %v, want one install after both signals", manager.steps)
	}
}

func TestServiceHelperRejectsDesktopExitWithoutSuccessfulShutdown(t *testing.T) {
	stubConsoleProgram(t)
	handover := testHelperHandover(t)
	replaceSeam(t, &prepareHelperHandover, func(ServiceActionOptions) (*helperHandover, error) { return handover, nil })
	manager := &fakeServiceManager{}
	replaceSeam(t, &newServiceManager, func(service.Scope) (service.Manager, error) {
		return manager, nil
	})
	messages := recordMessages(t)

	done := make(chan error, 1)
	go func() {
		done <- RunServiceAction(ServiceActionOptions{
			Action: "install", WaitPID: 4321,
			ReadyEvent: "ready", SuccessEvent: "success", InstanceMutex: "mutex",
		})
	}()
	if result, err := windows.WaitForSingleObject(handover.ready, 2_000); err != nil || result != windows.WAIT_OBJECT_0 {
		t.Fatalf("the helper did not signal ready: result=%#x err=%v", result, err)
	}
	if err := windows.SetEvent(handover.parent); err != nil {
		t.Fatalf("signal desktop exit: %v", err)
	}
	if err := <-done; err == nil {
		t.Fatal("the installation succeeded although the desktop supplied no successful-shutdown signal")
	}
	if len(manager.steps) != 0 {
		t.Errorf("the helper touched the service before authorization: %v", manager.steps)
	}
	if len(*messages) != 1 {
		t.Fatalf("the helper showed %d message boxes, want the authorization failure", len(*messages))
	}
	if !strings.Contains((*messages)[0].text, "without confirming successful graceful completion") {
		t.Errorf("the report %q does not identify the missing success signal", (*messages)[0].text)
	}
}

func TestServiceHelperDoesNotReviveAfterHandoverTimeout(t *testing.T) {
	replaceSeam(t, &serviceWaitBudget, 50*time.Millisecond)
	replaceSeam(t, &serviceParentExitBudget, 50*time.Millisecond)
	stubConsoleProgram(t)
	handover := testHelperHandover(t)
	lateParent := duplicateTestHandle(t, handover.parent)
	lateSuccess := duplicateTestHandle(t, handover.success)
	replaceSeam(t, &prepareHelperHandover, func(ServiceActionOptions) (*helperHandover, error) { return handover, nil })
	manager := &fakeServiceManager{}
	replaceSeam(t, &newServiceManager, func(service.Scope) (service.Manager, error) { return manager, nil })
	recordMessages(t)

	done := make(chan error, 1)
	go func() {
		done <- RunServiceAction(ServiceActionOptions{
			Action: "install", WaitPID: 4321,
			ReadyEvent: "ready", SuccessEvent: "success", InstanceMutex: "mutex",
		})
	}()
	if result, err := windows.WaitForSingleObject(handover.ready, 2_000); err != nil || result != windows.WAIT_OBJECT_0 {
		t.Fatalf("the helper did not signal ready: result=%#x err=%v", result, err)
	}
	if err := <-done; err == nil {
		t.Fatal("the helper accepted a handover that timed out")
	}
	if err := windows.SetEvent(lateSuccess); err != nil {
		t.Fatalf("signal a late completion: %v", err)
	}
	if err := windows.SetEvent(lateParent); err != nil {
		t.Fatalf("signal a late parent exit: %v", err)
	}
	if len(manager.steps) != 0 {
		t.Errorf("late handover signals revived service actions: %v", manager.steps)
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
	for _, unwanted := range []string{status.Executable, `NT AUTHORITY\LocalService`} {
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
		installStartErr: errors.New("the service control manager refused the registration"),
	}
	replaceSeam(t, &newServiceManager, func(service.Scope) (service.Manager, error) {
		return manager, nil
	})
	messages := recordMessages(t)

	if err := runHelperInstallForTest(""); err == nil {
		t.Fatal("the installation succeeded although the service control manager refused it")
	}
	if len(*messages) != 1 {
		t.Fatalf("the helper showed %d message boxes, want the failure of the installation", len(*messages))
	}
	for _, want := range []string{console, `NT AUTHORITY\LocalService`} {
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

func TestServiceHelperRejectsIncompleteInstallHandover(t *testing.T) {
	for _, tc := range []struct {
		name string
		o    ServiceActionOptions
	}{
		{"no handover", ServiceActionOptions{Action: "install"}},
		{"pid only", ServiceActionOptions{Action: "install", WaitPID: 4321}},
		{"missing success event", ServiceActionOptions{Action: "install", WaitPID: 4321, ReadyEvent: "ready", InstanceMutex: "mutex"}},
		{"missing mutex", ServiceActionOptions{Action: "install", WaitPID: 4321, ReadyEvent: "ready", SuccessEvent: "success"}},
		{"names without pid", ServiceActionOptions{Action: "install", ReadyEvent: "ready", SuccessEvent: "success", InstanceMutex: "mutex"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			replaceSeam(t, &newServiceManager, func(service.Scope) (service.Manager, error) {
				t.Fatal("the helper touched the service with incomplete handover arguments")
				return nil, nil
			})
			messages := recordMessages(t)
			if err := RunServiceAction(tc.o); err == nil {
				t.Fatal("the helper accepted incomplete handover arguments")
			}
			if len(*messages) != 1 || !strings.Contains((*messages)[0].text, "handover") {
				t.Errorf("the helper report is %v, want the handover failure", *messages)
			}
		})
	}
}

// TestServiceHelperArgumentsNameTheActionAndHandover proves that the helper
// receives the event names and existing singleton identity of one handover.
func TestServiceHelperArgumentsNameTheActionAndHandover(t *testing.T) {
	install := serviceHelperArguments(elevatedLaunch{
		Action: serviceActionInstall, WaitPID: 4321,
		ReadyEvent: `Local\ready`, SuccessEvent: `Local\success`, InstanceMutex: `Local\HFDesk.Desktop.sid.1`,
	})
	if want := `--service-action install --wait-pid 4321 --handover-ready-event Local\ready ` +
		`--handover-success-event Local\success --instance-mutex Local\HFDesk.Desktop.sid.1`; install != want {
		t.Errorf("the arguments are %q, want %q", install, want)
	}
	update := serviceHelperArguments(elevatedLaunch{Action: serviceActionUpdate})
	if want := "--service-action update"; update != want {
		t.Errorf("the arguments are %q, want %q", update, want)
	}
	updateWithMutex := serviceHelperArguments(elevatedLaunch{
		Action: serviceActionUpdate, InstanceMutex: `Local\HFDesk.Desktop.sid.1`,
	})
	if want := `--service-action update --instance-mutex Local\HFDesk.Desktop.sid.1`; updateWithMutex != want {
		t.Errorf("the arguments are %q, want %q", updateWithMutex, want)
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

	if err := runHelperInstallForTest(""); err == nil {
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

// TestServiceHelperSingletonArgumentRules proves the helper accepts exactly
// the gate command lines — update and start carrying the singleton alone,
// without a wait — and still refuses handover names without the process to
// wait for, so no action gains a wait it did not have.
func TestServiceHelperSingletonArgumentRules(t *testing.T) {
	for _, tc := range []struct {
		name   string
		action serviceAction
		o      ServiceActionOptions
	}{
		{"update carries the singleton alone", serviceActionUpdate,
			ServiceActionOptions{Action: "update", InstanceMutex: `Local\m`}},
		{"start carries the singleton alone", serviceActionStart,
			ServiceActionOptions{Action: "start", InstanceMutex: `Local\m`}},
	} {
		if err := validateServiceHandoverOptions(tc.action, tc.o); err != nil {
			t.Errorf("%s was rejected: %v", tc.name, err)
		}
	}
	for _, tc := range []struct {
		name   string
		action serviceAction
		o      ServiceActionOptions
	}{
		{"update with event names but no process", serviceActionUpdate,
			ServiceActionOptions{Action: "update", ReadyEvent: "ready", InstanceMutex: `Local\m`}},
		{"update with a wait but no event names", serviceActionUpdate,
			ServiceActionOptions{Action: "update", WaitPID: 4321}},
		{"install with the singleton alone", serviceActionInstall,
			ServiceActionOptions{Action: "install", InstanceMutex: `Local\m`}},
		{"install without a handover", serviceActionInstall,
			ServiceActionOptions{Action: "install"}},
	} {
		if err := validateServiceHandoverOptions(tc.action, tc.o); err == nil {
			t.Errorf("%s was accepted, want the refusal of incomplete handover arguments", tc.name)
		}
	}
}

// singletonObservingManager reports whether the per-session desktop singleton
// exists at the moment the service mutation runs.
type singletonObservingManager struct {
	*fakeServiceManager
	existedDuringMutation []bool
}

func (m *singletonObservingManager) observe() {
	lock, err := acquireInstanceLock()
	existed := err == nil && lock.Exists()
	if err == nil {
		lock.Release()
	}
	m.existedDuringMutation = append(m.existedDuringMutation, existed)
}

func (m *singletonObservingManager) Install(ctx context.Context, opts service.InstallOptions) (service.InstallOptions, error) {
	m.observe()
	return m.fakeServiceManager.Install(ctx, opts)
}

func (m *singletonObservingManager) Start(ctx context.Context) error {
	m.observe()
	return m.fakeServiceManager.Start(ctx)
}

// TestServiceHelperUpdateHoldsTheDesktopSingletonThroughItsMutation proves
// both sides of the update window: while the mutation runs, the per-session
// singleton exists — a desktop launched in that window finds the running
// instance instead of binding the address of the stopped service — and after
// the report the singleton is gone again, so ordinary launches are not
// refused. The hold is also proven to end before the final notice, the
// release point of the established install mechanism.
func TestServiceHelperUpdateHoldsTheDesktopSingletonThroughItsMutation(t *testing.T) {
	name := requireFreeSingleton(t)

	replaceSeam(t, &findSiblingExecutable, func(string, string) (string, error) {
		return `C:\portable\hfdesk.exe`, nil
	})
	stopped := runningServiceStatus(service.StateStopped, "127.0.0.1:18080")
	manager := &singletonObservingManager{fakeServiceManager: &fakeServiceManager{
		statuses: []service.Status{stopped, stopped},
	}}
	replaceSeam(t, &newServiceManager, func(service.Scope) (service.Manager, error) {
		return manager, nil
	})
	messages, existedAtNotice := recordMessagesWithSingletonProbe(t)

	if err := RunServiceAction(ServiceActionOptions{Action: "update", InstanceMutex: name}); err != nil {
		t.Fatalf("the update that holds the singleton failed: %v", err)
	}
	if len(manager.existedDuringMutation) != 1 || !manager.existedDuringMutation[0] {
		t.Fatalf("the singleton existed during the mutation = %v, want [true]: a desktop launched in that window would start a second server",
			manager.existedDuringMutation)
	}
	if len(*existedAtNotice) != 1 || (*existedAtNotice)[0] {
		t.Fatalf("the singleton existed when the final notice was shown = %v, want [false]: the hold must end before the notice",
			*existedAtNotice)
	}
	requireSingletonReleased(t)
	if len(*messages) != 1 || (*messages)[0].caption != serviceUpdatedCaption {
		t.Fatalf("the helper showed %v, want the report of the finished update", *messages)
	}
}

// TestServiceHelperStartHoldsTheDesktopSingletonThroughItsMutation is the
// same both-sides proof for the gate's start: the start takes the service out
// of the positively stopped state the gate decided on, so the singleton has
// to exist while the start runs and to be gone again after the report.
func TestServiceHelperStartHoldsTheDesktopSingletonThroughItsMutation(t *testing.T) {
	name := requireFreeSingleton(t)

	stopped := runningServiceStatus(service.StateStopped, "127.0.0.1:18080")
	running := runningServiceStatus(service.StateRunning, "127.0.0.1:18080")
	manager := &singletonObservingManager{fakeServiceManager: &fakeServiceManager{
		statuses: []service.Status{stopped, running},
	}}
	replaceSeam(t, &newServiceManager, func(service.Scope) (service.Manager, error) {
		return manager, nil
	})
	messages, existedAtNotice := recordMessagesWithSingletonProbe(t)

	if err := RunServiceAction(ServiceActionOptions{Action: "start", InstanceMutex: name}); err != nil {
		t.Fatalf("the start that holds the singleton failed: %v", err)
	}
	if manager.starts != 1 {
		t.Fatalf("the helper started the service %d times, want one start", manager.starts)
	}
	if len(manager.existedDuringMutation) != 1 || !manager.existedDuringMutation[0] {
		t.Fatalf("the singleton existed during the start = %v, want [true]: a desktop launched in that window would start a second server",
			manager.existedDuringMutation)
	}
	if len(*existedAtNotice) != 1 || (*existedAtNotice)[0] {
		t.Fatalf("the singleton existed when the final notice was shown = %v, want [false]: the hold must end before the notice",
			*existedAtNotice)
	}
	requireSingletonReleased(t)
	if len(*messages) != 1 || (*messages)[0].caption != serviceStartedCaption {
		t.Fatalf("the helper showed %v, want the report of the started service", *messages)
	}
}

// TestServiceHelperFailsWhenTheSingletonCannotBeHeld proves the hold fails
// closed for both gate actions: an update or a start that cannot hold the
// window must not move the service a relaunch could then bind.
func TestServiceHelperFailsWhenTheSingletonCannotBeHeld(t *testing.T) {
	for _, tc := range []struct {
		action serviceAction
		result string
	}{
		{serviceActionUpdate, "could not update"},
		{serviceActionStart, "could not start"},
	} {
		t.Run(string(tc.action), func(t *testing.T) {
			replaceSeam(t, &newServiceManager, func(service.Scope) (service.Manager, error) {
				t.Fatal("the action touched the service without holding the desktop singleton")
				return nil, nil
			})
			messages := recordMessages(t)

			// A name that cannot be encoded as a Windows object name makes
			// the hold itself fail: the action must then refuse to run.
			err := RunServiceAction(ServiceActionOptions{Action: string(tc.action), InstanceMutex: "Local\x00invalid"})
			if err == nil {
				t.Fatal("the action ran although the singleton name could not be used")
			}
			if len(*messages) != 1 {
				t.Fatalf("the helper showed %d message boxes, want the failure of the action", len(*messages))
			}
			if caption := serviceFailureCaption(tc.action); (*messages)[0].caption != caption {
				t.Errorf("the caption is %q, want %q", (*messages)[0].caption, caption)
			}
			for _, want := range []string{tc.result, "hfdesk service status"} {
				if !strings.Contains((*messages)[0].text, want) {
					t.Errorf("the report %q does not mention %q", (*messages)[0].text, want)
				}
			}
		})
	}
}

// TestServiceHelperUpdateRejectsAVanishedRegistration proves a stopped-service
// update never accepts a registration that is gone: StateNotInstalled is not
// the settled stopped state the update waits for, so the action fails
// immediately with the cause instead of reporting an update it cannot verify.
func TestServiceHelperUpdateRejectsAVanishedRegistration(t *testing.T) {
	replaceSeam(t, &findSiblingExecutable, func(string, string) (string, error) {
		return `C:\portable\hfdesk.exe`, nil
	})
	manager := &fakeServiceManager{statuses: []service.Status{
		runningServiceStatus(service.StateStopped, "127.0.0.1:18080"),
		{State: service.StateNotInstalled},
	}}
	replaceSeam(t, &newServiceManager, func(service.Scope) (service.Manager, error) {
		return manager, nil
	})
	messages := recordMessages(t)

	if err := RunServiceAction(ServiceActionOptions{Action: "update"}); err == nil {
		t.Fatal("the update succeeded although the registration is gone")
	}
	if len(*messages) != 1 {
		t.Fatalf("the helper showed %d message boxes, want the failure of the update", len(*messages))
	}
	if caption := serviceFailureCaption(serviceActionUpdate); (*messages)[0].caption != caption {
		t.Errorf("the caption is %q, want %q", (*messages)[0].caption, caption)
	}
	for _, want := range []string{"could not update", "registration of the service is gone", "hfdesk service status"} {
		if !strings.Contains((*messages)[0].text, want) {
			t.Errorf("the report %q does not mention %q", (*messages)[0].text, want)
		}
	}
}

// TestServiceHelperStartRejectsAVanishedRegistration proves the same rule for
// the start action: a registration that disappears before the start is
// confirmed is named as gone instead of being waited out until the verify
// budget ends with a state the machine no longer has.
func TestServiceHelperStartRejectsAVanishedRegistration(t *testing.T) {
	manager := &fakeServiceManager{statuses: []service.Status{
		runningServiceStatus(service.StateStopped, "127.0.0.1:18080"),
		{State: service.StateNotInstalled},
	}}
	replaceSeam(t, &newServiceManager, func(service.Scope) (service.Manager, error) {
		return manager, nil
	})
	messages := recordMessages(t)

	if err := RunServiceAction(ServiceActionOptions{Action: "start"}); err == nil {
		t.Fatal("the start succeeded although the registration is gone")
	}
	if len(*messages) != 1 || !strings.Contains((*messages)[0].text, "registration of the service is gone") {
		t.Fatalf("the helper showed %v, want the report of the vanished registration", *messages)
	}
}

// TestServiceGateRechecksTheStateBeforeDesktopMode proves the desktop mode is
// admitted only for a state that is still positively stopped when the choice
// is honored: the dialog is modal, and a service another actor started while
// it was open would serve the address this launch binds.
func TestServiceGateRechecksTheStateBeforeDesktopMode(t *testing.T) {
	reads := 0
	replaceSeam(t, &serviceStatus, func(context.Context) (service.Status, error) {
		reads++
		if reads == 1 {
			return runningServiceStatus(service.StateStopped, "127.0.0.1:18080"), nil
		}
		return runningServiceStatus(service.StateRunning, "127.0.0.1:18080"), nil
	})
	replaceSeam(t, &showServiceChoice, func(serviceChoicePrompt) serviceChoice {
		return serviceChoiceDesktop
	})
	replaceSeam(t, &launchElevated, func(elevatedLaunch) (uint32, error) {
		t.Fatal("the gate launched a helper for a launch that ends without a server")
		return 0, nil
	})
	replaceSeam(t, &openBrowser, func(string) error {
		t.Fatal("the gate opened a browser although this launch starts no server")
		return nil
	})
	messages := recordMessages(t)

	if serviceGate(testGateSingleton) {
		t.Fatal("the gate admitted desktop mode although the service was started while the dialog was open")
	}
	if reads != 2 {
		t.Errorf("the gate read the state %d times, want the original read and the re-check", reads)
	}
	if len(*messages) != 1 {
		t.Fatalf("the gate showed %v, want the notice of the changed state", *messages)
	}
	if caption := serviceGateChangedCaption; (*messages)[0].caption != caption {
		t.Errorf("the caption is %q, want %q", (*messages)[0].caption, caption)
	}
	for _, want := range []string{"changed while the dialog was open", "running now", "started no server", "hfdesk service status"} {
		if !strings.Contains((*messages)[0].text, want) {
			t.Errorf("the notice %q does not mention %q", (*messages)[0].text, want)
		}
	}
}

// TestServiceGateStopsWhenTheRecheckIsNotPositivelyStopped covers the other
// re-read outcomes: a registration that is gone, a state that cannot be read
// again and a state still in transition are all not the positively confirmed
// stopped service the dialog asked about, so none of them admits a server and
// each is reported with its own honest notice.
func TestServiceGateStopsWhenTheRecheckIsNotPositivelyStopped(t *testing.T) {
	for _, tc := range []struct {
		name      string
		second    service.Status
		secondErr error
		timeout   time.Duration
		caption   string
		want      string
	}{
		{
			name:    "registration removed while the dialog was open",
			second:  service.Status{State: service.StateNotInstalled},
			caption: serviceGateChangedCaption,
			want:    "no longer registered",
		},
		{
			name:      "state unreadable while the dialog was open",
			secondErr: errors.New("the service control manager did not answer"),
			caption:   serviceGateUnreadableCaption,
			want:      "hfdesk service status",
		},
		{
			name:    "service still starting when the dialog closed",
			second:  runningServiceStatus(service.StateStarting, "127.0.0.1:18080"),
			timeout: 50 * time.Millisecond,
			caption: serviceGatePendingCaption,
			want:    "still starting",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads := 0
			replaceSeam(t, &serviceStatus, func(context.Context) (service.Status, error) {
				reads++
				if reads == 1 {
					return runningServiceStatus(service.StateStopped, "127.0.0.1:18080"), nil
				}
				return tc.second, tc.secondErr
			})
			if tc.timeout != 0 {
				replaceSeam(t, &serviceStatusTimeout, tc.timeout)
			}
			replaceSeam(t, &showServiceChoice, func(serviceChoicePrompt) serviceChoice {
				return serviceChoiceDesktop
			})
			replaceSeam(t, &launchElevated, func(elevatedLaunch) (uint32, error) {
				t.Fatal("the gate launched a helper for a launch that ends without a server")
				return 0, nil
			})
			replaceSeam(t, &openBrowser, func(string) error {
				t.Fatal("the gate opened a browser although this launch starts no server")
				return nil
			})
			messages := recordMessages(t)

			if serviceGate(testGateSingleton) {
				t.Fatal("the gate admitted desktop mode although the re-read state is not positively stopped")
			}
			if len(*messages) != 1 {
				t.Fatalf("the gate showed %v, want one honest notice", *messages)
			}
			if (*messages)[0].caption != tc.caption {
				t.Errorf("the caption is %q, want %q", (*messages)[0].caption, tc.caption)
			}
			if !strings.Contains((*messages)[0].text, tc.want) {
				t.Errorf("the notice %q does not mention %q", (*messages)[0].text, tc.want)
			}
		})
	}
}

// TestServiceHelperInstallRejectsABadImageBeforeReady proves the console
// program is checked before the ready signal: a program the host cannot
// execute fails with no ready event sent — so the desktop never commits its
// shutdown and stays open — and the service is never touched.
func TestServiceHelperInstallRejectsABadImageBeforeReady(t *testing.T) {
	for _, tc := range []struct {
		name  string
		image func(t *testing.T) string
		want  string
	}{
		{
			// The file must be readable as a whole DOS header (64 bytes),
			// so the refusal comes from the missing MZ signature class
			// instead of the short-read class the truncated case pins.
			name: "not a Windows program",
			image: func(t *testing.T) string {
				path := filepath.Join(t.TempDir(), "hfdesk.exe")
				if err := os.WriteFile(path, []byte(strings.Repeat("this is not a program\n", 8)), 0o644); err != nil {
					t.Fatalf("write the fixture: %v", err)
				}
				return path
			},
			want: "no MZ signature",
		},
		{
			name: "truncated image",
			image: func(t *testing.T) string {
				path := filepath.Join(t.TempDir(), "hfdesk.exe")
				if err := os.WriteFile(path, []byte("MZ"), 0o644); err != nil {
					t.Fatalf("write the fixture: %v", err)
				}
				return path
			},
			want: "read the DOS header",
		},
		{
			name:  "machine the host cannot execute",
			image: func(t *testing.T) string { return testPEImage(t, testPEMachineIA64) },
			want:  "cannot execute",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The budgets shorten the falsification of this test: without
			// the preflight the helper would sign ready and then time out
			// in the authorization wait, and the test must fail fast on
			// the sent ready signal instead of hanging.
			replaceSeam(t, &serviceWaitBudget, 50*time.Millisecond)
			replaceSeam(t, &serviceParentExitBudget, 50*time.Millisecond)
			handover := testHelperHandover(t)
			readyProbe := duplicateTestHandle(t, handover.ready)
			replaceSeam(t, &prepareHelperHandover, func(ServiceActionOptions) (*helperHandover, error) { return handover, nil })
			replaceSeam(t, &findSiblingExecutable, func(string, string) (string, error) { return tc.image(t), nil })
			replaceSeam(t, &newServiceManager, func(service.Scope) (service.Manager, error) {
				t.Fatal("the installation touched the service although the image preflight failed")
				return nil, nil
			})
			messages := recordMessages(t)

			err := RunServiceAction(ServiceActionOptions{
				Action: "install", WaitPID: 4321,
				ReadyEvent: "ready", SuccessEvent: "success", InstanceMutex: "mutex",
			})
			if err == nil {
				t.Fatal("the installation accepted an image the host cannot execute")
			}
			if result, waitErr := windows.WaitForSingleObject(readyProbe, 0); waitErr != nil || result != uint32(windows.WAIT_TIMEOUT) {
				t.Fatalf("the ready signal was sent before the preflight passed: result=%#x err=%v", result, waitErr)
			}
			if len(*messages) != 1 {
				t.Fatalf("the helper showed %d message boxes, want the failure of the installation", len(*messages))
			}
			if caption := serviceFailureCaption(serviceActionInstall); (*messages)[0].caption != caption {
				t.Errorf("the caption is %q, want %q", (*messages)[0].caption, caption)
			}
			for _, want := range []string{"No service action was performed", tc.want} {
				if !strings.Contains((*messages)[0].text, want) {
					t.Errorf("the report %q does not mention %q", (*messages)[0].text, want)
				}
			}
		})
	}
}

// TestServiceHelperInstallSignalsReadyAfterAPassedPreflight proves the
// legitimate program still flows: the preflight accepts the real executable,
// the ready signal is sent, and the installation runs after authorization —
// the preflight moved the rejection earlier without gating the normal path.
func TestServiceHelperInstallSignalsReadyAfterAPassedPreflight(t *testing.T) {
	replaceSeam(t, &serviceVerifyBudget, 100*time.Millisecond)
	stubConsoleProgram(t)
	running := runningServiceStatus(service.StateRunning, "0.0.0.0:18080")
	manager := &fakeServiceManager{
		statuses:      []service.Status{running},
		installResult: service.InstallOptions{Executable: `C:\portable\hfdesk.exe`, Listen: running.Listen, Paths: running.Paths},
	}
	replaceSeam(t, &newServiceManager, func(service.Scope) (service.Manager, error) {
		return manager, nil
	})
	handover := testHelperHandover(t)
	replaceSeam(t, &prepareHelperHandover, func(ServiceActionOptions) (*helperHandover, error) { return handover, nil })
	messages := recordMessages(t)

	done := make(chan error, 1)
	go func() {
		done <- RunServiceAction(ServiceActionOptions{
			Action: "install", WaitPID: 4321,
			ReadyEvent: "ready", SuccessEvent: "success", InstanceMutex: "mutex",
		})
	}()
	if result, err := windows.WaitForSingleObject(handover.ready, 2_000); err != nil || result != windows.WAIT_OBJECT_0 {
		t.Fatalf("the helper did not signal ready after the preflight passed: result=%#x err=%v", result, err)
	}
	if err := windows.SetEvent(handover.success); err != nil {
		t.Fatalf("signal successful completion: %v", err)
	}
	if err := windows.SetEvent(handover.parent); err != nil {
		t.Fatalf("signal desktop exit: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("the authorized installation failed: %v", err)
	}
	if len(manager.steps) != 1 || manager.steps[0] != "install-start" {
		t.Fatalf("the helper used service steps %v, want one install after authorization", manager.steps)
	}
	if len(*messages) != 1 || (*messages)[0].caption != serviceInstalledCaption {
		t.Fatalf("the helper showed %v, want the report of the installation", *messages)
	}
}
