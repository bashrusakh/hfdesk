//go:build !windows

// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import "os"

func renameStateFile(oldpath, newpath string) error {
	return os.Rename(oldpath, newpath)
}
