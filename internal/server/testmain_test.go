// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bashrusakh/hfdesk/internal/runtimecoordination"
)

var serverTestTempRoot string

func TestMain(m *testing.M) {
	goModuleCache, _ := exec.Command("go", "env", "GOMODCACHE").Output()
	goBuildCache, _ := exec.Command("go", "env", "GOCACHE").Output()
	root, err := os.MkdirTemp("", "hfdesk-server-tests-")
	if err != nil {
		panic(err)
	}
	for key, value := range map[string]string{
		"HOME":                         root,
		"TMPDIR":                       root,
		"TMP":                          root,
		"TEMP":                         root,
		"XDG_CACHE_HOME":               filepath.Join(root, "cache"),
		"XDG_CONFIG_HOME":              filepath.Join(root, "config"),
		"XDG_DATA_HOME":                filepath.Join(root, "data"),
		"XDG_STATE_HOME":               filepath.Join(root, "state"),
		"XDG_RUNTIME_DIR":              filepath.Join(root, "runtime"),
		"APPDATA":                      filepath.Join(root, "appdata"),
		"LOCALAPPDATA":                 filepath.Join(root, "localappdata"),
		"USERPROFILE":                  root,
		"HFDESK_TEST_COORDINATION_DIR": filepath.Join(root, "coordination-runtime"),
		"GOMODCACHE":                   strings.TrimSpace(string(goModuleCache)),
		"GOCACHE":                      strings.TrimSpace(string(goBuildCache)),
	} {
		if err := os.Setenv(key, value); err != nil {
			_ = os.RemoveAll(root)
			panic(err)
		}
	}
	for _, key := range []string{"HF_HUB_CACHE", "HF_HOME", "HF_TOKEN"} {
		if err := os.Unsetenv(key); err != nil {
			_ = os.RemoveAll(root)
			panic(err)
		}
	}
	serverTestTempRoot = root
	coordinationBase := filepath.Join(root, "coordination-runtime")
	runtimecoordination.SetBaseDirProvider(func() (string, error) { return coordinationBase, nil })

	code := m.Run()
	if err := os.RemoveAll(root); err != nil {
		_, _ = os.Stderr.WriteString("remove test sandbox: " + err.Error() + "\n")
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

func newTestJobManager(t *testing.T, cfg Config, hub *WSHub) *JobManager {
	t.Helper()
	statePath := filepath.Join(t.TempDir(), "jobs_state.json")
	m := newJobManagerWithStatePath(cfg, hub, statePath)
	registerTestJobManagerCleanup(t, m)
	return m
}

func registerTestJobManagerCleanup(t *testing.T, m *JobManager) {
	t.Helper()
	t.Cleanup(func() {
		select {
		case <-m.closeDone:
			return
		default:
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := m.Close(ctx); err != nil {
			t.Errorf("close test job manager: %v", err)
			<-m.closeDone
		}
	})
}
