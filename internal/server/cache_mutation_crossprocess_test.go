// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
)

// crossProcessWriterMode names which downloader layout a re-executed writer
// child builds; each mode reads only the environment its call site sets.
type crossProcessWriterMode string

const (
	crossProcessCacheMode crossProcessWriterMode = "cache"
	crossProcessFlatMode  crossProcessWriterMode = "flat"
)

// TestCrossProcessWriterHelper runs one real download — the cache-mode or
// flat-mode layout its parent's environment selects — against the gated
// endpoint its parent supplies; it never runs standalone. The mode is derived
// from which endpoint variable the parent set, so every call site's
// environment keeps working exactly as it does today.
func TestCrossProcessWriterHelper(t *testing.T) {
	mode := crossProcessCacheMode
	if os.Getenv("HFDESK_XPROC_FLAT_ENDPOINT") != "" {
		mode = crossProcessFlatMode
	}
	runCrossProcessWriter(t, mode)
}

// runCrossProcessWriter performs the single download of the given writer
// mode, reading only the environment variables that mode's call site sets.
func runCrossProcessWriter(t *testing.T, mode crossProcessWriterMode) {
	t.Helper()
	var settings hfdownloader.Settings
	switch mode {
	case crossProcessFlatMode:
		endpoint, root := os.Getenv("HFDESK_XPROC_FLAT_ENDPOINT"), os.Getenv("HFDESK_XPROC_FLAT_ROOT")
		if endpoint == "" || root == "" {
			t.Skip("cross-process flat writer helper needs its parent fixture")
		}
		settings = hfdownloader.Settings{
			OutputDir:          root,
			OwnershipDir:       os.Getenv("HFDESK_XPROC_FLAT_OWNERSHIP"),
			Endpoint:           endpoint,
			Concurrency:        1,
			MaxActiveDownloads: 1,
			Verify:             "sha256",
		}
	default:
		endpoint, cacheDir := os.Getenv("HFDESK_XPROC_ENDPOINT"), os.Getenv("HFDESK_XPROC_CACHE")
		if endpoint == "" || cacheDir == "" {
			t.Skip("cross-process writer helper needs its parent fixture")
		}
		settings = hfdownloader.Settings{
			CacheDir:           cacheDir,
			HubDir:             filepath.Join(cacheDir, "hub"),
			Endpoint:           endpoint,
			Concurrency:        1,
			MaxActiveDownloads: 1,
			Verify:             "sha256",
		}
	}
	if err := hfdownloader.Download(context.Background(), hfdownloader.Job{Repo: "owner/model", Revision: "main"}, settings, nil); err != nil {
		t.Fatal(err)
	}
}

type crossProcessWriter struct {
	cmd    *exec.Cmd
	output bytes.Buffer
}

// startCrossProcessWriter re-executes this test binary as a foreign writer,
// passing through exactly the environment its call site provides.
func startCrossProcessWriter(t *testing.T, env ...string) *crossProcessWriter {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	writer := &crossProcessWriter{cmd: exec.Command(executable, "-test.run=^TestCrossProcessWriterHelper$")}
	writer.cmd.Env = append(os.Environ(), env...)
	writer.cmd.Stdout = &writer.output
	writer.cmd.Stderr = &writer.output
	if err := writer.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = writer.cmd.Process.Kill()
		_ = writer.cmd.Wait()
	})
	return writer
}

func (writer *crossProcessWriter) wait(t *testing.T, timeout time.Duration) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- writer.cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("cross-process writer failed: %v\n%s", err, writer.output.String())
		}
	case <-time.After(timeout):
		_ = writer.cmd.Process.Kill()
		t.Fatalf("cross-process writer unfinished after %s\n%s", timeout, writer.output.String())
	}
}

// newCrossProcessTestServer builds a scenario server and registers the shared
// teardown the cross-process fixtures always used: wait for its job manager to
// close within ten seconds, swallowing the close error.
func newCrossProcessTestServer(t *testing.T, cfg Config) *Server {
	t.Helper()
	s := New(cfg)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.jobs.Close(ctx)
	})
	return s
}

func awaitGatedWriter(t *testing.T, fx *gatedHubEndpoint) {
	t.Helper()
	select {
	case <-fx.reached:
	case <-time.After(15 * time.Second):
		t.Fatal("cross-process writer never reached its gated payload transfer")
	}
}

// crossProcessScenario carries the state the cross-process refusal scenarios
// share from setup through teardown.
type crossProcessScenario struct {
	iso      string // isolated HOME/XDG root returned by isolateCacheState
	fx       *gatedHubEndpoint
	cacheDir string
	hubDir   string
	s        *Server
	writer   *crossProcessWriter
}

