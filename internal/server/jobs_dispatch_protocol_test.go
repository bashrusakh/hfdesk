package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestReservationDispatchCollectsOffLockAndCoalescesRequests(t *testing.T) {
	root := t.TempDir()
	m := newJobManagerWithStatePath(Config{MaxActive: 1}, nil, filepath.Join(root, "state"))
	defer m.Close(context.Background())
	job := &Job{ID: "queued", Repo: "owner/repo", OutputDir: root, Status: JobStatusQueued, CreatedAt: time.Now(), partialFilesMu: &sync.Mutex{}}
	m.jobs[job.ID] = job
	entered, release := make(chan struct{}), make(chan struct{})
	var calls int
	m.writeInventory = func(base string) (writableNamespaceInventory, bool) {
		m.mu.Lock()
		calls++
		if calls == 1 {
			close(entered)
		}
		m.mu.Unlock()
		<-release
		return inventoryWritableNamespace(base)
	}
	target := filepath.Join(jobDestinationBase(job), "some-file.gguf")
	m.mu.Lock()
	m.deleteReservations[1] = []string{target}
	m.bumpWriteDecisionLocked()
	m.dispatchLocked()
	m.mu.Unlock()
	<-entered // dispatch holds no manager lock while collecting
	if got, ok := m.GetJob(job.ID); !ok || got.Status != JobStatusQueued {
		t.Fatal("manager read blocked or job changed during inventory")
	}
	if !m.CancelJob(job.ID) {
		t.Fatal("cancel blocked or failed during inventory")
	}
	m.mu.Lock()
	m.dispatchLocked()
	m.dispatchLocked()
	m.mu.Unlock()
	close(release)
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if calls != 2 {
		t.Fatalf("inventory calls = %d, want two namespaces in one coalesced worker attempt", calls)
	}
	if job.starting || job.Status != JobStatusCancelled {
		t.Fatalf("job state after concurrent cancel = %q, starting=%v", job.Status, job.starting)
	}
	if len(m.jobs) != 1 {
		t.Fatal("unexpected job count")
	}
}

func TestReservationDispatchCloseDuringCollectDoesNotStart(t *testing.T) {
	root := t.TempDir()
	m := newJobManagerWithStatePath(Config{MaxActive: 1}, nil, filepath.Join(root, "state"))
	job := &Job{ID: "queued", Repo: "owner/repo", OutputDir: root, Status: JobStatusQueued, CreatedAt: time.Now(), partialFilesMu: &sync.Mutex{}}
	m.jobs[job.ID] = job
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	m.writeInventory = func(base string) (writableNamespaceInventory, bool) {
		once.Do(func() { close(entered) })
		<-release
		return inventoryWritableNamespace(base)
	}
	m.mu.Lock()
	m.deleteReservations[1] = []string{filepath.Join(jobDestinationBase(job), "some-file.gguf")}
	m.bumpWriteDecisionLocked()
	m.dispatchLocked()
	m.mu.Unlock()
	<-entered
	done := make(chan error, 1)
	go func() { done <- m.Close(context.Background()) }()
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if job.starting || job.Status == JobStatusRunning {
		t.Fatal("job started after shutdown fence")
	}
}

