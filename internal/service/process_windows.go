// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package service

import (
	"context"

	"golang.org/x/sys/windows/svc"
)

// runProcess runs HFDesk as the process of a registered service.
//
// A process started by the service control manager must connect to the
// service dispatcher first: that is how the manager learns the lifecycle
// state and how it requests a stop. A process started by hand (a console
// run) has no dispatcher and runs the plain headless host instead.
func runProcess(ctx context.Context, opts RunOptions) error {
	if isService, err := svc.IsWindowsService(); err == nil && isService {
		return runAsWindowsService(opts)
	}
	return Run(ctx, opts)
}
