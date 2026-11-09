// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build !linux && !darwin && !windows

package service

import (
	"fmt"
	"runtime"
)

// newPlatformManager reports that service mode is not supported on this
// platform.
func newPlatformManager(scope Scope) (Manager, error) {
	return nil, fmt.Errorf("service mode is not supported on %s", runtime.GOOS)
}
