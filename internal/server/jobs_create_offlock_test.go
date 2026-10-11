package server

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
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
