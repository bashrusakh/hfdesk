// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build !windows && !linux && !darwin && !freebsd && !openbsd && !netbsd && !dragonfly && !solaris

package hfdownloader

import "fmt"

func branchVariantFilesystemIdentity(string) (string, error) {
	return "", fmt.Errorf("filesystem identity is unsupported on this platform")
}