// crossProcessScenarioOpts supplies the parts each scenario owns: which
// fixture to build (and any pre-server validation it performs), how to
// configure its server, what to seed before the foreign writer starts, and
// which environment the writer child reads.
type crossProcessScenarioOpts struct {
	fixture   func(*testing.T, *crossProcessScenario) *gatedHubEndpoint
	config    func(*crossProcessScenario) Config
	seed      func(*crossProcessScenario) // may be nil
	writerEnv func(*crossProcessScenario) []string
}

// startCrossProcessScenario runs the setup shape the four cross-process
// refusal scenarios share: isolate cache state, build the gated fixture,
// create the cache root and test server, run the scenario's own pre-writer
// seeding, then start the foreign writer and await its gate. Server close,
// writer kill and fixture release are registered as teardown on t; each
// scenario keeps its own seeds, requests and assertions in its own body.
func startCrossProcessScenario(t *testing.T, opts crossProcessScenarioOpts) *crossProcessScenario {
	t.Helper()
	sc := &crossProcessScenario{iso: isolateCacheState(t)}
	sc.fx = opts.fixture(t, sc)
	sc.cacheDir = t.TempDir()
	sc.hubDir = filepath.Join(sc.cacheDir, "hub")
	sc.s = newCrossProcessTestServer(t, opts.config(sc))
	if opts.seed != nil {
		opts.seed(sc)
	}
	sc.writer = startCrossProcessWriter(t, opts.writerEnv(sc)...)
	awaitGatedWriter(t, sc.fx)
	return sc
}

// cacheModeScenarioOpts is the shared option set of the three cache-mode
// cross-process refusal scenarios: the gated hub fixture, the standard
// scenario server, and the cache-mode writer environment.
func cacheModeScenarioOpts() crossProcessScenarioOpts {
	return crossProcessScenarioOpts{
		fixture: func(t *testing.T, _ *crossProcessScenario) *gatedHubEndpoint {
			return newGatedHubEndpoint(t, "weights.safetensors", []byte("gated cache weights payload"), "commit123")
		},
		config: func(sc *crossProcessScenario) Config {
			return Config{CacheDir: sc.cacheDir, MaxActive: 1, Concurrency: 1, Verify: "sha256", Endpoint: sc.fx.srv.URL}
		},
		writerEnv: func(sc *crossProcessScenario) []string {
			return []string{
				"HFDESK_XPROC_ENDPOINT=" + sc.fx.srv.URL,
				"HFDESK_XPROC_CACHE=" + sc.cacheDir,
			}
		},
	}
}

// pickDeletableGGUFGroup builds the selected-delete request for the first
// deletable model-Q4_K_M.gguf group whose location the caller's predicate
// accepts; when no such group exists the test fails under the caller's
// location label. It is the one group picker behind both the HF-cache and the
// local-root selection flows.
func pickDeletableGGUFGroup(t *testing.T, s *Server, repo string, inLocation func(cacheSelectionLocation) bool, locationLabel string) testSelectedDeleteRequest {
	t.Helper()
	code, selection := getSelection(t, s, repo, "model", "")
	if code != http.StatusOK {
		t.Fatalf("selection for %s: status=%d", repo, code)
	}
	for _, loc := range selection.Locations {
		if !inLocation(loc) {
			continue
		}
		for _, group := range loc.Groups {
			if group.CanDelete && len(group.Members) > 0 && group.Members[0].Path == "model-Q4_K_M.gguf" {
				return testSelectedDeleteRequest{Repo: repo, Type: "model", LocationID: loc.ID, GroupID: group.ID, Members: group.Members}
			}
		}
	}
	t.Fatalf("no deletable %s GGUF group for %s: %+v", locationLabel, repo, selection.Locations)
	return testSelectedDeleteRequest{}
}

// A whole-repository DELETE through the real server route must be refused
// while a writer in ANOTHER process owns the repository: this process's
// JobManager sees no activity at all, so only the cross-process admission can
// refuse. An unrelated repository stays deletable, the foreign writer still
// completes with correct bytes, and the DELETE succeeds only afterwards.
func TestCacheDeleteRefusesCrossProcessWriter(t *testing.T) {
	sc := startCrossProcessScenario(t, cacheModeScenarioOpts())
	fx, s, cacheDir, hubDir, writer := sc.fx, sc.s, sc.cacheDir, sc.hubDir, sc.writer
	repoDir := filepath.Join(hubDir, "models--owner--model")
	friendlyDir := filepath.Join(cacheDir, "models", "owner", "model")

	w := cacheRequest(t, s, http.MethodDelete, "/api/cache/owner/model", "")
	if w.Code != http.StatusConflict {
		t.Fatalf("whole-repo DELETE during a foreign live writer must be refused: status=%d body=%s", w.Code, w.Body.String())
	}
	for _, path := range []string{repoDir, filepath.Join(hubDir, ".hfdesk-control")} {
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			t.Fatalf("refused DELETE damaged %s: %v", path, err)
		}
	}

	other := filepath.Join(hubDir, "models--owner--other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	w = cacheRequest(t, s, http.MethodDelete, "/api/cache/owner/other", "")
	if w.Code != http.StatusOK {
		t.Fatalf("unrelated whole-repo DELETE was blocked by the foreign writer: status=%d body=%s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(other); !os.IsNotExist(err) {
		t.Fatalf("unrelated repository survived its DELETE: %v", err)
	}

	fx.open()
	writer.wait(t, 60*time.Second)
	assertCacheArtifacts(t, cacheDir, hubDir, fx.body, fx.hash)

	w = cacheRequest(t, s, http.MethodDelete, "/api/cache/owner/model", "")
	if w.Code != http.StatusOK {
		t.Fatalf("whole-repo DELETE after the foreign writer finished must succeed: status=%d body=%s", w.Code, w.Body.String())
	}
	for _, path := range []string{repoDir, friendlyDir} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("finished DELETE left %s behind: %v", path, err)
		}
	}
}

