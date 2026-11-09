// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Cross-process destination-scope ownership over staging claims: a live
// holder excludes a second claimant even with an aged record, a crashed
// holder's scope is released at process exit so the next run resumes, and no
// payload-side legacy lock file is ever created.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

const (
	stagingOSLockChildEnv  = "HFDESK_STAGING_OSLOCK_CHILD"
	stagingOSLockConfigEnv = "HFDESK_STAGING_OSLOCK_CONFIG"
)

type stagingOSLockChildConfig struct {
	Mode         string `json:"mode"` // "hold" or "crash"
	OwnershipDir string `json:"ownership_dir"`
	Dst          string `json:"dst"`
	Staging      string `json:"staging"`
	Publish      string `json:"publish"`
	Size         int64  `json:"size"`
	Prefix       int64  `json:"prefix"`
	ReadyPath    string `json:"ready"`
	GoPath       string `json:"go"`
	ResultPath   string `json:"result"`
	Commit       string `json:"commit"`
}

type stagingOSLockChildResult struct {
	Err string `json:"err,omitempty"`
}

func writeOSLockChildResult(t *testing.T, path string, res stagingOSLockChildResult) {
	t.Helper()
	data, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal child result: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write child result: %v", err)
	}
}

// TestOwnershipScopeChildProcess is the re-executed helper that claims and
// seeds staging, then holds the ownership scope or exits without releasing it.
func TestOwnershipScopeChildProcess(t *testing.T) {
	if os.Getenv(stagingOSLockChildEnv) != "1" {
		t.Skip("helper process for the ownership scope tests")
	}
	raw, err := os.ReadFile(os.Getenv(stagingOSLockConfigEnv))
	if err != nil {
		t.Fatalf("child config: %v", err)
	}
	var cfg stagingOSLockChildConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("child config: %v", err)
	}

	full := rangedTestPayload(int(cfg.Size))
	it := PlanItem{RelativePath: filepath.Base(cfg.Publish), URL: "http://127.0.0.1:1/lock.bin", Size: cfg.Size, AcceptRanges: true}
	job := Job{Repo: "o/r"}
	metaPath := cfg.Staging + ".meta"
	// Hold the external destination scope throughout the child claim and transfer.
	ownerDir := cfg.OwnershipDir
	if ownerDir == "" {
		ownerDir = DefaultOwnershipDir()
	}
	realm, derr := newOwnershipRealm(ownerDir)
	if derr != nil {
		writeOSLockChildResult(t, cfg.ResultPath, stagingOSLockChildResult{Err: derr.Error()})
		return
	}
	domain, derr := acquireBlobDomainScoped(context.Background(), flatDomainKey(cfg.Publish), cfg.Staging, realm,
		[]string{cfg.Publish, cfg.Staging, cfg.Staging + ".meta", cfg.Staging + ".meta" + stagingRecordTempSuffix}, cfg.Publish)
	if derr != nil {
		writeOSLockChildResult(t, cfg.ResultPath, stagingOSLockChildResult{Err: derr.Error()})
		return
	}
	defer domain.release()
	if _, err := claimStaging(context.Background(), cfg.Staging, cfg.Publish, metaPath, it, job, cfg.Commit, true,
		Settings{Concurrency: 1, Retries: 0, StallTimeout: "0"}, false, domain); err != nil {
		writeOSLockChildResult(t, cfg.ResultPath, stagingOSLockChildResult{Err: err.Error()})
		return
	}

	out, err := os.OpenFile(cfg.Staging, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		writeOSLockChildResult(t, cfg.ResultPath, stagingOSLockChildResult{Err: err.Error()})
		return
	}
	if err := out.Truncate(cfg.Size); err != nil {
		out.Close()
		writeOSLockChildResult(t, cfg.ResultPath, stagingOSLockChildResult{Err: err.Error()})
		return
	}
	if _, err := out.WriteAt(full[:cfg.Prefix], 0); err != nil {
		out.Close()
		writeOSLockChildResult(t, cfg.ResultPath, stagingOSLockChildResult{Err: err.Error()})
		return
	}
	defer out.Close()
	meta, err := readStagingMeta(metaPath)
	if err != nil {
		writeOSLockChildResult(t, cfg.ResultPath, stagingOSLockChildResult{Err: err.Error()})
		return
	}
	meta.Ranges = []RangeProgress{{Start: 0, End: cfg.Size - 1, Committed: cfg.Prefix}}
	if err := writeStagingMeta(metaPath, meta); err != nil {
		writeOSLockChildResult(t, cfg.ResultPath, stagingOSLockChildResult{Err: err.Error()})
		return
	}
	// Deliberately age the payload and record: an age-based model would call
	// this owner stale. Only the held ownership scope keeps ownership here.
	past := time.Now().Add(-2 * time.Hour)
	for _, p := range []string{cfg.Staging, metaPath} {
		if err := os.Chtimes(p, past, past); err != nil {
			writeOSLockChildResult(t, cfg.ResultPath, stagingOSLockChildResult{Err: err.Error()})
			return
		}
	}

	if cfg.Mode == "crash" {
		// Exit while holding the scope; os.Exit skips defers and the kernel
		// releases the external lock.
		os.Exit(0)
	}

	// "hold": signal readiness, wait for the parent's release, then complete
	// the file under the still-held ownership scope.
	if err := os.WriteFile(cfg.ReadyPath, []byte("ready"), 0o644); err != nil {
		writeOSLockChildResult(t, cfg.ResultPath, stagingOSLockChildResult{Err: err.Error()})
		return
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(cfg.GoPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			writeOSLockChildResult(t, cfg.ResultPath, stagingOSLockChildResult{Err: "parent never released the holder"})
			return
		}
		time.Sleep(time.Millisecond)
	}
	_, werr := out.WriteAt(full[cfg.Prefix:], cfg.Prefix)
	if werr != nil {
		writeOSLockChildResult(t, cfg.ResultPath, stagingOSLockChildResult{Err: werr.Error()})
		return
	}
	if err := out.Close(); err != nil {
		writeOSLockChildResult(t, cfg.ResultPath, stagingOSLockChildResult{Err: err.Error()})
		return
	}
	if err := os.Rename(cfg.Staging, cfg.Publish); err != nil {
		writeOSLockChildResult(t, cfg.ResultPath, stagingOSLockChildResult{Err: err.Error()})
		return
	}
	_ = os.Remove(metaPath)
	domain.release()
	writeOSLockChildResult(t, cfg.ResultPath, stagingOSLockChildResult{})
}

