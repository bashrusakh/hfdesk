// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package service

import (
	"context"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/windows/svc"

	"github.com/bashrusakh/hfdesk/internal/runtimepaths"
	"github.com/bashrusakh/hfdesk/internal/server"
)

// TestSCMHandlerReportsTheLifecycle drives the handler of the service
// without the service control manager.
//
// The test binds a free loopback port, so the instance never touches a
// real registration and never touches the machine it runs on.
func TestSCMHandlerReportsTheLifecycle(t *testing.T) {
	root := t.TempDir()
	model := runtimepaths.Paths{
		ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
		StateDir:   filepath.Join(root, "var"),
		LogDir:     filepath.Join(root, "var", "logs"),
	}
	t.Cleanup(runtimepaths.Reset)

	addr := "127.0.0.1:" + strconv.Itoa(freePort(t))
	args := []string{
		ServiceName,
		"service", "run",
		"--config", model.ConfigFile,
		"--state-dir", model.StateDir,
		"--log-dir", model.LogDir,
		"--listen", addr,
	}

	handler := newSCMHandler(RunOptions{})
	handler.readinessTimeout = 30 * time.Second

	changes := make(chan svc.Status, 16)
	requests := make(chan svc.ChangeRequest)

	type result struct {
		specific bool
		code     uint32
	}
	done := make(chan result, 1)
	go func() {
		specific, code := handler.Execute(args, requests, changes)
		done <- result{specific: specific, code: code}
		close(changes)
	}()

	var states []svc.State
	for {
		status, ok := <-changes
		if !ok {
			t.Fatalf("the handler ended before it reported the running state: %v", states)
		}
		states = append(states, status.State)
		if status.State != svc.Running {
			continue
		}
		if status.Accepts&svc.AcceptStop == 0 {
			t.Errorf("the running status accepts %v, want the stop control", status.Accepts)
		}
		if status.Accepts&svc.AcceptShutdown == 0 {
			t.Errorf("the running status accepts %v, want the shutdown control", status.Accepts)
		}
		break
	}

	// The manager asks the instance to stop.
	requests <- svc.ChangeRequest{Cmd: svc.Stop}

	for status := range changes {
		states = append(states, status.State)
	}

	res := <-done
	if res.code != 0 {
		t.Errorf("the exit code = %d, want 0 for a clean stop", res.code)
	}

	want := []svc.State{svc.StartPending, svc.Running, svc.StopPending, svc.Stopped}
	if !reflect.DeepEqual(states, want) {
		t.Errorf("the reported states = %v, want %v", states, want)
	}
}

