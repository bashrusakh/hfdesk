// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package service

import "context"

// runProcess runs the headless host: on this platform the supervisor starts
// an ordinary foreground process.
func runProcess(ctx context.Context, opts RunOptions) error {
	return Run(ctx, opts)
}
