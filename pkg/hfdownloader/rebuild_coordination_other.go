// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build !windows && !linux && !darwin && !freebsd && !openbsd && !netbsd && !dragonfly && !illumos

package hfdownloader

import (
	"context"
	"fmt"
	"os"
	"os/user"
)

func coordinationOSUserID() (string, error) {
	current, err := user.Current()
	if err != nil {
		return "", err
	}
	return current.Uid, nil
}

func validatePrivateCoordinationDirectory(_ string, info os.FileInfo) error {
	if !info.IsDir() {
		return os.ErrInvalid
	}
	return nil
}

func acquireBranchVariantOSFileLock(context.Context, *os.File, bool, func()) (func(), error) {
	return nil, fmt.Errorf("cross-process HF rebuild coordination leases are unsupported on this platform")
}
