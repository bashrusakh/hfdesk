// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
)

// JobStatus represents the state of a download job.
type JobStatus string

const (
	JobStatusQueued    JobStatus = "queued"
	JobStatusRunning   JobStatus = "running"
	JobStatusPaused    JobStatus = "paused"
	JobStatusCompleted JobStatus = "completed"
	JobStatusFailed    JobStatus = "failed"
	JobStatusCancelled JobStatus = "cancelled"
)

// Job represents a download job.
type Job struct {
	ID         string            `json:"id"`
	Repo       string            `json:"repo"`
	Revision   string            `json:"revision"`
	IsDataset  bool              `json:"isDataset,omitempty"`
	Filters    []string          `json:"filters,omitempty"`
	Excludes   []string          `json:"excludes,omitempty"`
	OutputDir  string            `json:"outputDir"`
	LocalDir   string            `json:"localDir,omitempty"`   // Effective local-dir for this job (per-request or server-global)
	LocalRepo  string            `json:"localRepo,omitempty"`  // Override destination folder name (used for upstream mmproj)
	Flat       bool              `json:"flat,omitempty"`       // Save real files (flat mode) instead of HF cache layout
	ExactMatch bool              `json:"exactMatch,omitempty"` // Match filters by whole name segment, not substring
	Status     JobStatus         `json:"status"`
	Phase      string            `json:"phase,omitempty"` // sub-state while running, e.g. "finalizing"
	Progress   JobProgress       `json:"progress"`
	Error      string            `json:"error,omitempty"`
	CreatedAt  time.Time         `json:"createdAt"`
	StartedAt  *time.Time        `json:"startedAt,omitempty"`
	EndedAt    *time.Time        `json:"endedAt,omitempty"`
	Files      []JobFileProgress `json:"files,omitempty"`

	cancel     context.CancelFunc `json:"-"`
	generation int                `json:"-"` // Tracks which runJob instance is current
	starting   bool               `json:"-"` // Dispatched to a runJob goroutine but not yet Running (scheduler gate)

	// resumeFloor is the monotonic floor on Progress.DownloadedBytes
	// during the recovery window that starts when ResumeJob re-queues a
	// paused job. While set, applyJobProgress only updates
	// DownloadedBytes when the freshly summed total exceeds the current
	// value, and clears the floor once the sum catches up. This keeps
	// the UI from dropping while the partial sum is dominated by files
	// the downloader has not yet processed, and is then released so a
	// genuine rollback (e.g. downloadSingle's 200 fallback truncating
	// the .part file) is reflected in real time. 0 means no floor.
	resumeFloor int64 `json:"-"`

	// Speed is a moving average over a short window of bytes actually
	// transferred this run (see the file_progress handler), so the reading is
	// steady and resuming a partial repo doesn't count already-present bytes.
	speedSamples []speedSample `json:"-"`
	// speedRunStart is when this run's first speed sample was taken; the
	// whole-run average rate measured from it anchors the ETA estimate.
	speedRunStart time.Time `json:"-"`
}

// speedSample is one (time, cumulative-transferred-bytes) point in a job's
// speed window.
type speedSample struct {
	t     time.Time
	bytes int64
}

// JobProgress holds aggregate progress info.
type JobProgress struct {
	TotalFiles      int   `json:"totalFiles"`
	CompletedFiles  int   `json:"completedFiles"`
	TotalBytes      int64 `json:"totalBytes"`
	DownloadedBytes int64 `json:"downloadedBytes"`
	BytesPerSecond  int64 `json:"bytesPerSecond"`
	// EtaSeconds is the server-computed remaining-time estimate; 0 means
	// unknown. It is anchored to the whole-run average rate rather than the
	// displayed speed, because remaining/currentSpeed multiplies any speed
	// wobble by the bytes left and makes the ETA jump.
	EtaSeconds int64 `json:"etaSeconds"`
}

// JobFileProgress holds per-file progress.
type JobFileProgress struct {
	Path       string `json:"path"`
	TotalBytes int64  `json:"totalBytes"`
	Downloaded int64  `json:"downloaded"`
	Status     string `json:"status"` // pending, active, complete, skipped, error

	progressed     bool  `json:"-"` // saw a real file_progress event for this file
	baseDownloaded int64 `json:"-"` // bytes already on disk at first progress (resume position), excluded from speed
}

