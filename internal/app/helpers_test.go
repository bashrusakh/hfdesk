// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/bashrusakh/hfdesk/internal/server"
)

// useTemporaryConfigDir points the platform's user config dir at a temporary
// directory and returns the directory the server package reads its config
// file from.
//
// The working directory is moved as well, so the server package cannot pick
// up a config file that happens to sit next to the test binary.
func useTemporaryConfigDir(t *testing.T) string {
	t.Helper()

	root := t.TempDir()
	switch runtime.GOOS {
	case "windows":
		t.Setenv("AppData", root)
	case "darwin":
		t.Setenv("HOME", root)
	default:
		t.Setenv("XDG_CONFIG_HOME", root)
	}

	t.Chdir(t.TempDir())
	return server.AppConfigDir()
}

// writeConfigFile writes the config file the server package reads.
func writeConfigFile(t *testing.T, configDir, content string) {
	t.Helper()

	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("create config dir: %v", err)
	}
	path := filepath.Join(configDir, "hfdesk.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config file: %v", err)
	}
}

// unsetEnv removes an environment variable for the duration of the test.
func unsetEnv(t *testing.T, key string) {
	t.Helper()

	value, ok := os.LookupEnv(key)
	if !ok {
		return
	}
	t.Cleanup(func() { os.Setenv(key, value) })
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("unset %s: %v", key, err)
	}
}

// captureOutput runs fn with standard error and standard output replaced by
// pipes and returns what each of them received.
func captureOutput(t *testing.T, fn func()) (stderr string, stdout string) {
	t.Helper()

	errReader, errWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("create stderr pipe: %v", err)
	}
	outReader, outWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("create stdout pipe: %v", err)
	}

	originalStderr, originalStdout := os.Stderr, os.Stdout
	os.Stderr, os.Stdout = errWriter, outWriter
	defer func() {
		os.Stderr, os.Stdout = originalStderr, originalStdout
	}()

	fn()

	errWriter.Close()
	outWriter.Close()

	errBytes, err := io.ReadAll(errReader)
	if err != nil {
		t.Fatalf("read stderr: %v", err)
	}
	outBytes, err := io.ReadAll(outReader)
	if err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	return string(errBytes), string(outBytes)
}
