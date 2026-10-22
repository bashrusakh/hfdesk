// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package hfdownloader

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bashrusakh/hfdesk/internal/runtimecoordination"
)

func TestWindowsGoCLIRebuildWaitsForAppDownloadAcrossRootAlias(t *testing.T) {
	// The CLI actor is the production binary, which derives its coordinator
	// from the OS account. The in-process app actor must use that same
	// coordinator for the two to fence the same physical root.
	restoreCoordinationProvider := runtimecoordination.SetBaseDirProvider(nil)
	defer restoreCoordinationProvider()
	binary := ensureTestHFDeskBinary(t)
	root := filepath.Join(t.TempDir(), "cache-root")
	registerProfileCoordinationCleanup(t, root)
	revisions := branchVariantTestRevisions()
	hub := newBranchVariantTestHub(t, revisions)
	defer hub.Close()
	settings := Settings{CacheDir: root, Endpoint: hub.URL, Concurrency: 1, Verify: "sha256"}
	job := Job{Repo: "owner/model", Revision: "4.00bpw", BranchVariant: true}
	if err := Download(context.Background(), job, settings, nil); err != nil {
		t.Fatal(err)
	}
	artifact, err := ReadBranchVariantArtifact(root, RepoTypeModel, job.Repo, job.Revision, revisions[job.Revision].commit)
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

	alias := filepath.Join(t.TempDir(), "cache-alias")
	rootArg := root
	if err := os.Symlink(root, alias); err != nil {
		caseAlias := strings.ToUpper(root)
		originalInfo, originalErr := os.Stat(root)
		aliasInfo, aliasErr := os.Stat(caseAlias)
		if originalErr == nil && aliasErr == nil && os.SameFile(originalInfo, aliasInfo) {
			rootArg = caseAlias
		} else {
			t.Logf("directory symlink/case alias unavailable; using the canonical root: %v", err)
		}
	} else {
		rootArg = alias
	}

	// Hold the actual app downloader after it has acquired a shared root lease,
	// before proof invalidation or payload changes.
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
	go func() { retryDone <- Download(context.Background(), job, retrySettings, nil) }()
	select {
	case <-planReady:
	case <-time.After(30 * time.Second):
		releaseDownload()
		select {
		case <-retryDone:
		case <-time.After(30 * time.Second):
		}
		t.Fatal("Windows app download did not reach the post-lease/pre-mutation barrier")
	}

	command := exec.Command(binary, "--rebuild", "--cache-dir="+rootArg, "--hub-dir="+filepath.Join(rootArg, "hub"), "--verbose")
	otherHome := filepath.Join(t.TempDir(), "cli-home")
	command.Env = rebuildTestEnvWithOverrides(map[string]string{
		"HOME":            otherHome,
		"USERPROFILE":     otherHome,
		"LOCALAPPDATA":    filepath.Join(otherHome, "localappdata"),
		"APPDATA":         filepath.Join(otherHome, "appdata"),
		"XDG_CACHE_HOME":  filepath.Join(otherHome, "cache"),
		"XDG_CONFIG_HOME": filepath.Join(otherHome, "config"),
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
	waiting := make(chan struct{})
	var waitingOnce sync.Once
	var stderrMu sync.Mutex
	var stderrText strings.Builder
	go func() {
		for scanner := bufio.NewScanner(stderr); scanner.Scan(); {
			line := scanner.Text()
			stderrMu.Lock()
			_, _ = stderrText.WriteString(line + "\n")
			stderrMu.Unlock()
			if strings.Contains(line, "Waiting for the HF cache mutation lease") {
				waitingOnce.Do(func() { close(waiting) })
			}
		}
	}()
	select {
	case <-waiting:
	case err := <-commandDone:
		releaseDownload()
		<-retryDone
		stderrMu.Lock()
		defer stderrMu.Unlock()
		t.Fatalf("Windows CLI exited before observing the app lease: %v stdout=%s stderr=%s", err, stdout.String(), stderrText.String())
	case <-time.After(30 * time.Second):
		releaseDownload()
		<-retryDone
		if command.Process != nil {
			_ = command.Process.Kill()
		}
		<-commandDone
		t.Fatal("Windows CLI did not report contention on the app-held physical root lease")
	}
	if got, err := os.ReadFile(payload); err != nil || !bytes.Equal(got, payloadBefore) {
		t.Fatalf("Windows CLI changed the payload while app lease was held: %q %v", got, err)
	}
	if got, err := os.ReadFile(artifact.ManifestPath); err != nil || !bytes.Equal(got, manifestBefore) {
		t.Fatalf("Windows CLI changed completion proof while app lease was held: %v", err)
	}

	releaseDownload()
	if err := <-retryDone; err != nil {
		t.Fatalf("Windows app retry after CLI contention: %v", err)
	}
	select {
	case err := <-commandDone:
		if err != nil {
			t.Fatalf("Windows CLI rebuild after app lease release: %v stdout=%s", err, stdout.String())
		}
	case <-time.After(30 * time.Second):
		if command.Process != nil {
			_ = command.Process.Kill()
		}
		<-commandDone
		t.Fatal("Windows CLI did not finish after app lease release")
	}
	if got, err := os.ReadFile(payload); err != nil || !bytes.Equal(got, payloadBefore) {
		t.Fatalf("Windows app/CLI operations changed branch bytes: %q %v", got, err)
	}
	manifest, err := ReadManifest(artifact.ManifestPath)
	if err != nil || !manifest.IsCompleteBranchVariant(job.Repo, RepoTypeModel, job.Revision) {
		t.Fatalf("Windows completion proof after app/CLI operations=%+v err=%v", manifest, err)
	}
}
