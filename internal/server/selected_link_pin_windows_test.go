// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWindowsPinPathPreservesShortAndSpecialPathForms(t *testing.T) {
	for _, path := range []string{
		`C:\short\file.gguf`,
		`\\?\C:\already\extended`,
		`\??\C:\nt-extended`,
		`\\.\PhysicalDrive0`,
	} {
		got, err := windowsPinPath(path)
		if err != nil || got != path {
			t.Errorf("windowsPinPath(%q) = %q, %v; want unchanged", path, got, err)
		}
	}
}

func TestOpenSelectedWindowsEntryPinsLongRootedDestination(t *testing.T) {
	repo := t.TempDir()
	relative := filepath.Join(strings.Repeat("a", 60), strings.Repeat("b", 60), strings.Repeat("c", 60), strings.Repeat("d", 60), "selected.gguf")
	full := filepath.Join(repo, relative)
	if len(full) <= 260 || len(filepath.Base(full)) > 255 {
		t.Fatalf("invalid long-path fixture length=%d leaf=%d", len(full), len(filepath.Base(full)))
	}
	if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte("payload"), 0600); err != nil {
		t.Fatal(err)
	}

	root, err := os.OpenRoot(repo)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	opened, err := root.Open(relative)
	if err != nil {
		t.Fatalf("rooted open of long destination: %v", err)
	}
	_ = opened.Close()

	pin, err := openSelectedWindowsEntry(repo, filepath.ToSlash(relative))
	if err != nil {
		t.Fatalf("pin long destination: %v", err)
	}
	if _, err := pin.Stat(); err != nil {
		t.Fatalf("stat pinned destination: %v", err)
	}
	_ = pin.Close()
	if err := root.Remove(relative); err != nil {
		t.Fatalf("rooted remove of long destination: %v", err)
	}
}

func TestWindowsPinPathQualifiesLongDOSAndUNCPaths(t *testing.T) {
	longComponent := strings.Repeat("a", 250)
	dos := filepath.Join(`C:\root`, longComponent, "selected.gguf")
	if len(dos) <= 260 || len(filepath.Base(dos)) > 255 {
		t.Fatalf("invalid long-path fixture length=%d leaf=%d", len(dos), len(filepath.Base(dos)))
	}
	got, err := windowsPinPath(dos)
	if err != nil || !strings.HasPrefix(got, `\\?\C:\`) {
		t.Fatalf("long DOS path = %q, %v; want extended DOS path", got, err)
	}

	unc := `\\server\share\` + longComponent + `\selected.gguf`
	got, err = windowsPinPath(unc)
	if err != nil || !strings.HasPrefix(got, `\\?\UNC\server\share\`) {
		t.Fatalf("long UNC path = %q, %v; want extended UNC path", got, err)
	}
}
