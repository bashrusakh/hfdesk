// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package server

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func openSelectedLinkEntry(physicalRepo, name string) (*os.File, error) {
	path := filepath.Join(physicalRepo, filepath.FromSlash(name))
	// macOS O_SYMLINK opens the symlink inode itself rather than its target.
	fd, err := syscall.Open(path, syscall.O_SYMLINK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("pin selected link entry: %w", err)
	}
	return os.NewFile(uintptr(fd), path), nil
}

func openSelectedRegularEntry(root *os.Root, _, name string) (*os.File, error) {
	return root.Open(filepath.FromSlash(name))
}

func shouldHardLinkSelectedEntry(err error) bool {
	return os.IsPermission(err)
}
