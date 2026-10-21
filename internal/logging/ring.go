// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package logging

import (
	"sync"
)

// RingSink keeps the most recent records in a hard bounded ring buffer.
//
// The ring never grows beyond its capacity: appending to a full ring
// overwrites the oldest record. Every record carries the sequence number
// assigned by the pipeline, which lets a reader ask for "records after
// cursor N" and detect that the cursor has already fallen out of the ring.
type RingSink struct {
	mu       sync.Mutex
	capacity int
	buf      []Record
	start    int
	count    int
	oldest   uint64
	newest   uint64
}

// NewRingSink returns a ring sink that retains at most capacity records.
// A capacity of zero or less disables retention.
func NewRingSink(capacity int) *RingSink {
	if capacity < 0 {
		capacity = 0
	}
	return &RingSink{
		capacity: capacity,
		buf:      make([]Record, capacity),
	}
}

// Capacity returns the maximum number of retained records.
func (r *RingSink) Capacity() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.capacity
}

// Len returns the number of currently retained records.
func (r *RingSink) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.count
}

// OldestSeq returns the sequence number of the oldest retained record, or
// zero when the ring is empty.
func (r *RingSink) OldestSeq() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.oldest
}

// NewestSeq returns the sequence number of the newest retained record, or
// zero when the ring is empty.
func (r *RingSink) NewestSeq() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.newest
}

// Append stores a record, dropping the oldest one when the ring is full.
func (r *RingSink) Append(rec Record) {
	if r.capacity <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.count < r.capacity {
		r.buf[(r.start+r.count)%r.capacity] = rec
		r.count++
	} else {
		r.buf[r.start] = rec
		r.start = (r.start + 1) % r.capacity
	}
	if r.count == 1 {
		r.oldest = rec.Seq
	}
	r.newest = rec.Seq
	if r.count > 1 {
		r.oldest = r.buf[r.start].Seq
	}
}

// Records returns the retained records that were emitted after the given
// cursor, together with a flag that reports whether the requested window is
// still fully covered by the ring.
//
// The flag is true when records after the cursor have already been dropped
// from the ring, which means the caller cannot be served a complete answer
// for that cursor and has to fall back to a fresh snapshot.
func (r *RingSink) Records(after uint64, order Order) ([]Record, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.count == 0 || after >= r.newest {
		return nil, false
	}

	window := r.newest - after
	retained := r.newest - r.oldest + 1
	truncated := window > retained

	out := make([]Record, 0, min(window, retained))
	for i := range r.count {
		rec := r.buf[(r.start+i)%r.capacity]
		if rec.Seq <= after {
			continue
		}
		out = append(out, cloneRecord(rec))
	}
	if order == RecentFirst {
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	return out, truncated
}

// cloneRecord returns a copy of the record with its own attribute map, so
// callers cannot mutate the retained state.
func cloneRecord(rec Record) Record {
	out := rec
	if rec.Attrs != nil {
		attrs := make(map[string]any, len(rec.Attrs))
		for key, value := range rec.Attrs {
			attrs[key] = value
		}
		out.Attrs = attrs
	}
	return out
}
