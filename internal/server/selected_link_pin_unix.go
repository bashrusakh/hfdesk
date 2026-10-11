// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package server

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func openSelectedNonblockingRegularEntry(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(filepath.FromSlash(name), os.O_RDONLY|syscall.O_NONBLOCK, 0)
}

func openLocalSelectionDirectory(root *os.Root, name string) (*os.File, error) {
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.IsDir() {
		_ = f.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("local selection path %q is no longer a directory", name)
	}
	return f, nil
}
