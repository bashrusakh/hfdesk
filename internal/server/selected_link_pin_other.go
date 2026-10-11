// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build !linux && !windows && !darwin && !unix

package server

import (
	"fmt"
	"os"
	"runtime"
)

func openLocalSelectionDirectory(root *os.Root, name string) (*os.File, error) {
	before, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("local selection path %q is no longer a directory", name)
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.IsDir() || !os.SameFile(before, info) {
		_ = f.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("local selection path %q is no longer a directory", name)
	}
	return f, nil
}

func openSelectedLinkEntry(_, _ string) (*os.File, error) {
	return nil, fmt.Errorf("cannot pin selected symbolic-link identity on %s", runtime.GOOS)
}

func openSelectedRegularEntry(root *os.Root, _, name string) (*os.File, error) {
	return root.Open(name)
}

func shouldHardLinkSelectedEntry(error) bool { return false }
