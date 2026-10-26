// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// OS-level staging ownership coverage for issue #140 (T4 correction):
//   - the non-blocking ownership probe and CheckBlob's use of it;
//   - publish refuses a missing/foreign record and leaves the staging alone;
//   - a real helper process holding the lock (with a deliberately stale-looking
//     record) blocks takeover, and completes when released;
//   - a helper process that dies while holding the lock releases it at death,
//     so the next claimant resumes the committed prefix and publishes.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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

// TestCheckBlobHeldOwnershipLockReportsDownloading pins the status consumer: a
// held OS ownership lock reports an active writer even if the record carries
// this process's own instance token (a suspended/active owner is still live).
func TestCheckBlobHeldOwnershipLockReportsDownloading(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	cache := NewHFCache(t.TempDir(), time.Minute)
	repo, err := cache.Repo("owner/name", RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("e", 64)
	staging := repo.IncompletePath(sha)
	if err := os.WriteFile(staging, []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	meta := IncompleteMeta{
		PID:        os.Getpid(),
		Instance:   processInstanceToken,
		Host:       processHostname,
		StartedAt:  time.Now().UTC(),
		Size:       100,
		Version:    stagingMetaVersion,
		Repo:       "o/r",
		Path:       "x.bin",
		Ranges:     buildRangeLayout(100, 2),
		LockFenced: true,
	}
	if err := writeStagingMeta(repo.IncompleteMetaPath(sha), &meta); err != nil {
		t.Fatal(err)
	}

	lock, err := acquireStagingOwnershipLock(staging)
	if err != nil {
		t.Fatal(err)
	}
	status, _, err := repo.CheckBlob(sha)
	lock.release()
	if err != nil {
		t.Fatal(err)
	}
	if status != BlobDownloading {
		t.Errorf("held ownership lock status = %v, want BlobDownloading", status)
	}
	// With the lock released, the same-instance record is an earlier run of
	// this process: take-over-able.
	if status, _, err = repo.CheckBlob(sha); err != nil || status != BlobStale {
		t.Errorf("released ownership lock status = %v (err=%v), want BlobStale", status, err)
	}
}

// TestStagingPublishRefusesForeignClaim pins B2: publish re-reads the record
// under the transition lock while it holds the OS ownership lock, and refuses
// a missing or foreign record with errStagingClaimLost instead of renaming
// another writer's staging file.
//
// The record the owned case installs is a COMPLETE layout, because publish
// also checks the publication invariant (every range Done, committed total
// equal to the expected size) before it renames. The ownership semantics
// under test are unchanged: an owned, complete record still publishes.
func TestStagingPublishRefusesForeignClaim(t *testing.T) {
	payload := []byte("someone else's payload")
	makeDownload := func(t *testing.T) (*stagingDownload, string, string, *stagingOwnershipLock) {
		t.Helper()
		dir := t.TempDir()
		staging := filepath.Join(dir, "x.incomplete")
		pub := filepath.Join(dir, "x.bin")
		if err := os.WriteFile(staging, payload, 0o644); err != nil {
			t.Fatal(err)
		}
		own, oerr := acquireStagingOwnershipLock(staging)
		if oerr != nil {
			t.Fatalf("acquire ownership lock: %v", oerr)
		}
		d := &stagingDownload{
			ctx:         context.Background(),
			dst:         pub,
			stg:         staging,
			pub:         pub,
			meta:        staging + ".meta",
			it:          PlanItem{RelativePath: "x.bin", Size: int64(len(payload))},
			ranges:      completeRangeLayout(int64(len(payload)), 1),
			osExclusive: own != nil,
			own:         own,
		}
		return d, staging, pub, own
	}

	t.Run("foreign record", func(t *testing.T) {
		d, staging, pub, own := makeDownload(t)
		defer own.release()
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
		d, staging, pub, own := makeDownload(t)
		defer own.release()
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
		d, staging, pub, own := makeDownload(t)
		defer own.release()
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

// TestStagingOSLockChildProcess is the re-executed helper used by the
// real-process ownership tests. It claims the staging pair through the real
// claim path (so the OS lock and the record are both established), seeds a
// committed prefix, and then either holds the lock until the parent releases it
// or exits abruptly without releasing.
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
	state, err := claimStaging(context.Background(), cfg.Staging, cfg.Publish, metaPath, it, job, cfg.Commit,
		Settings{Concurrency: 1, Retries: 0, StallTimeout: "0"}, false)
	if err != nil {
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
	// Deliberately age the payload and record: the lease model would call this
	// owner stale. Only the OS ownership lock keeps ownership here.
	past := time.Now().Add(-2 * DefaultStaleTimeout)
	for _, p := range []string{cfg.Staging, metaPath} {
		if err := os.Chtimes(p, past, past); err != nil {
			writeOSLockChildResult(t, cfg.ResultPath, stagingOSLockChildResult{Err: err.Error()})
			return
		}
	}

	if cfg.Mode == "crash" {
		// No state.release(): the process exits while holding the lock, and
		// the kernel drops it. os.Exit skips every deferred release.
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
	state.release()
	writeOSLockChildResult(t, cfg.ResultPath, stagingOSLockChildResult{})
}

// startStagingOSLockChild starts the helper and returns its command and config
// paths; the caller waits for it explicitly.
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

// TestStagingOSLockHeldByLiveProcessBlocksTakeover is the real-process form of
// B3(i)/(iii): a helper holds the OS ownership lock while its record looks
// stale-looking (aged mtime), so only lifetime ownership can refuse takeover.
// The second claimant must back off as errBlobIncompleteOwned and leave the
// staging untouched; once the helper is released it completes and publishes.
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

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", strconv.Itoa(total))
			w.Header().Set("Accept-Ranges", "bytes")
			return
		}
		http.Error(w, "body must not be requested while the live owner holds the lock", http.StatusInternalServerError)
	}))
	defer srv.Close()

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
	// Pre-condition: the record is already older than the former lease grace;
	// its timestamp cannot grant another process ownership.
	metaBefore, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("helper record: %v", err)
	}
	metaInfo, statErr := os.Stat(metaPath)
	if statErr != nil {
		t.Fatalf("helper record stat: %v", statErr)
	}
	if age := time.Since(metaInfo.ModTime()); age < claimGracePeriod {
		t.Fatalf("the helper record must look stale to the lease model (age=%v, grace=%v)", age, claimGracePeriod)
	}
	var beforeMeta IncompleteMeta
	if err := json.Unmarshal(metaBefore, &beforeMeta); err != nil {
		t.Fatal(err)
	}
	stagingBefore, err := os.ReadFile(staging)
	if err != nil {
		t.Fatal(err)
	}

	it := PlanItem{RelativePath: "lock.bin", URL: srv.URL + "/lock.bin", Size: total, AcceptRanges: true}
	_, claimErr := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"},
		Settings{Concurrency: 1, Retries: 0, StallTimeout: "0"}, it, dst,
		multipartOpts{Commit: commit}, func(ProgressEvent) {})
	if !errors.Is(claimErr, errBlobIncompleteOwned) {
		_ = cmd.Process.Kill()
		t.Fatalf("second claimant error = %v, want errBlobIncompleteOwned while the OS lock is held", claimErr)
	}
	// Cleanup must observe the same held lifetime fence; stale diagnostics do
	// not authorize deleting a suspended owner's payload or metadata.
	removeStagingArtifactsIfUnclaimed(staging, metaPath)
	// The claimant must not have touched the owner's staging or record.
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

	// Release the helper: it wakes up, completes the payload, and publishes.
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

// TestStagingOSLockReleasedByCrashedProcess is the real-process form of B3(ii):
// a helper claims and seeds a committed prefix, then dies abruptly while
// holding the OS lock. The kernel releases it at death, so the next claimant
// acquires immediately, resumes only the missing suffix, and publishes.
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
		multipartOpts{Commit: commit}, func(ProgressEvent) {}); err != nil {
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
