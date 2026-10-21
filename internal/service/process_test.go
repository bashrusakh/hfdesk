// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"path/filepath"
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