// JobManager manages download jobs.
type JobManager struct {
	mu          sync.RWMutex
	jobs        map[string]*Job
	config      Config
	listeners   []chan *Job
	listenerMu  sync.RWMutex
	wsHub       *WSHub
	wsCoalescer *jobCoalescer
	// speedLimiter is the process-wide token bucket shared by every running
	// job so the configured cap limits total bandwidth, not per-job. Always
	// non-nil; a 0 limit means unlimited.
	speedLimiter *hfdownloader.RateLimiter
	// runWG tracks in-flight runJob goroutines so shutdown paths (and
	// tests) can wait for every download to actually unwind — not just
	// for Status to flip to Cancelled. Without this a t.TempDir cleanup
	// can race a still-in-flight mkdir inside the downloader and fail
	// with "directory not empty".
	runWG sync.WaitGroup
}

// wsBroadcastMinGap is the minimum interval between consecutive WebSocket
// broadcasts for the same job. Progress events arriving inside this window
// are coalesced — only the latest job state is flushed when the window
// elapses. Terminal status changes (completed, failed, cancelled, paused)
// bypass this gate and are sent immediately. See github issue #62.
const wsBroadcastMinGap = 250 * time.Millisecond

// NewJobManager creates a new job manager.
func NewJobManager(cfg Config, wsHub *WSHub) *JobManager {
	m := &JobManager{
		jobs:         make(map[string]*Job),
		config:       cfg,
		wsHub:        wsHub,
		speedLimiter: hfdownloader.NewRateLimiter(hfdownloader.ParseSize(cfg.MaxSpeed)),
	}
	if wsHub != nil {
		m.wsCoalescer = newJobCoalescer(wsBroadcastMinGap, func(j *Job) {
			wsHub.BroadcastJob(j)
		})
	}
	return m
}

// LoadState restores jobs from jobs_state.json. Call once after NewJobManager,
// before accepting requests. Errors are non-fatal (logged but not returned).
func (m *JobManager) LoadState() {
	jobs, err := LoadJobsState()
	if err != nil {
		log.Printf("warning: could not load jobs state: %v", err)
		return
	}
	if len(jobs) == 0 {
		return
	}
	m.mu.Lock()
	for _, j := range jobs {
		j.cancel = nil
		// Ensure no zombie running/queued jobs survive a restart
		if j.Status == JobStatusRunning || j.Status == JobStatusQueued {
			j.Status = JobStatusPaused
		}
		m.jobs[j.ID] = j
	}
	m.mu.Unlock()
	log.Printf("restored %d job(s) from state file", len(jobs))
}

// saveStateLocked persists all jobs. Must NOT be called while m.mu is held
// (it takes its own lock internally).
func (m *JobManager) saveState() {
	m.mu.RLock()
	snapshot := make([]*Job, 0, len(m.jobs))
	for _, j := range m.jobs {
		snapshot = append(snapshot, m.cloneJobLocked(j))
	}
	m.mu.RUnlock()

	if err := SaveJobsState(snapshot); err != nil {
		log.Printf("warning: could not save jobs state: %v", err)
	}
}

// generateID creates a short random ID.
func generateID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// cloneJobLocked returns a fully-independent copy of a Job. Must be called
// while m.mu is held (any lock, read or write) so the fields being copied
// are stable. The returned *Job can be safely handed to JSON encoders or
// WebSocket broadcasters without racing against runJob's in-place mutations
// of the live Job stored in m.jobs. Slice fields are deep-copied so that
// subsequent mutations of the live job's slices can't leak through a shared
// backing array.
func (m *JobManager) cloneJobLocked(j *Job) *Job {
	if j == nil {
		return nil
	}
	clone := *j
	clone.cancel = nil
	if j.Filters != nil {
		clone.Filters = append([]string(nil), j.Filters...)
	}
	if j.Excludes != nil {
		clone.Excludes = append([]string(nil), j.Excludes...)
	}
	if j.Files != nil {
		clone.Files = append([]JobFileProgress(nil), j.Files...)
	}
	if j.StartedAt != nil {
		t := *j.StartedAt
		clone.StartedAt = &t
	}
	if j.EndedAt != nil {
		t := *j.EndedAt
		clone.EndedAt = &t
	}
	return &clone
}

// stringSlicesEqual reports whether two string slices contain the same
// elements regardless of order.
func stringSlicesEqual(a, b []string) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		seen[s]--
		if seen[s] < 0 {
			return false
		}
	}
	return true
}

