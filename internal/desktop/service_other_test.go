// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package desktop

import (
	"errors"
	"testing"
)

// TestServiceHelperIsNotSupportedOutsideWindows proves that the desktop host of
// the other platforms stays honest about the service helper: it reports that the
// helper is a feature of Windows instead of performing half of an action.
func TestServiceHelperIsNotSupportedOutsideWindows(t *testing.T) {
	err := RunServiceAction(ServiceActionOptions{Action: "install"})
	if !errors.Is(err, ErrUnsupportedServiceAction) {
		t.Fatalf("RunServiceAction returned %v, want %v", err, ErrUnsupportedServiceAction)
	}
}
