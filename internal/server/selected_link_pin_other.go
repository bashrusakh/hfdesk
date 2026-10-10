// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build !linux && !windows && !darwin

package server

import (
	"fmt"
	"os"
	"runtime"
)

func openSelectedLinkEntry(_, _ string) (*os.File, error) {
	return nil, fmt.Errorf("cannot pin selected symbolic-link identity on %s", runtime.GOOS)
}

func openSelectedRegularEntry(root *os.Root, _, name string) (*os.File, error) {
	return root.Open(name)
}

func shouldHardLinkSelectedEntry(error) bool { return false }
