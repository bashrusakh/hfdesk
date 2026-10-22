// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly || solaris

package hfdownloader

import (
	"fmt"
	"os"
	"syscall"
)

func branchVariantFilesystemIdentity(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("filesystem did not expose a stable directory identity")
	}
	return fmt.Sprintf("%x:%x", uint64(stat.Dev), uint64(stat.Ino)), nil
}
