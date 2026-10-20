// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package hfdownloader

import (
	"errors"
	"os"
	"syscall"
)

// isProcessAlive reports whether a process with the given PID is still
// running on this host. On Unix, signal 0 performs the permission/existence
// check without delivering a signal. A permission error means the process
// exists but belongs to another user, so it is treated as alive: an
// indeterminate answer must never displace a live owner.
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
