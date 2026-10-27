// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Covers ownership probes and claim checks, plus live/crashed helper processes.

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

func TestStagingOwnershipLockProbe(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	staging := filepath.Join(t.TempDir(), "x.incomplete")

	if busy, err := stagingOwnershipBusy(staging); err != nil || busy {
		t.Fatalf("probe of a never-created lock = (%v, %v), want (false, nil)", busy, err)
	}
	lock, err := acquireStagingOwnershipLock(staging)
	if err != nil {
		t.Fatal(err)
	}
	if busy, err := stagingOwnershipBusy(staging); err != nil || !busy {
		t.Fatalf("probe under a held lock = (%v, %v), want (true, nil)", busy, err)
	}
	lock.release()
	if busy, err := stagingOwnershipBusy(staging); err != nil || busy {
		t.Fatalf("probe after release = (%v, %v), want (false, nil)", busy, err)
	}
	// The lock file is persistent by design: unlinking it would allow two
	// owners on different inodes.
	if _, err := os.Stat(stagingOwnershipLockPath(staging)); err != nil {
		t.Errorf("the lock file must persist after release: %v", err)
	}
}

// Publish refuses missing/foreign claims under the ownership lock and still
// accepts an owned claim with a complete layout.
func TestStagingPublishRefusesForeignClaim(t *testing.T) {
	payload := []byte("someone else's payload")
	makeDownload := func(t *testing.T) (*stagingDownload, string, string) {
		t.Helper()
		dir := t.TempDir()
		staging := filepath.Join(dir, "x.incomplete")
		pub := filepath.Join(dir, "x.bin")
		if err := os.WriteFile(staging, payload, 0o644); err != nil {
			t.Fatal(err)
		}
		domain := acquireTestDomain(t, pub)
		d := &stagingDownload{
			ctx:    context.Background(),
			dst:    pub,
			stg:    staging,
			pub:    pub,
			meta:   staging + ".meta",
			it:     PlanItem{RelativePath: "x.bin", Size: int64(len(payload))},
			ranges: completeRangeLayout(int64(len(payload)), 1),
			domain: domain,
		}
		return d, staging, pub
	}

	t.Run("foreign record", func(t *testing.T) {
		d, staging, pub := makeDownload(t)
		foreign := IncompleteMeta{
			PID:       os.Getpid(),
			Instance:  "another-process-instance",
			Host:      processHostname,
			StartedAt: time.Now().UTC(),
			Size:      int64(len("someone else's payload")),
			Version:   stagingMetaVersion,
			Repo:      "o/r",
			Path:      "x.bin",
			Ranges:    buildRangeLayout(int64(len("someone else's payload")), 1),
		}
		if err := writeStagingMeta(d.meta, &foreign); err != nil {
			t.Fatal(err)
		}
		if err := d.publish(); !errors.Is(err, errStagingClaimLost) {
			t.Fatalf("publish with a foreign record = %v, want errStagingClaimLost", err)
		}
		if _, err := os.Stat(staging); err != nil {
			t.Errorf("publish moved a foreign staging file: %v", err)
		}
		if _, err := os.Stat(pub); !os.IsNotExist(err) {
			t.Errorf("publish created the destination under a foreign record, stat err=%v", err)
		}
	})

	t.Run("missing record", func(t *testing.T) {
		d, staging, pub := makeDownload(t)
		if err := d.publish(); !errors.Is(err, errStagingClaimLost) {
			t.Fatalf("publish with a missing record = %v, want errStagingClaimLost", err)
		}
		if _, err := os.Stat(staging); err != nil {
			t.Errorf("publish moved an unclaimed staging file: %v", err)
		}
		if _, err := os.Stat(pub); !os.IsNotExist(err) {
			t.Errorf("publish created the destination without a claim, stat err=%v", err)
		}
	})

	t.Run("owned record still publishes", func(t *testing.T) {
		d, staging, pub := makeDownload(t)
		ours := IncompleteMeta{
			PID:       os.Getpid(),
			Instance:  processInstanceToken,
			Host:      processHostname,
			StartedAt: time.Now().UTC(),
			Size:      int64(len("someone else's payload")),
			Version:   stagingMetaVersion,
			Repo:      "o/r",
			Path:      "x.bin",
			Ranges:    completeRangeLayout(int64(len("someone else's payload")), 1),
		}
		if err := writeStagingMeta(d.meta, &ours); err != nil {
			t.Fatal(err)
		}
		if err := d.publish(); err != nil {
			t.Fatalf("publish of an owned record: %v", err)
		}
		if _, err := os.Stat(staging); !os.IsNotExist(err) {
			t.Errorf("published staging file survived, stat err=%v", err)
		}
		if got, err := os.ReadFile(pub); err != nil || string(got) != "someone else's payload" {
			t.Errorf("published payload = %q (err=%v)", got, err)
		}
	})
}

