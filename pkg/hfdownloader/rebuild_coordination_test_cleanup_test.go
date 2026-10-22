// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bashrusakh/hfdesk/internal/runtimecoordination"
)

type testCoordinationLeaseFile struct {
	path    string
	rootID  string
	purpose string
	wasNew  bool
}

// registerProfileCoordinationCleanup tracks only newly-created per-root lease
// files for a workflow test's temporary roots. The app coordination directory
// and its owner marker are never removed.
func registerProfileCoordinationCleanup(t *testing.T, roots ...string) {
	t.Helper()
	rootScope, err := filepath.Abs(filepath.Clean(testHFDeskTempRoot))
	if err != nil {
		t.Fatal(err)
	}
	if physical, err := filepath.EvalSymlinks(rootScope); err == nil {
		rootScope = physical
	}
	restore := runtimecoordination.SetBaseDirProvider(nil)
	var files []testCoordinationLeaseFile
	seen := make(map[string]struct{})
	for _, root := range roots {
		physical, err := ensureCoordinationRootObject(root)
		if err != nil {
			restore()
			t.Fatalf("resolve temporary coordination root %q: %v", root, err)
		}
		rootID, err := rebuildCoordinationIdentity(physical)
		if err != nil {
			restore()
			t.Fatalf("identify temporary coordination root %q: %v", root, err)
		}
		for _, admission := range []bool{false, true} {
			path, err := coordinationFilePathForIdentity(context.Background(), rootID, admission)
			if err != nil {
				restore()
				t.Fatalf("resolve temporary coordination entry: %v", err)
			}
			if _, duplicate := seen[path]; duplicate {
				continue
			}
			seen[path] = struct{}{}
			_, statErr := os.Lstat(path)
			if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
				restore()
				t.Fatalf("inspect temporary coordination entry %q: %v", path, statErr)
			}
			if errors.Is(statErr, os.ErrNotExist) {
				purpose := "data"
				if admission {
					purpose = "admission"
				}
				files = append(files, testCoordinationLeaseFile{path: path, rootID: rootID, purpose: purpose, wasNew: true})
			}
		}
	}
	restore()
	if len(files) == 0 {
		return
	}
	t.Cleanup(func() {
		for _, entry := range files {
			if !entry.wasNew {
				continue
			}
			if err := removeUnusedTestCoordinationFile(entry, rootScope); err != nil {
				t.Errorf("cleanup temporary coordination entry %q: %v", entry.path, err)
			}
		}
	})
}

// ensureCoordinationRootObject materializes a planned coordination root the
// same way the lease acquirer does before it identifies the root. A root that
// does not exist yet has no filesystem object, so its coordinator identity can
// only be derived after the actor that owns it created it. Registering the
// cleanup before that point would identify a different physical object than
// the one every actor selects for the root.
func ensureCoordinationRootObject(root string) (string, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	return resolvedPathForCreate(root)
}

func removeUnusedTestCoordinationFile(entry testCoordinationLeaseFile, testRoot string) error {
	info, err := os.Lstat(entry.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refuse to remove a non-regular lease entry")
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
	var record coordinationLeaseRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return fmt.Errorf("lease record is not valid JSON: %w", err)
	}
	if record.Version != coordinationLeaseVersion || record.Purpose != entry.purpose || record.RootID != entry.rootID {
		return fmt.Errorf("lease record does not match the tracked temporary root")
	}
	digest := sha256.Sum256([]byte(record.RootID))
	suffix := ".lock"
	if record.Purpose == "admission" {
		suffix = ".admission.lock"
	}
	if filepath.Base(entry.path) != hex.EncodeToString(digest[:])+suffix {
		return fmt.Errorf("lease filename does not match its owner record")
	}
	parts := strings.SplitN(record.RootID, "\x00", 2)
	if len(parts) != 2 || !coordinationPathContains(testRoot, parts[1]) {
		return fmt.Errorf("lease record is not bound to this workflow's temporary root")
	}
	unlock, acquired, err := tryLockCoordinationFileForCleanup(file)
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
