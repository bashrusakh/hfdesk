package server

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func createWithDeleteTargets(t *testing.T, m *JobManager, targets ...string) {
	t.Helper()
	m.mu.Lock()
	// Keep dispatcher behavior outside this CreateJob-only test boundary.
	m.fenced = true
	m.deleteReservations[1] = append([]string(nil), targets...)
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
	m.deleteReservations[1] = []string{filepath.Join(root, "one.gguf")}
	m.deleteReservations[2] = []string{filepath.Join(root, "two.gguf")}
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
	m.deleteReservations[2] = []string{filepath.Join(root, "transient.gguf")}
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
	close(release)
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

func addTwoDeleteReservations(m *JobManager, root string) {
	m.mu.Lock()
	m.deleteReservations[1] = []string{filepath.Join(root, "reserved-one.gguf")}
	m.deleteReservations[2] = []string{filepath.Join(root, "reserved-two.gguf")}
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
			addTwoDeleteReservations(m, root)
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

func TestRestartReservationStateChangeDuringInventoryDoesNotReset(t *testing.T) {
	root := t.TempDir()
	m := newJobManagerWithStatePath(Config{CacheDir: root}, nil, filepath.Join(root, "state"))
	job := addRestartCandidate(m, root, "changed", JobStatusFailed)
	addTwoDeleteReservations(m, root)
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
	addTwoDeleteReservations(m, root)
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
	addTwoDeleteReservations(m, root)
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