func startStagingOSLockChild(t *testing.T, cfg stagingOSLockChildConfig) (*exec.Cmd, *bytes.Buffer) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("test executable: %v", err)
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := cfg.ResultPath + ".cfg"
	if err := os.WriteFile(cfgPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestOwnershipScopeChildProcess$", "-test.timeout=120s")
	cmd.Env = append(os.Environ(),
		stagingOSLockChildEnv+"=1",
		stagingOSLockConfigEnv+"="+cfgPath)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return cmd, &out
}

// An aged record cannot allow takeover while the child holds the scope;
// after the parent signals it, the child completes and publishes. No
// payload-side legacy lock file may ever be created.
func TestOwnershipScopeHeldByLiveProcessBlocksTakeover(t *testing.T) {
	if os.Getenv(stagingOSLockChildEnv) == "1" {
		t.Skip("child process")
	}
	if !ownershipOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	const total = 8_000
	const prefix = 3_000
	const commit = "commit-lock"
	full := rangedTestPayload(total)

	dir := t.TempDir()
	dst := filepath.Join(dir, "lock.bin")
	staging := dst + ".incomplete"
	metaPath := staging + ".meta"

	cfg := stagingOSLockChildConfig{
		Mode:         "hold",
		OwnershipDir: DefaultOwnershipDir(),
		Dst:          dst,
		Staging:      staging,
		Publish:      dst,
		Size:         total,
		Prefix:       prefix,
		ReadyPath:    filepath.Join(dir, "ready"),
		GoPath:       filepath.Join(dir, "go"),
		ResultPath:   filepath.Join(dir, "result.json"),
		Commit:       commit,
	}
	cmd, childOut := startStagingOSLockChild(t, cfg)

	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat(cfg.ReadyPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			debug, _ := os.ReadFile(cfg.ResultPath)
			t.Fatalf("helper never acquired the ownership scope (result=%q); output:\n%s", debug, childOut.String())
		}
		time.Sleep(2 * time.Millisecond)
	}
	metaBefore, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("helper record: %v", err)
	}
	metaInfo, statErr := os.Stat(metaPath)
	if statErr != nil {
		t.Fatalf("helper record stat: %v", statErr)
	}
	if age := time.Since(metaInfo.ModTime()); age < time.Hour {
		t.Fatalf("the helper record must look stale to an age-based model (age=%v)", age)
	}
	var beforeMeta IncompleteMeta
	if err := json.Unmarshal(metaBefore, &beforeMeta); err != nil {
		t.Fatal(err)
	}
	stagingBefore, err := os.ReadFile(staging)
	if err != nil {
		t.Fatal(err)
	}

	shortBlobOwnershipWait(t, 250*time.Millisecond)
	// The second claimant acquires the way production does: the ownership-realm
	// scope over real destination paths from downloadOwnershipPaths.
	ownerDir := cfg.OwnershipDir
	if ownerDir == "" {
		ownerDir = DefaultOwnershipDir()
	}
	secondRealm, rerr := newOwnershipRealm(ownerDir)
	if rerr != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("second claimant ownership realm: %v", rerr)
	}
	key, domainDst, proofPath, ownershipPaths, perr := downloadOwnershipPaths(false, nil, dst, dst, PlanItem{})
	if perr != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("second claimant ownership paths: %v", perr)
	}
	second, derr := acquireBlobDomainScoped(context.Background(), key, domainDst, secondRealm, ownershipPaths, proofPath)
	if derr == nil {
		second.release()
	}
	if !errors.Is(derr, errStagingOwnershipBusy) {
		_ = cmd.Process.Kill()
		t.Fatalf("second claimant error = %v, want errStagingOwnershipBusy while the ownership scope is held", derr)
	}
	// Cleanup must preserve the waiting child's data while its domain is held.
	if err := CleanupJobPartFiles(Settings{OutputDir: dir}, []string{dst}); err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("cleanup of a live owner's destination: %v", err)
	}
	stagingAfter, err := os.ReadFile(staging)
	if err != nil {
		t.Fatalf("live owner's staging disappeared: %v", err)
	}
	if !bytes.Equal(stagingBefore, stagingAfter) {
		t.Error("the second claimant wrote into the live owner's staging file")
	}
	metaAfter, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("live owner's record disappeared: %v", err)
	}
	if !bytes.Equal(metaBefore, metaAfter) {
		t.Error("the second claimant rewrote the live owner's claim record")
	}

	if err := os.WriteFile(cfg.GoPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("helper exited with %v; output:\n%s", err, childOut.String())
	}
	rawResult, err := os.ReadFile(cfg.ResultPath)
	if err != nil {
		t.Fatalf("helper result: %v", err)
	}
	var res stagingOSLockChildResult
	if err := json.Unmarshal(rawResult, &res); err != nil {
		t.Fatal(err)
	}
	if res.Err != "" {
		t.Fatalf("helper failed: %s", res.Err)
	}
	assertExactFile(t, dst, full)
	for _, p := range []string{staging, metaPath} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s survived the helper's successful publish, stat err=%v", filepath.Base(p), err)
		}
	}
	// Residue check: current writers never create the legacy payload-side
	// .lock file beside the payload (see the doc.go residue contract).
	if _, err := os.Stat(staging + stagingLockSuffix); !os.IsNotExist(err) {
		t.Errorf("an obsolete payload-side lock was created: %v", err)
	}
	assertExternalRealmLockFiles(t, cfg.OwnershipDir)
}