// CreateJob creates a new download job.
// Returns existing job only when repo, revision, dataset AND filters match
// an active job. Different filters on the same repo (e.g. Q4_K_M vs mmproj-f16)
// create independent jobs.
func (m *JobManager) CreateJob(req DownloadRequest) (*Job, bool, error) {
	revision := req.Revision
	if revision == "" {
		revision = "main"
	}

	// Snapshot config under the lock so a concurrent settings update can't race
	// these reads (UpdateConfig replaces m.config under the write lock).
	cfg := m.snapshotConfig()

	// Use HuggingFace cache directory (v3 mode)
	cacheDir := cfg.CacheDir
	if cacheDir == "" {
		cacheDir = hfdownloader.DefaultCacheDir()
	}

	// Determine effective local-dir: per-request overrides server-global.
	// If either is set, use flat/real-file mode; otherwise use HF cache layout.
	effectiveLocalDir := cfg.LocalDir
	if req.LocalDir != "" {
		effectiveLocalDir = req.LocalDir
	}

	flat := effectiveLocalDir != ""
	outputDir := cacheDir
	if flat {
		outputDir = effectiveLocalDir
	}

	// Check for existing active job with identical parameters.
	// Deduplication is filter-aware: only match when filters and excludes
	// are also identical, so the user can download a quantization and a
	// vision encoder (mmproj) for the same repo at the same time.
	m.mu.Lock()
	for _, existing := range m.jobs {
		if existing.Repo == req.Repo &&
			existing.Revision == revision &&
			existing.IsDataset == req.Dataset &&
			(existing.Status == JobStatusQueued || existing.Status == JobStatusRunning) &&
			stringSlicesEqual(existing.Filters, req.Filters) &&
			stringSlicesEqual(existing.Excludes, req.Excludes) {
			snapshot := m.cloneJobLocked(existing)
			m.mu.Unlock()
			return snapshot, true, nil
		}
	}

	job := &Job{
		ID:         generateID(),
		Repo:       req.Repo,
		Revision:   revision,
		IsDataset:  req.Dataset,
		Filters:    req.Filters,
		Excludes:   req.Excludes,
		OutputDir:  outputDir,
		LocalDir:   effectiveLocalDir,
		LocalRepo:  req.LocalRepo,
		Flat:       flat,
		ExactMatch: req.ExactMatch,
		Status:     JobStatusQueued,
		CreatedAt:  time.Now(),
		Progress:   JobProgress{},
	}

	m.jobs[job.ID] = job
	// Queue the job and let the scheduler start it only if we're under the
	// max-active limit; otherwise it waits as 'queued'.
	m.dispatchLocked()
	snapshot := m.cloneJobLocked(job)
	m.mu.Unlock()
	m.notifyListeners(snapshot)

	return snapshot, false, nil
}

// GetJob retrieves a snapshot of a job by ID. The returned pointer is a
// standalone copy; the caller can read its fields without racing against
// the runJob goroutine that owns the live version in m.jobs.
func (m *JobManager) GetJob(id string) (*Job, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	job, ok := m.jobs[id]
	if !ok {
		return nil, false
	}
	return m.cloneJobLocked(job), true
}

// ListJobs returns snapshots of all jobs. Each returned *Job is an
// independent copy — safe to JSON-encode or hand to the WebSocket hub
// without holding any lock.
func (m *JobManager) ListJobs() []*Job {
	m.mu.RLock()
	defer m.mu.RUnlock()

	jobs := make([]*Job, 0, len(m.jobs))
	for _, job := range m.jobs {
		jobs = append(jobs, m.cloneJobLocked(job))
	}
	return jobs
}

// CancelJob cancels a running or queued job.
func (m *JobManager) CancelJob(id string) bool {
	m.mu.Lock()
	job, ok := m.jobs[id]
	if !ok {
		m.mu.Unlock()
		return false
	}

	if job.Status != JobStatusQueued && job.Status != JobStatusRunning && job.Status != JobStatusPaused {
		m.mu.Unlock()
		return false
	}

	if job.cancel != nil {
		job.cancel()
	}
	job.Status = JobStatusCancelled
	now := time.Now()
	job.EndedAt = &now
	snapshot := m.cloneJobLocked(job)
	m.mu.Unlock()

	m.notifyListeners(snapshot)
	go m.saveState()
	return true
}

