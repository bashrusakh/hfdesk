// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package hfdownloader

import (
	"errors"
	"syscall"
)

const (
	// processQueryLimitedInformation is PROCESS_QUERY_LIMITED_INFORMATION, enough to query exit status.
	processQueryLimitedInformation = 0x1000
	// stillActive is STILL_ACTIVE, returned by GetExitCodeProcess for a
	// running process.
	stillActive = 259
	// errorInvalidParameter is ERROR_INVALID_PARAMETER: OpenProcess uses it
	// when the PID does not name an existing process.
	errorInvalidParameter = syscall.Errno(87)
)

// isProcessAlive uses OpenProcess/GetExitCodeProcess: Signal(0) is unsupported
// on Windows. Access-denied and other uncertain API failures count as alive.
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
