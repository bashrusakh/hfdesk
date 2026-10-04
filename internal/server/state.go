// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// jobsStateFile is the on-disk format for jobs_state.json.
type jobsStateFile struct {
	Jobs []*Job `json:"jobs"`
}

// stateMu serializes reads and writes of jobs_state.json.
var stateMu sync.Mutex

// SaveJobsState writes all jobs to jobs_state.json in the run directory.
// Only non-active terminal states and paused jobs are persisted (running/queued
// jobs are serialized as paused so they can be resumed after restart).
func SaveJobsState(jobs []*Job) error {
	return saveJobsState(JobsStatePath(), jobs)
}

// saveJobsStateLocked persists jobs to path. stateMu must be held.
func saveJobsStateLocked(path string, jobs []*Job) error {
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
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return replaceJobsStateFile(path, data, defaultStateFileOps{})
}

// LoadJobsState reads jobs_state.json. Returns empty slice (not error) if the
// file does not exist yet.
func LoadJobsState() ([]*Job, error) {
	return loadJobsState(JobsStatePath())
}

func saveJobsState(path string, jobs []*Job) error {
	stateMu.Lock()
	defer stateMu.Unlock()
	return saveJobsStateLocked(path, jobs)
}

func loadJobsState(path string) ([]*Job, error) {
	stateMu.Lock()
	defer stateMu.Unlock()
	return loadJobsStateLocked(path)
}

// loadJobsStateLocked reads jobs from path. stateMu must be held.
func loadJobsStateLocked(path string) ([]*Job, error) {
	data, err := os.ReadFile(path)
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

type stateFileOps interface {
	createTemp(dir string) (stateTempFile, error)
	rename(oldpath, newpath string) error
	remove(path string) error
	stat(path string) (os.FileInfo, error)
}

type stateTempFile interface {
	Name() string
	Write([]byte) (int, error)
	Chmod(os.FileMode) error
	Sync() error
	Close() error
}

type defaultStateFileOps struct{}

func (defaultStateFileOps) createTemp(dir string) (stateTempFile, error) {
	for range 10 {
		var random [12]byte
		if _, err := rand.Read(random[:]); err != nil {
			return nil, err
		}
		name := filepath.Join(dir, ".jobs_state-"+hex.EncodeToString(random[:])+".tmp")
		file, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		return file, err
	}
	return nil, fmt.Errorf("could not allocate unique jobs state temp file")
}
func (defaultStateFileOps) rename(oldpath, newpath string) error {
	return renameStateFile(oldpath, newpath)
}
func (defaultStateFileOps) remove(path string) error              { return os.Remove(path) }
func (defaultStateFileOps) stat(path string) (os.FileInfo, error) { return os.Stat(path) }

// replaceJobsStateFile commits a complete state file without truncating the old one.
func replaceJobsStateFile(path string, data []byte, ops stateFileOps) (retErr error) {
	tmp, err := ops.createTemp(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("create jobs state temp file: %w", err)
	}
	tmpPath := tmp.Name()
	renamed := false
	defer func() {
		if !renamed {
			if err := ops.remove(tmpPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				retErr = errors.Join(retErr, fmt.Errorf("remove jobs state temp file: %w", err))
			}
		}
	}()

	mode := os.FileMode(0)
	preserveMode := false
	if info, err := ops.stat(path); err == nil {
		mode = info.Mode().Perm()
		preserveMode = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.Join(fmt.Errorf("stat jobs state file: %w", err), wrapStateErr("close jobs state temp file", tmp.Close()))
	}
	if preserveMode {
		if err := tmp.Chmod(mode); err != nil {
			return errors.Join(fmt.Errorf("set jobs state temp permissions: %w", err), wrapStateErr("close jobs state temp file", tmp.Close()))
		}
	}
	n, writeErr := tmp.Write(data)
	if writeErr == nil && n != len(data) {
		writeErr = fmt.Errorf("short write: wrote %d of %d bytes", n, len(data))
	}
	var syncErr error
	if writeErr == nil {
		syncErr = tmp.Sync()
	}
	closeErr := tmp.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		return errors.Join(wrapStateErr("write jobs state temp file", writeErr), wrapStateErr("sync jobs state temp file", syncErr), wrapStateErr("close jobs state temp file", closeErr))
	}
	if err := ops.rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace jobs state file: %w", err)
	}
	renamed = true
	return nil
}

func wrapStateErr(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
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