// A crashed process releases its scope; a fresh record with a committed
// prefix lets the next claimant resume exactly the missing suffix and publish.
func TestOwnershipScopeReleasedByCrashedProcess(t *testing.T) {
	if os.Getenv(stagingOSLockChildEnv) == "1" {
		t.Skip("child process")
	}
	if !ownershipOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	const total = 8_000
	const prefix = 3_000
	const commit = "commit-crash"
	full := rangedTestPayload(total)

	dir := t.TempDir()
	dst := filepath.Join(dir, "crash.bin")
	staging := dst + ".incomplete"
	metaPath := staging + ".meta"

	cfg := stagingOSLockChildConfig{
		Mode:         "crash",
		OwnershipDir: DefaultOwnershipDir(),
		Dst:          dst,
		Staging:      staging,
		Publish:      dst,
		Size:         total,
		Prefix:       prefix,
		ResultPath:   filepath.Join(dir, "result.json"),
		Commit:       commit,
	}
	cmd, childOut := startStagingOSLockChild(t, cfg)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("crashing helper exited with %v; output:\n%s", err, childOut.String())
	}
	// The destination scope must be free again once the holder died.
	ownerDir := cfg.OwnershipDir
	if ownerDir == "" {
		ownerDir = DefaultOwnershipDir()
	}
	realm, rerr := newOwnershipRealm(ownerDir)
	if rerr != nil {
		t.Fatalf("ownership realm after the owner died: %v", rerr)
	}
	_, _, _, ownershipPaths, perr := downloadOwnershipPaths(false, nil, dst, dst, PlanItem{})
	if perr != nil {
		t.Fatalf("ownership paths after the owner died: %v", perr)
	}
	busy, probeErr := realm.probeWriter(context.Background(), ownershipPaths)
	if probeErr != nil || busy {
		t.Fatalf("scope probe after the owner died = (%v, %v), want (false, nil)", busy, probeErr)
	}
	// The record is deliberately fresh (the child just wrote it) and the
	// payload holds a committed prefix.
	meta, err := readStagingMeta(metaPath)
	if err != nil {
		t.Fatalf("crashed owner's record: %v", err)
	}
	if len(meta.Ranges) != 1 || meta.Ranges[0].Committed != prefix {
		t.Fatalf("crashed owner's record = %+v, want a committed prefix of %d", meta.Ranges, prefix)
	}

	srv, gotRanges := rangedTestServer(t, full)
	it := PlanItem{RelativePath: "crash.bin", URL: srv.URL + "/crash.bin", Size: total, AcceptRanges: true}
	start := time.Now()
	if _, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"},
		Settings{Concurrency: 1, Retries: 0, StallTimeout: "0"}, it, dst,
		withTestDomain(t, dst, multipartOpts{Commit: commit, CommitPinned: true}), func(ProgressEvent) {}); err != nil {
		t.Fatalf("takeover after the holder died: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("takeover after death took %v; the ownership scope must be released at process exit", elapsed)
	}
	if len(*gotRanges) != 1 || (*gotRanges)[0] != fmt.Sprintf("bytes=%d-%d", prefix, total-1) {
		t.Errorf("takeover fetched %v, want exactly the missing suffix bytes=%d-%d", *gotRanges, prefix, total-1)
	}
	assertExactFile(t, dst, full)
	for _, p := range []string{staging, metaPath} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s survived the successful takeover publish, stat err=%v", filepath.Base(p), err)
		}
	}
	if _, err := os.Stat(staging + stagingLockSuffix); !os.IsNotExist(err) {
		t.Errorf("an obsolete payload-side lock was created: %v", err)
	}
	assertExternalRealmLockFiles(t, cfg.OwnershipDir)
}
