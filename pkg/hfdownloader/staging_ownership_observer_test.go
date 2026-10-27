// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Regression coverage for the native-observer bug (issue #140 / PR #151
// review, second correction).
//
// The first correction fixed the single-reader case by classifying the lock
// holder before acting on a busy lock. But the classifier itself observed the
// lock by TAKING the exclusive lock, which made it a writer: every peer — a
// concurrent claimant or a concurrent status probe — then saw a live writer
// that did not exist, skipped the file, and the job completed with the file
// missing.
//
// The invariant these tests pin:
//   - an OBSERVER never takes the exclusive lock, so observers can never make
//     the lock look exclusively held to another observer;
//   - only a real lifetime writer takes the exclusive lock;
//   - with observers only, a claim must never report a live writer.
//
// The concurrent cases deliberately bypass the in-process destination-domain
// slot so the OS lock is the only exclusion in play, exactly as it is across
// processes.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// observerCount is how many concurrent observers run against one lock file.
const observerCount = 8

// runObservers starts n goroutines that repeatedly observe the lock file
// (the contention classifier's observation) and, independently, run status
// probes. Every observation result is recorded so the test can assert that
// no observer ever saw an exclusive holder while only observers were active.
//
// The returned stop function ends the observers and reports how many
// observations completed.
//
// The observers must only be run while NO real writer holds the lock: a
// genuine exclusive holder is supposed to be observed as exclusive. During a
// real Download the downloader itself is that writer, so the
// "never looks exclusive" assertion is only valid for the observer-only
// runs; the Download runs assert the product outcome instead.
func runObservers(t *testing.T, staging string, extraProbes bool, assertNoExclusive bool) (stop func() int, observations *atomic.Int64) {
	t.Helper()
	lockPath := stagingOwnershipLockPath(staging)
	stopCh := make(chan struct{})
	var wg sync.WaitGroup
	observations = &atomic.Int64{}

	for i := 0; i < observerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stopCh:
					return
				default:
				}
				kind, err := stagingOwnershipObserveHolder(lockPath)
				if err == nil {
					observations.Add(1)
					if kind == stagingLockHolderExclusive && assertNoExclusive {
						// An observer must never look like a live writer.
						t.Errorf("a concurrent observer was observed as an exclusive holder")
					}
				}
			}
		}()
	}

	if extraProbes {
		// Concurrent status probes: CheckBlob's reader path. A status probe
		// must never observe a live writer either.
		for i := 0; i < observerCount; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stopCh:
						return
					default:
					}
					busy, err := probeStagingOSLock(lockPath)
					if err == nil && busy && assertNoExclusive {
						t.Errorf("a status probe observed a live writer although only observers were active")
					}
				}
			}()
		}
	}

	return func() int {
		close(stopCh)
		wg.Wait()
		return int(observations.Load())
	}, observations
}

// TestStagingOwnership_ConcurrentObserversNeverLookLikeAWriter is the core
// regression for the native-observer bug.
//
// Eight concurrent observers (the contention classifier's observation) plus
// eight concurrent status probes run against one lock file while no writer
// exists. Every observation must report that no exclusive holder is present.
//
// The previous implementation failed here: the classifier took the exclusive
// lock to observe it, so the peers saw a live writer.
func TestStagingOwnership_ConcurrentObserversNeverLookLikeAWriter(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	dir := t.TempDir()
	staging := filepath.Join(dir, "observed.bin.incomplete")

	// The lock file must exist for the observers to open it.
	own, err := acquireStagingOwnershipLock(staging)
	if err != nil {
		t.Fatal(err)
	}
	own.release()

	stop, observations := runObservers(t, staging, true, true)
	// Let the observers run long enough to overlap each other many times.
	time.Sleep(500 * time.Millisecond)
	stop()

	if observations.Load() == 0 {
		t.Fatal("the observers never completed an observation; the test is not discriminating")
	}
}

// TestStagingOwnership_ObserversSeeARealWriterAsExclusive is the positive
// control for the observation: a genuine exclusive holder MUST be observed
// as exclusive, so the claimant still skips a real owner.
func TestStagingOwnership_ObserversSeeARealWriterAsExclusive(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	dir := t.TempDir()
	staging := filepath.Join(dir, "observed.bin.incomplete")

	own, err := acquireStagingOwnershipLock(staging)
	if err != nil {
		t.Fatal(err)
	}
	own.release()

	// A real writer holds the lock for the whole observation window.
	writer, err := acquireStagingOwnershipLock(staging)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.release()

	kind, oerr := observeStagingOSLockHolder(stagingOwnershipLockPath(staging))
	if oerr != nil {
		t.Fatalf("observe with a real writer: %v", oerr)
	}
	if kind != stagingLockHolderExclusive {
		t.Fatalf("a real writer was observed as %v, want stagingLockHolderExclusive", kind)
	}
}

