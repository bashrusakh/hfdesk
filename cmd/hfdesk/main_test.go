// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// hostBinary is the console host the tests of this package run.
//
// The host is run as a process, because the behavior under test is the
// behavior of the process: the exit code, the standard error output and the
// content of the log file.
var hostBinary string

// hostBuildErr holds the reason why the console host could not be built.
//
// The tests skip themselves when it is set: a host that cannot be built
// cannot prove anything about the reporting of a failure.
var hostBuildErr error

// TestMain builds the console host once for the tests of this package and
// removes it afterwards.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "hfdesk-host-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "prepare the console host: %v\n", err)
		os.Exit(1)
	}

	name := "hfdesk"
	if runtime.GOOS == "windows" {
		name = "hfdesk.exe"
	}
	hostBinary = filepath.Join(dir, name)

	build := exec.Command("go", "build", "-o", hostBinary, ".")
	if out, err := build.CombinedOutput(); err != nil {
		hostBuildErr = fmt.Errorf("build the console host: %v\n%s", err, out)
	}

	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// TestStartupFailureIsRecordedInTheLogFile proves that a failure of the
// startup is recorded in the log file.
//
// The logging pipeline is closed when the startup ends, and a closed file
// sink drops the records that are emitted afterwards. A failure that is
// reported after the close is therefore missing from the log file the user
// is asked to attach to a bug report.
//
// The failure is a rejected configuration, so it happens before the server
// is built.
func TestStartupFailureIsRecordedInTheLogFile(t *testing.T) {
	requireHost(t)

	dir := t.TempDir()
	logPath := filepath.Join(dir, "hfdesk.log")

	result := runHost(t, dir, "--log-file", logPath, "--stall-timeout", "not-a-duration")

	if result.exitCode != 1 {
		t.Fatalf("the exit code is %d, want 1; standard error: %s", result.exitCode, result.stderr)
	}
	if result.stdout != "" {
		t.Errorf("standard output is not empty: %q", result.stdout)
	}
	if !strings.Contains(result.stderr, "invalid --stall-timeout") {
		t.Errorf("standard error does not report the failure: %q", result.stderr)
	}

	logged := readLogFile(t, logPath)
	if !strings.Contains(logged, "invalid --stall-timeout") {
		t.Fatalf("the log file does not contain the failure: %q", logged)
	}
}

// TestStartupPortClashIsRecordedInTheLogFile proves that a failure of the
// server startup is recorded in the log file.
//
// The failure happens after the configuration was accepted, so it is another
// stage of the startup than the rejected configuration of
// TestStartupFailureIsRecordedInTheLogFile.
func TestStartupPortClashIsRecordedInTheLogFile(t *testing.T) {
	requireHost(t)

	// The port is held by this process, so the host cannot bind it.
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("hold a port: %v", err)
	}
	defer listener.Close()

	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("the address of the listener is %T, want a TCP address", listener.Addr())
	}

	dir := t.TempDir()
	logPath := filepath.Join(dir, "hfdesk.log")

	result := runHost(t, dir,
		"--log-file", logPath,
		"--port", strconv.Itoa(address.Port),
		"--cache-dir", filepath.Join(dir, "cache"),
	)

	if result.exitCode != 1 {
		t.Fatalf("the exit code is %d, want 1; standard error: %s", result.exitCode, result.stderr)
	}
	if result.stdout != "" {
		t.Errorf("standard output is not empty: %q", result.stdout)
	}

	bind := "listen on 0.0.0.0:" + strconv.Itoa(address.Port)
	if !strings.Contains(result.stderr, bind) {
		t.Errorf("standard error does not report the failed bind: %q", result.stderr)
	}

	logged := readLogFile(t, logPath)
	if !strings.Contains(logged, bind) {
		t.Fatalf("the log file does not contain the failure: %q", logged)
	}
}

// hostResult is the observable result of one run of the console host.
type hostResult struct {
	exitCode int
	stdout   string
	stderr   string
}

// runHost runs the console host with the given arguments in the given
// directory.
//
// The run uses an isolated environment: the home directory and the working
// directory are the given directory, so the run neither reads the
// configuration of the installation of the developer nor writes into it.
func runHost(t *testing.T, dir string, args ...string) hostResult {
	t.Helper()

	cmd := exec.Command(hostBinary, args...)
	cmd.Dir = dir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Env = isolatedEnv(dir)

	err := cmd.Run()
	result := hostResult{stdout: stdout.String(), stderr: stderr.String()}
	if err == nil {
		return result
	}

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("run the console host: %v", err)
	}
	result.exitCode = exitErr.ExitCode()
	return result
}

// requireHost skips the test when the console host could not be built.
func requireHost(t *testing.T) {
	t.Helper()

	if hostBuildErr != nil {
		t.Skipf("the console host could not be built: %v", hostBuildErr)
	}
}

// readLogFile reads the content of the log file of a host run.
func readLogFile(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the log file %s: %v", path, err)
	}
	return string(data)
}

// isolatedEnv returns the environment of a host run that must not touch the
// state of the installation of the developer that runs the test.
func isolatedEnv(dir string) []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + dir,
		"XDG_CONFIG_HOME=" + filepath.Join(dir, "config"),
		"XDG_CACHE_HOME=" + filepath.Join(dir, "cache"),
		"XDG_DATA_HOME=" + filepath.Join(dir, "data"),
		"XDG_STATE_HOME=" + filepath.Join(dir, "state"),
		"APPDATA=" + filepath.Join(dir, "appdata"),
		"LOCALAPPDATA=" + filepath.Join(dir, "localappdata"),
		"HF_HOME=" + filepath.Join(dir, "hf"),
	}
}
