// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package hfdownloader

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
)

func coordinationOSUserID() (string, error) {
	current, err := user.Current()
	if err != nil {
		return "", err
	}
	return current.Uid, nil
}

// coordinationHomeDir returns the private base directory of the current
// account. Windows resolves the account natively, so the account record is
// the primary source and the profile environment is the fallback.
func coordinationHomeDir() (string, error) {
	if current, err := user.Current(); err == nil && current.HomeDir != "" {
		return current.HomeDir, nil
	}
	if home, err := os.UserHomeDir(); err == nil && filepath.IsAbs(home) {
		return home, nil
	}
	return "", fmt.Errorf("no account record and no absolute profile directory in the environment")
}

func validatePrivateCoordinationDirectory(_ string, info os.FileInfo) error {
	if !info.IsDir() {
		return os.ErrInvalid
	}
	return nil
}
