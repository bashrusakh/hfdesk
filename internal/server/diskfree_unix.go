//go:build !windows

// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"os"
	"path/filepath"
	"syscall"
)

// diskFreeBytes returns available bytes for the filesystem containing path.
// If path does not exist, walks up to nearest existing ancestor.
func diskFreeBytes(path string) (free, total uint64, err error) {
	p := path
	for {
		if _, err := os.Stat(p); err == nil {
			break
		}
		parent := filepath.Dir(p)
		if parent == p {
			break
		}
		p = parent
	}
	var stat syscall.Statfs_t
	if err = syscall.Statfs(p, &stat); err != nil {
		return 0, 0, err
	}
	free = stat.Bavail * uint64(stat.Bsize)
	total = stat.Blocks * uint64(stat.Bsize)
	return free, total, nil
}
