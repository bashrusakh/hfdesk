// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Observers must not acquire the exclusive writer lock: doing so can make
// claimants falsely report a live owner. These tests bypass the in-process slot
// to exercise OS-lock observation independently.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const observerCount = 8

// runObservers runs OS observations and optional status probes. Enable
// assertNoExclusive only when no real writer may hold the lock.
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
						t.Errorf("a concurrent observer was observed as an exclusive holder")
					}
				}
			}
		}()
	}

	if extraProbes {
		// Exercise CheckBlob's status-probe path with the same no-writer condition.
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

// Concurrent non-owning observations and status probes must not report an
// exclusive writer when no writer holds the lock.
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

// Concurrent observers must still permit a real writer to acquire the lock.
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

	// A shared probe may briefly block LOCK_EX, so retry acquisition while the
	// observers run rather than treating one busy result as writer starvation.
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

// Under concurrent observers, Download must fetch exactly the missing suffix
// and publish correct bytes, not report a successful skip with missing content.
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
	assertExactFile(t, f.dst, f.full)
	want := fmt.Sprintf("bytes=%d-%d", f.total/2, f.total-1)
	if got := f.getRanges(); len(got) != 1 || got[0] != want {
		t.Errorf("ranged GETs = %v, want exactly the missing suffix %q", got, want)
	}
}

const contentionChildEnv = "HFDESK_CONTENTION_CHILD"

const contentionChildConfigEnv = "HFDESK_CONTENTION_CONFIG"

type contentionChildConfig struct {
	Staging    string `json:"staging"`
	ResultPath string `json:"result_path"`
	HoldMS     int    `json:"hold_ms"`
}

type contentionChildResult struct {
	// Owned is true when an observation saw an exclusive holder.
	Owned bool `json:"owned"`
}

// Independent helper processes only observe; none holds an exclusive writer
// lock. They must never report a live owner created by another observer.
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
			Staging:    staging,
			ResultPath: filepath.Join(dir, fmt.Sprintf("obs-%d.json", i)),
			HoldMS:     200,
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
			invented++
		}
	}
	if invented != 0 {
		t.Fatalf("%d of %d cross-process observers reported a live writer although no process held the lock", invented, observers)
	}
}

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
	cmd := exec.Command(exe, "-test.run=^TestStagingOwnership_CrossProcessObserversDoNotInventALiveWriter$", "-test.timeout=120s")
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

// runContentionChild only observes for the configured window and reports any
// exclusive holder; it never claims the writer lock itself.
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
}
