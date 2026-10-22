// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bashrusakh/hfdesk/internal/runtimecoordination"
	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
)

type coordinationFixtureRevision struct {
	commit   string
	expected []byte
	response []byte
	path     string
}

func newCoordinationFixtureHub(t *testing.T, repoID string, revisions map[string]coordinationFixtureRevision, onResolve func(string, string)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prefix := "/api/models/" + repoID + "/"
		if strings.HasPrefix(r.URL.Path, prefix+"revision/") {
			revision := strings.TrimPrefix(r.URL.Path, prefix+"revision/")
			fixture, ok := revisions[revision]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"sha": fixture.commit})
			return
		}
		if strings.HasPrefix(r.URL.Path, prefix+"tree/") {
			revision := strings.TrimPrefix(r.URL.Path, prefix+"tree/")
			fixture, ok := revisions[revision]
			if !ok {
				for _, candidate := range revisions {
					if candidate.commit == revision {
						fixture, ok = candidate, true
						break
					}
				}
			}
			if !ok {
				http.NotFound(w, r)
				return
			}
			digest := sha256.Sum256(fixture.expected)
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"type": "file", "path": fixture.path, "size": len(fixture.expected),
				"lfs": map[string]any{"oid": hex.EncodeToString(digest[:]), "size": len(fixture.expected)},
			}})
			return
		}
		for _, resolvePrefix := range []string{prefix + "resolve/", "/" + repoID + "/resolve/"} {
			if !strings.HasPrefix(r.URL.Path, resolvePrefix) {
				continue
			}
			rest := strings.TrimPrefix(r.URL.Path, resolvePrefix)
			revision, _, ok := strings.Cut(rest, "/")
			fixture, found := revisions[revision]
			if !found {
				for _, candidate := range revisions {
					if candidate.commit == revision {
						fixture, found = candidate, true
						break
					}
				}
			}
			if !ok || !found {
				http.NotFound(w, r)
				return
			}
			if onResolve != nil {
				onResolve(revision, r.Method)
			}
			w.Header().Set("Content-Length", fmt.Sprint(len(fixture.response)))
			if r.Method != http.MethodHead {
				_, _ = w.Write(fixture.response)
			}
			return
		}
		http.NotFound(w, r)
	}))
}

