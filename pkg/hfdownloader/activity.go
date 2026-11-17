// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"io"
	"sync"
	"time"
)

// fileActivity records, per file, when a response-body read last delivered
// bytes, when the file last made local progress, how many bytes the network
// delivered this run, and whether local work is blocked in right now. The
// server uses the instants and the in-flight flag to choose the writing phase,
// and the read counter for the download speed and ETA. It stores no durations,
// shares or accumulated wait.
type fileActivity struct {
	mu        sync.Mutex
	readAt    time.Time
	writeAt   time.Time
	readBytes int64
	inFlight  int
}

func (a *fileActivity) noteRead(n int) {
	now := time.Now()
	a.mu.Lock()
	a.readAt = now
	a.readBytes += int64(n)
	a.mu.Unlock()
}

func (a *fileActivity) noteLocalWrite() {
	now := time.Now()
	a.mu.Lock()
	a.writeAt = now
	a.mu.Unlock()
}

// beginLocalWork marks a staging operation or payload write as in flight. The
// flag is carried on every progress sample, so a long preallocation or blocked
// WriteAt keeps the file writing without relying on a staleness window.
func (a *fileActivity) beginLocalWork() {
	a.mu.Lock()
	a.inFlight++
	a.mu.Unlock()
}

func (a *fileActivity) finishLocalWork() {
	a.mu.Lock()
	if a.inFlight > 0 {
		a.inFlight--
	}
	a.mu.Unlock()
}

func (a *fileActivity) snapshot() (readAt, writeAt time.Time, readBytes int64, inFlight bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.readAt, a.writeAt, a.readBytes, a.inFlight > 0
}

// activityReader notes a successful read only when the read delivered bytes.
type activityReader struct {
	r io.Reader
	a *fileActivity
}

func (r activityReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if n > 0 {
		r.a.noteRead(n)
	}
	return n, err
}
