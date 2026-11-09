// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newLifecycleTestManager(t *testing.T, cfg Config) *JobManager {
	t.Helper()
	m := newJobManagerWithStatePath(cfg, nil, filepath.Join(t.TempDir(), "jobs.json"))
	t.Cleanup(func() {
		select {
		case <-m.closeDone:
			return
		default:
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := m.Close(ctx); err != nil {
			t.Errorf("close lifecycle test manager: %v", err)
			<-m.closeDone
		}
	})
	return m
}

func TestJobManagerQuiesceFencesAlreadyDispatchedRunner(t *testing.T) {
	m := newLifecycleTestManager(t, Config{MaxActive: 1})
	job := &Job{ID: "starting", Repo: "test/repo", Status: JobStatusQueued, CreatedAt: time.Now(), partialFilesMu: &sync.Mutex{}}
	m.mu.Lock()
	m.jobs[job.ID] = job
	m.dispatchLocked()
	if !job.starting {
		m.mu.Unlock()
		t.Fatal("expected runner to be dispatched before fence")
	}
	m.quiesceLocked()
	m.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := m.Close(ctx)
	assertNoError(t, err, "")
	got, ok := m.GetJob(job.ID)
	if !ok || got.Status != JobStatusQueued || got.starting {
		t.Fatalf("starting runner crossed fence: %#v", got)
	}
}

func TestJobManagerClosePausesRealRunnerAndPreservesPartials(t *testing.T) {
	entered := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-r.Context().Done()
	}))
	defer server.Close()
	m := newLifecycleTestManager(t, Config{CacheDir: t.TempDir(), Endpoint: server.URL, MaxActive: 1})
	job, _, err := m.CreateJob(DownloadRequest{Repo: "owner/model"})
	assertNoError(t, err, "")
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("runner did not reach the blocking endpoint")
	}
	m.mu.Lock()
	m.jobs["queued"] = &Job{ID: "queued", Repo: "owner/queued", Status: JobStatusQueued, CreatedAt: time.Now()}
	m.mu.Unlock()

	partial := filepath.Join(t.TempDir(), "resume.part")
	if err := os.WriteFile(partial, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	m.mu.RLock()
	live := m.jobs[job.ID]
	m.mu.RUnlock()
	live.partialFilesMu.Lock()
	partials := map[string]struct{}{partial: {}}
	live.partialFilesPtr = &partials
	live.partialFilesMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.Close(ctx); err != nil {
		t.Fatal(err)
	}
	got, ok := m.GetJob(job.ID)
	if !ok || got.Status != JobStatusPaused {
		t.Fatalf("runner was not paused at shutdown: %#v", got)
	}
	if queued, ok := m.GetJob("queued"); !ok || queued.Status != JobStatusQueued || queued.starting {
		t.Fatalf("queued job started during shutdown: %#v", queued)
	}
	if _, err := os.Stat(partial); err != nil {
		t.Fatalf("shutdown removed resumable partial: %v", err)
	}
}