// TestSCMHandlerWaitsForTheHostWhenTheReadinessFails checks that the
// handler waits for the host of the instance before it reports the instance
// as stopped.
//
// The readiness of the instance fails while the host keeps running: the
// handler has to cancel the instance and wait for the host, so the drain of
// the HTTP server and the write back of the job manager are not cut short.
// The instance is reported as stopped only after the host ended.
//
// The wait is the one of the normal stop path, so it is bounded by the
// shared budget of the service layer.
func TestSCMHandlerWaitsForTheHostWhenTheReadinessFails(t *testing.T) {
	root := t.TempDir()
	model := runtimepaths.Paths{
		ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
		StateDir:   filepath.Join(root, "var"),
		LogDir:     filepath.Join(root, "var", "logs"),
	}
	t.Cleanup(runtimepaths.Reset)

	// The address of the instance is occupied by something that is not the
	// HTTP server of the instance: the readiness of the instance fails and
	// it fails for sure, because the port belongs to the test.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() = %v, want nil", err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	addr := listener.Addr().String()
	args := []string{
		ServiceName,
		"service", "run",
		"--config", model.ConfigFile,
		"--state-dir", model.StateDir,
		"--log-dir", model.LogDir,
		"--listen", addr,
	}

	handler := newSCMHandler(RunOptions{})
	handler.readinessTimeout = 50 * time.Millisecond
	handler.readinessInterval = 10 * time.Millisecond

	// The host of the instance: it runs until the context of the instance
	// is cancelled, and it takes its time to finish, the way a server that
	// drains its connections does.
	started := make(chan struct{})
	cancelled := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseHost := func() { releaseOnce.Do(func() { close(release) }) }
	// The host never outlives the test, not even when the test fails
	// before it releases the host.
	t.Cleanup(releaseHost)
	handler.run = func(ctx context.Context, opts RunOptions) error {
		close(started)
		<-ctx.Done()
		close(cancelled)
		<-release
		return nil
	}

	changes := make(chan svc.Status, 16)
	requests := make(chan svc.ChangeRequest)

	type result struct {
		specific bool
		code     uint32
	}
	done := make(chan result, 1)
	go func() {
		specific, code := handler.Execute(args, requests, changes)
		done <- result{specific: specific, code: code}
		close(changes)
	}()

	// Every state the handler reports is recorded, so the states of the
	// instance are the ones the test asserts on.
	var states []svc.State

	// The host of the instance runs.
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the host of the instance did not start")
	}

	// The instance reports that it starts.
	select {
	case status := <-changes:
		states = append(states, status.State)
		if status.State != svc.StartPending {
			t.Fatalf("the first reported state = %v, want %v", status.State, svc.StartPending)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the instance did not report that it starts")
	}

	// The readiness of the instance fails: the handler cancels the
	// instance and waits for the host.
	select {
	case <-cancelled:
	case <-time.After(10 * time.Second):
		t.Fatal("the instance was not cancelled after the failed readiness")
	}

	// The instance is not reported as stopped while the host still runs.
	select {
	case status := <-changes:
		states = append(states, status.State)
		t.Fatalf("the handler reported the state %v while the host of the instance was still running", status.State)
	case <-time.After(200 * time.Millisecond):
	}

	// The host ends: the instance is reported as stopped afterwards.
	releaseHost()

	for status := range changes {
		states = append(states, status.State)
	}

	res := <-done
	if res.code == 0 {
		t.Errorf("the exit code = %d, want a non-zero code for a failed start", res.code)
	}
	want := []svc.State{svc.StartPending, svc.Stopped}
	if !reflect.DeepEqual(states, want) {
		t.Errorf("the reported states = %v, want %v", states, want)
	}
}

// TestSCMHandlerFailsWithoutAUsableArgumentVector drives the handler with a
// process that carries no argument vector at all.
//
// The instance must report the failure and end with a non-zero exit code,
// so the service control manager marks the service as failed instead of
// leaving a service that does nothing.
func TestSCMHandlerFailsWithoutAUsableArgumentVector(t *testing.T) {
	handler := newSCMHandler(RunOptions{})

	changes := make(chan svc.Status, 8)
	requests := make(chan svc.ChangeRequest)

	type result struct {
		specific bool
		code     uint32
	}
	done := make(chan result, 1)
	go func() {
		specific, code := handler.Execute([]string{ServiceName}, requests, changes)
		done <- result{specific: specific, code: code}
		close(changes)
	}()

	var states []svc.State
	for status := range changes {
		states = append(states, status.State)
	}
	res := <-done

	if res.code == 0 {
		t.Errorf("the exit code = %d, want a non-zero code for a failed start", res.code)
	}
	want := []svc.State{svc.StartPending, svc.Stopped}
	if !reflect.DeepEqual(states, want) {
		t.Errorf("the reported states = %v, want %v", states, want)
	}
}

// TestSCMShutdownWaitCoversTheShutdownOfTheServer checks that the bounded
// wait of the service handler covers the graceful shutdown of the server.
//
// The server publishes the budget of its own orderly shutdown: the drain of
// the HTTP server and the close of the job manager, each with its own
// budget. The handler has to wait at least the total of those budgets, or
// the service control manager gives up while the instance is still saving
// its state.
//
// The wait is the shared budget of the service layer, so the platforms
// cannot drift apart: a number of its own in the handler would be a second
// copy of the same decision.
func TestSCMShutdownWaitCoversTheShutdownOfTheServer(t *testing.T) {
	if ShutdownBudget < server.GracefulShutdownTimeout {
		t.Errorf("the shared budget of the service layer = %s, want at least %s", ShutdownBudget, server.GracefulShutdownTimeout)
	}

	handler := newSCMHandler(RunOptions{})
	wait := handler.shutdownTimeout
	if wait < server.GracefulShutdownTimeout {
		t.Errorf("the bounded wait of the handler = %s, want at least %s: the wait of the supervisor has to cover the graceful shutdown of the server", wait, server.GracefulShutdownTimeout)
	}
	if wait != ShutdownBudget {
		t.Errorf("the bounded wait of the handler = %s, want the shared budget %s", wait, ShutdownBudget)
	}
}