func TestCopiedLocalRootRebindSurvivesFailedSecondArtifact(t *testing.T) {
	t.Setenv("HF_HUB_CACHE", "")
	base := isolateCacheState(t)
	sourceRoot := filepath.Join(base, "source-local")
	copiedRoot := filepath.Join(base, "copied-local")
	file := "weights.safetensors"
	revisions := map[string]coordinationFixtureRevision{
		"4_25":    {commit: "commit-a", expected: []byte("AAAAAAAA"), response: []byte("AAAAAAAA"), path: file},
		"5_00bpw": {commit: "commit-b", expected: []byte("BBBBBBBB"), response: []byte("CCCCCCCC"), path: file},
	}
	var ordinaryPayloadRequested atomic.Bool
	ordinaryRel := filepath.ToSlash(filepath.Join(
		hfdownloader.BranchVariantRevisionDirName("4_25"),
		hfdownloader.BranchVariantCommitDirName("commit-a"), "files", file,
	))
	revisions["main"] = coordinationFixtureRevision{
		commit: "ordinary-commit", expected: []byte("DDDDDDDD"), response: []byte("DDDDDDDD"), path: ordinaryRel,
	}
	hub := newCoordinationFixtureHub(t, "owner/model", revisions, func(revision, method string) {
		if revision == "main" && method != http.MethodHead {
			ordinaryPayloadRequested.Store(true)
		}
	})
	defer hub.Close()

	if err := hfdownloader.Download(context.Background(), hfdownloader.Job{Repo: "owner/model", Revision: "4_25", BranchVariant: true}, hfdownloader.Settings{
		OutputDir: sourceRoot, Endpoint: hub.URL, Concurrency: 1, Verify: "sha256",
	}, nil); err != nil {
		t.Fatalf("download complete artifact A: %v", err)
	}
	sourceService, err := hfdownloader.BranchVariantServiceRoot(sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	sourceServiceInfo, err := os.Stat(sourceService)
	if err != nil {
		t.Fatal(err)
	}
	ownerPath := filepath.Join(sourceService, ".hfdesk-owner.json")
	ownerBefore, err := os.ReadFile(ownerPath)
	if err != nil {
		t.Fatal(err)
	}
	copyHFRootBytes(t, sourceRoot, copiedRoot)
	copiedService, err := hfdownloader.BranchVariantServiceRoot(copiedRoot)
	if err != nil {
		t.Fatal(err)
	}
	copiedServiceInfo, err := os.Stat(copiedService)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(sourceServiceInfo, copiedServiceInfo) {
		t.Fatal("copy fixture retained the source service-root object")
	}
	if err := os.RemoveAll(sourceRoot); err != nil {
		t.Fatalf("remove source before copied-root mutations: %v", err)
	}

	presenceServer := newTestServerWithConfig(t, Config{CacheDir: filepath.Join(base, "unused-cache"), Endpoint: hub.URL})
	queryA := "revision=4_25&localDir=" + url.QueryEscape(copiedRoot)
	status, initialA := branchPresenceRequest(t, presenceServer, queryA)
	if status != http.StatusOK || !initialA.Downloaded {
		t.Fatalf("complete copied artifact A was not initially present: status=%d response=%+v", status, initialA)
	}
	ownerReadOnly, err := os.ReadFile(filepath.Join(copiedService, ".hfdesk-owner.json"))
	if err != nil || !bytes.Equal(ownerReadOnly, ownerBefore) {
		t.Fatalf("read-only copied-root presence rewrote its owner record: %q err=%v", ownerReadOnly, err)
	}

	err = hfdownloader.Download(context.Background(), hfdownloader.Job{Repo: "owner/model", Revision: "5_00bpw", BranchVariant: true}, hfdownloader.Settings{
		OutputDir: copiedRoot, RebuildCoordinationRoots: []string{copiedRoot}, Endpoint: hub.URL, Concurrency: 1, Verify: "sha256",
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "sha256 verify failed") {
		t.Fatalf("artifact B failure=%v; want trusted-hash failure", err)
	}
	ownerAfterFailure, err := os.ReadFile(filepath.Join(copiedService, ".hfdesk-owner.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ownerBeforeRecord, ownerAfterRecord map[string]any
	if err := json.Unmarshal(ownerBefore, &ownerBeforeRecord); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(ownerAfterFailure, &ownerAfterRecord); err != nil {
		t.Fatal(err)
	}
	if ownerBeforeRecord["storeId"] != ownerAfterRecord["storeId"] || ownerBeforeRecord["fileId"] == ownerAfterRecord["fileId"] {
		t.Fatalf("copied root binding was not durably rebound before failed artifact B: before=%v after=%v", ownerBeforeRecord, ownerAfterRecord)
	}
	if owned, err := hfdownloader.BranchVariantServiceOwned(copiedRoot); err != nil || !owned {
		t.Fatalf("failed artifact B declassified the copied root: owned=%t err=%v", owned, err)
	}
	artifactA, err := hfdownloader.ReadBranchVariantArtifact(copiedRoot, hfdownloader.RepoTypeModel, "owner/model", "4_25", "commit-a")
	if err != nil || artifactA == nil {
		t.Fatalf("complete artifact A after failed B=%+v err=%v", artifactA, err)
	}
	payloadA := filepath.Join(artifactA.PayloadRoot, file)
	if got, err := os.ReadFile(payloadA); err != nil || string(got) != "AAAAAAAA" {
		t.Fatalf("artifact A after failed B=%q err=%v", got, err)
	}
	metadataB, err := hfdownloader.BranchVariantMetadataPath(copiedRoot, hfdownloader.RepoTypeModel, "owner/model", "5_00bpw", "commit-b")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(metadataB, hfdownloader.ManifestFilename)); !os.IsNotExist(err) {
		t.Fatalf("failed artifact B retained a completion manifest: %v", err)
	}
	if status, aPresence := branchPresenceRequest(t, presenceServer, queryA); status != http.StatusOK || !aPresence.Downloaded {
		t.Fatalf("artifact A presence after failed B status=%d response=%+v", status, aPresence)
	}
	queryB := "revision=5_00bpw&localDir=" + url.QueryEscape(copiedRoot)
	if status, bPresence := branchPresenceRequest(t, presenceServer, queryB); status != http.StatusOK || bPresence.Downloaded {
		t.Fatalf("failed artifact B presence status=%d response=%+v", status, bPresence)
	}

	ordinaryOutput := filepath.Join(copiedService, "payloads", "models")
	ordinaryRelToRepo := filepath.Join(hfdownloader.BranchVariantRevisionDirName("4_25"), hfdownloader.BranchVariantCommitDirName("commit-a"), "files", file)
	ordinaryErr := hfdownloader.Download(context.Background(), hfdownloader.Job{Repo: "owner/model", Revision: "main"}, hfdownloader.Settings{
		OutputDir: ordinaryOutput, RebuildCoordinationRoots: []string{copiedRoot}, Endpoint: hub.URL, Concurrency: 1, Verify: "size",
	}, nil)
	if ordinaryErr == nil || !strings.Contains(ordinaryErr.Error(), "app-owned EXL branch variant service tree") {
		t.Fatalf("ordinary writer targeting A after failed B error=%v; want owned-payload refusal", ordinaryErr)
	}
	if ordinaryPayloadRequested.Load() {
		t.Fatal("ordinary writer fetched its replacement payload before the owned-path refusal")
	}
	if got, err := os.ReadFile(payloadA); err != nil || string(got) != "AAAAAAAA" {
		t.Fatalf("ordinary writer changed A after failed B: %q err=%v (relative target %q)", got, err, ordinaryRelToRepo)
	}
}

func TestSameUIDDifferentHomeCLIWaitsForOrdinaryAPIDownload(t *testing.T) {
	restoreCoordinationProvider := runtimecoordination.SetBaseDirProvider(nil)
	defer restoreCoordinationProvider()
	t.Setenv("HF_HUB_CACHE", "")
	base := isolateCacheState(t)
	cacheRoot := filepath.Join(base, "api-cache")
	registerServerProfileCoordinationCleanup(t, cacheRoot)
	body := bytes.Repeat([]byte("ordinary-download-chunk-"), 8192)
	digest := sha256.Sum256(body)
	hash := hex.EncodeToString(digest[:])
	firstChunkWritten := make(chan struct{})
	releaseBody := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseBody) }) }
	defer release()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/revision/"):
			_ = json.NewEncoder(w).Encode(map[string]string{"sha": "ordinary-commit"})
		case strings.Contains(r.URL.Path, "/tree/"):
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"type": "file", "path": "weights.safetensors", "size": len(body),
				"lfs": map[string]any{"oid": hash, "size": len(body)},
			}})
		case strings.Contains(r.URL.Path, "/resolve/"):
			w.Header().Set("Content-Length", fmt.Sprint(len(body)))
			if r.Method == http.MethodHead {
				return
			}
			cut := len(body) / 2
			if _, err := w.Write(body[:cut]); err != nil {
				return
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			close(firstChunkWritten)
			<-releaseBody
			_, _ = w.Write(body[cut:])
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	app := newTestServerWithConfig(t, Config{CacheDir: cacheRoot, Endpoint: server.URL, MaxActive: 1, Concurrency: 1, Verify: "sha256"})
	started := cacheRequest(t, app, http.MethodPost, "/api/download", `{"repo":"owner/model","revision":"main"}`)
	if started.Code != http.StatusAccepted {
		t.Fatalf("ordinary API download status=%d body=%s", started.Code, started.Body.String())
	}
	var job Job
	if err := json.Unmarshal(started.Body.Bytes(), &job); err != nil {
		t.Fatal(err)
	}
	select {
	case <-firstChunkWritten:
	case <-time.After(20 * time.Second):
		release()
		t.Fatal("ordinary API transfer did not reach its gated response body")
	}
	repo, err := hfdownloader.NewHFCache(cacheRoot, 0).Repo("owner/model", hfdownloader.RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	partial := filepath.Join(repo.BlobsDir(), "tmp-"+hash+".part")
	partialReady := time.NewTimer(10 * time.Second)
	partialPoll := time.NewTicker(5 * time.Millisecond)
	partialPresent := false
	for !partialPresent {
		select {
		case <-partialReady.C:
			partialPoll.Stop()
			release()
			t.Fatalf("ordinary API transfer never materialized nonempty partial %q", partial)
		case <-partialPoll.C:
			if info, err := os.Stat(partial); err == nil && info.Size() > 0 {
				partialPresent = true
			}
		}
	}
	partialPoll.Stop()
	if !partialReady.Stop() {
		<-partialReady.C
	}

	cache := hfdownloader.NewHFCache(cacheRoot, 0)
	orphan := filepath.Join(cache.ModelsDir(), "owner", "orphan", "stale-link")
	if err := os.MkdirAll(filepath.Dir(orphan), 0o755); err != nil {
		release()
		t.Fatal(err)
	}
	orphanCreated := false
	if err := os.Symlink(filepath.Join(base, "missing-friendly-target"), orphan); err == nil {
		orphanCreated = true
	} else {
		t.Logf("dangling-link cleanup oracle unavailable: %v", err)
	}
	cliBinary := buildServerTestHFDeskBinary(t)
	cliHome := filepath.Join(base, "different-cli-home")
	command := exec.Command(cliBinary, "--rebuild", "--cache-dir="+cacheRoot, "--hub-dir="+filepath.Join(cacheRoot, "hub"), "--clean", "--verbose")
	command.Env = serverTestEnvWithOverrides(map[string]string{
		"HOME":            cliHome,
		"USERPROFILE":     cliHome,
		"XDG_CACHE_HOME":  filepath.Join(cliHome, "cache"),
		"XDG_CONFIG_HOME": filepath.Join(cliHome, "config"),
		"LOCALAPPDATA":    filepath.Join(cliHome, "localappdata"),
		"APPDATA":         filepath.Join(cliHome, "appdata"),
		"TMPDIR":          filepath.Join(cliHome, "tmpdir"),
		"TMP":             filepath.Join(cliHome, "tmp"),
		"TEMP":            filepath.Join(cliHome, "temp"),
	})
	var stdout bytes.Buffer
	command.Stdout = &stdout
	stderr, err := command.StderrPipe()
	if err != nil {
		release()
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		release()
		t.Fatal(err)
	}
	cliDone := make(chan error, 1)
	go func() { cliDone <- command.Wait() }()
	cliFinished := false
	defer func() {
		if !cliFinished && command.Process != nil {
			_ = command.Process.Kill()
			<-cliDone
		}
	}()
	waiting := make(chan struct{})
	var waitingOnce sync.Once
	var stderrMu sync.Mutex
	var stderrText strings.Builder
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
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
	case cliErr := <-cliDone:
		cliFinished = true
		release()
		app.jobs.WaitAll(10 * time.Second)
		stderrMu.Lock()
		defer stderrMu.Unlock()
		t.Fatalf("CLI completed before observing the app data lease: err=%v stdout=%s stderr=%s", cliErr, stdout.String(), stderrText.String())
	case <-time.After(20 * time.Second):
		release()
		app.jobs.WaitAll(10 * time.Second)
		if command.Process != nil {
			_ = command.Process.Kill()
		}
		<-cliDone
		cliFinished = true
		t.Fatal("CLI did not report contention on the same-UID physical-root lease")
	}
	if info, err := os.Stat(partial); err != nil || info.Size() == 0 {
		t.Fatalf("partial disappeared while different-HOME CLI was blocked: info=%v err=%v", info, err)
	}
	if orphanCreated {
		if _, err := os.Lstat(orphan); err != nil {
			t.Fatalf("CLI clean changed the dangling friendly entry while the app body was held: %v", err)
		}
	}
	release()
	if !app.jobs.WaitAll(15 * time.Second) {
		t.Fatal("ordinary API download did not finish after its response body was released")
	}
	completed, ok := app.jobs.GetJob(job.ID)
	if !ok || completed.Status != JobStatusCompleted {
		t.Fatalf("ordinary API download completion=%+v found=%t", completed, ok)
	}
	select {
	case cliErr := <-cliDone:
		cliFinished = true
		if cliErr != nil {
			t.Fatalf("CLI rebuild after app transfer: %v stdout=%s", cliErr, stdout.String())
		}
	case <-time.After(20 * time.Second):
		if command.Process != nil {
			_ = command.Process.Kill()
		}
		<-cliDone
		cliFinished = true
		t.Fatal("CLI rebuild did not finish after the ordinary app lease released")
	}
	if orphanCreated {
		if _, err := os.Lstat(orphan); !os.IsNotExist(err) {
			t.Fatalf("clean did not remove the dangling friendly entry after release: %v", err)
		}
	}
	if got, err := os.ReadFile(filepath.Join(cacheRoot, "models", "owner", "model", "weights.safetensors")); err != nil || !bytes.Equal(got, body) {
		t.Fatalf("ordinary model after app/CLI serialization=%q err=%v", got, err)
	}
}

func buildServerTestHFDeskBinary(t *testing.T) string {
	t.Helper()
	moduleRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "hfdesk")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	build := exec.Command("go", "build", "-o", binary, "./cmd/hfdesk")
	build.Dir = moduleRoot
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI process for different-HOME coordination test: %v: %s", err, output)
	}
	return binary
}

func serverTestEnvWithOverrides(overrides map[string]string) []string {
	env := make([]string, 0, len(os.Environ())+len(overrides))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if _, replaced := overrides[key]; !replaced {
			env = append(env, entry)
		}
	}
	for key, value := range overrides {
		env = append(env, key+"="+value)
	}
	return env
}
