// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// jobsStateFile is the on-disk format for jobs_state.json.
type jobsStateFile struct {
	Jobs []*Job `json:"jobs"`
}

// stateMu serializes reads and writes of jobs_state.json so
// concurrent SaveJobsState / LoadJobsState calls don't tear the file.
var stateMu sync.Mutex

// SaveJobsState writes all jobs to jobs_state.json in the run directory.
// Only non-active terminal states and paused jobs are persisted (running/queued
// jobs are serialized as paused so they can be resumed after restart).
func SaveJobsState(jobs []*Job) error {
	stateMu.Lock()
	defer stateMu.Unlock()

	// Snapshot: clamp running/queued → paused so they appear resumable on next start
	persisted := make([]*Job, 0, len(jobs))
	for _, j := range jobs {
		copy := *j
		copy.cancel = nil
		if copy.Status == JobStatusRunning || copy.Status == JobStatusQueued {
			copy.Status = JobStatusPaused
		}
		persisted = append(persisted, &copy)
	}

	data, err := json.MarshalIndent(jobsStateFile{Jobs: persisted}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(AppConfigDir(), 0o755); err != nil {
		return err
	}
	return os.WriteFile(JobsStatePath(), data, 0o644)
}

// LoadJobsState reads jobs_state.json. Returns empty slice (not error) if the
// file does not exist yet.
func LoadJobsState() ([]*Job, error) {
	data, err := os.ReadFile(JobsStatePath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var sf jobsStateFile
	if err := json.Unmarshal(data, &sf); err != nil {
		return nil, err
	}
	return sf.Jobs, nil
}

// HistoryEntry is a completed download recorded in download_history.json.
type HistoryEntry struct {
	ID        string    `json:"id"`
	Repo      string    `json:"repo"`
	Revision  string    `json:"revision"`
	IsDataset bool      `json:"isDataset,omitempty"`
	OutputDir string    `json:"outputDir"`
	Status    JobStatus `json:"status"` // completed or failed
	Error     string    `json:"error,omitempty"`
	StartedAt time.Time `json:"startedAt"`
	EndedAt   time.Time `json:"endedAt"`
	// Aggregate totals at end of run
	TotalFiles int   `json:"totalFiles"`
	TotalBytes int64 `json:"totalBytes"`
}

// historyFile is the on-disk shape of download_history.json: a flat
// list of HistoryEntry records, one per completed or failed job.
type historyFile struct {
	Entries []HistoryEntry `json:"entries"`
}

// historyMu serializes reads and writes of download_history.json.
var historyMu sync.Mutex

// AppendHistory appends a completed/failed job to download_history.json.
func AppendHistory(job *Job) error {
	historyMu.Lock()
	defer historyMu.Unlock()

	hf, err := loadHistoryLocked()
	if err != nil {
		return err
	}

	entry := HistoryEntry{
		ID:         job.ID,
		Repo:       job.Repo,
		Revision:   job.Revision,
		IsDataset:  job.IsDataset,
		OutputDir:  job.OutputDir,
		Status:     job.Status,
		Error:      job.Error,
		TotalFiles: job.Progress.TotalFiles,
		TotalBytes: job.Progress.TotalBytes,
	}
	if job.StartedAt != nil {
		entry.StartedAt = *job.StartedAt
	}
	if job.EndedAt != nil {
		entry.EndedAt = *job.EndedAt
	}

	hf.Entries = append(hf.Entries, entry)

	data, err := json.MarshalIndent(hf, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(AppConfigDir(), 0o755); err != nil {
		return err
	}
	return os.WriteFile(HistoryPath(), data, 0o644)
}

// LoadHistory reads download_history.json. Returns empty slice if file absent.
func LoadHistory() ([]HistoryEntry, error) {
	historyMu.Lock()
	defer historyMu.Unlock()
	hf, err := loadHistoryLocked()
	if err != nil {
		return nil, err
	}
	return hf.Entries, nil
}

// loadHistoryLocked reads download_history.json. Caller MUST hold
// historyMu. Returns an empty historyFile if the file does not yet
// exist.
func loadHistoryLocked() (*historyFile, error) {
	data, err := os.ReadFile(HistoryPath())
	if err != nil {
		if os.IsNotExist(err) {
			return &historyFile{}, nil
		}
		return nil, err
	}
	var hf historyFile
	if err := json.Unmarshal(data, &hf); err != nil {
		return nil, err
	}
	return &hf, nil
}
