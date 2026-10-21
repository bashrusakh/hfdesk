// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package service

import "context"

// RunProcess runs HFDesk as the process of a registered service.
//
// On most platforms the registered process is an ordinary foreground process
// and this is the headless host. On Windows a process that the service
// control manager started must first connect to the service dispatcher, so
// that the manager can request a stop and receive the lifecycle states.
// A process started by hand (a console run) runs the plain host instead.
//
// RunProcess never opens a browser and never creates a tray icon.
func RunProcess(ctx context.Context, opts RunOptions) error {
	return runProcess(ctx, opts)
}