// PauseJob pauses a running job.
func (m *JobManager) PauseJob(id string) bool {
	m.mu.Lock()
	job, ok := m.jobs[id]
	if !ok {
		m.mu.Unlock()
		return false
	}

	if job.Status != JobStatusRunning {
		m.mu.Unlock()
		return false
	}

	if job.cancel != nil {
		job.cancel()
	}
	job.Status = JobStatusPaused
	snapshot := m.cloneJobLocked(job)
	m.mu.Unlock()

	m.notifyListeners(snapshot)
	go m.saveState()
	return true
}

// ResumeJob resumes a paused job.
func (m *JobManager) ResumeJob(id string) bool {
	m.mu.Lock()
	job, ok := m.jobs[id]
	if !ok {
		m.mu.Unlock()
		return false
	}

	if job.Status != JobStatusPaused {
		m.mu.Unlock()
		return false
	}

	job.Status = JobStatusQueued
	// Reset progress totals — the downloader will re-scan the repo and
	// emit fresh plan_item events that rebuild TotalFiles / TotalBytes.
	// Preserve DownloadedBytes as an initial estimate so the UI doesn't
	// jump from e.g. 60% to 0% while the scan runs. The downloader's
	// file_progress and file_done events recompute DownloadedBytes from
	// the live job.Files list as soon as files are processed (skipped
	// blob → immediate file_done; partial → file_progress at the on-disk
	// position from the .part file), so the estimate corrects itself
	// within a second or two. resumeFloor arms the monotonic guard in
	// applyJobProgress for this recovery window and is cleared by
	// applyJobProgress once the running sum catches up.
	oldBytes := job.Progress.DownloadedBytes
	job.Progress = JobProgress{}
	job.Progress.DownloadedBytes = oldBytes
	job.resumeFloor = oldBytes
	job.Files = nil
	snapshot := m.cloneJobLocked(job)
	// Re-queue through the scheduler so resuming respects max-active.
	m.dispatchLocked()
	m.mu.Unlock()

	// Notify listeners of status change
	m.notifyListeners(snapshot)

	return true
}

// RetryJob restarts a failed or cancelled job using its original
// parameters. The job keeps its ID but its progress is reset and it is
// re-queued and run again. Already-downloaded files are skipped by the
// downloader, so a retry resumes where it left off.
func (m *JobManager) RetryJob(id string) bool {
	m.mu.Lock()
	job, ok := m.jobs[id]
	if !ok {
		m.mu.Unlock()
		return false
	}

	if job.Status != JobStatusFailed && job.Status != JobStatusCancelled {
		m.mu.Unlock()
		return false
	}

	job.Status = JobStatusQueued
	job.Error = ""
	job.EndedAt = nil
	// Reset progress - the downloader will re-scan and report all files.
	// No resume floor: a retry is a clean restart from zero, so the
	// running sum drives DownloadedBytes directly.
	job.Progress = JobProgress{}
	job.resumeFloor = 0
	job.Files = nil
	snapshot := m.cloneJobLocked(job)
	// Re-queue through the scheduler so retrying respects max-active.
	m.dispatchLocked()
	m.mu.Unlock()

	// Notify listeners of status change
	m.notifyListeners(snapshot)

	return true
}

// DeleteJob removes a job from the list.
func (m *JobManager) DeleteJob(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	job, ok := m.jobs[id]
	if !ok {
		return false
	}

	// Cancel if running
	if job.cancel != nil && (job.Status == JobStatusQueued || job.Status == JobStatusRunning) {
		job.cancel()
	}

	delete(m.jobs, id)
	return true
}

// WaitAll blocks until every in-flight runJob goroutine has returned or
// until timeout elapses. Returns true if all goroutines exited cleanly,
// false on timeout. Primarily for tests and graceful shutdown — lets
// callers observe actual goroutine exit rather than just Status==Cancelled,
// which is set before the downloader's filesystem operations fully unwind.
func (m *JobManager) WaitAll(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		m.runWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// DismissJobResult distinguishes the three possible outcomes of a dismiss
// attempt so the HTTP layer can map them to appropriate status codes.
type DismissJobResult int

const (
	// DismissJobOK means the job was in a terminal state and has been removed.
	DismissJobOK DismissJobResult = iota
	// DismissJobNotFound means no job with that ID exists.
	DismissJobNotFound
	// DismissJobStillActive means the job is queued or running; it must be
	// cancelled first (or completed) before it can be dismissed.
	DismissJobStillActive
)

// DismissJob removes a job from the manager if and only if it is in a
// terminal state (completed, failed, cancelled, paused). Dismissal is the
// user's way of hiding a finished job from the UI permanently, and the
// guarantee that matters for github issue #68 is that the job does not
// come back on the next page refresh — so the underlying storage drops it.
// Dismissing a queued or running job is rejected so a stray click can't
// wipe a live download.
func (m *JobManager) DismissJob(id string) bool {
	res, _ := m.DismissJobResult(id)
	return res == DismissJobOK
}

// DismissJobResult is the richer variant of DismissJob that returns the
// reason a dismissal failed, for use by the HTTP handler.
func (m *JobManager) DismissJobResult(id string) (DismissJobResult, *Job) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[id]
	if !ok {
		return DismissJobNotFound, nil
	}
	if !isTerminalJobStatus(job.Status) {
		return DismissJobStillActive, job
	}
	delete(m.jobs, id)
	return DismissJobOK, job
}

