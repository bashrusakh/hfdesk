// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Ownership waiters return their MaxActiveDownloads lease and reacquire it
// before transfer; failed acquisition leaves it returned. Domain acquisition
// signals both local and OS-lock waits; fast paths and multipart claims do not.
// Distinct path/blob namespaces and one acquisition per destination prevent
// self-conflict and slot/domain cycles; see doc.go for the complete protocol.

import "context"

// downloadSlotWaitNotice lets destination acquisition return an item's slot
// during contention and reacquire it before transfer. beginWait is idempotent;
// endWait returns false if cancellation prevents reacquisition.
type downloadSlotWaitNotice interface {
	beginWait()
	endWait() bool
}

type downloadSlotWaitNoticeKey struct{}

// withDownloadSlotWaitNotice carries this item's lease to domain acquisition.
// Standalone store, finalization and cleanup contexts do not carry an item lease.
func withDownloadSlotWaitNotice(ctx context.Context, notice downloadSlotWaitNotice) context.Context {
	if notice == nil {
		return ctx
	}
	return context.WithValue(ctx, downloadSlotWaitNoticeKey{}, notice)
}

func downloadSlotWaitNoticeFromContext(ctx context.Context) downloadSlotWaitNotice {
	notice, _ := ctx.Value(downloadSlotWaitNoticeKey{}).(downloadSlotWaitNotice)
	return notice
}

// downloadSlotLease belongs to one item goroutine. Its acquisition callbacks
// and deferred release run in that goroutine; concurrent use is not permitted.
type downloadSlotLease struct {
	lim chan struct{}
	// ctx bounds taking the slot back: an item that was cancelled while it
	// waited must not block on the semaphore.
	ctx  context.Context
	held bool
}

// newDownloadSlotLease wraps the slot the item goroutine ALREADY holds (the
// download loop acquired it before launching the goroutine) in a lease.
func newDownloadSlotLease(ctx context.Context, lim chan struct{}) *downloadSlotLease {
	return &downloadSlotLease{lim: lim, ctx: ctx, held: true}
}

// release returns a currently held slot and is otherwise a no-op.
func (l *downloadSlotLease) release() {
	if l == nil || !l.held {
		return
	}
	l.held = false
	<-l.lim
}

func (l *downloadSlotLease) beginWait() {
	l.release()
}

// endWait reacquires the slot, returning false if the item context ends first.
func (l *downloadSlotLease) endWait() bool {
	if l == nil || l.held {
		return true
	}
	select {
	case l.lim <- struct{}{}:
		l.held = true
		return true
	case <-l.ctx.Done():
		return false
	}
}
