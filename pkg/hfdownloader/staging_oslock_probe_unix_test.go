// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build unix && !solaris && !aix

package hfdownloader

// The ownership probe must be non-owning: it may take a shared flock, which
// coexists with other probes and never looks like a writer. A real writer's
// exclusive lock is the only thing that should report busy.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
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

func TestCheckBlobUnsupportedFlockProbeIsNotFree(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	oldFlock := stagingOwnershipProbeFlock
	defer func() { stagingOwnershipProbeFlock = oldFlock }()
	stagingOwnershipProbeFlock = func(fd int, how int) error {
		if how == syscall.LOCK_SH|syscall.LOCK_NB {
			return syscall.ENOLCK
		}
		return syscall.Flock(fd, how)
	}

	cache := NewHFCache(t.TempDir())
	repo, err := cache.Repo("owner/repo", RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("d", 64)
	staging := repo.IncompletePath(sha)
	if err := os.WriteFile(staging, []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	meta := IncompleteMeta{Version: stagingMetaVersion, Repo: "owner/repo", Path: "model.bin", Size: 100, LockFenced: true, Ranges: buildRangeLayout(100, 1)}
	if err := writeStagingMeta(repo.IncompleteMetaPath(sha), &meta); err != nil {
		t.Fatal(err)
	}
	if own, err := acquireStagingOwnershipLock(mustDomainDst(t, repo, sha)); err != nil {
		t.Fatal(err)
	} else {
		own.release()
	}

	status, _, err := repo.CheckBlob(sha)
	// The probe failed, so the ownership state is UNKNOWN. It must not be
	// reported as "free"/stale (which would hand the file to a second
	// writer) and it must not be reported as a proven live owner either
	// (which would become a successful skip with the blob still missing).
	// It is an observable error instead.
	if !errors.Is(err, ErrBlobOwnershipUnknown) || status != BlobOwnershipUnknown {
		t.Fatalf("CheckBlob with ENOLCK probe = %v, %v; want an unknown-ownership error, not stale/free", status, err)
	}
}