// Subscribe adds a listener for job updates.
func (m *JobManager) Subscribe() chan *Job {
	ch := make(chan *Job, 100)
	m.listenerMu.Lock()
	m.listeners = append(m.listeners, ch)
	m.listenerMu.Unlock()
	return ch
}

// Unsubscribe removes a listener.
func (m *JobManager) Unsubscribe(ch chan *Job) {
	m.listenerMu.Lock()
	defer m.listenerMu.Unlock()

	for i, listener := range m.listeners {
		if listener == ch {
			m.listeners = append(m.listeners[:i], m.listeners[i+1:]...)
			close(ch)
			return
		}
	}
}

// notifyListeners forwards an already-snapshotted job update to channel
// listeners and the WebSocket broadcast path. The caller MUST pass in a
// snapshot (produced by cloneJobLocked while holding m.mu) — this function
// does not take m.mu itself, so it is safe to call from sites that already
// hold m.mu.Lock() (like CancelJob / PauseJob with a deferred unlock).
func (m *JobManager) notifyListeners(snapshot *Job) {
	// Notify channel listeners (tests and other internal subscribers see
	// every raw update; only the WebSocket path is throttled).
	m.listenerMu.RLock()
	for _, ch := range m.listeners {
		select {
		case ch <- snapshot:
		default:
			// Listener is slow, skip
		}
	}
	m.listenerMu.RUnlock()

	// Broadcast to WebSocket clients through the per-job coalescer so the
	// browser isn't asked to re-render at 5Hz × file-count.
	if m.wsCoalescer != nil {
		m.wsCoalescer.schedule(snapshot)
	} else if m.wsHub != nil {
		m.wsHub.BroadcastJob(snapshot)
	}
}

// effectiveMaxActive returns the configured limit on how many download jobs
// may run at once, with a sane fallback when the setting is unset.
func (m *JobManager) effectiveMaxActive() int {
	if m.config.MaxActive > 0 {
		return m.config.MaxActive
	}
	return 3
}

// dispatchLocked starts queued jobs (oldest first) until the number of
// running-or-starting jobs reaches the max-active limit. It is the single
// gate through which jobs become active, so the concurrent-download count
// always respects the setting. Caller MUST hold m.mu.
func (m *JobManager) dispatchLocked() {
	limit := m.effectiveMaxActive()

	active := 0
	var queued []*Job
	for _, j := range m.jobs {
		if j.Status == JobStatusRunning || j.starting {
			active++
		} else if j.Status == JobStatusQueued {
			queued = append(queued, j)
		}
	}
	if active >= limit || len(queued) == 0 {
		return
	}

	// Oldest first, so downloads start in the order they were added.
	sort.Slice(queued, func(a, b int) bool {
		return queued[a].CreatedAt.Before(queued[b].CreatedAt)
	})

	for _, j := range queued {
		if active >= limit {
			break
		}
		j.starting = true
		m.runWG.Add(1)
		go m.runJob(j)
		active++
	}
}

// enforceLimitLocked re-queues the most-recently-started running jobs when the
// active count exceeds a lowered max-active limit, so reducing the setting
// actually reduces the number of concurrent downloads. Older (more-progressed)
// downloads are kept running; the rest go back to 'queued' and the dispatcher
// restarts them automatically as slots free up. Caller MUST hold m.mu.
func (m *JobManager) enforceLimitLocked() {
	limit := m.effectiveMaxActive()

	var running []*Job
	for _, j := range m.jobs {
		if j.Status == JobStatusRunning {
			running = append(running, j)
		}
	}
	if len(running) <= limit {
		return
	}

	// Newest-started first, so the longest-running downloads keep going.
	sort.Slice(running, func(a, b int) bool {
		ta, tb := running[a].StartedAt, running[b].StartedAt
		if ta == nil || tb == nil {
			return false
		}
		return ta.After(*tb)
	})

	for i := 0; i < len(running)-limit; i++ {
		j := running[i]
		if j.cancel != nil {
			j.cancel()
		}
		// Re-queue (not pause) so the dispatcher auto-starts it again once a
		// slot frees. Bump the generation so the in-flight runJob recognizes
		// it has been superseded and stops without marking a terminal status.
		j.generation++
		j.starting = false
		j.Status = JobStatusQueued
		m.notifyListeners(m.cloneJobLocked(j))
	}
}

