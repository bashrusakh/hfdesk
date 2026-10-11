// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package server

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
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

// The open handle supplies Windows' volume/file-index identity and keeps the
// original object alive until the selected operation releases its pin.
func openSelectedWindowsEntry(physicalRepo, name string) (*os.File, error) {
	path := filepath.Join(physicalRepo, filepath.FromSlash(name))
	path16, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	const fileReadAttributes = 0x80
	handle, err := syscall.CreateFile(
		path16,
		fileReadAttributes,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil,
		syscall.OPEN_EXISTING,
		syscall.FILE_FLAG_OPEN_REPARSE_POINT|syscall.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return nil, fmt.Errorf("pin selected entry: %w", err)
	}
	return os.NewFile(uintptr(handle), path), nil
}

func openSelectedLinkEntry(physicalRepo, name string) (*os.File, error) {
	return openSelectedWindowsEntry(physicalRepo, name)
}

func openSelectedRegularEntry(_ *os.Root, physicalRepo, name string) (*os.File, error) {
	return openSelectedWindowsEntry(physicalRepo, name)
}

func shouldHardLinkSelectedEntry(error) bool { return false }
