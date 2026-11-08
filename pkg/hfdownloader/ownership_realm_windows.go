// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package hfdownloader

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const ownershipOSLockAvailable = true

type ownershipFileLock struct {
	handle     windows.Handle
	overlapped windows.Overlapped
}

func openOwnershipLock(path string, mode ownershipLockMode) (ownershipLockHandle, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(
		p,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_ALWAYS,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		if errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, errOwnershipBusy
		}
		if errors.Is(err, windows.ERROR_NOT_SUPPORTED) || errors.Is(err, windows.ERROR_INVALID_FUNCTION) {
			return nil, fmt.Errorf("%w: %v", errOwnershipUnavailable, err)
		}
		return nil, err
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		_ = windows.CloseHandle(h)
		return nil, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		_ = windows.CloseHandle(h)
		return nil, fmt.Errorf("ownership lock %q is not a regular file", path)
	}
	flags := uint32(windows.LOCKFILE_FAIL_IMMEDIATELY)
	if mode == ownershipExclusive {
		flags |= windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	lock := &ownershipFileLock{handle: h}
	if err := windows.LockFileEx(h, flags, 0, 1, 0, &lock.overlapped); err != nil {
		_ = windows.CloseHandle(h)
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			return nil, errOwnershipBusy
		}
		if errors.Is(err, windows.ERROR_NOT_SUPPORTED) || errors.Is(err, windows.ERROR_INVALID_FUNCTION) {
			return nil, fmt.Errorf("%w: %v", errOwnershipUnavailable, err)
		}
		return nil, err
	}
	return lock, nil
}

func (l *ownershipFileLock) release() {
	if l == nil || l.handle == 0 {
		return
	}
	_ = windows.UnlockFileEx(l.handle, 0, 1, 0, &l.overlapped)
	_ = windows.CloseHandle(l.handle)
	l.handle = 0
}

func ownershipDirectoryIdentity(path string) (string, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	h, err := windows.CreateFile(
		p,
		0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return "", err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return "", fmt.Errorf("%q is not a directory", path)
	}
	return fmt.Sprintf("windows:%08x:%08x%08x", info.VolumeSerialNumber, info.FileIndexHigh, info.FileIndexLow), nil
}

func ownershipComponentKey(parentPath, name string) (string, error) {
	caseSensitive, err := ownershipDirectoryCaseSensitive(parentPath)
	if err != nil {
		return "", err
	}
	if caseSensitive {
		return "case-sensitive:" + name, nil
	}
	upper, err := windowsInvariantUpper(name)
	if err != nil {
		return "", err
	}
	return "ordinal-insensitive:" + upper, nil
}

func ownershipVolumeKey(root string) string { return strings.ToUpper(root) }

type ownershipFileCaseSensitiveInfo struct {
	Flags uint32
}

func ownershipDirectoryCaseSensitive(path string) (bool, error) {
	for current := filepath.Clean(path); ; current = filepath.Dir(current) {
		p, err := windows.UTF16PtrFromString(current)
		if err != nil {
			return false, err
		}
		h, err := windows.CreateFile(
			p,
			0,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
			nil,
			windows.OPEN_EXISTING,
			windows.FILE_FLAG_BACKUP_SEMANTICS,
			0,
		)
		if err != nil {
			if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
				next := filepath.Dir(current)
				if next != current {
					continue
				}
			}
			return false, err
		}
		var info ownershipFileCaseSensitiveInfo
		err = windows.GetFileInformationByHandleEx(
			h,
			windows.FileCaseSensitiveInfo,
			(*byte)(unsafe.Pointer(&info)),
			uint32(unsafe.Sizeof(info)),
		)
		_ = windows.CloseHandle(h)
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) || errors.Is(err, windows.ERROR_INVALID_FUNCTION) || errors.Is(err, windows.ERROR_NOT_SUPPORTED) {
			return false, fmt.Errorf("%w: %v", errOwnershipCaseModeUnsupported, err)
		}
		if err != nil {
			return false, err
		}
		return info.Flags&windows.FILE_CS_FLAG_CASE_SENSITIVE_DIR != 0, nil
	}
}

func ownershipDirectoryCaseProfile(path, _ string) (ownershipCaseProfile, error) {
	caseSensitive, err := queryOwnershipDirectoryCaseSensitive(path)
	if err != nil {
		return ownershipCaseProfile{}, err
	}
	return ownershipCaseProfile{Version: ownershipNodeFormatVersion, Provider: "windows-file-case-info-v1", Sensitive: caseSensitive}, nil
}

func ownershipControlDirectoryCaseProfile(path, controlProbe string) (ownershipCaseProfile, error) {
	return ownershipDirectoryCaseProfile(path, controlProbe)
}

func setOwnershipControlDirectoryCaseProfile(path string, profile ownershipCaseProfile) error {
	if profile.Provider != "windows-file-case-info-v1" {
		return errOwnershipCaseProfileUnavailable
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(
		p,
		windows.FILE_WRITE_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	info := ownershipFileCaseSensitiveInfo{}
	if profile.Sensitive {
		info.Flags = windows.FILE_CS_FLAG_CASE_SENSITIVE_DIR
	}
	return windows.SetFileInformationByHandle(h, windows.FileCaseSensitiveInfo,
		(*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
}

func queryOwnershipDirectoryCaseSensitive(path string) (bool, error) {
	p, err := windows.UTF16PtrFromString(filepath.Clean(path))
	if err != nil {
		return false, err
	}
	h, err := windows.CreateFile(
		p,
		0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return false, err
	}
	defer windows.CloseHandle(h)
	var info ownershipFileCaseSensitiveInfo
	err = windows.GetFileInformationByHandleEx(h, windows.FileCaseSensitiveInfo,
		(*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) || errors.Is(err, windows.ERROR_INVALID_FUNCTION) || errors.Is(err, windows.ERROR_NOT_SUPPORTED) {
		return false, errOwnershipCaseModeUnsupported
	}
	if err != nil {
		return false, err
	}
	return info.Flags&windows.FILE_CS_FLAG_CASE_SENSITIVE_DIR != 0, nil
}

var ownershipLCMapStringEx = syscall.NewLazyDLL("kernel32.dll").NewProc("LCMapStringEx")

func windowsInvariantUpper(value string) (string, error) {
	source, err := syscall.UTF16FromString(value)
	if err != nil {
		return "", err
	}
	locale, _ := syscall.UTF16PtrFromString("")
	const lcmapUppercase = 0x00000200
	count, _, callErr := ownershipLCMapStringEx.Call(
		uintptr(unsafe.Pointer(locale)),
		lcmapUppercase,
		uintptr(unsafe.Pointer(&source[0])),
		uintptr(len(source)-1),
		0,
		0,
		0,
		0,
		0,
	)
	if count == 0 {
		return "", fmt.Errorf("Windows ordinal case map: %w", callErr)
	}
	dest := make([]uint16, int(count))
	count, _, callErr = ownershipLCMapStringEx.Call(
		uintptr(unsafe.Pointer(locale)),
		lcmapUppercase,
		uintptr(unsafe.Pointer(&source[0])),
		uintptr(len(source)-1),
		uintptr(unsafe.Pointer(&dest[0])),
		uintptr(len(dest)),
		0,
		0,
		0,
	)
	if count == 0 {
		return "", fmt.Errorf("Windows ordinal case map: %w", callErr)
	}
	return syscall.UTF16ToString(dest[:int(count)]), nil
}
