// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package service

import (
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/windows/svc"

	"github.com/bashrusakh/hfdesk/internal/runtimepaths"
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

func TestSCMArgumentCandidates(t *testing.T) {
	args := []string{ServiceName, "service", "run", "--config", `C:\hfdesk\hfdesk.json`}
	process := []string{`C:\hfdesk\hfdesk.exe`, "service", "run", "--config", `C:\hfdesk\hfdesk.json`}

	candidates := scmArgumentCandidates(args, process)
	want := [][]string{
		{"service", "run", "--config", `C:\hfdesk\hfdesk.json`},
		{"service", "run", "--config", `C:\hfdesk\hfdesk.json`},
		{"service", "run", "--config", `C:\hfdesk\hfdesk.json`},
	}
	if !reflect.DeepEqual(candidates, want) {
		t.Errorf("the candidates = %q, want %q", candidates, want)
	}

	// A process without an argument vector of its own has no candidate
	// besides the ones of the handler.
	if got := scmArgumentCandidates([]string{ServiceName}, nil); len(got) != 2 {
		t.Errorf("the candidates = %q, want the two vectors of the handler", got)
	}
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
