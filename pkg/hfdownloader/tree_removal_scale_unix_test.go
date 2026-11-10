// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build unix && !solaris && !aix

package hfdownloader

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// lowerOwnershipFileLimitsForTest sets both NOFILE rlimits in one call.
// Lowering the hard limit cannot be undone in a process, so this runs only
// in the disposable child of TestTreeRemovalAdmissionReportsActionableFileLimitError.
func lowerOwnershipFileLimitsForTest(t *testing.T, soft, hard uint64) {
	t.Helper()
	var limit syscall.Rlimit
	assignOwnershipRlimit(&limit, soft, hard)
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &limit); err != nil {
		t.Fatalf("lower NOFILE limits to %d/%d: %v", soft, hard, err)
	}
}

// TestTreeRemovalAdmissionFileLimitChild forces tree admission past a hard
// file-descriptor limit no raise can help with (400 directories need ~800
// held lock descriptors against a limit of 128) and pins the actionable
// error: it names the observed soft/hard limits and the remedy, keeps the
// underlying exhaustion cause wrapped, never reads as a vanished target, and
// removes nothing.
func TestTreeRemovalAdmissionFileLimitChild(t *testing.T) {
	if os.Getenv(treeRemovalFdChildEnv) != "1" {
		t.Skip("helper process for the actionable file-limit test")
	}
	const childLimit = 128
	lowerOwnershipFileLimitsForTest(t, childLimit, childLimit)

	hub := t.TempDir()
	repo := filepath.Join(hub, "models--owner--capped")
	for i := 0; i < 400; i++ {
		if err := os.MkdirAll(filepath.Join(repo, fmt.Sprintf("dir-%03d", i)), 0o755); err != nil {
			t.Fatalf("create capped directory %d: %v", i, err)
		}
	}
	admission, err := OpenCacheAdmission(t.Context(), hub)
	if err != nil {
		t.Fatalf("open cache admission under the capped limit: %v", err)
	}
	release, err := admission.AdmitTreeRemoval(t.Context(), repo)
	if err == nil {
		release()
		t.Fatalf("tree admission succeeded although the hard file-descriptor limit is %d", childLimit)
	}
	if release != nil {
		t.Fatal("a failed tree admission must not return a release")
	}
	if !ownershipFileDescriptorPressure(err) {
		t.Fatalf("admission error = %v, want the wrapped descriptor-exhaustion cause", err)
	}
	message := err.Error()
	for _, want := range []string{
		"file-descriptor limit",
		"soft limit at 128",
		"hard limit at 128",
		"ulimit",
		"directory count",
	} {
		if !strings.Contains(message, want) {
			t.Errorf("actionable error misses %q: %v", want, message)
		}
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Errorf("descriptor exhaustion must never be readable as a vanished target: %v", err)
	}
	if errors.Is(err, ErrOwnershipBusy) {
		t.Errorf("descriptor exhaustion must not claim an ownership conflict: %v", err)
	}
	if _, statErr := os.Lstat(repo); statErr != nil {
		t.Errorf("failed admission altered the tree: %v", statErr)
	}
}
