// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly || illumos

package hfdownloader

import (
	"errors"
	"os"
	"syscall"
)

func tryLockCoordinationFileForCleanup(file *os.File) (func(), bool, error) {
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN) }, true, nil
		}
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, false, nil
		}
		return nil, false, err
	}
}

// removeTestCoordinationLeaseEntry removes one tracked lease entry.
//
// An open handle does not prevent unlinking on this platform, so the entry is
// removed while the exclusive lease is still held.
func removeTestCoordinationLeaseEntry(path string, held *os.File) error {
	return os.Remove(path)
}
