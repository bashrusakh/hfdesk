// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build !linux && !darwin && !windows

package service

import (
	"fmt"
	"runtime"

	"github.com/bashrusakh/hfdesk/internal/runtimepaths"
)

// defaultPathsForScope reports that no service scope is supported on this
// platform, so an instance never picks a state root by accident.
func defaultPathsForScope(scope Scope) (runtimepaths.Paths, error) {
	return runtimepaths.Paths{}, fmt.Errorf("service: scope %q is not supported on %s", scope, runtime.GOOS)
}
