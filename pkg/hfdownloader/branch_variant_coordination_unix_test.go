// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly || illumos

package hfdownloader

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bashrusakh/hfdesk/internal/runtimecoordination"
)

func TestGoCLIRebuildWaitsForAppDownloadAcrossRootAlias(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("generated wrapper is a POSIX Bash script")
	}
	// The CLI actor is the production binary, which derives its coordinator
	// from the OS account. The in-process app actor must use that same
	// coordinator for the two to fence the same physical root.
	restoreCoordinationProvider := runtimecoordination.SetBaseDirProvider(nil)
	defer restoreCoordinationProvider()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skipf("bash unavailable: %v", err)
	}
	testBinary := ensureTestHFDeskBinary(t)

	root := filepath.Join(t.TempDir(), "cache ' $; root")
	registerProfileCoordinationCleanup(t, root)
	revisions := branchVariantTestRevisions()
	hub := newBranchVariantTestHub(t, revisions)
	defer hub.Close()
	settings := Settings{CacheDir: root, Endpoint: hub.URL, Concurrency: 1, Verify: "sha256"}
	if err := Download(context.Background(), Job{Repo: "owner/model", Revision: "4.00bpw", BranchVariant: true}, settings, nil); err != nil {
		t.Fatal(err)
	}
	cache := NewHFCache(root, 0)
	artifact, err := ReadBranchVariantArtifact(root, RepoTypeModel, "owner/model", "4.00bpw", "commit-a")
	if err != nil || artifact == nil {
		t.Fatalf("branch artifact=%+v err=%v", artifact, err)
	}
	payload := filepath.Join(artifact.PayloadRoot, "weights.safetensors")
	payloadBefore, err := os.ReadFile(payload)
	if err != nil {
		t.Fatal(err)
	}
	manifestBefore, err := os.ReadFile(artifact.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}

	ordinary, err := cache.Repo("zzz/model", RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := ordinary.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if err := ordinary.WriteRef("main", "ordinary-commit"); err != nil {
		t.Fatal(err)
	}
	snapshot, err := ordinary.SnapshotPath("ordinary-commit", "alias/weights.safetensors")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(snapshot), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(snapshot, []byte("BBBBBBBB"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(ordinary.FriendlyPath(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(artifact.PayloadRoot, filepath.Join(ordinary.FriendlyPath(), "alias")); err != nil {
		t.Skipf("directory symlink unavailable: %v", err)
	}
	if _, err := cache.WriteRebuildScript(); err != nil {
		t.Fatal(err)
	}

	// Block the real app download after it has acquired its shared root lease
	// but before any payload or proof mutation.
	planReady := make(chan struct{})
	continuePlan := make(chan struct{})
	var releaseOnce sync.Once
	releaseDownload := func() { releaseOnce.Do(func() { close(continuePlan) }) }
	defer releaseDownload()
	retrySettings := settings
	retrySettings.OnPlanComplete = func(Plan) {
		close(planReady)
		<-continuePlan
	}
	retryDone := make(chan error, 1)
	go func() {
		retryDone <- Download(context.Background(), Job{Repo: "owner/model", Revision: "4.00bpw", BranchVariant: true}, retrySettings, nil)
	}()
	select {
	case <-planReady:
	case <-time.After(20 * time.Second):
		t.Fatal("app download did not reach the post-lease/pre-mutation barrier")
	}

	aliasRoot := filepath.Join(t.TempDir(), "cache-alias")
	if err := os.Symlink(root, aliasRoot); err != nil {
		t.Skipf("directory symlink unavailable for root-alias test: %v", err)
	}
	emptyPath := t.TempDir()
	command := exec.Command(bash, filepath.Join(aliasRoot, "rebuild.sh"), "--verbose")
	command.Env = filteredRebuildTestEnv("PATH", "HFDESK_BIN")
	command.Env = append(command.Env,
		"PATH="+emptyPath,
		"HFDESK_BIN="+testBinary,
	)
	otherHome := filepath.Join(t.TempDir(), "cli-home")
	command.Env = overrideRebuildTestEnv(command.Env, map[string]string{
		"HOME":            otherHome,
		"USERPROFILE":     otherHome,
		"XDG_CACHE_HOME":  filepath.Join(otherHome, "cache"),
		"XDG_CONFIG_HOME": filepath.Join(otherHome, "config"),
		"LOCALAPPDATA":    filepath.Join(otherHome, "localappdata"),
		"APPDATA":         filepath.Join(otherHome, "appdata"),
		"TMPDIR":          filepath.Join(otherHome, "tmpdir"),
		"TMP":             filepath.Join(otherHome, "tmp"),
		"TEMP":            filepath.Join(otherHome, "temp"),
	})
	var stdout bytes.Buffer
	command.Stdout = &stdout
	stderr, err := command.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	commandDone := make(chan error, 1)
	go func() { commandDone <- command.Wait() }()
	var stderrMu sync.Mutex
	var stderrText strings.Builder
	waitingForLease := make(chan struct{})
	var waitingOnce sync.Once
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			line := scanner.Text()
			stderrMu.Lock()
			_, _ = stderrText.WriteString(line + "\n")
			stderrMu.Unlock()
			if strings.Contains(line, "Waiting for the HF cache mutation lease") {
				waitingOnce.Do(func() { close(waitingForLease) })
			}
		}
	}()

	select {
	case <-waitingForLease:
	case err := <-commandDone:
		releaseDownload()
		<-retryDone
		stderrMu.Lock()
		defer stderrMu.Unlock()
		t.Fatalf("CLI exited before observing the app's active lease: %v; stdout=%s stderr=%s", err, stdout.String(), stderrText.String())
	case <-time.After(20 * time.Second):
		releaseDownload()
		<-retryDone
		if command.Process != nil {
			_ = command.Process.Kill()
		}
		t.Fatal("Go CLI did not report contention on the app-held physical root lease")
	}
	if got, err := os.ReadFile(payload); err != nil || !bytes.Equal(got, payloadBefore) {
		t.Fatalf("payload changed while the app held its lease: %q err=%v", got, err)
	}
	if got, err := os.ReadFile(artifact.ManifestPath); err != nil || !bytes.Equal(got, manifestBefore) {
		t.Fatalf("completion proof changed while the app held its lease: err=%v", err)
	}

	releaseDownload()
	if err := <-retryDone; err != nil {
		t.Fatalf("app retry after CLI contention: %v", err)
	}
	var rebuildErr error
	select {
	case rebuildErr = <-commandDone:
	case <-time.After(30 * time.Second):
		if command.Process != nil {
			_ = command.Process.Kill()
		}
		t.Fatal("CLI rebuild did not finish after the app released its lease")
	}
	if rebuildErr == nil {
		t.Fatalf("CLI rebuild unexpectedly succeeded through an ordinary/service alias; stdout=%s", stdout.String())
	}
	stderrMu.Lock()
	output := stderrText.String()
	stderrMu.Unlock()
	combined := stdout.String() + output
	if !strings.Contains(combined, "ordinary download path") || !strings.Contains(combined, "app-owned EXL branch variant service tree") {
		t.Fatalf("CLI rebuild did not report the expected no-effect conflict: %s", combined)
	}
	if got, err := os.ReadFile(payload); err != nil || !bytes.Equal(got, payloadBefore) {
		t.Fatalf("serialized CLI rebuild/retry changed branch payload: %q err=%v", got, err)
	}
	manifest, err := ReadManifest(artifact.ManifestPath)
	if err != nil || !manifest.IsCompleteBranchVariant("owner/model", RepoTypeModel, "4.00bpw") {
		t.Fatalf("completion proof after serialized app/CLI operations=%+v err=%v", manifest, err)
	}
}