func TestJobManagerCompletionDuringQuiesceDoesNotDispatchQueue(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-entered:
		default:
			close(entered)
		}
		select {
		case <-release:
			http.Error(w, "test terminal response", http.StatusNotFound)
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	m := newLifecycleTestManager(t, Config{CacheDir: t.TempDir(), Endpoint: server.URL, MaxActive: 1})
	updates := m.Subscribe()
	defer m.Unsubscribe(updates)
	first, _, err := m.CreateJob(DownloadRequest{Repo: "owner/active"})
	assertNoError(t, err, "")
	if _, _, err := m.CreateJob(DownloadRequest{Repo: "owner/queued"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("active runner did not reach endpoint")
	}
	m.Quiesce()
	close(release)
	deadline := time.After(5 * time.Second)
	for {
		select {
		case update := <-updates:
			if update.ID == first.ID && update.Status == JobStatusFailed {
				goto completed
			}
		case <-deadline:
			t.Fatal("runner did not complete while manager was fenced")
		}
	}

completed:
	var queued *Job
	for _, job := range m.ListJobs() {
		if job.Repo == "owner/queued" {
			queued = job
		}
	}
	if queued == nil || queued.Status != JobStatusQueued || queued.starting {
		t.Fatalf("completion dispatched queued work through fence: %#v", queued)
	}
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestJobManagerCloseDrainsAcceptedMutationAndSharesResult(t *testing.T) {
	m := newLifecycleTestManager(t, Config{})
	m.jobs["paused"] = &Job{ID: "paused", Status: JobStatusPaused}
	saveStarted := make(chan struct{})
	allowSave := make(chan struct{})
	var calls int
	m.persistStateFile = func(path string, jobs []*Job) error {
		calls++
		if calls == 1 {
			close(saveStarted)
			<-allowSave
		}
		return saveJobsState(path, jobs)
	}
	cancelDone := make(chan bool, 1)
	go func() { cancelDone <- m.CancelJob("paused") }()
	select {
	case <-saveStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("accepted cancel did not reach its persistence work")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.Close(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Close with cancelled wait context = %v, want context.Canceled", err)
	}
	select {
	case <-m.closeDone:
		t.Fatal("close completed before accepted mutation drained")
	default:
	}
	close(allowSave)
	if !<-cancelDone {
		t.Fatal("accepted cancel was rejected")
	}
	select {
	case <-m.closeDone:
	case <-time.After(5 * time.Second):
		t.Fatal("close coordinator did not finish after mutation drained")
	}
	err := m.Close(context.Background())
	assertNoError(t, err, "repeated Close changed result: %v", err)
	if calls != 2 {
		t.Fatalf("persistence calls = %d, want mutation + final snapshot", calls)
	}
	loaded, err := loadJobsState(m.statePath)
	assertNoError(t, err, "")
	if len(loaded) != 1 || loaded[0].Status != JobStatusCancelled {
		t.Fatalf("final snapshot omitted accepted mutation: %#v", loaded)
	}
}

func TestJobManagerUpdateConfigWorkDrainsDuringFence(t *testing.T) {
	m := newLifecycleTestManager(t, Config{MaxActive: 2})
	olderStart, newerStart := time.Now().Add(-time.Minute), time.Now()
	m.jobs["older"] = &Job{ID: "older", Status: JobStatusRunning, StartedAt: &olderStart}
	m.jobs["newer"] = &Job{ID: "newer", Status: JobStatusRunning, StartedAt: &newerStart}
	startCancel := make(chan struct{})
	allowCancel := make(chan struct{})
	m.jobs["newer"].cancel = func() {
		// GetJob takes m.mu; this succeeds only when cancel runs after unlock.
		m.GetJob("newer")
		close(startCancel)
		<-allowCancel
	}
	m.Quiesce()
	updateDone := make(chan struct{})
	go func() {
		m.UpdateConfig(Config{MaxActive: 1})
		close(updateDone)
	}()
	select {
	case <-startCancel:
	case <-time.After(5 * time.Second):
		t.Fatal("UpdateConfig did not reach its outside-lock cancellation")
	}
	if got, _ := m.GetJob("newer"); got.Status != JobStatusQueued || got.starting {
		t.Fatalf("limit requeue started while quiesced: %#v", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.Close(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Close error = %v, want context.Canceled", err)
	}
	select {
	case <-m.closeDone:
		t.Fatal("close did not wait for accepted UpdateConfig work")
	default:
	}
	close(allowCancel)
	select {
	case <-updateDone:
	case <-time.After(5 * time.Second):
		t.Fatal("UpdateConfig did not finish after cancellation released")
	}
	select {
	case <-m.closeDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not drain UpdateConfig")
	}
}

func TestJobManagerFinalSnapshotAndNoWritesAfterClose(t *testing.T) {
	m := newLifecycleTestManager(t, Config{MaxActive: 1})
	m.jobs["paused"] = &Job{ID: "paused", Repo: "owner/model", Status: JobStatusPaused}
	var calls int
	m.persistStateFile = func(path string, jobs []*Job) error {
		calls++
		return saveJobsState(path, jobs)
	}
	m.Quiesce()
	if !m.ResumeJob("paused") {
		t.Fatal("resume mutation should be admitted while fenced")
	}
	if got, _ := m.GetJob("paused"); got.Status != JobStatusQueued || got.starting {
		t.Fatalf("quiesce dispatched a resumed job: %#v", got)
	}
	err := m.Close(context.Background())
	assertNoError(t, err, "")
	loaded, err := loadJobsState(m.statePath)
	assertNoError(t, err, "")
	if len(loaded) != 1 || loaded[0].Status != JobStatusPaused {
		t.Fatalf("final snapshot did not include unsaved mutation: %#v", loaded)
	}
	if m.CancelJob("paused") || m.PauseJob("paused") || m.ResumeJob("paused") || m.RetryJob("paused") || m.DeleteJob("paused") {
		t.Fatal("mutator succeeded after stopping")
	}
	if result, _ := m.DismissJobResult("paused"); result == DismissJobOK {
		t.Fatal("dismiss succeeded after stopping")
	}
	if _, _, err := m.CreateJob(DownloadRequest{Repo: "new/job"}); !errors.Is(err, errJobManagerStopping) {
		t.Fatalf("CreateJob error = %v, want stopping error", err)
	}
	if err := m.saveState(); !errors.Is(err, errJobManagerPersistenceClosed) {
		t.Fatalf("late save error = %v, want persistence closed", err)
	}
	if calls != 1 {
		t.Fatalf("late save wrote state: persistence calls = %d", calls)
	}
}

func TestJobManagerCloseReturnsFinalPersistenceErrorOnEveryCall(t *testing.T) {
	m := newLifecycleTestManager(t, Config{})
	want := errors.New("final write failed")
	m.persistStateFile = func(string, []*Job) error { return want }
	if err := m.Close(context.Background()); !errors.Is(err, want) {
		t.Fatalf("first Close error = %v, want %v", err, want)
	}
	if err := m.Close(context.Background()); !errors.Is(err, want) {
		t.Fatalf("repeated Close error = %v, want same result %v", err, want)
	}
	if err := m.saveState(); !errors.Is(err, errJobManagerPersistenceClosed) {
		t.Fatalf("save after failed close = %v, want closed", err)
	}
}

func TestJobManagerCloseTimeoutDoesNotCancelFinalPersistence(t *testing.T) {
	m := newLifecycleTestManager(t, Config{})
	writeStarted := make(chan struct{})
	allowWrite := make(chan struct{})
	m.persistStateFile = func(string, []*Job) error {
		close(writeStarted)
		<-allowWrite
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.Close(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Close error = %v, want context.Canceled", err)
	}
	select {
	case <-writeStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("final persistence did not start")
	}
	select {
	case <-m.closeDone:
		t.Fatal("close reported completion during blocked persistence")
	default:
	}
	close(allowWrite)
	select {
	case <-m.closeDone:
	case <-time.After(5 * time.Second):
		t.Fatal("close did not complete after persistence returned")
	}
	err := m.Close(context.Background())
	assertNoError(t, err, "repeated Close result = %v", err)
}

func TestJobManagerCloseWaitsForTerminalHistoryAndReportsWriteFailures(t *testing.T) {
	for _, tc := range []struct {
		name, first string
		status      JobStatus
		historyErr  error
	}{{"completed", "", JobStatusCompleted, nil}, {"failed", "fail", JobStatusFailed, errors.New("history write failed")}} {
		t.Run(tc.name, func(t *testing.T) {
			isolateCacheState(t)
			endpoint, _, _, _ := cacheDownloadFixture(t, tc.first)
			m := newLifecycleTestManager(t, Config{CacheDir: t.TempDir(), Endpoint: endpoint.URL, MaxActive: 1})
			entered, release := make(chan struct{}), make(chan struct{})
			// The runner blocks in appendHistory until the test releases it.
			// The release is idempotent and runs at the end of the test, so
			// an assertion that fails first never leaves the runner blocked
			// until the suite times out.
			var releaseOnce sync.Once
			releaseHistory := func() { releaseOnce.Do(func() { close(release) }) }
			t.Cleanup(releaseHistory)
			realAppend, historyErr := m.appendHistory, tc.historyErr
			m.appendHistory = func(terminal *Job) error {
				close(entered)
				<-release
				if historyErr != nil {
					return historyErr
				}
				return realAppend(terminal)
			}
			job, _, err := m.CreateJob(DownloadRequest{Repo: "owner/model"})
			assertNoError(t, err, "")
			select {
			case <-entered:
			case <-time.After(10 * time.Second):
				t.Fatal("terminal runner did not reach history persistence")
			}
			if got, _ := m.GetJob(job.ID); got.Status != tc.status {
				t.Fatalf("terminal status = %q, want %q", got.Status, tc.status)
			}

			var finalErr error
			if historyErr != nil {
				finalErr = errors.New("final job snapshot failed")
				realPersist := m.persistStateFile
				m.persistStateFile = func(path string, jobs []*Job) error {
					return errors.Join(realPersist(path, jobs), finalErr)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := m.Close(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("Close while terminal history is blocked = %v, want context.Canceled", err)
			}
			// The close coordinator (finishClose) may close closeDone
			// only after the runner releases runWG, and the runner
			// holds runWG for the whole history write — but the check
			// has to give the coordinator goroutine a window to run
			// before it can observe that it waits: an instant
			// nonblocking select could pass while the goroutine is
			// still unscheduled, which proves nothing either way.
			// There is no in-process signal of the coordinator before
			// the history is released — its first actions are
			// WaitGroup waits, and its first observable call (the
			// final persistStateFile) happens only after the drain —
			// so a bounded observation is the strongest evidence this
			// test can hold without a production seam. The window
			// cannot fail on a correct coordinator: closeDone closes
			// only behind runWG.Wait, which the blocked runner keeps
			// closed for the whole window.
			select {
			case <-m.closeDone:
				t.Fatal("close completed before the terminal history write")
			case <-time.After(time.Second):
			}

			releaseHistory()
			ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
			closeErr := m.Close(ctx)
			cancel()
			if historyErr != nil {
				if !errors.Is(closeErr, historyErr) || !errors.Is(closeErr, finalErr) {
					t.Fatalf("close error = %v, want both history and final snapshot errors", closeErr)
				}
				return
			}
			assertNoError(t, closeErr, "Close after successful history persistence: %v", closeErr)
			entries, err := LoadHistory()
			assertNoError(t, err, "")
			if len(entries) != 1 || entries[0].ID != job.ID || entries[0].Status != tc.status {
				t.Fatalf("persisted terminal history = %#v, want job %s with status %s", entries, job.ID, tc.status)
			}
		})
	}
}

func TestJobManagerClosePreventsLateLoadMerge(t *testing.T) {
	m := newLifecycleTestManager(t, Config{})
	loadStarted := make(chan struct{})
	allowLoad := make(chan struct{})
	m.loadStateFile = func(string) ([]*Job, error) {
		close(loadStarted)
		<-allowLoad
		return []*Job{{ID: "late", Repo: "owner/late", Status: JobStatusRunning}}, nil
	}
	loadDone := make(chan struct{})
	go func() {
		m.LoadState()
		close(loadDone)
	}()
	select {
	case <-loadStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("LoadState did not start")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.Close(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Close error = %v, want context.Canceled", err)
	}
	select {
	case <-m.closeDone:
		t.Fatal("close did not wait for in-flight LoadState")
	default:
	}
	close(allowLoad)
	select {
	case <-loadDone:
	case <-time.After(5 * time.Second):
		t.Fatal("LoadState did not finish")
	}
	select {
	case <-m.closeDone:
	case <-time.After(5 * time.Second):
		t.Fatal("close did not finish after LoadState")
	}
	if _, ok := m.GetJob("late"); ok {
		t.Fatal("late LoadState merged data after manager entered stopping")
	}
}
