// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package hfdownloader

import (
	"errors"
	"syscall"
)

const (
	// processQueryLimitedInformation is PROCESS_QUERY_LIMITED_INFORMATION,
	// available since Windows Vista. It is enough to read the exit code.
	processQueryLimitedInformation = 0x1000
	// stillActive is STILL_ACTIVE, returned by GetExitCodeProcess for a
	// running process.
	stillActive = 259
	// errorInvalidParameter is ERROR_INVALID_PARAMETER: OpenProcess uses it
	// when the PID does not name an existing process.
	errorInvalidParameter = syscall.Errno(87)
)

// isProcessAlive reports whether a process with the given PID is still
// running on this host. Unlike the Unix implementation it does not use
// Signal(0): Go returns EWINDOWS for signal 0 on Windows, which would make
// every live owner look dead. OpenProcess/GetExitCodeProcess is the real
// platform check. Access-denied and other indeterminate failures are treated
// as alive so a live owner is never displaced on an uncertain answer; an owner
// whose same-host death cannot be proven is preserved rather than displaced.
func isProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		if errors.Is(err, errorInvalidParameter) {
			return false
		}
		return true
	}
	defer syscall.CloseHandle(h)

	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		return true
	}
	return code == stillActive
}
