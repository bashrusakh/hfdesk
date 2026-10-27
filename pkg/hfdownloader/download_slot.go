// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// The per-item MaxActiveDownloads slot lease (issue #140 / PR #151).
//
// An item is not always transferring: when its destination is owned by a
// PROVEN live writer it WAITS for that destination ownership domain
// (acquireBlobDomain, through acquireBlobDomainWithin). A waiter consumes no
// bandwidth, so it must not consume one of the job's MaxActiveDownloads slots:
// with a low limit a single waiting item would otherwise keep every other file
// of the job from starting, the job would look stalled, and a concurrency
// budget meant for TRANSFERS would be spent on a wait.
//
// The slot is therefore held through a small per-item LEASE:
//
//   - the item goroutine owns one slot from its start (the download loop
//     acquired it before launching the goroutine);
//   - when a domain acquisition is about to BLOCK, it signals the lease
//     through the item's context (withDownloadSlotWaitNotice), the lease
//     returns the slot, and the wait continues without it;
//   - before returning a domain the caller will transfer under, the lease
//     REACQUIRES the slot, bounded by the item's context. An item never
//     continues to transfer without its slot. A FAILED acquisition returns no
//     domain, keeps the slot returned, and the item reports the error instead
//     of transferring: nothing reaches the caller, and re-taking the slot
//     would make the failure wait for an unrelated transfer.
//
// The signal is emitted by the acquisition itself, because that is the only
// place that knows about both waits: acquireBlobDomainWithin covers the
// in-process domain slot and the cross-process lifetime OS lock. The multipart
// claim performs no ownership wait of its own any more - it runs under the
// domain the item already holds and only takes the local metadata gate, which
// is intra-item state - so it emits no signal. A caller guessing "this looks
// contended" would miss a wait, and the fast path (the resource is free, which
// is the normal case) never emits the signal, so it neither releases nor
// reacquires the slot.
//
// Deadlock freedom: the lease returns the slot BEFORE the item blocks on a
// destination domain, so an item that waits never holds a slot while it waits.
// The slot wait of a contended item happens only AFTER it owns its domain, and
// the slot it waits for can only be held by
//
//   - an item that is transferring (it finishes or is cancelled), or
//   - an item that acquired a domain and is itself reacquiring its slot (that
//     item waits for a DIFFERENT domain: a second contender for the same key
//     is stopped at that key's in-process slot, so two contenders for one key
//     cannot both hold it).
//
// So the wait graph cannot contain a cycle. The OS ownership locks are still
// taken exactly once: this mechanism only moves the job's semaphore token, and
// no item takes the same lock twice.

import "context"

// downloadSlotWaitNotice is the item-side signal of an ownership-domain
// acquisition that must WAIT for a PROVEN live owner (or for an in-process
// holder of the same destination).
//
// beginWait is idempotent for one acquisition; endWait reports whether the
// item's slot was taken back (false when the item's context ended first, in
// which case the item must not continue).
type downloadSlotWaitNotice interface {
	beginWait()
	endWait() bool
}

// downloadSlotWaitNoticeKey is the context key under which an item's slot
// notice is carried to the ownership-domain acquisition and the multipart
// staging claim.
type downloadSlotWaitNoticeKey struct{}

// withDownloadSlotWaitNotice attaches an item's download-slot notice to its
// context.
//
// The context is the item's own per-file context, so the item's one domain
// acquisition (cache or flat) sees exactly this item's lease.
// Acquisitions that are not item work (the cleanup, the cache store's
// standalone path, exported FinalizeBlob) are not given this context and keep
// their behavior unchanged.
func withDownloadSlotWaitNotice(ctx context.Context, notice downloadSlotWaitNotice) context.Context {
	if notice == nil {
		return ctx
	}
	return context.WithValue(ctx, downloadSlotWaitNoticeKey{}, notice)
}

// downloadSlotWaitNoticeFromContext returns the item's slot notice, if the
// context carries one.
func downloadSlotWaitNoticeFromContext(ctx context.Context) downloadSlotWaitNotice {
	notice, _ := ctx.Value(downloadSlotWaitNoticeKey{}).(downloadSlotWaitNotice)
	return notice
}

// downloadSlotLease is ONE item's lease on a MaxActiveDownloads slot.
//
// Only the item's own goroutine touches it: beginWait/endWait are called from
// the domain acquisition that runs in that goroutine, and release is deferred
// by the same goroutine. No mutex is needed, and no other
// goroutine may use the lease.
type downloadSlotLease struct {
	// lim is the job's MaxActiveDownloads semaphore.
	lim chan struct{}
	// ctx bounds taking the slot back: an item that was cancelled while it
	// waited must not block on the semaphore.
	ctx context.Context
	// held reports whether this lease currently owns a token of lim.
	held bool
}

// newDownloadSlotLease wraps the slot the item goroutine ALREADY holds (the
// download loop acquired it before launching the goroutine) in a lease.
func newDownloadSlotLease(ctx context.Context, lim chan struct{}) *downloadSlotLease {
	return &downloadSlotLease{lim: lim, ctx: ctx, held: true}
}

// release returns the slot when the lease still holds it, and does nothing
// otherwise (the lease returned it for a wait and never took it back). It is
// idempotent, so the item's deferred release is safe after any wait path.
func (l *downloadSlotLease) release() {
	if l == nil || !l.held {
		return
	}
	l.held = false
	<-l.lim
}

// beginWait returns the slot for the duration of an ownership-domain wait or
// a staging-claim wait.
func (l *downloadSlotLease) beginWait() {
	l.release()
}

// endWait takes the slot back before the acquisition/claim returns, bounded by
// the item's context. It reports false when the context ended first: the item
// is cancelled, so it must not continue to transfer.
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
