// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bashrusakh/hfdesk/internal/runtimecoordination"
	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
)

// TestMain keeps the CLI's runtime coordination inside this package's own
// fixture root. The CLI is exercised in-process here, so the injection seam
// scopes its coordinator without touching the user's real profile.
func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("", "hfdesk-cmd-tests-")
	if err != nil {
		panic(err)
	}
	coordinationBase := filepath.Join(root, "coordination-runtime")
	runtimecoordination.SetBaseDirProvider(func() (string, error) { return coordinationBase, nil })
	code := m.Run()
	if err := os.RemoveAll(root); err != nil {
		fmt.Fprintln(os.Stderr, "remove test sandbox: "+err.Error())
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

func TestRebuildProbeAndCLIUseSharedGoSync(t *testing.T) {
	var stdout, stderr bytes.Buffer
	handled, code := runRebuildCLI([]string{"--hfdesk-rebuild-probe"}, &stdout, &stderr)
	if !handled || code != 0 || strings.TrimSpace(stdout.String()) != rebuildProtocolVersion || stderr.Len() != 0 {
		t.Fatalf("probe handled=%t code=%d stdout=%q stderr=%q", handled, code, stdout.String(), stderr.String())
	}

	r := filepath.Join(t.TempDir(), "R $; ' output")
	h := filepath.Join(t.TempDir(), "H $; ' source")
	cache := hfdownloader.NewHFCacheResolved(r, h, 0)
	repo, err := cache.Repo("owner/model", hfdownloader.RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	snapshot, err := repo.SnapshotDir("commit-main")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(snapshot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snapshot, "weights.bin"), []byte("ordinary-source"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := repo.WriteRef("main", "commit-main"); err != nil {
		t.Fatal(err)
	}

	stdout.Reset()
	stderr.Reset()
	handled, code = runRebuildCLI([]string{"--rebuild", "--cache-dir", r, "--hub-dir", h}, &stdout, &stderr)
	if !handled || code != 0 || stderr.Len() != 0 {
		t.Fatalf("rebuild handled=%t code=%d stdout=%q stderr=%q", handled, code, stdout.String(), stderr.String())
	}
	friendly := filepath.Join(r, "models", "owner", "model", "weights.bin")
	if got, err := os.ReadFile(friendly); err != nil || string(got) != "ordinary-source" {
		t.Fatalf("CLI friendly view=%q err=%v", got, err)
	}
	if !strings.Contains(stdout.String(), "Repos scanned:     1") {
		t.Fatalf("CLI result did not surface Sync counters: %s", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(r, "hub")); !os.IsNotExist(err) {
		t.Fatalf("CLI with exact H created an unintended R/hub: %v", err)
	}
}
