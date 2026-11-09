// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package logging

import (
	"sync"
	"sync/atomic"
)

// Subscription is a live feed of newly emitted records.
//
// The feed is bounded: when the consumer does not keep up, records are
// dropped instead of blocking the logging pipeline. Dropped() reports how
// many records were lost.
//
// The channel returned by C() is closed by Close(), so a consumer can range
// over it. Close() may be called more than once.
type Subscription struct {
	ch   chan Record
	done chan struct{}

	mu     sync.RWMutex
	closed bool

	dropped atomic.Uint64
	close   sync.Once

	// onClose is called when the subscription is closed, so the pipeline can
	// stop feeding it.
	onClose func()
}

// newSubscription returns a subscription with the given channel capacity.
func newSubscription(capacity int) *Subscription {
	if capacity < 0 {
		capacity = 0
	}
	return &Subscription{
		ch:   make(chan Record, capacity),
		done: make(chan struct{}),
	}
}

// C returns the channel that carries the emitted records.
func (s *Subscription) C() <-chan Record {
	return s.ch
}

// Done returns a channel that is closed when the subscription is closed.
func (s *Subscription) Done() <-chan struct{} {
	return s.done
}

// Dropped returns the number of records that were dropped because the
// consumer did not keep up with the pipeline.
func (s *Subscription) Dropped() uint64 {
	return s.dropped.Load()
}

// Close unsubscribes the consumer and closes the record channel.
func (s *Subscription) Close() {
	s.close.Do(func() {
		s.mu.Lock()
		s.closed = true
		close(s.ch)
		s.mu.Unlock()
		close(s.done)
		if s.onClose != nil {
			s.onClose()
		}
	})
}

// send offers a record to the subscriber without blocking the pipeline.
func (s *Subscription) send(rec Record) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return
	}
	select {
	case s.ch <- rec:
	default:
		s.dropped.Add(1)
	}
}
