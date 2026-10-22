// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bashrusakh/hfdesk/internal/runtimepaths"
)

func TestRunProcessInstallsTheRuntimeModelAndStopsOnCancel(t *testing.T) {
	root := t.TempDir()
	model := runtimepaths.Paths{
		ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
		StateDir:   filepath.Join(root, "var"),
		LogDir:     filepath.Join(root, "var", "logs"),
	}
	t.Cleanup(runtimepaths.Reset)

	start := time.Now()
	if err := runServiceWith(t, RunOptions{Paths: model}, RunProcess); err != nil {
		t.Fatalf("RunProcess() = %v, want nil", err)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Errorf("RunProcess() returned after %s, want a prompt shutdown", elapsed)
	}

	// The entry point of the registered process installs the path model of
	// the instance, exactly like the headless host does.
	if got := runtimepaths.Current(); got != model {
		t.Errorf("runtimepaths.Current() = %+v, want %+v", got, model)
	}
}

func TestRunProcessRejectsAnInvalidListenAddress(t *testing.T) {
	root := t.TempDir()
	model := runtimepaths.Paths{
		ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
		StateDir:   filepath.Join(root, "var"),
		LogDir:     filepath.Join(root, "var", "logs"),
	}
	t.Cleanup(runtimepaths.Reset)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The entry point of the registered process reports the error of the
	// host instead of starting a half-configured instance.
	err := RunProcess(ctx, RunOptions{Paths: model, Listen: "not-an-address"})
	if err == nil {
		t.Fatal("RunProcess() = nil, want the error of the host for an invalid listen address")
	}
	if !strings.Contains(err.Error(), "invalid listen address") {
		t.Errorf("RunProcess() = %v, want an invalid listen address error", err)
	}
}

// The registered process never opens a browser and never creates a tray
// icon. The marker guard of TestServiceLayerNeverLaunchesAGUI covers every
// file of the package, so it is not repeated here.

// TestServiceHostShutdownPersistsTheJobListInTheInstanceStateDir drives the
// headless service host through a complete lifecycle in a service context:
// the instance serves on loopback, the context is cancelled and the host
// returns.
//
// A cancelled context is the normal shutdown path of a supervised instance,
// so the host has to return without an error. The persistence path of the
// instance is exercised as well: the job list is committed to the state
// directory of the instance itself and not to a directory of the ambient
// environment.
//
// The helpers of the test are local on purpose: the file of the host tests
// belongs to another package, so this test does not depend on it.
func TestServiceHostShutdownPersistsTheJobListInTheInstanceStateDir(t *testing.T) {
	root := t.TempDir()
	model := runtimepaths.Paths{
		ConfigFile: filepath.Join(root, "etc", "hfdesk.json"),
		StateDir:   filepath.Join(root, "var"),
		LogDir:     filepath.Join(root, "var", "logs"),
	}
	t.Cleanup(runtimepaths.Reset)

	// The cache identity and the credential of the instance are pinned
	// inside the temporary directory of the test, and the environment of
	// the test is restored when it ends.
	t.Setenv("HF_HOME", "")
	t.Setenv("HF_HUB_CACHE", "")
	t.Setenv("HF_TOKEN", "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client := &http.Client{Timeout: 2 * time.Second}

	// healthOK reports whether the instance answers the health endpoint.
	healthOK := func(url string) error {
		resp, err := client.Get(url)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("status %d: %s", resp.StatusCode, body)
		}
		if !strings.Contains(string(body), `"status":"ok"`) {
			return fmt.Errorf("unexpected health payload: %s", body)
		}
		return nil
	}

	// freeLoopbackPort returns a loopback port that was free a moment ago.
	freeLoopbackPort := func() int {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("reserve a port: %v", err)
		}
		defer listener.Close()
		return listener.Addr().(*net.TCPAddr).Port
	}

	var (
		runErr        error
		served        bool
		shutdown      time.Duration
		lastHealthErr error
	)

attempt:
	for attempt := 0; attempt < 3; attempt++ {
		addr := "127.0.0.1:" + strconv.Itoa(freeLoopbackPort())
		url := "http://" + addr + "/api/health"

		attemptCtx, attemptCancel := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() {
			done <- Run(attemptCtx, RunOptions{
				Paths:    model,
				Listen:   addr,
				CacheDir: filepath.Join(root, "cache"),
				HubDir:   filepath.Join(root, "cache", "hub"),
			})
		}()

		// The instance has to serve before the shutdown is requested: a
		// host that never came up must not report a clean shutdown.
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
			err := healthOK(url)
			if err == nil {
				served = true
				break
			}
			lastHealthErr = err
			select {
			case hostErr := <-done:
				// The host returned before it served.
				attemptCancel()
				runErr = hostErr
				if hostErr != nil && strings.Contains(hostErr.Error(), "address already in use") {
					// The port was taken between the reservation and
					// the bind: retry with a new one.
					continue attempt
				}
				t.Fatalf("the host returned before it served: %v (last health error: %v)", hostErr, lastHealthErr)
			case <-time.After(20 * time.Millisecond):
			}
		}

		if !served {
			attemptCancel()
			runErr = <-done
			break
		}

		// The shutdown of a supervised instance is a cancelled context.
		start := time.Now()
		attemptCancel()
		runErr = <-done
		shutdown = time.Since(start)
		break
	}

	if !served {
		t.Fatalf("the instance never served: %v (last health error: %v)", runErr, lastHealthErr)
	}
	if runErr != nil {
		t.Fatalf("Run() = %v, want nil", runErr)
	}
	if shutdown > 30*time.Second {
		t.Errorf("the host returned after %s, want a prompt shutdown", shutdown)
	}

	// The persistence path of the instance ran: the job list is committed
	// to the state directory of the instance itself.
	stateFile := model.JobsStateFile()
	data, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatalf("read %s: %v", stateFile, err)
	}
	var persisted struct {
		Jobs []json.RawMessage `json:"jobs"`
	}
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatalf("parse %s: %v", stateFile, err)
	}
	if persisted.Jobs == nil {
		t.Errorf("%s = %s, want a job list document", stateFile, data)
	}
}
