// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/bashrusakh/hfdesk/internal/runtimecoordination"
	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
)

type serverTestCoordinationRecord struct {
	Version int    `json:"version"`
	Purpose string `json:"purpose"`
	RootID  string `json:"rootId"`
}

type serverTestCoordinationFile struct {
	path    string
	purpose string
}

// registerServerProfileCoordinationCleanup captures only new lease entries
// for an isolated workflow root. The shared OS-profile coordinator directory
// and owner marker are never removed.
func registerServerProfileCoordinationCleanup(t *testing.T, root string) {
	t.Helper()
	// A planned root has no filesystem object until an actor creates it, and the
	// coordinator identity is derived from that object. Create it the same way
	// the lease acquirer does so the tracked entries are the ones the app and
	// the separate CLI process will select for this root.
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("create temporary coordination root %q: %v", root, err)
	}
	restore := runtimecoordination.SetBaseDirProvider(nil)
	dataPath := hfdownloader.NewHFCache(root, 0).RebuildCoordinationPath()
	if dataPath == "" {
		restore()
		t.Fatal("resolve canonical profile data-lease path")
	}
	base := strings.TrimSuffix(dataPath, ".lock")
	paths := []serverTestCoordinationFile{{path: dataPath, purpose: "data"}, {path: base + ".admission.lock", purpose: "admission"}}
	restore()
	testRoot, err := filepath.Abs(filepath.Clean(serverTestTempRoot))
	if err != nil {
		t.Fatal(err)
	}
	if physical, err := filepath.EvalSymlinks(testRoot); err == nil {
		testRoot = physical
	}
	var created []serverTestCoordinationFile
	for _, entry := range paths {
		_, err := os.Lstat(entry.path)
		if errors.Is(err, os.ErrNotExist) {
			created = append(created, entry)
		} else if err != nil {
			t.Fatalf("inspect profile lease entry %q: %v", entry.path, err)
		}
	}
	if len(created) == 0 {
		return
	}
	t.Cleanup(func() {
		for _, entry := range created {
			if err := cleanupServerCoordinationFile(entry, testRoot); err != nil {
				t.Errorf("cleanup temporary profile lease %q: %v", entry.path, err)
			}
		}
	})
}

func cleanupServerCoordinationFile(entry serverTestCoordinationFile, testRoot string) error {
	info, err := os.Lstat(entry.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refuse non-regular lease entry")
	}
	file, err := os.OpenFile(entry.path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	current, err := os.Lstat(entry.path)
	if err != nil {
		return err
	}
	if !current.Mode().IsRegular() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, current) {
		return fmt.Errorf("lease entry changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil {
		return err
	}
	if len(data) > 4096 {
		return fmt.Errorf("lease record exceeds expected size")
	}
	var record serverTestCoordinationRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return fmt.Errorf("lease record is not valid JSON: %w", err)
	}
	if record.Version != 1 || record.Purpose != entry.purpose {
		return fmt.Errorf("lease record does not match the tracked temporary root")
	}
	digest := sha256.Sum256([]byte(record.RootID))
	suffix := ".lock"
	if entry.purpose == "admission" {
		suffix = ".admission.lock"
	}
	if filepath.Base(entry.path) != hex.EncodeToString(digest[:])+suffix {
		return fmt.Errorf("lease filename does not match its owner record")
	}
	parts := strings.SplitN(record.RootID, "\x00", 2)
	if len(parts) != 2 || !serverCoordinationPathContains(testRoot, parts[1]) {
		return fmt.Errorf("lease record is not bound to this workflow's temporary root")
	}
	unlock, acquired, err := tryLockServerCoordinationFile(file)
	if err != nil {
		return err
	}
	if !acquired {
		return fmt.Errorf("lease is still held; preserve the runtime entry")
	}
	defer unlock()
	current, err = os.Lstat(entry.path)
	if err != nil {
		return err
	}
	if !os.SameFile(opened, current) {
		return fmt.Errorf("lease entry changed while exclusively locked")
	}
	return os.Remove(entry.path)
}

func serverCoordinationPathContains(root, target string) bool {
	root, errRoot := filepath.Abs(filepath.Clean(root))
	target, errTarget := filepath.Abs(filepath.Clean(target))
	if errRoot != nil || errTarget != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		root, target = strings.ToLower(root), strings.ToLower(target)
	}
	rel, err := filepath.Rel(root, target)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