// UpdateConfig replaces the manager's config (called when settings change),
// re-queues any running jobs above a lowered max-active limit, then starts any
// queued jobs that a raised limit now allows.
func (m *JobManager) UpdateConfig(cfg Config) {
	m.mu.Lock()
	m.config = cfg
	if m.speedLimiter != nil {
		m.speedLimiter.SetLimit(hfdownloader.ParseSize(cfg.MaxSpeed))
	}
	m.enforceLimitLocked()
	m.dispatchLocked()
	m.mu.Unlock()
}

// snapshotConfig returns a copy of the manager's config taken under the read
// lock. Job runners use this instead of touching m.config directly: a settings
// update (UpdateConfig) replaces m.config under the write lock, so an unguarded
// read from a runJob goroutine would be a data race. The copy shares the
// *ProxyConfig pointer, which is safe because handleUpdateSettings replaces that
// pointer (copy-on-write) rather than mutating its target.
func (m *JobManager) snapshotConfig() Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.config
}

// speedWindow is how far back the windowed average looks. Progress at the
// application level is bursty (part-file stats, chunked reads), so the window
// must be long enough to average the bursts out, the way the OS smooths its
// network-adapter graph.
const speedWindow = 10 * time.Second

// updateJobSpeed recomputes BytesPerSecond from the cumulative bytes
// transferred this run (skipped/already-present bytes excluded). The rate is
// averaged over a sliding speedWindow, then blended into the previous reading
// with a mild EMA — the windowed average alone still jitters as old samples
// fall off the window, and that jitter makes the displayed speed (and any ETA
// derived from it) jump around. Samples are throttled to a few per second so
// the window stays small. now is passed in for deterministic testing.
func updateJobSpeed(job *Job, transferred int64, now time.Time) {
	// Throttle to a few samples per second.
	if n := len(job.speedSamples); n > 0 && now.Sub(job.speedSamples[n-1].t) < 400*time.Millisecond {
		return
	}
	if len(job.speedSamples) == 0 {
		job.speedRunStart = now
	}
	// Drop samples older than the window, then add the current one.
	cutoff := now.Add(-speedWindow)
	kept := make([]speedSample, 0, len(job.speedSamples)+1)
	for _, s := range job.speedSamples {
		if s.t.After(cutoff) {
			kept = append(kept, s)
		}
	}
	kept = append(kept, speedSample{t: now, bytes: transferred})
	job.speedSamples = kept

	oldest := kept[0]
	span := now.Sub(oldest.t).Seconds()
	if span >= 1.0 {
		rate := float64(transferred-oldest.bytes) / span
		if rate < 0 {
			rate = 0
		}
		if prev := job.Progress.BytesPerSecond; prev > 0 {
			rate = 0.7*float64(prev) + 0.3*rate
		}
		job.Progress.BytesPerSecond = int64(rate)
	}
	updateJobETA(job, transferred, now)
}

// updateJobETA recomputes EtaSeconds. The predictor is mostly the whole-run
// average rate (transferred / elapsed) — for remaining-time estimation the
// long-run average is far steadier than the displayed speed, the way curl and
// rsync compute their ETAs — blended with a little of the current smoothed
// speed so a genuine, lasting throughput change still pulls the estimate over.
func updateJobETA(job *Job, transferred int64, now time.Time) {
	remaining := job.Progress.TotalBytes - job.Progress.DownloadedBytes
	elapsed := now.Sub(job.speedRunStart).Seconds()
	if remaining <= 0 || elapsed < 1.0 || transferred <= 0 {
		job.Progress.EtaSeconds = 0
		return
	}
	runAvg := float64(transferred) / elapsed
	predictor := 0.7*runAvg + 0.3*float64(job.Progress.BytesPerSecond)
	if predictor <= 0 {
		job.Progress.EtaSeconds = 0
		return
	}
	job.Progress.EtaSeconds = int64(float64(remaining)/predictor + 0.5)
}

