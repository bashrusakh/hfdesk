// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build unix && !solaris && !aix

package hfdownloader

// The ownership probe must be non-owning: it may take a shared flock, which
// coexists with other probes and never looks like a writer. A real writer's
// exclusive lock is the only thing that should report busy.

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestStagingOwnershipProbeIsNonOwning(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	staging := filepath.Join(t.TempDir(), "x.incomplete")
	// Create the persistent lock file once.
	lock, err := acquireStagingOwnershipLock(staging)
	if err != nil {
		t.Fatal(err)
	}
	lock.release()

	// A concurrent probe-equivalent holds a shared lock. The probe must not
	// mistake it for a writer (the old exclusive probe did).
	f, err := os.Open(stagingOwnershipLockPath(staging))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		f.Close()
		t.Fatalf("take shared probe lock: %v", err)
	}
	busy, err := stagingOwnershipBusy(staging)
	if err != nil || busy {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
		t.Fatalf("probe while another probe holds a shared lock = (%v, %v), want (false, nil)", busy, err)
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	f.Close()

	// A real writer (exclusive) is busy.
	owner, err := acquireStagingOwnershipLock(staging)
	if err != nil {
		t.Fatal(err)
	}
	busy, err = stagingOwnershipBusy(staging)
	if err != nil || !busy {
		owner.release()
		t.Fatalf("probe while a writer holds the lock = (%v, %v), want (true, nil)", busy, err)
	}
	owner.release()
	if busy, err := stagingOwnershipBusy(staging); err != nil || busy {
		t.Fatalf("probe after release = (%v, %v), want (false, nil)", busy, err)
	}
}
