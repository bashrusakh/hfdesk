// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Sidecar cleanup requires proof the partial is gone. Any stat error other
// than not-exist leaves its state unknown, so the identity record must survive.

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveEmptyPartialFile(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "missing.part")
		if got := removeEmptyPartialFile(path); !got {
			t.Errorf("removeEmptyPartialFile(missing) = false, want true: there is nothing left for a sidecar to describe")
		}
	})

	t.Run("empty regular file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "empty.part")
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if got := removeEmptyPartialFile(path); !got {
			t.Errorf("removeEmptyPartialFile(empty) = false, want true")
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("the empty partial survived removal, stat err=%v", err)
		}
	})

	t.Run("non-empty regular file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "partial.part")
		const content = "resumable bytes"
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := removeEmptyPartialFile(path); got {
			t.Errorf("removeEmptyPartialFile(non-empty) = true, want false")
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("the non-empty partial was removed: %v", err)
		}
		if string(got) != content {
			t.Errorf("partial content = %q, want %q", got, content)
		}
	})

	t.Run("stat error that is not not-exist", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "loop.part")
		// A self-referential symlink makes os.Stat fail with ELOOP, which is
		// not os.IsNotExist: the existence of the partial is UNKNOWN.
		if err := os.Symlink(path, path); err != nil {
			t.Skipf("cannot create the self-referential symlink: %v", err)
		}
		_, statErr := os.Stat(path)
		if statErr == nil {
			t.Fatalf("os.Stat(%s) succeeded; the symlink loop did not produce the required stat error", path)
		}
		if os.IsNotExist(statErr) {
			t.Fatalf("os.Stat(%s) returned a not-exist error (%v); this case requires a different stat failure", path, statErr)
		}
		if got := removeEmptyPartialFile(path); got {
			t.Errorf("removeEmptyPartialFile(stat error %v) = true, want false: an unknown existence state must keep the identity sidecar", statErr)
		}
		fi, lerr := os.Lstat(path)
		if lerr != nil {
			t.Fatalf("the stat-error path was removed: %v", lerr)
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			t.Errorf("the stat-error path is no longer the symlink loop: mode %v", fi.Mode())
		}
	})
}