func TestReservationDispatchKeepsYoungerQueuedBehindBlockedHead(t *testing.T) {
	root := t.TempDir()
	m := newJobManagerWithStatePath(Config{MaxActive: 2}, nil, filepath.Join(root, "state"))
	older := &Job{ID: "older", Repo: "owner/older", LocalDir: filepath.Join(root, "older"), Flat: true, Filters: []string{"blocked.gguf"}, Status: JobStatusQueued, CreatedAt: time.Now(), partialFilesMu: &sync.Mutex{}}
	younger := &Job{ID: "younger", Repo: "owner/younger", LocalDir: filepath.Join(root, "younger"), Flat: true, Filters: []string{"other.gguf"}, Status: JobStatusQueued, CreatedAt: older.CreatedAt.Add(time.Second), partialFilesMu: &sync.Mutex{}}
	m.jobs[older.ID], m.jobs[younger.ID] = older, younger
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	m.writeInventory = func(base string) (writableNamespaceInventory, bool) {
		once.Do(func() { close(entered); <-release })
		return inventoryWritableNamespace(base)
	}
	target := filepath.Join(jobDestinationBase(older), "blocked.gguf")
	m.mu.Lock()
	m.deleteReservations[1] = []string{target}
	m.bumpWriteDecisionLocked()
	m.dispatchLocked()
	m.mu.Unlock()
	<-entered
	finished := m.writeDecisionChangeChannel()
	close(release)
	<-finished
	if jobMayWriteSelectedPaths(younger, []string{target}, inventoryWritableNamespace) {
		t.Fatal("younger job was not independently eligible for the reserved path")
	}
	m.mu.RLock()
	if older.Status != JobStatusQueued || older.starting || younger.Status != JobStatusQueued || younger.starting {
		t.Fatalf("FIFO bypass: older=(%s,%v), younger=(%s,%v)", older.Status, older.starting, younger.Status, younger.starting)
	}
	m.mu.RUnlock()
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestReservationDispatchRejectsCapacitySnapshotChangedBeforeCommit(t *testing.T) {
	root := t.TempDir()
	requests := make(chan struct{}, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- struct{}{}
		<-r.Context().Done()
	}))
	defer server.Close()
	m := newJobManagerWithStatePath(Config{CacheDir: root, MaxActive: 2, Endpoint: server.URL}, nil, filepath.Join(root, "state"))
	defer m.Close(context.Background())
	jobs := []*Job{
		{ID: "first", Repo: "owner/first", LocalDir: filepath.Join(root, "first"), Flat: true, Status: JobStatusQueued, CreatedAt: time.Now(), partialFilesMu: &sync.Mutex{}},
		{ID: "second", Repo: "owner/second", LocalDir: filepath.Join(root, "second"), Flat: true, Status: JobStatusQueued, CreatedAt: time.Now().Add(time.Second), partialFilesMu: &sync.Mutex{}},
	}
	for _, job := range jobs {
		m.jobs[job.ID] = job
		if err := os.MkdirAll(jobDestinationBase(job), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseInventory := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseInventory()
	var once sync.Once
	var inventoryCalls int
	m.writeInventory = func(base string) (writableNamespaceInventory, bool) {
		m.mu.Lock()
		inventoryCalls++
		m.mu.Unlock()
		once.Do(func() { close(entered) })
		<-release
		return inventoryWritableNamespace(base)
	}
	m.mu.Lock()
	m.deleteReservations[1] = []string{filepath.Join(root, "reserved.gguf")}
	m.bumpWriteDecisionLocked()
	m.dispatchLocked()
	m.mu.Unlock()
	<-entered

	// Change capacity after the dispatch snapshot was taken but while its
	// off-lock namespace inventory is still in progress.
	m.UpdateConfig(Config{CacheDir: root, MaxActive: 1, Endpoint: server.URL})
	releaseInventory()
	select {
	case <-requests:
	case <-time.After(5 * time.Second):
		m.mu.RLock()
		first, firstStarting, second, secondStarting := jobs[0].Status, jobs[0].starting, jobs[1].Status, jobs[1].starting
		dispatchRunning, dispatchPending, inventoryCount := m.dispatchRunning, m.dispatchPending, inventoryCalls
		m.mu.RUnlock()
		t.Fatalf("no queued job reached the blocking endpoint: first=%s/%v second=%s/%v dispatcher=%v pending=%v inventories=%d", first, firstStarting, second, secondStarting, dispatchRunning, dispatchPending, inventoryCount)
	}
	m.mu.RLock()
	active, queued := 0, 0
	for _, job := range jobs {
		if job.Status == JobStatusRunning || job.starting {
			active++
		}
		if job.Status == JobStatusQueued {
			queued++
		}
	}
	m.mu.RUnlock()
	if active != 1 || queued != 1 {
		t.Fatalf("stale capacity snapshot started wrong prefix: active=%d queued=%d", active, queued)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.Close(ctx); err != nil {
		t.Fatal(err)
	}
}