// TestSCMArgumentCandidates checks the argument vectors the handler of the
// service tries to read the options of the instance from.
//
// The candidates are tried in the order they are assembled: the start
// parameters of the start request first, then the whole vector of the
// handler and last the command line of the process, which is the registered
// command line of the instance.
//
// The whole vector of the handler starts with the name of the service or
// with the path of the program, so the parser reads the options from it
// only when the handler carries the command tokens of the invocation
// itself.
func TestSCMArgumentCandidates(t *testing.T) {
	args := []string{ServiceName, "service", "run", "--config", `C:\hfdesk\hfdesk.json`}
	process := []string{`C:\hfdesk\hfdesk.exe`, "service", "run", "--config", `C:\hfdesk\hfdesk.json`}

	candidates := scmArgumentCandidates(args, process)
	want := [][]string{
		// The start parameters of the start request.
		{"service", "run", "--config", `C:\hfdesk\hfdesk.json`},
		// The whole vector of the handler.
		{ServiceName, "service", "run", "--config", `C:\hfdesk\hfdesk.json`},
		// The command line of the process.
		{"service", "run", "--config", `C:\hfdesk\hfdesk.json`},
	}
	if !reflect.DeepEqual(candidates, want) {
		t.Errorf("the candidates = %q, want %q", candidates, want)
	}

	// A process without an argument vector of its own has no candidate
	// besides the one of the handler: there are no start parameters
	// without a second element and no command line of the process.
	if got := scmArgumentCandidates([]string{ServiceName}, nil); len(got) != 1 {
		t.Errorf("the candidates = %q, want the one vector of the handler", got)
	}
}

// TestSCMArgumentCandidatesCarryTheOptionsOfTheInstance checks that the
// handler of the service finds the options of the instance in one of the
// candidates.
//
// The instance runs with the options of its registration, no matter how the
// process was started: the service control manager hands the handler the
// argument vector of the service process, and the command line of the
// process is the registered command line. One of the candidates has to carry
// the options, or the instance would run without them.
func TestSCMArgumentCandidatesCarryTheOptionsOfTheInstance(t *testing.T) {
	// The argument vector of the service process: the path of the
	// program and the arguments of the registration.
	process := []string{
		`C:\hfdesk\hfdesk.exe`,
		"service", "run",
		"--config", `C:\hfdesk\hfdesk.json`,
		"--state-dir", `C:\hfdesk\var`,
		"--log-dir", `C:\hfdesk\logs`,
	}
	// The vector the handler receives: the name of the service and the
	// arguments of the process.
	args := append([]string{ServiceName}, process[1:]...)

	candidates := scmArgumentCandidates(args, process)
	for _, candidate := range candidates {
		if _, err := ParseRunArgs(candidate); err == nil {
			// One of the candidates carries the options of the
			// registration: the handler finds them.
			return
		}
	}
	t.Errorf("the candidates = %q, want one that carries the options of the instance", candidates)
}

func TestResolveOptionsFallsBackToTheHandedOverOptions(t *testing.T) {
	model := runtimepaths.Paths{
		ConfigFile: filepath.Join("C:\\ProgramData", "HFDesk", "hfdesk.json"),
		StateDir:   filepath.Join("C:\\ProgramData", "HFDesk"),
		LogDir:     filepath.Join("C:\\ProgramData", "HFDesk", "logs"),
	}
	handler := newSCMHandler(RunOptions{Paths: model, Listen: "127.0.0.1:8080"})

	opts, err := handler.resolveOptions([]string{ServiceName})
	if err != nil {
		t.Fatalf("resolveOptions() = %v, want nil", err)
	}
	if opts.Paths != model {
		t.Errorf("the paths = %+v, want %+v", opts.Paths, model)
	}
	if opts.Listen != "127.0.0.1:8080" {
		t.Errorf("the listen address = %q, want %q", opts.Listen, "127.0.0.1:8080")
	}
}