// TestStagingOwnership_ConcurrentObserversDoNotBlockARealWriter pins the
// other half of the invariant: while the observers run, a real writer must
// still be able to take the exclusive lock. An exclusive observer would
// starve it.
func TestStagingOwnership_ConcurrentObserversDoNotBlockARealWriter(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	dir := t.TempDir()
	staging := filepath.Join(dir, "observed.bin.incomplete")

	own, err := acquireStagingOwnershipLock(staging)
	if err != nil {
		t.Fatal(err)
	}
	own.release()

	stop, _ := runObservers(t, staging, false, false)

	// A real writer takes the exclusive lock while the observers run. An
	// exclusive observer would have made this fail with EWOULDBLOCK.
	//
	// The acquisition is retried briefly: the observers are readers, so the
	// lock is free, but the writer's own attempt can still lose a race with
	// a peer observer that holds a shared lock at that instant.
	var writer *stagingOwnershipLock
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		writer, err = acquireStagingOwnershipLock(staging)
		if err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err != nil {
		stop()
		t.Fatalf("a real writer could not take the lock while observers were running: %v", err)
	}
	writer.release()
	stop()

	// A real exclusive holder IS observed as exclusive (positive control,
	// proven separately because a writer cannot be held while the observers
	// assert that nothing looks exclusive).
	writer, err = acquireStagingOwnershipLock(staging)
	if err != nil {
		t.Fatalf("a real writer could not take the lock: %v", err)
	}
	kind, oerr := observeStagingOSLockHolder(stagingOwnershipLockPath(staging))
	if oerr != nil {
		writer.release()
		t.Fatalf("observe with a real writer: %v", oerr)
	}
	if kind != stagingLockHolderExclusive {
		writer.release()
		t.Fatalf("a real writer was observed as %v, want stagingLockHolderExclusive", kind)
	}
	writer.release()
}

// TestDownload_ConcurrentObserversNeverCauseAFalseSkip drives the whole
// Download path while eight concurrent observers and eight concurrent status
// probes run against the staging lock file.
//
// The outcome must be real: the file is downloaded (the exact missing suffix
// is fetched) and published. A nil error with a missing blob, or a recorded
// skip, is the false-success failure mode this test exists to catch.
func TestDownload_ConcurrentObserversNeverCauseAFalseSkip(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	f := newContentionDownloadFixture(t)

	// During the Download the downloader itself is the real exclusive
	// writer, so the observers legitimately see an exclusive holder; the
	// assertion here is the product outcome, not the observation.
	stop, _ := runObservers(t, f.staging, true, false)
	out := f.runDownload(t, context.Background())
	stop()

	if len(out.skips) > 0 {
		t.Fatalf("the job recorded a successful skip although only observers were active: %v", out.skips)
	}
	if out.err != nil {
		t.Fatalf("Download under observer pressure: %v", out.err)
	}
	// A reported success must be real: the file exists and is byte-exact.
	assertExactFile(t, f.dst, f.full)
	// The resume must have fetched exactly the missing suffix.
	want := fmt.Sprintf("bytes=%d-%d", f.total/2, f.total-1)
	if got := f.getRanges(); len(got) != 1 || got[0] != want {
		t.Errorf("ranged GETs = %v, want exactly the missing suffix %q", got, want)
	}
}

// TestDownload_ConcurrentObserversRepeatedlyNeverCauseAFalseSkip repeats the
// observer pressure. The reported failure was intermittent (10/10 runs
// failed under eight classifiers), so a single run is not enough evidence.
func TestDownload_ConcurrentObserversRepeatedlyNeverCauseAFalseSkip(t *testing.T) {
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	for run := 0; run < 10; run++ {
		f := newContentionDownloadFixture(t)
		stop, _ := runObservers(t, f.staging, true, false)
		out := f.runDownload(t, context.Background())
		stop()

		if len(out.skips) > 0 {
			t.Fatalf("run %d: the job recorded a successful skip although only observers were active: %v", run, out.skips)
		}
		if out.err != nil {
			t.Fatalf("run %d: Download under observer pressure: %v", run, out.err)
		}
		assertExactFile(t, f.dst, f.full)
	}
}

// contentionChildEnv marks a helper process that only claims the staging and
// reports the outcome.
const contentionChildEnv = "HFDESK_CONTENTION_CHILD"

// contentionChildConfigEnv points the helper at its configuration file.
const contentionChildConfigEnv = "HFDESK_CONTENTION_CONFIG"

