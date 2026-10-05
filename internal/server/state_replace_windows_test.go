//go:build windows

// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWindowsReplaceJobsStateFileUsesNativeReplacement(t *testing.T) {
	t.Run("success replaces existing target", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "jobs.json")
		if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := replaceJobsStateFile(path, []byte("new"), defaultStateFileOps{}); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "new" {
			t.Fatalf("replacement content = %q, want new", got)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !info.Mode().IsRegular() {
			t.Fatalf("replacement is not a regular file: %v", info.Mode())
		}
		if info.Mode().Perm()&0o200 == 0 {
			t.Fatalf("replacement lost writable attribute: %v", info.Mode())
		}
		assertNoStateTemps(t, dir)
	})

	t.Run("pre-commit sync failure preserves target and removes temp", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "jobs.json")
		old := []byte("old")
		if err := os.WriteFile(path, old, 0o600); err != nil {
			t.Fatal(err)
		}
		want := errors.New("injected sync failure")
		err := replaceJobsStateFile(path, []byte("new"), failingSyncOps{defaultStateFileOps{}, want})
		if !errors.Is(err, want) {
			t.Fatalf("replacement error = %v, want sync error", err)
		}
		got, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if string(got) != string(old) {
			t.Fatalf("failed replacement changed target: %q", got)
		}
		assertNoStateTemps(t, dir)
	})
}