func TestResolveOptionsReadsTheRegisteredArguments(t *testing.T) {
	root := filepath.Join("C:\\ProgramData", "HFDesk")
	model := runtimepaths.Paths{
		ConfigFile: filepath.Join(root, "hfdesk.json"),
		StateDir:   root,
		LogDir:     filepath.Join(root, "logs"),
	}
	handler := newSCMHandler(RunOptions{})

	opts, err := handler.resolveOptions([]string{
		ServiceName,
		"service", "run",
		"--config", model.ConfigFile,
		"--state-dir", model.StateDir,
		"--log-dir", model.LogDir,
		"--listen", "127.0.0.1:9090",
	})
	if err != nil {
		t.Fatalf("resolveOptions() = %v, want nil", err)
	}
	if opts.Paths != model {
		t.Errorf("the paths = %+v, want %+v", opts.Paths, model)
	}
	if opts.Listen != "127.0.0.1:9090" {
		t.Errorf("the listen address = %q, want %q", opts.Listen, "127.0.0.1:9090")
	}
}

// scmTestServe starts an HTTP server of the test on the given address.
//
// The server answers the readiness probe of the handler, so the handler
// reports the instance as running without the real host of the service. The
// server is closed when the test ends.
func scmTestServe(t *testing.T, addr string) {
	t.Helper()
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("Listen(%q) = %v, want nil", addr, err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})}
	go func() {
		_ = server.Serve(listener)
	}()
	t.Cleanup(func() {
		_ = server.Close()
	})
}

// scmTestHandlerOptions returns the argument vector of an instance of the test
// that listens on the given address.
func scmTestHandlerOptions(t *testing.T, addr string) []string {
	t.Helper()
	root := t.TempDir()
	model := runtimepaths.Paths{
		ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
		StateDir:   filepath.Join(root, "var"),
		LogDir:     filepath.Join(root, "var", "logs"),
	}
	t.Cleanup(runtimepaths.Reset)
	return []string{
		ServiceName,
		"service", "run",
		"--config", model.ConfigFile,
		"--state-dir", model.StateDir,
		"--log-dir", model.LogDir,
		"--listen", addr,
	}
}

// TestSCMHandlerReportsACleanStopWhenTheShutdownFails checks the exit code of a
// stop the manager requested while the orderly shutdown reports an error.
//
// A stop the user asked for is a clean end: the instance reports the stopped
// state with the exit code 0 and returns 0, so the recovery of the platform,
// which also keys on a non-zero exit code, never restarts a service the user
// stopped. The error of the shutdown is written to the event log instead.
func TestSCMHandlerReportsACleanStopWhenTheShutdownFails(t *testing.T) {
	addr := "127.0.0.1:" + strconv.Itoa(freePort(t))
	scmTestServe(t, addr)

	handler := newSCMHandler(RunOptions{})
	handler.readinessTimeout = 5 * time.Second
	handler.run = func(ctx context.Context, _ RunOptions) error {
		<-ctx.Done()
		return errors.New("the orderly shutdown of the test reported an error")
	}

	changes := make(chan svc.Status, 16)
	requests := make(chan svc.ChangeRequest)
	args := scmTestHandlerOptions(t, addr)
	done := make(chan uint32, 1)
	go func() {
		_, code := handler.Execute(args, requests, changes)
		done <- code
		close(changes)
	}()

	// The manager asks the instance to stop once it runs: the request is
	// sent after the running state was reported, so the stop path of a
	// running instance is the one the test drives.
	var statuses []svc.Status
	for status := range changes {
		statuses = append(statuses, status)
		if status.State == svc.Running {
			requests <- svc.ChangeRequest{Cmd: svc.Stop}
		}
	}
	if code := <-done; code != scmExitCodeOK {
		t.Errorf("the exit code = %d, want %d for a stop the manager requested", code, scmExitCodeOK)
	}
	stopped := scmTestLastStatus(t, statuses, svc.Stopped)
	if stopped.Win32ExitCode != scmExitCodeOK {
		t.Errorf("the exit code of the stopped status = %d, want %d: a stop the manager requested is never a recovery signal", stopped.Win32ExitCode, scmExitCodeOK)
	}
}