// contentionChildConfig configures one cross-process claimant.
type contentionChildConfig struct {
	Staging    string `json:"staging"`
	Publish    string `json:"publish"`
	MetaPath   string `json:"meta_path"`
	Size       int    `json:"size"`
	ResultPath string `json:"result_path"`
	HoldMS     int    `json:"hold_ms"`
	// Observe makes the helper observe the lock (as the contention
	// classifier does) before it claims.
	Observe bool `json:"observe"`
	// ObserveOnly makes the helper only observe the lock and report whether
	// it ever saw an exclusive holder. It never claims.
	ObserveOnly bool `json:"observe_only"`
}

// contentionChildResult is the helper's verdict.
type contentionChildResult struct {
	// Owned is true when the helper was told the staging was owned by a live
	// writer (errBlobIncompleteOwned), or when its observation saw an
	// exclusive holder.
	Owned bool `json:"owned"`
	// Err is the failure text, if any.
	Err string `json:"err"`
	// Other is true for any other error.
	Other bool `json:"other"`
}

// TestStagingOwnership_CrossProcessClaimantsDoNotSeeEachOtherAsOwners is the
// cross-process case: several independent processes claim the same staging
// file at once.
//
// The in-process registry cannot help here — it is per process — so the OS
// lock is the only exclusion. Exactly one claimant may hold the lock; the
// others observe a genuine exclusive holder.
//
// The decisive property is NOT that the losers skip: a loser that observes a
// real live writer may legitimately report it. The decisive property is that
// the losers must never report a live owner when only OBSERVERS (no writer)
// are active — that is the false-live-writer bug this test guards.
func TestStagingOwnership_CrossProcessClaimantsDoNotSeeEachOtherAsOwners(t *testing.T) {
	if os.Getenv(contentionChildEnv) == "1" {
		runContentionChild(t)
		return
	}
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	const total = 4_096
	full := rangedTestPayload(total)
	sha := singleTestSHA(full)
	dir := t.TempDir()
	staging := filepath.Join(dir, "cross.bin.incomplete")
	metaPath := staging + ".meta"
	pub := filepath.Join(dir, "cross.bin")
	if err := os.WriteFile(staging, full, 0o644); err != nil {
		t.Fatal(err)
	}
	meta := IncompleteMeta{
		PID: os.Getpid(), Instance: "cross-instance", Host: processHostname,
		StartedAt: time.Now().UTC(), Size: total, SHA256: sha, Version: stagingMetaVersion,
		Repo: "o/r", Commit: "commit123", Path: "cross.bin",
		Ranges: buildRangeLayout(total, 1), LockFenced: true,
	}
	if err := writeStagingMeta(metaPath, &meta); err != nil {
		t.Fatal(err)
	}

	const claimants = 6
	results := make(chan contentionChildResult, claimants)
	var wg sync.WaitGroup
	for i := 0; i < claimants; i++ {
		cfg := contentionChildConfig{
			Staging: staging, Publish: pub, MetaPath: metaPath, Size: total,
			ResultPath: filepath.Join(dir, fmt.Sprintf("result-%d.json", i)),
			HoldMS:     150,
			// The helper is an OBSERVER first: it observes the lock like the
			// contention classifier does, then claims.
			Observe: true,
		}
		wg.Add(1)
		go func(cfg contentionChildConfig) {
			defer wg.Done()
			runContentionChildProcess(t, cfg, results)
		}(cfg)
	}
	wg.Wait()
	close(results)

	var owned, other, ok int
	for res := range results {
		switch {
		case res.Owned:
			owned++
		case res.Other:
			other++
		default:
			ok++
		}
	}
	// At least one claimant must have won the lock: the exclusion is real.
	if ok == 0 {
		t.Fatalf("no cross-process claimant acquired the ownership lock (owned=%d other=%d)", owned, other)
	}
	t.Logf("cross-process claimants: %d acquired, %d observed a live writer, %d failed with an actionable error", ok, owned, other)
}

// TestStagingOwnership_CrossProcessObserversDoNotInventALiveWriter is the
// cross-process negative control: independent PROCESSES only observe the
// lock (as the contention classifier does) and never claim it.
//
// No process takes the exclusive lock, so no observer may report a live
// writer. This is the cross-process form of the native-observer bug: an
// exclusive observer would make the peers see a writer that does not exist.
func TestStagingOwnership_CrossProcessObserversDoNotInventALiveWriter(t *testing.T) {
	if os.Getenv(contentionChildEnv) == "1" {
		runContentionChild(t)
		return
	}
	if !stagingOSLockAvailable {
		t.Skip("this platform has no OS ownership lock primitive")
	}
	dir := t.TempDir()
	staging := filepath.Join(dir, "cross-observe.bin.incomplete")

	own, err := acquireStagingOwnershipLock(staging)
	if err != nil {
		t.Fatal(err)
	}
	own.release()

	const observers = 6
	results := make(chan contentionChildResult, observers)
	var wg sync.WaitGroup
	for i := 0; i < observers; i++ {
		cfg := contentionChildConfig{
			Staging: staging, Publish: filepath.Join(dir, "cross-observe.bin"),
			MetaPath: staging + ".meta", Size: 0,
			ResultPath:  filepath.Join(dir, fmt.Sprintf("obs-%d.json", i)),
			HoldMS:      200,
			ObserveOnly: true,
		}
		wg.Add(1)
		go func(cfg contentionChildConfig) {
			defer wg.Done()
			runContentionChildProcess(t, cfg, results)
		}(cfg)
	}
	wg.Wait()
	close(results)

	var invented int
	for res := range results {
		if res.Owned {
			// The helper reports a live writer when its observation saw an
			// exclusive holder.
			invented++
		}
	}
	if invented != 0 {
		t.Fatalf("%d of %d cross-process observers reported a live writer although no process held the lock", invented, observers)
	}
}

