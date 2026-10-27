// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Covers bounded metadata serialization and cross-process destination
// ownership: contenders complete serially, with at most one writer at a time.

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
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestStagingStateLockMutualExclusion(t *testing.T) {
	metaPath := filepath.Join(t.TempDir(), "x.incomplete.meta")

	var current, peak, acquired atomic.Int32
	const workers = 8
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lock := &stagingStateLock{path: stagingLockPath(metaPath)}
			if err := lock.acquire(context.Background(), 10*time.Second); err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			n := current.Add(1)
			acquired.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			current.Add(-1)
			lock.release()
		}()
	}
	wg.Wait()

	if got := acquired.Load(); got != workers {
		t.Errorf("acquired the lock %d times; want %d", got, workers)
	}
	if got := peak.Load(); got != 1 {
		t.Errorf("%d holders were inside the critical section at once; want 1", got)
	}
	if _, err := os.Stat(stagingLockPath(metaPath)); !os.IsNotExist(err) {
		t.Errorf("lock file survived the last release, stat err=%v", err)
	}
}

func TestStagingStateLockBoundedWait(t *testing.T) {
	metaPath := filepath.Join(t.TempDir(), "x.incomplete.meta")
	held := &stagingStateLock{path: stagingLockPath(metaPath)}
	if err := held.acquire(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
	defer held.release()

	waiter := &stagingStateLock{path: stagingLockPath(metaPath)}
	start := time.Now()
	err := waiter.acquire(context.Background(), 100*time.Millisecond)
	if !errors.Is(err, errStagingLockBusy) {
		t.Fatalf("acquisition under a live holder = %v, want errStagingLockBusy", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("bounded acquisition took %v", elapsed)
	}
}

func TestStagingStateLockIgnoresAbandonedTransitionFile(t *testing.T) {
	metaPath := filepath.Join(t.TempDir(), "x.incomplete.meta")
	lockPath := stagingLockPath(metaPath)
	if err := os.WriteFile(lockPath, []byte(`{"historical":"ignored"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Second)
	if err := os.Chtimes(lockPath, past, past); err != nil {
		t.Fatal(err)
	}

	lock := &stagingStateLock{path: lockPath}
	if err := lock.acquire(context.Background(), time.Second); err != nil {
		t.Fatalf("an abandoned lock must remain recoverable: %v", err)
	}
	lock.release()
	if _, err := os.Stat(lockPath); err != nil {
		t.Errorf("transition file should not be mutated as an authority: stat err=%v", err)
	}
}

func TestStagingMetaWriteRefusesForeignClaim(t *testing.T) {
	dir := t.TempDir()
	metaPath := filepath.Join(dir, "x.incomplete.meta")
	foreign := IncompleteMeta{
		PID:       os.Getpid(),
		Instance:  "another-process-instance",
		Host:      processHostname,
		StartedAt: time.Now().UTC(),
		Size:      100,
		Version:   stagingMetaVersion,
		Repo:      "o/r",
		Path:      "x.bin",
		Ranges:    buildRangeLayout(100, 1),
	}
	if err := writeStagingMeta(metaPath, &foreign); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}

	lock := &stagingStateLock{path: stagingLockPath(metaPath)}
	if err := lock.acquire(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
	ours := foreign
	ours.Instance = processInstanceToken
	werr := writeStagingMetaOwnedLocked(metaPath, &ours)
	lock.release()
	if !errors.Is(werr, errStagingClaimLost) {
		t.Fatalf("rewriting a foreign claim = %v, want errStagingClaimLost", werr)
	}
	after, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Error("a non-owner replaced the current claim record")
	}
}

func TestRemoveStagingArtifactsOwnedRefusesForeignClaim(t *testing.T) {
	dir := t.TempDir()
	staging := filepath.Join(dir, "x.incomplete")
	metaPath := staging + ".meta"
	if err := os.WriteFile(staging, []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	foreign := IncompleteMeta{
		PID:       os.Getpid(),
		Instance:  "another-process-instance",
		Host:      processHostname,
		StartedAt: time.Now().UTC(),
		Size:      7,
		Version:   stagingMetaVersion,
		Repo:      "o/r",
		Path:      "x.bin",
		Ranges:    buildRangeLayout(7, 1),
	}
	if err := writeStagingMeta(metaPath, &foreign); err != nil {
		t.Fatal(err)
	}
	if removeStagingArtifactsOwned(staging, metaPath) {
		t.Fatal("a non-owner removed a foreign claim")
	}
	for _, p := range []string{staging, metaPath} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("non-owner cleanup removed %s: %v", filepath.Base(p), err)
		}
	}
}

const (
	stagingTakeoverChildEnv  = "HFDESK_STAGING_TAKEOVER_CHILD"
	stagingTakeoverConfigEnv = "HFDESK_STAGING_TAKEOVER_CONFIG"
)

type stagingTakeoverChildConfig struct {
	Index       int    `json:"index"`
	Dst         string `json:"dst"`
	Staging     string `json:"staging"`
	Publish     string `json:"publish"`
	URL         string `json:"url"`
	GoPath      string `json:"go"`
	ResultPath  string `json:"result"`
	Size        int64  `json:"size"`
	Concurrency int    `json:"concurrency"`
}

type stagingTakeoverChildResult struct {
	Index    int    `json:"index"`
	Instance string `json:"instance"`
	Owned    bool   `json:"owned"`
	Err      string `json:"err,omitempty"`
}

// stagingTakeoverChild couples a contender process with its own output buffer
// (a shared buffer would be written concurrently by os/exec's copy goroutines).
type stagingTakeoverChild struct {
	cmd *exec.Cmd
	out bytes.Buffer
}

// TestStagingTakeoverChildProcess is a re-exec helper for the contender test.
// It acquires the destination domain before multipart work; ordinary runs skip it.
func TestStagingTakeoverChildProcess(t *testing.T) {
	if os.Getenv(stagingTakeoverChildEnv) != "1" {
		t.Skip("helper process for TestClaimStagingConcurrentTakeover")
	}
	raw, err := os.ReadFile(os.Getenv(stagingTakeoverConfigEnv))
	if err != nil {
		t.Fatalf("child config: %v", err)
	}
	var cfg stagingTakeoverChildConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("child config: %v", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, serr := os.Stat(cfg.GoPath); serr == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("start barrier was never released")
		}
		time.Sleep(time.Millisecond)
	}

	it := PlanItem{RelativePath: "takeover.bin", URL: cfg.URL, Size: cfg.Size, AcceptRanges: true}
	key, lockSrc := testDomainIdentity(cfg.Dst)
	domain, derr := acquireBlobDomain(context.Background(), key, lockSrc)
	res := stagingTakeoverChildResult{Index: cfg.Index, Instance: processInstanceToken}
	if derr != nil {
		res.Err = derr.Error()
		if errors.Is(derr, errStagingOwnershipBusy) {
			res.Owned = true
		}
	} else {
		defer domain.release()
		opts := multipartOpts{Staging: cfg.Staging, Publish: cfg.Publish, Domain: domain}
		_, derr = downloadMultipart(context.Background(), http.DefaultClient, "", Job{Repo: "o/r"},
			Settings{Concurrency: cfg.Concurrency, Retries: 0, StallTimeout: "0"}, it, cfg.Dst, opts, func(ProgressEvent) {})
		if derr != nil {
			res.Err = derr.Error()
			if errors.Is(derr, errBlobIncompleteOwned) {
				res.Owned = true
			}
		}
	}
	data, merr := json.Marshal(res)
	if merr != nil {
		t.Fatalf("marshal result: %v", merr)
	}
	if werr := os.WriteFile(cfg.ResultPath, data, 0o644); werr != nil {
		t.Fatalf("write result: %v", werr)
	}
}

// Real processes contend for one stale record and destination domain. Every
// contender must complete without a false live-owner refusal, and final bytes
// must match the payload with no stale record remaining.
func TestClaimStagingConcurrentTakeover(t *testing.T) {
	if os.Getenv(stagingTakeoverChildEnv) == "1" {
		t.Skip("child process")
	}

	const total = 8000
	const concurrency = 4
	const contenders = 4
	const staleTakeoverSeedInstance = "dead-takeover-owner"
	full := rangedTestPayload(total)
	dir := filepath.Join(t.TempDir(), "race")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "takeover.bin")
	staging := dst + ".incomplete"
	metaPath := staging + ".meta"

	// The dead-owner seed has unpinned identity and garbage payload. It must be
	// discarded, then fetched completely under a fresh claim.
	if err := os.WriteFile(staging, bytes.Repeat([]byte{0xA5}, total), 0o644); err != nil {
		t.Fatal(err)
	}
	meta := IncompleteMeta{
		PID:       99999999,
		Instance:  staleTakeoverSeedInstance,
		Host:      processHostname,
		StartedAt: time.Now().UTC(),
		Size:      total,
		Version:   stagingMetaVersion,
		Repo:      "o/r",
		Path:      "takeover.bin",
		Ranges:    buildRangeLayout(total, concurrency),
	}
	if err := writeStagingMeta(metaPath, &meta); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-2 * time.Hour)
	for _, p := range []string{staging, metaPath} {
		if err := os.Chtimes(p, past, past); err != nil {
			t.Fatal(err)
		}
	}

	var (
		mu        sync.Mutex
		ranges    []string
		firstOnce sync.Once
	)
	firstRange := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", strconv.Itoa(total))
			w.Header().Set("Accept-Ranges", "bytes")
			return
		}
		var start, end int64
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil || start < 0 || end >= total || start > end {
			http.Error(w, "bad range", http.StatusBadRequest)
			return
		}
		mu.Lock()
		ranges = append(ranges, r.Header.Get("Range"))
		mu.Unlock()
		// A first range request follows payload open/preallocation. A new record
		// alone does not prove the payload has been recreated after discard.
		firstOnce.Do(func() { close(firstRange) })
		// Hold range bodies while observing the first writer's claim; other contenders
		// remain blocked on the destination domain.
		<-release
		content := full[start : end+1]
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(content)
	}))
	defer srv.Close()

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("test executable: %v", err)
	}
	goPath := filepath.Join(dir, "go")
	resultPaths := make([]string, contenders)
	cfgPaths := make([]string, contenders)
	children := make([]stagingTakeoverChild, contenders)
	for i := 0; i < contenders; i++ {
		cfgPaths[i] = filepath.Join(dir, fmt.Sprintf("cfg-%d.json", i))
		resultPaths[i] = filepath.Join(dir, fmt.Sprintf("result-%d.json", i))
		cfg := stagingTakeoverChildConfig{
			Index:       i,
			Dst:         dst,
			Staging:     staging,
			Publish:     dst,
			URL:         srv.URL + "/takeover.bin",
			GoPath:      goPath,
			ResultPath:  resultPaths[i],
			Size:        total,
			Concurrency: concurrency,
		}
		data, merr := json.Marshal(cfg)
		if merr != nil {
			t.Fatal(merr)
		}
		if werr := os.WriteFile(cfgPaths[i], data, 0o644); werr != nil {
			t.Fatal(werr)
		}
		cmd := exec.Command(exe, "-test.run=^TestStagingTakeoverChildProcess$", "-test.timeout=120s")
		cmd.Env = append(os.Environ(),
			stagingTakeoverChildEnv+"=1",
			stagingTakeoverConfigEnv+"="+cfgPaths[i])
		cmd.Stdout = &children[i].out
		cmd.Stderr = &children[i].out
		if serr := cmd.Start(); serr != nil {
			t.Fatalf("start contender %d: %v", i, serr)
		}
		children[i].cmd = cmd
	}

	// Release the child-process start barrier.
	time.Sleep(300 * time.Millisecond)
	if werr := os.WriteFile(goPath, nil, 0o644); werr != nil {
		t.Fatal(werr)
	}

	// Poll until the first domain holder replaces the stale seed. The record may
	// be absent during discard/recreate, but installed records must be complete.
	// Range bodies remain blocked until the server barrier is released.
	var claimMeta IncompleteMeta
	lastObserved := "record was never read"
	claimDeadline := time.Now().Add(10 * time.Second)
	for {
		raw, rerr := os.ReadFile(metaPath)
		switch {
		case rerr != nil:
			lastObserved = fmt.Sprintf("read error: %v", rerr)
		default:
			var recorded IncompleteMeta
			switch uerr := json.Unmarshal(raw, &recorded); {
			case uerr != nil:
				lastObserved = fmt.Sprintf("unreadable claim record: %v", uerr)
			case recorded.Instance == "":
				lastObserved = "claim record has an empty instance"
			case recorded.Instance == staleTakeoverSeedInstance:
				lastObserved = fmt.Sprintf("stale seed instance %q is still recorded", recorded.Instance)
			default:
				claimMeta = recorded
			}
		}
		if claimMeta.Instance != "" {
			break
		}
		if time.Now().After(claimDeadline) {
			t.Fatalf("winner never replaced the stale claim record: %s", lastObserved)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Wait for the first range request before asserting payload existence:
	// claim installation precedes payload open/preallocation.
	select {
	case <-firstRange:
	case <-time.After(10 * time.Second):
		// Release the parked handlers before failing, so the test's
		// httptest.Server.Close does not wait for them and the failure is
		// reported instead of hanging until the test timeout.
		releaseOnce.Do(func() { close(release) })
		t.Fatalf("winner never issued a range request after replacing the claim record (instance %q): the staging payload was never opened", claimMeta.Instance)
	}
	if _, serr := os.Stat(staging); serr != nil {
		t.Fatalf("winner's staging payload is missing after its first range request: %v", serr)
	}
	releaseOnce.Do(func() { close(release) })

	for i := range children {
		if werr := children[i].cmd.Wait(); werr != nil {
			t.Errorf("contender %d exited with %v; output:\n%s", i, werr, children[i].out.String())
		}
	}

	// All contenders must complete serially, never fail as a false live owner.
	var unexpect []stagingTakeoverChildResult
	delivered := 0
	for _, p := range resultPaths {
		data, rerr := os.ReadFile(p)
		if rerr != nil {
			t.Fatalf("missing contender result %s: %v", filepath.Base(p), rerr)
		}
		var res stagingTakeoverChildResult
		if err := json.Unmarshal(data, &res); err != nil {
			t.Fatalf("contender result %s: %v", filepath.Base(p), err)
		}
		if res.Err != "" || res.Owned {
			unexpect = append(unexpect, res)
			continue
		}
		delivered++
	}
	if len(unexpect) != 0 {
		t.Errorf("contenders failed although the destination domain serializes them: %+v", unexpect)
	}
	if delivered != contenders {
		t.Fatalf("%d contenders completed a delivery, want all %d", delivered, contenders)
	}

	if _, serr := os.Stat(metaPath); serr == nil {
		t.Errorf("the staging record survived the contenders' successful publications")
	}
	if got := buildRangeLayout(total, concurrency); len(got) != concurrency {
		t.Fatalf("test layout has %d ranges, want %d", len(got), concurrency)
	}
	mu.Lock()
	got := append([]string(nil), ranges...)
	mu.Unlock()
	if len(got) == 0 || len(got)%concurrency != 0 {
		t.Errorf("fetched %d ranges (%v); want whole-transfer batches per serialized contender", len(got), got)
	}
	assertExactFile(t, dst, full)
	for _, p := range []string{staging, metaPath, stagingLockPath(metaPath)} {
		if _, serr := os.Stat(p); !os.IsNotExist(serr) {
			t.Errorf("%s survived a successful publish, stat err=%v", filepath.Base(p), serr)
		}
	}
}
