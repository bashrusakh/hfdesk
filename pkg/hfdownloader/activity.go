// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"io"
	"sync"
	"time"
)

// fileActivity records, per file, when a response-body read last delivered
// bytes and when the file last made local progress. The server compares these
// two timestamps to decide whether a stalled transfer is waiting on the
// network or on the disk. It stores instants only: no durations, shares or
// accumulated wait.
type fileActivity struct {
	mu      sync.Mutex
	readAt  time.Time
	writeAt time.Time
}

func (a *fileActivity) noteRead() {
	now := time.Now()
	a.mu.Lock()
	a.readAt = now
	a.mu.Unlock()
}

func (a *fileActivity) noteLocalWrite() {
	now := time.Now()
	a.mu.Lock()
	a.writeAt = now
	a.mu.Unlock()
}

func (a *fileActivity) snapshot() (readAt, writeAt time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.readAt, a.writeAt
}

// activityReader notes a successful read only when the read delivered bytes.
type activityReader struct {
	r io.Reader
	a *fileActivity
}

func (r activityReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if n > 0 {
		r.a.noteRead()
	}
	return n, err
}