// applyJobProgress folds one downloader progress event into the job's
// aggregate state. The caller must hold the manager lock. now feeds the
// speed/ETA estimators and is passed in for deterministic testing.
func applyJobProgress(job *Job, evt hfdownloader.ProgressEvent, now time.Time) {
	switch evt.Event {
	case "plan_item":
		job.Progress.TotalFiles++
		job.Progress.TotalBytes += evt.Total
		job.Files = append(job.Files, JobFileProgress{
			Path:       evt.Path,
			TotalBytes: evt.Total,
			Status:     "pending",
		})

	case "file_start":
		for i := range job.Files {
			if job.Files[i].Path == evt.Path {
				job.Files[i].Status = "active"
				break
			}
		}

	case "file_progress":
		for i := range job.Files {
			if job.Files[i].Path == evt.Path {
				if !job.Files[i].progressed {
					// First real progress for this file: record the bytes
					// already on disk (resume position) so they aren't
					// counted toward the live transfer speed.
					job.Files[i].progressed = true
					job.Files[i].baseDownloaded = evt.Downloaded
				}
				job.Files[i].Downloaded = evt.Downloaded
				break
			}
		}
		// total drives the progress bar (includes skipped/already-present
		// bytes); transferred drives the speed (only bytes moved this run).
		// During the resume-recovery window (resumeFloor > 0) the running
		// sum under-counts because plan_item has already added the full
		// file list but only the one processed file has Downloaded > 0,
		// which would clobber the value ResumeJob preserved. The
		// monotonic guard keeps the preserved value on screen until the
		// sum catches up, at which point the floor is released and a
		// genuine rollback (e.g. downloadSingle's 200 fallback on a
		// range request truncating the .part file) propagates immediately.
		var total, transferred int64
		for _, f := range job.Files {
			total += f.Downloaded
			if f.progressed {
				transferred += f.Downloaded - f.baseDownloaded
			}
		}
		if job.resumeFloor > 0 {
			if total > job.Progress.DownloadedBytes {
				job.Progress.DownloadedBytes = total
			}
			if total >= job.resumeFloor {
				job.resumeFloor = 0
			}
		} else {
			job.Progress.DownloadedBytes = total
		}
		updateJobSpeed(job, transferred, now)

	case "file_finalizing":
		// The file's bytes are all on disk; part assembly, hash verification
		// and the cache store are running — minutes of local I/O for a large
		// model, with no further file_progress events.
		for i := range job.Files {
			if job.Files[i].Path == evt.Path {
				job.Files[i].Status = "finalizing"
				break
			}
		}
		// Once no file is left downloading, the whole job is in local
		// post-processing: surface the phase and stop showing the stale
		// speed/ETA from the last transfer window.
		downloading := false
		for _, f := range job.Files {
			if f.Status == "active" || f.Status == "pending" {
				downloading = true
				break
			}
		}
		if !downloading {
			job.Phase = "finalizing"
			job.Progress.BytesPerSecond = 0
			job.Progress.EtaSeconds = 0
		}

	case "file_done":
		for i := range job.Files {
			if job.Files[i].Path == evt.Path {
				job.Files[i].Status = "complete"
				job.Files[i].Downloaded = job.Files[i].TotalBytes
				break
			}
		}
		job.Progress.CompletedFiles++
		// Recalculate total downloaded (skipped/completed files included).
		// Speed is intentionally not updated here: a skipped file emits only
		// file_done, and counting its full size would spike the reading.
		// Monotonic guard is scoped to the resume-recovery window — see
		// file_progress for the rationale.
		var total int64
		for _, f := range job.Files {
			total += f.Downloaded
		}
		if job.resumeFloor > 0 {
			if total > job.Progress.DownloadedBytes {
				job.Progress.DownloadedBytes = total
			}
			if total >= job.resumeFloor {
				job.resumeFloor = 0
			}
		} else {
			job.Progress.DownloadedBytes = total
		}

	case "finalizing":
		// Download is done; post-processing (friendly view, manifest) runs.
		job.Phase = "finalizing"
		job.Progress.BytesPerSecond = 0
		job.Progress.EtaSeconds = 0
	}
}

