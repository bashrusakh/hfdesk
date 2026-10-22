// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package hfdownloader

import (
	"fmt"
	"os"
	"syscall"
)

func branchVariantFilesystemIdentity(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(syscall.Handle(file.Fd()), &info); err != nil {
		return "", err
	}
	return fmt.Sprintf("%08x:%08x%08x", info.VolumeSerialNumber, info.FileIndexHigh, info.FileIndexLow), nil
}
