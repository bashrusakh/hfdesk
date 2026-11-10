// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build !linux && !darwin && !windows

package service

import (
	"errors"
	"os"
)

// runningImageInfo resolves the file info of the running image itself.
//
// The identity of the running image is unknown on this platform: it has
// no service mode, and nothing stands in for the platform answers the
// supported platforms prove (the loaded inode on Linux, the open handle
// on Windows). The caller withholds the version claim — uncertainty
// stays visible in an empty expectation instead of naming a program the
// claim cannot prove.
func runningImageInfo() (os.FileInfo, error) {
	return nil, errors.New("service: the identity of the running image is unknown on this platform")
}
