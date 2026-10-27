// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build unix && !solaris && !aix

package hfdownloader

// Unix probe handle: a shared flock stands for a read-only status probe, an
// exclusive flock stands for a real writer. Both are taken on their own open
// file description, exactly like the production probes and the ownership
// lock, so the test exercises the real conflict semantics.

import (
	"os"
	"syscall"
)

func openStagingProbeHandle(path string, mode stagingProbeHoldMode) (stagingProbeHandle, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		return stagingProbeHandle{}, err
	}
	how := syscall.LOCK_SH
	if mode == probeHoldExclusive {
		how = syscall.LOCK_EX
	}
	if err := syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return stagingProbeHandle{}, err
	}
	return stagingProbeHandle{
		releaseFn: func() {
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			_ = f.Close()
		},
		heldFn: func() bool {
			// A held flock is reported by the kernel as busy for another
			// exclusive request on the same file.
			probe, err := os.Open(path)
			if err != nil {
				return false
			}
			defer probe.Close()
			err = syscall.Flock(int(probe.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
			if err == nil {
				_ = syscall.Flock(int(probe.Fd()), syscall.LOCK_UN)
			}
			return err != nil
		},
	}, nil
}
