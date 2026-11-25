// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/bashrusakh/hfdesk/internal/runtimepaths"
	"github.com/bashrusakh/hfdesk/internal/server"
	"github.com/bashrusakh/hfdesk/internal/serviceview"
)

// hostServiceManagerStub is the manager of a test. Only Status is reachable:
// the runtime surface reads the registration and nothing else.
type hostServiceManagerStub struct {
	Manager
	status Status
	err    error
	// deadline records the budget of the read, so a test can prove that the
	// surface never waits for a supervisor without a bound.
	deadline    time.Time
	hasDeadline bool
}

func (m *hostServiceManagerStub) Status(ctx context.Context) (Status, error) {
	m.deadline, m.hasDeadline = ctx.Deadline()
	return m.status, m.err
}

// stubHostServiceManager replaces the registration manager factory for the
// duration of the test and clears the runtime surface afterwards, so a test
// never leaves a state read behind for the next one.
func stubHostServiceManager(t *testing.T, open func() (Manager, error)) {
	t.Helper()
	previous := openHostServiceManager
	openHostServiceManager = open
	t.Cleanup(func() {
		openHostServiceManager = previous
		server.SetRuntimeService(nil, nil)
	})
}

// TestReadServiceRegistrationState proves the contract of the registration
// read the service host exposes on its runtime page: the read returns the
// shared classified view of the manager's status, classifies an empty state
// as unknown, and reports every failure as an error. The runtime status turns
// both an empty view and an error into the unknown view, so the page never
// claims a state no read reported.
func TestReadServiceRegistrationState(t *testing.T) {
	readFailure := errors.New("the supervisor did not answer")
	managerFailure := errors.New("the registration cannot be read")

	tests := []struct {
		name    string
		open    func() (Manager, error)
		want    serviceview.Snapshot
		wantErr error
	}{
		{
			name: "the running word of the registration",
			open: func() (Manager, error) {
				return &hostServiceManagerStub{status: Status{State: StateRunning}}, nil
			},
			want: serviceview.From(string(StateRunning), false, false),
		},
		{
			name: "the stopped word of the registration",
			open: func() (Manager, error) {
				return &hostServiceManagerStub{status: Status{State: StateStopped}}, nil
			},
			want: serviceview.From(string(StateStopped), false, false),
		},
		{
			name: "the running fact of the status travels with it",
			open: func() (Manager, error) {
				return &hostServiceManagerStub{status: Status{State: StateBroken, Running: true}}, nil
			},
			want: serviceview.From(string(StateBroken), true, false),
		},
		{
			name: "an empty state classifies as unknown",
			open: func() (Manager, error) {
				return &hostServiceManagerStub{}, nil
			},
			want: serviceview.From("", false, false),
		},
		{
			name: "a failing read is an error",
			open: func() (Manager, error) {
				return &hostServiceManagerStub{err: managerFailure}, nil
			},
			wantErr: managerFailure,
		},
		{
			name: "a manager that cannot be opened is an error",
			open: func() (Manager, error) {
				return nil, readFailure
			},
			wantErr: readFailure,
		},
		{
			name:    "a host without a manager is an error",
			open:    nil,
			wantErr: errNoHostServiceSurface,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stubHostServiceManager(t, tc.open)

			got, err := readServiceRegistrationState()
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("readServiceRegistrationState() error = %v, want %v", err, tc.wantErr)
				}
				if got != (serviceview.Snapshot{}) {
					t.Errorf("readServiceRegistrationState() = %+v with an error, want an empty view", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("readServiceRegistrationState() error = %v, want nil", err)
			}
			if got != tc.want {
				t.Errorf("readServiceRegistrationState() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestReadServiceRegistrationStateBoundsTheRead proves that the read answers
// the Logs page within the budget: a supervisor that never answers must not
// hold the page.
func TestReadServiceRegistrationStateBoundsTheRead(t *testing.T) {
	manager := &hostServiceManagerStub{status: Status{State: StateRunning}}
	stubHostServiceManager(t, func() (Manager, error) { return manager, nil })

	if _, err := readServiceRegistrationState(); err != nil {
		t.Fatalf("readServiceRegistrationState() error = %v, want nil", err)
	}
	if !manager.hasDeadline {
		t.Fatal("the read has no deadline, want the runtime surface budget")
	}
	remaining := time.Until(manager.deadline)
	if remaining <= 0 || remaining > runtimeServiceStateTimeout {
		t.Errorf("the read budget is %s, want at most %s", remaining, runtimeServiceStateTimeout)
	}
}

// TestRunRegistersTheServiceStateOnItsRuntimePage proves the wiring of the
// fix: the page the instance itself serves asks GET /api/runtime of that
// instance, so only a surface the service host registered can reach it.
// Without the registration the page reports the unknown state although the
// service that serves it is installed.
//
// The manager factory is stubbed, so the test reads no registration of the
// machine it runs on and needs no supervisor of its own.
func TestRunRegistersTheServiceStateOnItsRuntimePage(t *testing.T) {
	stubHostServiceManager(t, func() (Manager, error) {
		return &hostServiceManagerStub{status: Status{State: StateRunning}}, nil
	})

	root := t.TempDir()
	model := runtimepaths.Paths{
		ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
		StateDir:   filepath.Join(root, "var"),
		LogDir:     filepath.Join(root, "var", "logs"),
	}
	t.Cleanup(runtimepaths.Reset)

	addr := "127.0.0.1:" + strconv.Itoa(freePort(t))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, RunOptions{Paths: model, Listen: addr})
	}()
	stop := func() {
		cancel()
		err := <-done
		assertNoError(t, err, "Run() = %v, want nil after the cancelled context", err)
	}

	client := &http.Client{Timeout: 2 * time.Second}
	url := "http://" + addr + "/api/runtime"
	var lastErr error
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		body, status, err := get(client, url)
		switch {
		case err != nil:
			lastErr = err
		case status != http.StatusOK:
			lastErr = fmt.Errorf("status %d: %s", status, body)
		default:
			var payload struct {
				ServiceState string `json:"serviceState"`
				Service      struct {
					Class string `json:"class"`
					Label string `json:"label"`
				} `json:"service"`
			}
			if err := json.Unmarshal([]byte(body), &payload); err != nil {
				stop()
				t.Fatalf("decode the runtime status %q: %v", body, err)
			}
			if payload.ServiceState != string(StateRunning) {
				stop()
				t.Fatalf("serviceState = %q, want %q", payload.ServiceState, StateRunning)
			}
			if payload.Service.Class != string(serviceview.ClassPresent) || payload.Service.Label != "Service: running" {
				stop()
				t.Fatalf("service = %+v, want the present view with the running line", payload.Service)
			}
			stop()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}

	stop()
	t.Fatalf("the runtime status never answered: %v", lastErr)
}
