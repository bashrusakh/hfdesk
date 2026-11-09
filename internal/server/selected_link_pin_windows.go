// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package server

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

func openLocalSelectionDirectory(root *os.Root, name string) (*os.File, error) {
	return openSelectedDirectoryFile(root, name)
}

// The open handle supplies Windows' volume/file-index identity and keeps the
// original object alive until the selected operation releases its pin.
func openSelectedWindowsEntry(physicalRepo, name string) (*os.File, error) {
	path := filepath.Join(physicalRepo, filepath.FromSlash(name))
	path, err := windowsPinPath(path)
	if err != nil {
		return nil, err
	}
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

// windowsPinPath gives long DOS and UNC paths the extended-length spelling
// needed by direct Win32 calls. Keep short paths untouched so their existing
// Win32 normalization behavior is preserved.
func windowsPinPath(path string) (string, error) {
	if strings.HasPrefix(path, `\\?\`) || strings.HasPrefix(path, `\??\`) || strings.HasPrefix(path, `\\.\`) {
		return path, nil
	}
	if len(path) < 248 {
		return path, nil
	}
	full, err := syscall.FullPath(path)
	if err != nil {
		return "", fmt.Errorf("resolve long pin path: %w", err)
	}
	if strings.HasPrefix(full, `\\`) {
		return `\\?\UNC\` + strings.TrimPrefix(full, `\\`), nil
	}
	return `\\?\` + full, nil
}

func openSelectedLinkEntry(physicalRepo, name string) (*os.File, error) {
	return openSelectedWindowsEntry(physicalRepo, name)
}

func openSelectedRegularEntry(_ *os.Root, physicalRepo, name string) (*os.File, error) {
	return openSelectedWindowsEntry(physicalRepo, name)
}

func shouldHardLinkSelectedEntry(error) bool { return false }
