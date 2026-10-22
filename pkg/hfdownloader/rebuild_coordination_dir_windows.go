// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package hfdownloader

import (
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
