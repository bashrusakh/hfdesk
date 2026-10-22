// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly || illumos

package hfdownloader

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
)

func coordinationOSUserID() (string, error) {
	return strconv.Itoa(os.Getuid()), nil
}

// coordinationHomeDir returns the private base directory of the current
// account.
//
// The account record is preferred, so the carrier does not follow a
// per-process HOME override: the app and the CLI of one account then meet on
// the same carrier even when they are started with different environments.
//
// A numeric container identity has no account record, so the environment the
// container already sets is used instead. HOME is defined there, and
// os.UserHomeDir reads it without any name-service lookup.
func coordinationHomeDir() (string, error) {
	if current, err := user.Current(); err == nil && current.HomeDir != "" {
		return current.HomeDir, nil
	}
	if home, err := os.UserHomeDir(); err == nil && filepath.IsAbs(home) {
		return home, nil
	}
	return "", fmt.Errorf("no account record and no absolute home directory in the environment")
}

func validatePrivateCoordinationDirectory(path string, info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Getuid() {
		return fmt.Errorf("refuse coordination directory %q not owned by the current user", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("refuse coordination directory %q accessible by other users", path)
	}
	return nil
}