const (
	stagingOSLockChildEnv  = "HFDESK_STAGING_OSLOCK_CHILD"
	stagingOSLockConfigEnv = "HFDESK_STAGING_OSLOCK_CONFIG"
)

type stagingOSLockChildConfig struct {
	Mode       string `json:"mode"` // "hold" or "crash"
	Dst        string `json:"dst"`
	Staging    string `json:"staging"`
	Publish    string `json:"publish"`
	Size       int64  `json:"size"`
	Prefix     int64  `json:"prefix"`
	ReadyPath  string `json:"ready"`
	GoPath     string `json:"go"`
	ResultPath string `json:"result"`
	Commit     string `json:"commit"`
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

// TestStagingOSLockChildProcess is the re-executed helper that claims and seeds
// staging, then holds the lock or exits without releasing it.
func TestStagingOSLockChildProcess(t *testing.T) {
	if os.Getenv(stagingOSLockChildEnv) != "1" {
		t.Skip("helper process for the staging ownership lock tests")
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
	// Hold the destination domain throughout the child claim and transfer.
	domain, derr := acquireBlobDomain(context.Background(), flatDomainKey(cfg.Publish), cfg.Staging)
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
	// this owner stale. Only the OS ownership lock keeps ownership here.
	past := time.Now().Add(-2 * time.Hour)
	for _, p := range []string{cfg.Staging, metaPath} {
		if err := os.Chtimes(p, past, past); err != nil {
			writeOSLockChildResult(t, cfg.ResultPath, stagingOSLockChildResult{Err: err.Error()})
			return
		}
	}

	if cfg.Mode == "crash" {
		// Exit while holding the domain; os.Exit skips defers and the kernel releases it.
		os.Exit(0)
	}

	// "hold": signal readiness, wait for the parent's release, then complete
	// the file under the still-held ownership lock.
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
	cmd := exec.Command(exe, "-test.run=^TestStagingOSLockChildProcess$", "-test.timeout=120s")
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

// An aged record cannot allow takeover while the child holds the domain;
// after the parent signals it, the child completes and publishes.
func TestStagingOSLockHeldByLiveProcessBlocksTakeover(t *testing.T) {
	if os.Getenv(stagingOSLockChildEnv) == "1" {
		t.Skip("child process")
	}
	if !stagingOSLockAvailable {
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
		Mode:       "hold",
		Dst:        dst,
		Staging:    staging,
		Publish:    dst,
		Size:       total,
		Prefix:     prefix,
		ReadyPath:  filepath.Join(dir, "ready"),
		GoPath:     filepath.Join(dir, "go"),
		ResultPath: filepath.Join(dir, "result.json"),
		Commit:     commit,
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
			t.Fatalf("helper never acquired the ownership lock (result=%q); output:\n%s", debug, childOut.String())
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
	if _, derr := blobDomainAcquire(context.Background(), flatDomainKey(dst), flatDomainLockSrc(dst)); !errors.Is(derr, errStagingOwnershipBusy) {
		_ = cmd.Process.Kill()
		t.Fatalf("second claimant error = %v, want errStagingOwnershipBusy while the OS lock is held", derr)
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
	if _, err := os.Stat(stagingOwnershipLockPath(staging)); err != nil {
		t.Errorf("the persistent ownership lock file was removed: %v", err)
	}
}

// A crashed process releases its lock; the next claimant resumes and publishes.
func TestStagingOSLockReleasedByCrashedProcess(t *testing.T) {
	if os.Getenv(stagingOSLockChildEnv) == "1" {
		t.Skip("child process")
	}
	if !stagingOSLockAvailable {
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
		Mode:       "crash",
		Dst:        dst,
		Staging:    staging,
		Publish:    dst,
		Size:       total,
		Prefix:     prefix,
		ResultPath: filepath.Join(dir, "result.json"),
		Commit:     commit,
	}
	cmd, childOut := startStagingOSLockChild(t, cfg)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("crashing helper exited with %v; output:\n%s", err, childOut.String())
	}
	if busy, err := stagingOwnershipBusy(staging); err != nil || busy {
		t.Fatalf("probe after the owner died = (%v, %v), want (false, nil)", busy, err)
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
		t.Errorf("takeover after death took %v; the OS lock must be released at process exit", elapsed)
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
	if _, err := os.Stat(stagingOwnershipLockPath(staging)); err != nil {
		t.Errorf("the persistent ownership lock file was removed: %v", err)
	}
}
