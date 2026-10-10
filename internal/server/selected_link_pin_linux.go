// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package server

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func openSelectedPathEntry(physicalRepo, name string) (*os.File, error) {
	const oPath = 0x200000 // Linux O_PATH; pin an entry without requiring read access.
	path := filepath.Join(physicalRepo, filepath.FromSlash(name))
	fd, err := syscall.Open(path, oPath|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("pin selected entry: %w", err)
	}
	return os.NewFile(uintptr(fd), path), nil
}

func openSelectedLinkEntry(physicalRepo, name string) (*os.File, error) {
	return openSelectedPathEntry(physicalRepo, name)
}

func openSelectedRegularEntry(_ *os.Root, physicalRepo, name string) (*os.File, error) {
	return openSelectedPathEntry(physicalRepo, name)
}
