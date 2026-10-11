// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package server

import (
	"os"
	"path/filepath"
	"syscall"
)

func openSelectedNonblockingRegularEntry(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(filepath.FromSlash(name), os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
