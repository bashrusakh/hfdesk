package server

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func createWithDeleteTargets(t *testing.T, m *JobManager, targets ...string) {
	t.Helper()
	m.mu.Lock()
	// Keep dispatcher behavior outside this CreateJob-only test boundary.
	m.fenced = true
	m.deleteReservations[1] = freezeTestEntries(t, targets...)
	m.bumpWriteDecisionLocked()
	m.mu.Unlock()
}

func TestCreateJobReservationInventoryRunsOffLockAndUnionsTargets(t *testing.T) {
	root := t.TempDir()
	m := newJobManagerWithStatePath(Config{CacheDir: root}, nil, filepath.Join(root, "state"))
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	calls := 0
	m.writeInventory = func(base string) (writableNamespaceInventory, bool) {
		calls++
		once.Do(func() { close(entered) })
		<-release
		return inventoryWritableNamespace(base)
	}
	m.mu.Lock()
	m.fenced = true
	m.deleteReservations[1] = freezeTestEntries(t, filepath.Join(root, "one.gguf"))
	m.deleteReservations[2] = freezeTestEntries(t, filepath.Join(root, "two.gguf"))
	m.bumpWriteDecisionLocked()
	m.mu.Unlock()
	done := make(chan error, 1)
	go func() { _, _, err := m.CreateJob(DownloadRequest{Repo: "owner/new"}); done <- err }()
	<-entered
	if _, found := m.GetJob("absent"); found || len(m.ListJobs()) != 0 {
		t.Fatal("manager readers blocked or observed an uncommitted job")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if calls != 2 {
		t.Fatalf("namespace inventories = %d, want one per writable base, independent of target count", calls)
	}
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCreateJobReservationConcurrentEqualRequestsDeduplicate(t *testing.T) {
	root := t.TempDir()
	m := newJobManagerWithStatePath(Config{CacheDir: root}, nil, filepath.Join(root, "state"))
	entered, release := make(chan struct{}, 2), make(chan struct{})
	m.writeInventory = func(base string) (writableNamespaceInventory, bool) {
		entered <- struct{}{}
		<-release
		return inventoryWritableNamespace(base)
	}
	createWithDeleteTargets(t, m, filepath.Join(root, "reserved.gguf"))
	type result struct {
		job       *Job
		duplicate bool
		err       error
	}
	results := make(chan result, 2)
	request := DownloadRequest{Repo: "owner/equal"}
	for range 2 {
		go func() { j, duplicate, err := m.CreateJob(request); results <- result{j, duplicate, err} }()
	}
	<-entered
	<-entered
	close(release)
	a, b := <-results, <-results
	if a.err != nil || b.err != nil {
		t.Fatalf("CreateJob errors: %v, %v", a.err, b.err)
	}
	if a.job.ID != b.job.ID || a.duplicate == b.duplicate {
		t.Fatalf("results not deduplicated: %#v %#v", a, b)
	}
	if got := len(m.ListJobs()); got != 1 {
		t.Fatalf("jobs = %d, want 1", got)
	}
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCreateJobReservationManagedMutationInvalidatesInventory(t *testing.T) {
	root := t.TempDir()
	m := newJobManagerWithStatePath(Config{CacheDir: root}, nil, filepath.Join(root, "state"))
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	calls := 0
	m.writeInventory = func(base string) (writableNamespaceInventory, bool) {
		calls++
		if calls == 1 {
			once.Do(func() { close(entered) })
			<-release
		}
		return inventoryWritableNamespace(base)
	}
	createWithDeleteTargets(t, m, filepath.Join(root, "reserved.gguf"))
	done := make(chan error, 1)
	go func() { _, _, err := m.CreateJob(DownloadRequest{Repo: "owner/mutation"}); done <- err }()
	<-entered
	finish := m.beginObservedMutation([]string{filepath.Join(root, "managed-change")})
	finish()
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("CreateJob after fresh retry: %v", err)
	}
	if calls < 2 {
		t.Fatalf("mutation crossing scan did not force fresh inventory: calls=%d", calls)
	}
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCreateJobReservationBeginEndInvalidatesInventory(t *testing.T) {
	root := t.TempDir()
	m := newJobManagerWithStatePath(Config{CacheDir: root}, nil, filepath.Join(root, "state"))
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	calls := 0
	m.writeInventory = func(base string) (writableNamespaceInventory, bool) {
		calls++
		if calls == 1 {
			once.Do(func() { close(entered) })
			<-release
		}
		return inventoryWritableNamespace(base)
	}
	createWithDeleteTargets(t, m, filepath.Join(root, "reserved.gguf"))
	done := make(chan error, 1)
	go func() { _, _, err := m.CreateJob(DownloadRequest{Repo: "owner/reservation"}); done <- err }()
	<-entered
	m.mu.Lock()
	m.deleteReservations[2] = freezeTestEntries(t, filepath.Join(root, "transient.gguf"))
	m.bumpWriteDecisionLocked()
	delete(m.deleteReservations, 2)
	m.bumpWriteDecisionLocked()
	m.mu.Unlock()
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("CreateJob after fresh retry: %v", err)
	}
	if calls < 4 {
		t.Fatalf("transient reservation crossing scan did not force fresh inventory: calls=%d", calls)
	}
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCreateJobReservationCloseDuringInventoryDoesNotInsertOrLeak(t *testing.T) {
	root := t.TempDir()
	m := newJobManagerWithStatePath(Config{CacheDir: root}, nil, filepath.Join(root, "state"))
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	var once sync.Once
	m.writeInventory = func(base string) (writableNamespaceInventory, bool) {
		once.Do(func() { close(entered) })
		<-release
		return inventoryWritableNamespace(base)
	}
	createWithDeleteTargets(t, m, filepath.Join(root, "reserved.gguf"))
	done := make(chan error, 1)
	go func() { _, _, err := m.CreateJob(DownloadRequest{Repo: "owner/late"}); done <- err }()
	<-entered
	closeDone := make(chan error, 1)
	go func() { closeDone <- m.Close(context.Background()) }()
	waitForJobManagerStopping(t, m)
	releaseOnce.Do(func() { close(release) })
	if err := <-done; err != errJobManagerStopping {
		t.Fatalf("CreateJob error = %v, want stopping", err)
	}
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
	if jobs := m.ListJobs(); len(jobs) != 0 {
		t.Fatalf("late insertion after Close: %d jobs", len(jobs))
	}
}

func waitForJobManagerStopping(t *testing.T, m *JobManager) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		m.mu.RLock()
		stopping := m.stopping
		m.mu.RUnlock()
		if stopping {
			return
		}
		select {
		case <-deadline:
			t.Fatal("Close did not establish stopping fence")
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

func addRestartCandidate(m *JobManager, root, id string, status JobStatus) *Job {
	m.mu.Lock()
	m.fenced = true // Keep scheduler execution outside this admission test.
	job := &Job{ID: id, Repo: "owner/model", Revision: "main", OutputDir: root,
		HubDir: filepath.Join(root, "hub"), Status: status, CreatedAt: time.Now(),
		Progress: JobProgress{DownloadedBytes: 17, TotalBytes: 29}, Files: []JobFileProgress{{Path: "old"}},
		partialFilesMu: &sync.Mutex{}}
	m.jobs[id] = job
	m.mu.Unlock()
	return job
}

func addTwoDeleteReservations(t *testing.T, m *JobManager, root string) {
	t.Helper()
	paths := []frozenMutationPath{}
	for _, name := range []string{"reserved-one.gguf", "reserved-two.gguf"} {
		frozen := freezeTestEntries(t, filepath.Join(root, name))
		paths = append(paths, frozen[0])
	}
	m.mu.Lock()
	m.deleteReservations[1] = paths[:1]
	m.deleteReservations[2] = paths[1:]
	m.bumpWriteDecisionLocked()
	m.mu.Unlock()
}

func TestResumeRetryReservationInventoryRunsOffLockAndUnionsTargets(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status JobStatus
		act    func(*JobManager, string) bool
	}{
		{"resume", JobStatusPaused, (*JobManager).ResumeJob},
		{"retry", JobStatusFailed, (*JobManager).RetryJob},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			m := newJobManagerWithStatePath(Config{CacheDir: root}, nil, filepath.Join(root, "state"))
			job := addRestartCandidate(m, root, "restart", tc.status)
			addTwoDeleteReservations(t, m, root)
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			calls := make(map[string]int)
			m.writeInventory = func(base string) (writableNamespaceInventory, bool) {
				calls[base]++
				once.Do(func() { close(entered) })
				<-release
				return inventoryWritableNamespace(base)
			}
			done := make(chan bool, 1)
			go func() { done <- tc.act(m, job.ID) }()
			<-entered
			if _, ok := m.GetJob(job.ID); !ok || len(m.ListJobs()) != 1 {
				t.Fatal("manager readers blocked or observed a partial transition during inventory")
			}
			close(release)
			if !<-done {
				t.Fatal("restart admission unexpectedly rejected")
			}
			if len(calls) != 2 {
				t.Fatalf("inventoried namespaces=%v, want exactly the two unique writable bases, once each across reservations", calls)
			}
			for base, count := range calls {
				if count != 1 {
					t.Fatalf("inventory calls for %q=%d, want one", base, count)
				}
			}
			if err := m.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRestartChecksFrozenHFReservationDespiteUnrelatedSelectedDelete(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status JobStatus
		act    func(*JobManager, string) bool
	}{
		{"resume", JobStatusPaused, (*JobManager).ResumeJob},
		{"retry", JobStatusFailed, (*JobManager).RetryJob},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			physicalA, physicalB := filepath.Join(root, "A"), filepath.Join(root, "B")
			alias := filepath.Join(root, "alias")
			for _, dir := range []string{physicalA, physicalB} {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(physicalA, alias); err != nil {
				t.Fatal(err)
			}
			m := newJobManagerWithStatePath(Config{CacheDir: root, MaxActive: 1}, nil, filepath.Join(root, "state"))
			m.fenced = true // Keep the case at admission; no dispatcher runner is needed.
			job := addRestartCandidate(m, root, "restart", tc.status)
			job.Flat, job.LocalDir, job.LocalRepo = true, physicalA, "owner/repo"
			release, ok := m.reserveSelectedHF(alias, filepath.Join(alias, "friendly"))
			if !ok {
				t.Fatal("could not admit selected-HF reservation")
			}
			if err := os.Remove(alias); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(physicalB, alias); err != nil {
				t.Fatal(err)
			}
			// A distinct selected entry makes restartJob use its off-lock evidence
			// path; this reservation does not overlap the job's physical A target.
			addTwoDeleteReservations(t, m, filepath.Join(root, "unrelated"))
			before, ok := m.GetJob(job.ID)
			if !ok {
				t.Fatal("job disappeared")
			}
			if tc.act(m, job.ID) {
				t.Fatal("restart admitted despite frozen HF lease overlapping physical destination A")
			}
			after, ok := m.GetJob(job.ID)
			if !ok || !reflect.DeepEqual(before, after) {
				t.Fatalf("rejected restart changed job snapshot: before=%#v after=%#v", before, after)
			}
			release()
			if err := m.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRestartRecollectsAfterTransientRealRunIsRemoved(t *testing.T) {
	root := t.TempDir()
	restartDir, runDir := filepath.Join(root, "restart"), filepath.Join(root, "transient-run")
	for _, dir := range []string{restartDir, runDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	hf, started, releaseDownload := blockingPlanServer(t, "transient-Q4_K_M.gguf")
	defer hf.Close()
	var downloadOnce sync.Once
	finishDownload := func() { downloadOnce.Do(releaseDownload) }
	defer finishDownload()
	inventoryEntered, releaseInventory := make(chan struct{}), make(chan struct{})
	schedulerInventoryEntered, releaseSchedulerInventory := make(chan struct{}), make(chan struct{})
	m := newJobManagerWithStatePath(Config{LocalDir: runDir, Endpoint: hf.URL, MaxActive: 2}, nil, filepath.Join(root, "state"))
	defer func() {
		select {
		case <-releaseInventory:
		default:
			close(releaseInventory)
		}
		select {
		case <-releaseSchedulerInventory:
		default:
			close(releaseSchedulerInventory)
		}
		finishDownload()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := m.Close(ctx); err != nil {
			t.Errorf("close manager during test cleanup: %v", err)
		}
	}()
	candidate := addRestartCandidate(m, root, "restart-candidate", JobStatusFailed)
	candidate.Flat, candidate.LocalDir, candidate.LocalRepo = true, restartDir, "owner/restart"
	addTwoDeleteReservations(t, m, root)
	m.mu.Lock()
	m.fenced = false
	m.mu.Unlock()

	candidateBase := filepath.Clean(jobDestinationBase(candidate))
	var inventoryMu sync.Mutex
	inventoryCalls := 0
	var firstInventory sync.Once
	m.writeInventory = func(base string) (writableNamespaceInventory, bool) {
		if filepath.Clean(base) == candidateBase {
			inventoryMu.Lock()
			inventoryCalls++
			call := inventoryCalls
			inventoryMu.Unlock()
			if call == 3 {
				// A separate scheduler attempt may inventory the candidate after
				// the retry is accepted. Gate it to scope the retry oracle.
				close(schedulerInventoryEntered)
				<-releaseSchedulerInventory
			}
			firstInventory.Do(func() {
				close(inventoryEntered)
				<-releaseInventory
			})
		}
		return inventoryWritableNamespace(base)
	}

	restartResult := make(chan bool, 1)
	go func() { restartResult <- m.RetryJob(candidate.ID) }()
	select {
	case <-inventoryEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("restart attempt did not reach its controlled off-lock inventory")
	}
	before := m.captureWriteDecision()
	candidateProbe, err := captureWriteIdentityProbe(candidateBase, true)
	if err != nil {
		t.Fatalf("capture stable candidate namespace: %v", err)
	}
	targetProbes := make(map[string]writeIdentityProbe)
	m.mu.RLock()
	for _, paths := range m.deleteReservations {
		for _, target := range paths {
			probe, probeErr := captureWriteIdentityProbe(target.physical, false)
			if probeErr != nil {
				m.mu.RUnlock()
				t.Fatalf("capture selected-delete target %q: %v", target.physical, probeErr)
			}
			targetProbes[target.physical] = probe
		}
	}
	m.mu.RUnlock()

	transient, _, err := m.CreateJob(DownloadRequest{Repo: "owner/transient", LocalDir: runDir})
	if err != nil {
		t.Fatalf("CreateJob for transient real run: %v", err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatalf("real downloader did not start: %+v", m.ListJobs())
	}
	finishDownload()
	if !m.WaitAll(5 * time.Second) {
		t.Fatal("real runner did not unwind after the controlled download response")
	}
	finished, ok := m.GetJob(transient.ID)
	if !ok || finished.Status != JobStatusCompleted {
		t.Fatalf("transient real run did not complete before removal: %#v", finished)
	}
	if result, _ := m.DismissJobResult(transient.ID); result != DismissJobOK {
		t.Fatalf("could not remove completed transient job: %v", result)
	}
	currentCandidateProbe, err := captureWriteIdentityProbe(candidateBase, true)
	if err != nil || !reflect.DeepEqual(candidateProbe, currentCandidateProbe) {
		t.Fatalf("candidate namespace probe changed during unrelated run: before=%+v after=%+v err=%v", candidateProbe, currentCandidateProbe, err)
	}
	for path, expected := range targetProbes {
		current, probeErr := captureWriteIdentityProbe(path, false)
		if probeErr != nil || !reflect.DeepEqual(expected, current) {
			t.Fatalf("selected-delete target probe changed during unrelated run: path=%q before=%+v after=%+v err=%v", path, expected, current, probeErr)
		}
	}
	after := m.captureWriteDecision()
	before.epoch = after.epoch
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("transient run did not return manager decision state to its initial shape except history:\nbefore=%#v\nafter=%#v", before, after)
	}

	close(releaseInventory)
	select {
	case accepted := <-restartResult:
		if !accepted {
			t.Fatal("restart was rejected instead of recollecting against the stable post-run state")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("off-lock restart did not finish after inventory was released")
	}
	select {
	case <-schedulerInventoryEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("accepted retry did not reach its separate scheduler inventory")
	}
	inventoryMu.Lock()
	gotInventoryCalls := inventoryCalls
	inventoryMu.Unlock()
	if gotInventoryCalls != 3 {
		t.Fatalf("candidate namespace inventories before scheduler release=%d, want two retry scans plus one gated scheduler scan", gotInventoryCalls)
	}
	if got, ok := m.GetJob(candidate.ID); !ok || (got.Status != JobStatusQueued && got.Status != JobStatusRunning && got.Status != JobStatusCompleted) || got.Progress.DownloadedBytes != 0 || len(got.Files) != 0 {
		t.Fatalf("restart did not commit its expected clean retry after fresh evidence: %#v", got)
	}
	close(releaseSchedulerInventory)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("dispatched restart runner did not reach its controlled download")
	}
	finishDownload()
	if !m.WaitAll(5 * time.Second) {
		t.Fatal("dispatched restart runner did not unwind")
	}
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRestartReservationStateChangeDuringInventoryDoesNotReset(t *testing.T) {
	root := t.TempDir()
	m := newJobManagerWithStatePath(Config{CacheDir: root}, nil, filepath.Join(root, "state"))
	job := addRestartCandidate(m, root, "changed", JobStatusFailed)
	addTwoDeleteReservations(t, m, root)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	m.writeInventory = func(base string) (writableNamespaceInventory, bool) {
		once.Do(func() { close(entered) })
		<-release
		return inventoryWritableNamespace(base)
	}
	done := make(chan bool, 1)
	go func() { done <- m.RetryJob(job.ID) }()
	<-entered
	m.mu.Lock()
	job.Status = JobStatusCompleted
	job.generation++
	job.Error = "preserve"
	m.bumpWriteDecisionLocked()
	m.mu.Unlock()
	close(release)
	if <-done {
		t.Fatal("retry accepted after status/generation changed during inventory")
	}
	got, ok := m.GetJob(job.ID)
	if !ok || got.Status != JobStatusCompleted || got.Error != "preserve" || got.Progress.DownloadedBytes != 17 || len(got.Files) != 1 {
		t.Fatalf("stale attempt reset job state: %#v", got)
	}
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRestartReservationGenerationChangeForcesFreshInventory(t *testing.T) {
	root := t.TempDir()
	m := newJobManagerWithStatePath(Config{CacheDir: root}, nil, filepath.Join(root, "state"))
	job := addRestartCandidate(m, root, "generation", JobStatusFailed)
	addTwoDeleteReservations(t, m, root)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	calls := 0
	m.writeInventory = func(base string) (writableNamespaceInventory, bool) {
		calls++
		once.Do(func() { close(entered) })
		<-release
		return inventoryWritableNamespace(base)
	}
	done := make(chan bool, 1)
	go func() { done <- m.RetryJob(job.ID) }()
	<-entered
	m.mu.Lock()
	job.generation++ // Keep retry eligibility; only the manager decision changes.
	m.bumpWriteDecisionLocked()
	m.mu.Unlock()
	close(release)
	if !<-done {
		t.Fatal("retry did not succeed after collecting fresh evidence")
	}
	if calls != 4 {
		t.Fatalf("inventory calls=%d, want two writable bases inventoried again after generation change", calls)
	}
	got, ok := m.GetJob(job.ID)
	if !ok || got.Status != JobStatusQueued || got.generation != 1 || got.Progress.DownloadedBytes != 0 || len(got.Files) != 0 {
		t.Fatalf("freshly admitted retry state = %#v", got)
	}
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRestartReservationCloseDuringInventoryDrainsWithoutReset(t *testing.T) {
	root := t.TempDir()
	m := newJobManagerWithStatePath(Config{CacheDir: root}, nil, filepath.Join(root, "state"))
	job := addRestartCandidate(m, root, "closing", JobStatusPaused)
	addTwoDeleteReservations(t, m, root)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	m.writeInventory = func(base string) (writableNamespaceInventory, bool) {
		once.Do(func() { close(entered) })
		<-release
		return inventoryWritableNamespace(base)
	}
	done := make(chan bool, 1)
	go func() { done <- m.ResumeJob(job.ID) }()
	<-entered
	closeDone := make(chan error, 1)
	go func() { closeDone <- m.Close(context.Background()) }()
	for {
		m.mu.RLock()
		stopping := m.stopping
		m.mu.RUnlock()
		if stopping {
			break
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	if <-done {
		t.Fatal("resume accepted after Close began")
	}
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
	got, ok := m.GetJob(job.ID)
	if !ok || got.Status != JobStatusPaused || got.Progress.DownloadedBytes != 17 || len(got.Files) != 1 {
		t.Fatalf("Close-time inventory reset job state: %#v", got)
	}
}
