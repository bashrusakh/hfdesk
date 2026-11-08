// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build unix && !solaris && !aix

package hfdownloader

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

const ownershipOSLockAvailable = true

type ownershipFileLock struct {
	file *os.File
}

func openOwnershipLock(path string, mode ownershipLockMode) (ownershipLockHandle, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o666)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("ownership lock %q is not a regular file", path)
	}
	flag := syscall.LOCK_SH
	if mode == ownershipExclusive {
		flag = syscall.LOCK_EX
	}
	if err := syscall.Flock(int(f.Fd()), flag|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		switch {
		case errors.Is(err, syscall.EWOULDBLOCK), errors.Is(err, syscall.EAGAIN):
			return nil, errOwnershipBusy
		case errors.Is(err, syscall.ENOLCK), errors.Is(err, syscall.EOPNOTSUPP), errors.Is(err, syscall.ENOSYS):
			return nil, fmt.Errorf("%w: %v", errOwnershipUnavailable, err)
		default:
			return nil, err
		}
	}
	return &ownershipFileLock{file: f}, nil
}

func (l *ownershipFileLock) release() {
	if l == nil || l.file == nil {
		return
	}
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	_ = l.file.Close()
	l.file = nil
}

func ownershipDirectoryIdentity(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%q is not a directory", path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("directory identity is unavailable for %q", path)
	}
	return fmt.Sprintf("unix:%x:%x", uint64(stat.Dev), uint64(stat.Ino)), nil
}

func ownershipComponentKey(_ string, name string) (string, error) {
	// POSIX spelling is byte-sensitive here; case-folding mounts need a native
	// component comparer before callers can claim alias coverage on that mount.
	return "case-sensitive:" + name, nil
}

func ownershipVolumeKey(root string) string { return root }