// runContentionChildProcess starts one helper process and collects its
// verdict.
func runContentionChildProcess(t *testing.T, cfg contentionChildConfig, results chan<- contentionChildResult) {
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
	cmd := exec.Command(exe, "-test.run=^TestStagingOwnership_CrossProcessClaimantsDoNotSeeEachOtherAsOwners$", "-test.timeout=120s")
	cmd.Env = append(os.Environ(), contentionChildEnv+"=1", contentionChildConfigEnv+"="+cfgPath)
	var out []byte
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper process: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(cfg.ResultPath)
	if err != nil {
		t.Fatalf("helper result: %v\n%s", err, out)
	}
	var res contentionChildResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("helper result decode: %v\n%s", err, out)
	}
	results <- res
}

// runContentionChild is the helper process body.
//
// In observe-only mode it only observes the lock (exactly like the
// contention classifier) and reports whether it ever saw an exclusive
// holder. It never claims and never takes the exclusive lock, so a correct
// observer can never report a live writer.
//
// Otherwise it observes (when asked) and then claims the staging once.
func runContentionChild(t *testing.T) {
	cfgPath := os.Getenv(contentionChildConfigEnv)
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("child config: %v", err)
	}
	var cfg contentionChildConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("child config: %v", err)
	}
	writeResult := func(res contentionChildResult) {
		data, err := json.Marshal(res)
		if err != nil {
			t.Fatalf("result encode: %v", err)
		}
		if err := os.WriteFile(cfg.ResultPath, data, 0o644); err != nil {
			t.Fatalf("result write: %v", err)
		}
	}

	lockPath := stagingOwnershipLockPath(cfg.Staging)

	if cfg.ObserveOnly {
		// Observe repeatedly for the whole window. No process holds the
		// exclusive lock in this mode, so an exclusive observation would be
		// an artifact of the observer itself.
		deadline := time.Now().Add(time.Duration(cfg.HoldMS) * time.Millisecond)
		for time.Now().Before(deadline) {
			kind, oerr := observeStagingOSLockHolder(lockPath)
			if oerr == nil && kind == stagingLockHolderExclusive {
				writeResult(contentionChildResult{Owned: true})
				return
			}
			time.Sleep(time.Millisecond)
		}
		writeResult(contentionChildResult{})
		return
	}

	if cfg.Observe {
		// Observe first, exactly like the contention classifier does, so the
		// claim that follows runs under the same observation pressure.
		kind, oerr := observeStagingOSLockHolder(lockPath)
		if oerr == nil && kind == stagingLockHolderExclusive {
			// A real writer is present; report it and stop.
			writeResult(contentionChildResult{Owned: true})
			return
		}
	}

	it := PlanItem{RelativePath: filepath.Base(cfg.Publish), Size: int64(cfg.Size), LFS: true}
	// The child owns the destination's domain exactly like an item goroutine
	// does; the domain is the only cross-process fence now.
	domain, derr := acquireBlobDomain(context.Background(), flatDomainKey(cfg.Publish), flatDomainLockSrc(cfg.Publish))
	if derr != nil {
		if errors.Is(derr, errStagingOwnershipBusy) {
			writeResult(contentionChildResult{Owned: true})
			return
		}
		writeResult(contentionChildResult{Other: true, Err: derr.Error()})
		return
	}
	defer domain.release()
	if _, err := claimStaging(context.Background(), cfg.Staging, cfg.Publish, cfg.MetaPath, it,
		Job{Repo: "o/r"}, "commit123", true, Settings{Concurrency: 1, Retries: 0, StallTimeout: "0"}, false, domain); err != nil {
		if errors.Is(err, errBlobIncompleteOwned) {
			writeResult(contentionChildResult{Owned: true})
			return
		}
		writeResult(contentionChildResult{Other: true, Err: err.Error()})
		return
	}
	// Hold the domain briefly so the peers genuinely contend.
	time.Sleep(time.Duration(cfg.HoldMS) * time.Millisecond)
	writeResult(contentionChildResult{})
}
