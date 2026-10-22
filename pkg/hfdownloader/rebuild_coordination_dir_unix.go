// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly || illumos

package hfdownloader

import (
	"fmt"
	"os"
	"strconv"
	"syscall"
)

func coordinationOSUserID() (string, error) {
	return strconv.Itoa(os.Getuid()), nil
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