// Mirror pull's deleteExtra removal and its in-place payload overwrite must
// both be refused through the real server route while a writer in another
// process owns the repository: the repository survives deleteExtra, an
// existing destination blob is not truncated, the mirror result reports both
// refusals honestly, and the foreign writer still completes with correct bytes.
func TestMirrorMutationsRefuseCrossProcessWriter(t *testing.T) {
	sc := startCrossProcessScenario(t, cacheModeScenarioOpts())
	fx, s, cacheDir, hubDir, writer := sc.fx, sc.s, sc.cacheDir, sc.hubDir, sc.writer
	repoDir := filepath.Join(hubDir, "models--owner--model")

	seed := []byte("pre-existing destination payload")
	seedPath := filepath.Join(repoDir, "blobs", fx.hash)
	if err := os.MkdirAll(filepath.Dir(seedPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(seedPath, seed, 0o644); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(hubDir, "models--owner--other")
	if err := os.MkdirAll(filepath.Join(other, "blobs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "blobs", "other.bin"), []byte("other"), 0o644); err != nil {
		t.Fatal(err)
	}

	// deleteExtra must refuse the foreign writer's repository while still
	// removing an unrelated one in the same operation.
	target := filepath.Join(t.TempDir(), "target")
	payload, err := json.Marshal(map[string]any{"target": target, "deleteExtra": true})
	if err != nil {
		t.Fatal(err)
	}
	w := cacheRequest(t, s, http.MethodPost, "/api/mirror/pull", string(payload))
	var deleteResult MirrorSyncResult
	if err := json.Unmarshal(w.Body.Bytes(), &deleteResult); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusOK || deleteResult.Success {
		t.Fatalf("deleteExtra vs foreign writer: status=%d body=%s", w.Code, w.Body.String())
	}
	joinedErrors := strings.Join(deleteResult.Errors, "\n")
	if !strings.Contains(joinedErrors, "delete owner/model") || strings.Contains(joinedErrors, "delete owner/other") {
		t.Fatalf("deleteExtra errors do not isolate the refused repository: %v", deleteResult.Errors)
	}
	// The counts must match reality: only the unrelated repository was actually
	// removed, so the refused one may appear only in errors, never in deleted.
	if deleteResult.Deleted != 1 || deleteResult.DeletedSize != int64(len("other")) {
		t.Fatalf("deleteExtra counted refused removals as deleted: deleted=%d size=%d errors=%v", deleteResult.Deleted, deleteResult.DeletedSize, deleteResult.Errors)
	}
	if !strings.Contains(deleteResult.Message, "deleted 1 repos") {
		t.Fatalf("deleteExtra message does not report actual removals: %q", deleteResult.Message)
	}
	if info, err := os.Stat(repoDir); err != nil || !info.IsDir() {
		t.Fatalf("deleteExtra removed the foreign writer's repository: %v", err)
	}
	if _, err := os.Stat(other); !os.IsNotExist(err) {
		t.Fatalf("deleteExtra did not remove the unrelated repository: %v", err)
	}

	// The in-place overwrite must not truncate the destination blob whose
	// item admission the foreign writer holds.
	source := filepath.Join(t.TempDir(), "source")
	srcRepo := filepath.Join(source, "hub", "models--owner--model")
	for path, data := range map[string][]byte{
		filepath.Join(srcRepo, "blobs", fx.hash):     fx.body,
		filepath.Join(srcRepo, "blobs", "extra.bin"): []byte("extra blob"),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	payload, err = json.Marshal(map[string]any{"target": source, "force": true})
	if err != nil {
		t.Fatal(err)
	}
	w = cacheRequest(t, s, http.MethodPost, "/api/mirror/pull", string(payload))
	var copyResult MirrorSyncResult
	if err := json.Unmarshal(w.Body.Bytes(), &copyResult); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusOK || copyResult.Success {
		t.Fatalf("forced pull overwrite vs foreign writer: status=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(strings.Join(copyResult.Errors, "\n"), "copy owner/model") {
		t.Fatalf("forced pull did not report the refused overwrite: %v", copyResult.Errors)
	}
	// The refused copy must not be counted as copied: counts, message and
	// errors have to agree, at repository granularity.
	if copyResult.Copied != 0 || copyResult.CopiedSize != 0 || len(copyResult.Repos) != 0 {
		t.Fatalf("forced pull counted the refused copy: copied=%d size=%d repos=%v errors=%v", copyResult.Copied, copyResult.CopiedSize, copyResult.Repos, copyResult.Errors)
	}
	if !strings.Contains(copyResult.Message, "Copied 0 repos") {
		t.Fatalf("forced pull message does not report actual copies: %q", copyResult.Message)
	}
	if got, err := os.ReadFile(seedPath); err != nil || !bytes.Equal(got, seed) {
		t.Fatalf("mirror truncated the foreign writer's destination blob: %q %v", got, err)
	}

	// The writer completes correctly once the refused mirror steps are done.
	if err := os.Remove(seedPath); err != nil {
		t.Fatal(err)
	}
	fx.open()
	writer.wait(t, 60*time.Second)
	assertCacheArtifacts(t, cacheDir, hubDir, fx.body, fx.hash)
}

// The shared re-verified-vanish rule: a destination repository that vanished
// between scan and deletion is "nothing to remove" (like os.RemoveAll), never a
// delete failure — but only while the rule can prove it. While another admission
// of this process holds the missing target's creation keys, exactly what a
// download starting in the vanish window would hold, the removal must refuse
// instead of racing creation, and it succeeds again once those keys are released.
func TestRemoveMirrorExtraRepoReverifiedVanish(t *testing.T) {
	dstCache := filepath.Join(t.TempDir(), "hub")
	if err := os.MkdirAll(dstCache, 0o755); err != nil {
		t.Fatal(err)
	}
	vanished := mirrorEntry{Repo: "owner/gone", Path: filepath.Join(dstCache, "models--owner--gone")}

	if err := removeMirrorExtraRepo(dstCache, vanished); err != nil {
		t.Fatalf("re-verified vanished destination reported a delete failure: %v", err)
	}
	if _, err := os.Stat(vanished.Path); !os.IsNotExist(err) {
		t.Fatalf("vanished destination materialized during removal: %v", err)
	}

	creator, err := hfdownloader.OpenCacheAdmission(context.Background(), dstCache)
	if err != nil {
		t.Fatal(err)
	}
	hold, err := creator.AdmitTreeRemoval(context.Background(), vanished.Path)
	if err != nil {
		t.Fatalf("vanished target was not admitted under creator-side keys: %v", err)
	}
	err = removeMirrorExtraRepo(dstCache, vanished)
	if err == nil || !strings.Contains(err.Error(), "delete owner/gone") {
		t.Fatalf("removal proceeded while a creator of the vanished target held admission: %v", err)
	}
	hold()
	if err := removeMirrorExtraRepo(dstCache, vanished); err != nil {
		t.Fatalf("removal after the creator-side keys were released: %v", err)
	}
}

// An admission failure that only resembles a vanished target — here the control
// node's bootstrap lock is unavailable while the payload tree exists — must never
// be treated as a vanish: no release is returned, so every caller fails closed.
func TestTreeRemovalAdmissionFailsClosedOverExistingTree(t *testing.T) {
	dstCache := filepath.Join(t.TempDir(), "hub")
	repo := filepath.Join(dstCache, "models--owner--model")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	admission, err := hfdownloader.OpenCacheAdmission(context.Background(), dstCache)
	if err != nil {
		t.Fatal(err)
	}
	// Remove the root control node's metadata after the realm is open: resolving
	// the still-existing tree then fails with a wrapped ENOENT from control-node
	// bootstrap, which is not proof that the target is gone.
	if err := os.RemoveAll(filepath.Join(dstCache, ".hfdesk-control", "v1", "root", "metadata")); err != nil {
		t.Fatal(err)
	}
	release, err := admission.AdmitTreeRemoval(context.Background(), repo)
	if err == nil {
		release()
		t.Fatal("admission over an existing tree succeeded although its control node could not be bootstrapped")
	}
	if !strings.Contains(err.Error(), "bootstrap cache node") {
		t.Fatalf("expected the control-bootstrap failure, got: %v", err)
	}
	// The failure means "admission could not be established", never "the
	// target is gone": it must not carry an ENOENT identity a future caller
	// could mistake for a vanished target (fail-open trap).
	if errors.Is(err, os.ErrNotExist) {
		t.Fatalf("admission failure must not be readable as a vanished target: %v", err)
	}
	if _, statErr := os.Stat(repo); statErr != nil {
		t.Fatalf("existing repository did not survive the failed admission: %v", statErr)
	}
}

// seedSelectedGGUFGroup places a selectable GGUF (regular snapshot entry with
// content matching its blob name, so payload association resolves by digest)
// into the HF cache layout for the given repository.
func seedSelectedGGUFGroup(t *testing.T, hubDir, owner, name, version, fileName string, content []byte) (snapshotEntry, blobPath string) {
	t.Helper()
	repoDir := filepath.Join(hubDir, "models--"+owner+"--"+name)
	blobPath = filepath.Join(repoDir, "blobs", fmt.Sprintf("%x", sha256.Sum256(content)))
	if err := os.MkdirAll(filepath.Dir(blobPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blobPath, content, 0o644); err != nil {
		t.Fatal(err)
	}
	snapshotEntry = filepath.Join(repoDir, "snapshots", version, fileName)
	if err := os.MkdirAll(filepath.Dir(snapshotEntry), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(snapshotEntry, content, 0o644); err != nil {
		t.Fatal(err)
	}
	return snapshotEntry, blobPath
}

// A selected-GGUF delete through the real DELETE /api/cache-selection route must
// be refused BEFORE any unlink while a writer in ANOTHER process is publishing
// into the same repository: this process's JobManager sees no activity, so only
// the cross-process admission can refuse. An unrelated selection still succeeds
// while that writer is in flight, the foreign writer completes with correct
// bytes, and the refused selection deletes normally afterwards without touching
// the writer's output.
func TestSelectedDeleteRefusesCrossProcessWriter(t *testing.T) {
	sc := startCrossProcessScenario(t, cacheModeScenarioOpts())
	fx, s, cacheDir, hubDir, writer := sc.fx, sc.s, sc.cacheDir, sc.hubDir, sc.writer

	seededEntry, seededBlob := seedSelectedGGUFGroup(t, hubDir, "owner", "model", "seedcommit", "model-Q4_K_M.gguf", []byte("seeded gguf weights for owner/model"))
	otherEntry, _ := seedSelectedGGUFGroup(t, hubDir, "owner", "other", "seedcommit", "model-Q4_K_M.gguf", []byte("seeded gguf weights for owner/other"))

	// The HF-cache location filter: only a deletable HF-cache location may
	// yield the group, exactly as before the picker was unified.
	pickGroup := func(repo string) testSelectedDeleteRequest {
		t.Helper()
		return pickDeletableGGUFGroup(t, s, repo, func(loc cacheSelectionLocation) bool {
			return loc.Source == "HF cache" && loc.CanDelete
		}, "HF")
	}

	// The foreign writer owns owner/model: its selection must be refused with the
	// established 409 before any unlink.
	body, err := json.Marshal(pickGroup("owner/model"))
	if err != nil {
		t.Fatal(err)
	}
	w := cacheRequest(t, s, http.MethodDelete, "/api/cache-selection", string(body))
	if w.Code != http.StatusConflict {
		t.Fatalf("selected-GGUF delete during a foreign live writer must be refused: status=%d body=%s", w.Code, w.Body.String())
	}
	for _, path := range []string{seededEntry, seededBlob} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("refused selected delete unlinked %s: %v", path, err)
		}
	}

	// An unrelated selection still succeeds while that writer is in flight.
	body, err = json.Marshal(pickGroup("owner/other"))
	if err != nil {
		t.Fatal(err)
	}
	w = cacheRequest(t, s, http.MethodDelete, "/api/cache-selection", string(body))
	if w.Code != http.StatusOK {
		t.Fatalf("unrelated selected delete was blocked by the foreign writer: status=%d body=%s", w.Code, w.Body.String())
	}
	var unrelated selectedDeleteResponse
	if err := json.Unmarshal(w.Body.Bytes(), &unrelated); err != nil {
		t.Fatal(err)
	}
	if !unrelated.OK || len(unrelated.Removed) == 0 {
		t.Fatalf("unrelated selected delete did not remove its entries: %+v", unrelated)
	}
	if _, err := os.Lstat(otherEntry); !os.IsNotExist(err) {
		t.Fatalf("unrelated selection left its entry behind: %v", err)
	}

	// The writer completes with correct bytes.
	fx.open()
	writer.wait(t, 60*time.Second)
	assertCacheArtifacts(t, cacheDir, hubDir, fx.body, fx.hash)

	// Afterwards the refused selection deletes normally, and the foreign
	// writer's completed output survives it.
	body, err = json.Marshal(pickGroup("owner/model"))
	if err != nil {
		t.Fatal(err)
	}
	w = cacheRequest(t, s, http.MethodDelete, "/api/cache-selection", string(body))
	if w.Code != http.StatusOK {
		t.Fatalf("selected delete after the foreign writer finished must succeed: status=%d body=%s", w.Code, w.Body.String())
	}
	for _, path := range []string{seededEntry, seededBlob} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("finished selection left %s behind: %v", path, err)
		}
	}
	assertCacheArtifacts(t, cacheDir, hubDir, fx.body, fx.hash)
}

// The local-root branch of selected-delete must be refused BEFORE any unlink
// while a flat-mode writer in ANOTHER process holds the exact member file: this
// process's JobManager sees no activity, so only the flat-realm admission can
// refuse. The seeded member survives untouched, an unrelated local selection
// still succeeds while that writer is in flight, the writer then completes with
// correct bytes over the seed, and the refused selection deletes normally
// afterwards.
//
// TestCrossProcessWriterHelper runs its flat mode for this scenario; the
// fixture below serves a flat local-layout tree (one GGUF payload) so a
// flat-mode download in another process stays provably in flight while the
// test drives local selected deletion through the real server route.
func TestLocalSelectedDeleteRefusesCrossProcessWriter(t *testing.T) {
	root := t.TempDir()
	seed := []byte("seeded local gguf for owner/model") // differs from the served payload, so the writer cannot skip it
	seedPath := filepath.Join(root, "owner", "model", "model-Q4_K_M.gguf")
	otherPath := filepath.Join(root, "owner", "other", "model-Q4_K_M.gguf")
	var ownershipDir string
	sc := startCrossProcessScenario(t, crossProcessScenarioOpts{
		fixture: func(t *testing.T, sc *crossProcessScenario) *gatedHubEndpoint {
			fx := newGatedHubEndpoint(t, "model-Q4_K_M.gguf", []byte("gated flat local gguf payload"), "flatcommit")
			// The handler resolves the default realm from this process's UserConfigDir;
			// give the foreign writer the same explicit OwnershipDir (its own TestMain
			// would otherwise rederive a different default inside the child).
			ucd, err := os.UserConfigDir()
			if err != nil {
				t.Fatal(err)
			}
			ownershipDir = filepath.Join(ucd, "HFDesk", "ownership")
			if want := filepath.Join(sc.iso, "HFDesk", "ownership"); ownershipDir != want {
				t.Fatalf("default realm %q does not match the isolated sandbox %q", ownershipDir, want)
			}
			return fx
		},
		config: func(sc *crossProcessScenario) Config {
			return Config{CacheDir: t.TempDir(), MaxActive: 1, Concurrency: 1, Verify: "sha256", Endpoint: sc.fx.srv.URL, LocalScanDirs: []string{root}}
		},
		seed: func(sc *crossProcessScenario) {
			for path, data := range map[string][]byte{
				seedPath:  seed,
				otherPath: []byte("seeded local gguf for owner/other"),
			} {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, data, 0o644); err != nil {
					t.Fatal(err)
				}
			}
		},
		writerEnv: func(sc *crossProcessScenario) []string {
			return []string{
				"HFDESK_XPROC_FLAT_ENDPOINT=" + sc.fx.srv.URL,
				"HFDESK_XPROC_FLAT_ROOT=" + root,
				// The helper's own TestMain overwrites XDG_CONFIG_HOME with its sandbox,
				// so the default realm would not match this process's: point both sides
				// at one explicit OwnershipDir, exactly the documented shared-realm case.
				"HFDESK_XPROC_FLAT_OWNERSHIP=" + ownershipDir,
			}
		},
	})
	fx, s, writer := sc.fx, sc.s, sc.writer

	// The local- ID-prefix location filter: only a deletable local- location
	// may yield the group, exactly as before the picker was unified.
	pickLocalGroup := func(repo string) testSelectedDeleteRequest {
		t.Helper()
		return pickDeletableGGUFGroup(t, s, repo, func(loc cacheSelectionLocation) bool {
			return strings.HasPrefix(loc.ID, "local-") && loc.CanDelete
		}, "local")
	}

	body, err := json.Marshal(pickLocalGroup("owner/model"))
	if err != nil {
		t.Fatal(err)
	}
	w := cacheRequest(t, s, http.MethodDelete, "/api/cache-selection", string(body))
	if w.Code != http.StatusConflict {
		t.Fatalf("local selected-GGUF delete during a foreign flat writer must be refused: status=%d body=%s", w.Code, w.Body.String())
	}
	if got, readErr := os.ReadFile(seedPath); readErr != nil || !bytes.Equal(got, seed) {
		t.Fatalf("refused local selected delete touched the member: %q %v", got, readErr)
	}

	// An unrelated local selection still succeeds while that writer is in flight.
	body, err = json.Marshal(pickLocalGroup("owner/other"))
	if err != nil {
		t.Fatal(err)
	}
	w = cacheRequest(t, s, http.MethodDelete, "/api/cache-selection", string(body))
	if w.Code != http.StatusOK {
		t.Fatalf("unrelated local selected delete was blocked by the foreign writer: status=%d body=%s", w.Code, w.Body.String())
	}
	var unrelated selectedDeleteResponse
	if err := json.Unmarshal(w.Body.Bytes(), &unrelated); err != nil {
		t.Fatal(err)
	}
	if !unrelated.OK || len(unrelated.Removed) == 0 {
		t.Fatalf("unrelated local selected delete did not remove its member: %+v", unrelated)
	}
	if _, err := os.Lstat(otherPath); !os.IsNotExist(err) {
		t.Fatalf("unrelated local selection left its member behind: %v", err)
	}

	// The writer completes with correct bytes over the seed.
	fx.open()
	writer.wait(t, 60*time.Second)
	if got, readErr := os.ReadFile(seedPath); readErr != nil || !bytes.Equal(got, fx.body) {
		t.Fatalf("foreign flat writer's output missing after the refused delete: %q %v", got, readErr)
	}

	// Afterwards the refused selection deletes normally.
	body, err = json.Marshal(pickLocalGroup("owner/model"))
	if err != nil {
		t.Fatal(err)
	}
	w = cacheRequest(t, s, http.MethodDelete, "/api/cache-selection", string(body))
	if w.Code != http.StatusOK {
		t.Fatalf("local selected delete after the foreign writer finished must succeed: status=%d body=%s", w.Code, w.Body.String())
	}
	if _, err := os.Lstat(seedPath); !os.IsNotExist(err) {
		t.Fatalf("finished local selection left %s behind: %v", seedPath, err)
	}
}

const (
	xprocHoldRootEnv  = "HFDESK_XPROC_HOLD_ROOT"
	xprocHoldEntryEnv = "HFDESK_XPROC_HOLD_ENTRY"
	xprocHoldTreeEnv  = "HFDESK_XPROC_HOLD_TREE"
	xprocHoldReadyEnv = "HFDESK_XPROC_HOLD_READY"
	xprocHoldGoEnv    = "HFDESK_XPROC_HOLD_GO"
)

// TestCrossProcessScopeHolderHelper holds one destination admission scope — an
// entry's scope or a tree's — for its parent test; it never runs standalone.
func TestCrossProcessScopeHolderHelper(t *testing.T) {
	entry, tree := os.Getenv(xprocHoldEntryEnv), os.Getenv(xprocHoldTreeEnv)
	if entry == "" && tree == "" {
		t.Skip("scope holder helper needs its parent fixture")
	}
	admission, err := hfdownloader.OpenCacheAdmission(context.Background(), os.Getenv(xprocHoldRootEnv))
	if err != nil {
		t.Fatal(err)
	}
	var release func()
	if tree != "" {
		release, err = admission.AdmitTreeRemoval(context.Background(), tree)
	} else {
		release, err = admission.AdmitEntryReplacement(context.Background(), entry)
	}
	if err != nil {
		t.Fatalf("hold scope: %v", err)
	}
	defer release()
	if err := os.WriteFile(os.Getenv(xprocHoldReadyEnv), []byte("ready"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, os.Getenv(xprocHoldGoEnv))
}

type crossProcessScopeHolder struct {
	cmd               *exec.Cmd
	output            bytes.Buffer
	readyPath, goPath string
}

// startCrossProcessScopeHolder re-executes this test binary as a holder of one
// cache admission scope, following the cross-process writer helper's shape.
func startCrossProcessScopeHolder(t *testing.T, root, entryPath, treePath string) *crossProcessScopeHolder {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	markerDir := t.TempDir()
	holder := &crossProcessScopeHolder{
		cmd:       exec.Command(exe, "-test.run=^TestCrossProcessScopeHolderHelper$"),
		readyPath: filepath.Join(markerDir, "ready"),
		goPath:    filepath.Join(markerDir, "go"),
	}
	holder.cmd.Env = append(os.Environ(),
		xprocHoldRootEnv+"="+root,
		xprocHoldEntryEnv+"="+entryPath,
		xprocHoldTreeEnv+"="+treePath,
		xprocHoldReadyEnv+"="+holder.readyPath,
		xprocHoldGoEnv+"="+holder.goPath)
	holder.cmd.Stdout = &holder.output
	holder.cmd.Stderr = &holder.output
	if err := holder.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = holder.cmd.Process.Kill()
		_ = holder.cmd.Wait()
	})
	return holder
}

func (holder *crossProcessScopeHolder) await(t *testing.T) {
	t.Helper()
	waitForFile(t, holder.readyPath)
}

func (holder *crossProcessScopeHolder) release(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(holder.goPath, []byte("go"), 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- holder.cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("scope holder failed: %v\n%s", err, holder.output.String())
		}
	case <-time.After(30 * time.Second):
		_ = holder.cmd.Process.Kill()
		t.Fatalf("scope holder did not finish\n%s", holder.output.String())
	}
}