// runJob executes the download job.
func (m *JobManager) runJob(job *Job) {
	defer m.runWG.Done()

	ctx, cancel := context.WithCancel(context.Background())

	// Increment generation and store our generation number
	m.mu.Lock()
	job.cancel = cancel
	job.generation++
	myGeneration := job.generation // Track which generation we are
	job.Status = JobStatusRunning
	job.starting = false
	job.Phase = ""
	now := time.Now()
	job.StartedAt = &now
	// Reset the speed window so this run measures only its own throughput.
	job.speedSamples = nil
	startSnap := m.cloneJobLocked(job)
	m.mu.Unlock()
	m.notifyListeners(startSnap)

	// Create download job and settings
	dlJob := hfdownloader.Job{
		Repo:               job.Repo,
		Revision:           job.Revision,
		IsDataset:          job.IsDataset,
		Filters:            job.Filters,
		Excludes:           job.Excludes,
		ExactMatch:         job.ExactMatch,
		AppendFilterSubdir: false,
		LocalRepo:          job.LocalRepo,
	}

	// Snapshot the manager config under the lock: a concurrent POST /api/settings
	// (UpdateConfig) replaces m.config, so reading the fields directly here would
	// be a data race.
	cfg := m.snapshotConfig()

	// Use HuggingFace cache structure (v3 mode) instead of legacy OutputDir
	cacheDir := cfg.CacheDir
	if cacheDir == "" {
		cacheDir = hfdownloader.DefaultCacheDir()
	}

	settings := hfdownloader.Settings{
		CacheDir:           cacheDir, // Use HF cache structure
		Concurrency:        cfg.Concurrency,
		MaxActiveDownloads: cfg.MaxActive,
		Token:              cfg.Token,
		MultipartThreshold: cfg.MultipartThreshold,
		Verify:             cfg.Verify,
		Retries:            cfg.Retries,
		BackoffInitial:     "400ms",
		BackoffMax:         "10s",
		Endpoint:           cfg.Endpoint,
		Proxy:              cfg.Proxy,
		MaxSpeed:           cfg.MaxSpeed,
		SpeedLimiter:       m.speedLimiter,
	}

	// When the downloader's context is cancelled, check whether this was
	// an explicit cancel (user clicked Cancel) vs a pause or limit-enforce.
	// Only explicit cancels and deleted jobs should delete partial .part
	// files; pauses and re-queues must preserve them for resume.
	settings.CleanupPartialsOnCancel = func() bool {
		m.mu.RLock()
		defer m.mu.RUnlock()
		_, active := m.jobs[job.ID]
		return job.Status == JobStatusCancelled || !active
	}

	// Local mode: write real files into job.LocalDir instead of the HF cache
	// layout. job.LocalDir is already resolved (per-request or server-global).
	// Clearing CacheDir forces flat-file output.
	if job.LocalDir != "" {
		settings.OutputDir = job.LocalDir
		settings.CacheDir = ""
	}

	// Progress callback - NOTE: must not hold lock when calling notifyListeners
	progressFunc := func(evt hfdownloader.ProgressEvent) {
		m.mu.Lock()
		applyJobProgress(job, evt, time.Now())
		progressSnap := m.cloneJobLocked(job)
		m.mu.Unlock() // Unlock BEFORE notifying to avoid deadlock
		m.notifyListeners(progressSnap)
	}

	// Run the download
	err := hfdownloader.Run(ctx, dlJob, settings, progressFunc)

	// Update final status
	m.mu.Lock()
	// Don't update status if:
	// 1. Job was paused (user intentionally stopped it)
	// 2. We're a stale goroutine (a newer runJob has started)
	if job.Status == JobStatusPaused || job.generation != myGeneration {
		// A paused job has freed its slot, so let the next queued job start.
		// On a generation mismatch a newer runJob owns the slot — don't.
		if job.generation == myGeneration {
			m.dispatchLocked()
		}
		m.mu.Unlock()
		return
	}
	endTime := time.Now()
	job.EndedAt = &endTime
	if ctx.Err() != nil {
		job.Status = JobStatusCancelled
	} else if err != nil {
		job.Status = JobStatusFailed
		job.Error = err.Error()
	} else {
		job.Status = JobStatusCompleted
	}
	endSnap := m.cloneJobLocked(job)
	// This job finished and freed a slot; start the next queued job.
	m.dispatchLocked()
	m.mu.Unlock()

	m.notifyListeners(endSnap)

	// Persist state and record completed/failed jobs in history
	go m.saveState()
	if endSnap.Status == JobStatusCompleted || endSnap.Status == JobStatusFailed {
		go func() {
			if err := AppendHistory(endSnap); err != nil {
				log.Printf("warning: could not append history: %v", err)
			}
		}()
	}
}