// TestSCMHandlerReportsAFailureExitCodeWhenTheStartFails checks the exit code of
// an instance whose HTTP server never answered.
//
// The failure is reported with the stopped state as well as returned: the
// recovery of the platform that also covers a non-crash failure keys on the
// code, and a failed start has to reach it even when nothing but the reported
// status is read.
func TestSCMHandlerReportsAFailureExitCodeWhenTheStartFails(t *testing.T) {
	// The address of the instance is occupied by something that is not the
	// HTTP server of the instance: the readiness fails for sure, because
	// the port belongs to the test.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() = %v, want nil", err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	addr := listener.Addr().String()

	handler := newSCMHandler(RunOptions{})
	handler.readinessTimeout = 50 * time.Millisecond
	handler.readinessInterval = 10 * time.Millisecond
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	handler.run = func(ctx context.Context, _ RunOptions) error {
		<-ctx.Done()
		<-release
		return nil
	}

	requests := make(chan svc.ChangeRequest, 1)
	changes := make(chan svc.Status, 16)
	args := scmTestHandlerOptions(t, addr)
	done := make(chan uint32, 1)
	go func() {
		_, code := handler.Execute(args, requests, changes)
		done <- code
		close(changes)
	}()

	// The handler waits for the host before it reports the stopped state,
	// so the host is released once the start failed.
	var statuses []svc.Status
	for status := range changes {
		statuses = append(statuses, status)
		unblock()
	}
	if code := <-done; code != scmExitCodeFailure {
		t.Errorf("the exit code = %d, want %d for a failed start", code, scmExitCodeFailure)
	}
	stopped := scmTestLastStatus(t, statuses, svc.Stopped)
	if stopped.Win32ExitCode != scmExitCodeFailure {
		t.Errorf("the exit code of the stopped status = %d, want %d for a failed start", stopped.Win32ExitCode, scmExitCodeFailure)
	}
}

// TestSCMHandlerReportsAFailureExitCodeWhenTheHostFails checks the exit code of
// an instance whose host ended with an error on its own.
//
// The failure is the end of the instance and not a stop the manager asked for,
// so it is reported with the failure code and the recovery of the platform may
// restart the instance.
func TestSCMHandlerReportsAFailureExitCodeWhenTheHostFails(t *testing.T) {
	addr := "127.0.0.1:" + strconv.Itoa(freePort(t))
	scmTestServe(t, addr)

	fail := make(chan struct{})
	var failOnce sync.Once
	failHost := func() { failOnce.Do(func() { close(fail) }) }
	// The host never outlives the test, not even when the test fails
	// before it releases the host.
	t.Cleanup(failHost)
	handler := newSCMHandler(RunOptions{})
	handler.readinessTimeout = 5 * time.Second
	handler.run = func(ctx context.Context, _ RunOptions) error {
		select {
		case <-fail:
		case <-ctx.Done():
		}
		return errors.New("the host of the test failed")
	}

	changes := make(chan svc.Status, 16)
	requests := make(chan svc.ChangeRequest)
	args := scmTestHandlerOptions(t, addr)
	done := make(chan uint32, 1)
	go func() {
		_, code := handler.Execute(args, requests, changes)
		done <- code
		close(changes)
	}()

	// The instance runs before its host fails: the failure is observed on
	// the path of an instance that was serving.
	var statuses []svc.Status
	for status := range changes {
		statuses = append(statuses, status)
		if status.State == svc.Running {
			failHost()
		}
	}
	if code := <-done; code != scmExitCodeFailure {
		t.Errorf("the exit code = %d, want %d for a host that failed", code, scmExitCodeFailure)
	}
	stopped := scmTestLastStatus(t, statuses, svc.Stopped)
	if stopped.Win32ExitCode != scmExitCodeFailure {
		t.Errorf("the exit code of the stopped status = %d, want %d for a host that failed", stopped.Win32ExitCode, scmExitCodeFailure)
	}
}

// scmTestLastStatus returns the last reported status of the given state, and
// fails the test when the handler never reported it.
func scmTestLastStatus(t *testing.T, statuses []svc.Status, state svc.State) svc.Status {
	t.Helper()
	for i := len(statuses) - 1; i >= 0; i-- {
		if statuses[i].State == state {
			return statuses[i]
		}
	}
	t.Fatalf("the handler never reported the state %v: %v", state, statuses)
	return svc.Status{}
}