// Mirror pull's snapshot symlink-leaf replacement and its directory creation
// must both be refused through the real server route while a holder in ANOTHER
// process owns the destination entry/tree scope: this process's JobManager
// sees no activity, so only the cross-process admission can refuse. The held
// repository survives untouched, an unrelated repository still mirrors while
// the holder is in flight, and both operations succeed afterwards.
func TestMirrorEntryAndDirectoryMutationsRefuseCrossProcessHolder(t *testing.T) {
	isolateCacheState(t)
	cacheDir := t.TempDir()
	hubDir := filepath.Join(cacheDir, "hub")
	s := newCrossProcessTestServer(t, Config{CacheDir: cacheDir, MaxActive: 1, Concurrency: 1, Verify: "sha256"})

	blobName := strings.Repeat("a", 64)
	payload := []byte("mirror entry replacement payload")
	source := t.TempDir()
	srcRepo := filepath.Join(source, "hub", "models--owner--model")
	srcOther := filepath.Join(source, "hub", "models--owner--other")
	for path, data := range map[string][]byte{
		filepath.Join(srcRepo, "blobs", blobName):     payload,
		filepath.Join(srcOther, "blobs", "other.bin"): []byte("other"),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	srcEntry := filepath.Join(srcRepo, "snapshots", "seedcommit", "model.bin")
	if err := os.MkdirAll(filepath.Dir(srcEntry), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "..", "blobs", blobName), srcEntry); err != nil {
		t.Fatal(err)
	}

	pull := func() MirrorSyncResult {
		t.Helper()
		body, err := json.Marshal(map[string]any{"target": source, "force": true})
		if err != nil {
			t.Fatal(err)
		}
		w := cacheRequest(t, s, http.MethodPost, "/api/mirror/pull", string(body))
		if w.Code != http.StatusOK {
			t.Fatalf("mirror pull: status=%d body=%s", w.Code, w.Body.String())
		}
		var res MirrorSyncResult
		if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		return res
	}

	dstRepo := filepath.Join(hubDir, "models--owner--model")
	dstEntry := filepath.Join(dstRepo, "snapshots", "seedcommit", "model.bin")
	if err := os.MkdirAll(filepath.Dir(dstEntry), 0o755); err != nil {
		t.Fatal(err)
	}
	oldTarget := filepath.Join("..", "..", "blobs", strings.Repeat("b", 64))
	if err := os.Symlink(oldTarget, dstEntry); err != nil {
		t.Fatal(err)
	}

	// Entry-replacement class: the destination snapshot entry's scope is held
	// by another process, so the symlink-leaf replacement is refused while the
	// admitted file copy of the unrelated repository still proceeds.
	holder := startCrossProcessScopeHolder(t, hubDir, dstEntry, "")
	holder.await(t)
	res := pull()
	joinedErrors := strings.Join(res.Errors, "\n")
	if res.Success || !strings.Contains(joinedErrors, "copy owner/model") || !strings.Contains(joinedErrors, "admit entry replacement") {
		t.Fatalf("symlink-leaf replacement did not report its refused admission: success=%v errors=%v", res.Success, res.Errors)
	}
	if got, err := os.Readlink(dstEntry); err != nil || got != oldTarget {
		t.Fatalf("destination entry mutated while its scope was held: %q %v", got, err)
	}
	if res.Copied != 1 || len(res.Repos) != 1 || res.Repos[0] != "owner/other" {
		t.Fatalf("unrelated repository did not mirror while the holder was in flight: copied=%d repos=%v errors=%v", res.Copied, res.Repos, res.Errors)
	}
	holder.release(t)

	// With the holder gone the replacement completes normally.
	if err := os.RemoveAll(dstRepo); err != nil {
		t.Fatal(err)
	}
	res = pull()
	if !res.Success || res.Copied != 1 {
		t.Fatalf("mirror after release: success=%v copied=%d errors=%v", res.Success, res.Copied, res.Errors)
	}
	gotTarget, err := os.Readlink(dstEntry)
	if err != nil {
		t.Fatalf("destination entry missing after admitted replacement: %v", err)
	}

	// Directory-creation class: the destination repository's tree scope is held
	// by another process, so creating directories inside it is refused before
	// any directory materializes.
	if err := os.RemoveAll(dstRepo); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dstRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	treeHolder := startCrossProcessScopeHolder(t, hubDir, "", dstRepo)
	treeHolder.await(t)
	res = pull()
	joinedErrors = strings.Join(res.Errors, "\n")
	if res.Success || !strings.Contains(joinedErrors, "copy owner/model") || !strings.Contains(joinedErrors, "admit directory") {
		t.Fatalf("directory creation did not report its refused admission: success=%v errors=%v", res.Success, res.Errors)
	}
	if entries, err := os.ReadDir(dstRepo); err != nil || len(entries) != 0 {
		t.Fatalf("held repository gained directories during the refused mirror: %v %v", entries, err)
	}
	treeHolder.release(t)

	res = pull()
	if !res.Success || res.Copied != 1 {
		t.Fatalf("mirror after the tree holder released: success=%v copied=%d errors=%v", res.Success, res.Copied, res.Errors)
	}
	gotAgain, err := os.Readlink(dstEntry)
	if err != nil || gotAgain != gotTarget {
		t.Fatalf("destination entry after the admitted replacement: %q %v (want %q)", gotAgain, err, gotTarget)
	}
	if got, err := os.ReadFile(filepath.Join(dstRepo, "blobs", blobName)); err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("mirrored payload: %q %v", got, err)
	}
}
