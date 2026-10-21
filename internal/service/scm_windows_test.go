// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package service

import (
	"net"
	"net/http"
	"path/filepath"
	"reflect"
	"strconv"
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

// TestSCMProbeAddressMapsTheWildcardBindOntoTheLoopback checks the address
// the readiness probe dials.
//
// A wildcard bind has no address a client can connect to: 0.0.0.0 and ::
// are the addresses every interface answers on, and a connect() to them
// fails with WSAEADDRNOTAVAIL. The probe therefore dials the loopback
// address of the same family, and every other host is used unchanged.
func TestSCMProbeAddressMapsTheWildcardBindOntoTheLoopback(t *testing.T) {
	cases := []struct {
		name   string
		listen string
		want   string
	}{
		{"the IPv4 wildcard", "0.0.0.0:8080", "127.0.0.1:8080"},
		{"the IPv6 wildcard", "[::]:8080", "[::1]:8080"},
		{"an empty host", ":8080", "127.0.0.1:8080"},
		{"an IPv4 loopback", "127.0.0.1:8080", "127.0.0.1:8080"},
		{"an IPv6 loopback", "[::1]:8080", "[::1]:8080"},
		{"an address of the network", "192.168.1.5:8080", "192.168.1.5:8080"},
		{"a host name", "localhost:8080", "localhost:8080"},
		{"no address at all", "", DefaultListen},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := scmProbeAddress(tc.listen); got != tc.want {
				t.Errorf("scmProbeAddress(%q) = %q, want %q", tc.listen, got, tc.want)
			}
		})
	}
}

// TestProbeServiceReadyReachesAWildcardListener checks that the readiness
// probe reaches a server that binds every interface of the machine.
//
// The server answers with the status of the basic authentication of the
// instance: the probe accepts every answer, because the point of the probe
// is that an HTTP server listens on the address and not that it answers
// with a particular code.
func TestProbeServiceReadyReachesAWildcardListener(t *testing.T) {
	cases := []struct {
		name string
		bind string
		host string
	}{
		{"the IPv4 wildcard", "0.0.0.0:0", "0.0.0.0"},
		{"the IPv6 wildcard", "[::]:0", "::"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			listener, err := net.Listen("tcp", tc.bind)
			if err != nil {
				t.Skipf("the address %q cannot be bound: %v", tc.bind, err)
			}
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
			})}
			go func() {
				_ = server.Serve(listener)
			}()
			t.Cleanup(func() {
				_ = server.Close()
			})

			port := listener.Addr().(*net.TCPAddr).Port
			listen := net.JoinHostPort(tc.host, strconv.Itoa(port))
			if err := probeServiceReady(listen); err != nil {
				t.Errorf("probeServiceReady(%q) = %v, want nil for a server that binds %q", listen, err, tc.bind)
			}
		})
	}
}
