// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package hfdownloader

import (
	"errors"
	"os"
	"syscall"
)

// isProcessAlive checks this host with signal 0, which delivers no signal.
// Permission errors count as alive so they cannot authorize takeover.
func isProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = process.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	return errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EPERM)
}
