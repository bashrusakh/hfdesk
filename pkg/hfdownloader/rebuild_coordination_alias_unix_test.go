// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly || illumos

package hfdownloader

import "os"

func createCoordinationDirectoryAliasForTest(link, target string) error {
	return os.Symlink(target, link)
}
