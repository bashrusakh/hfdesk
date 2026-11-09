// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"fmt"
	"sync"
)

// Whole-tree removal admission holds open lock files for every directory in
// the target tree until the caller's removal finishes (see tryTreeRemovals):
// two per directory in the cache realm (the entry and children locks, after
// the directory's requests are normalized and deduplicated) and three per
// existing directory in the flat realm (the path, entry and object keys), so
// a tree with thousands of directories needs two to three times that many
// descriptors at once. Common default soft limits (1024 on desktop
// shells and systemd services) are far below that while the hard limit usually
// is not: for the duration of a tree admission the process soft
// file-descriptor limit is therefore raised toward the hard limit and restored
// when the admission's release runs. The raise is process-wide for that
// window: an ordinary operation that grows past the original soft limit while
// it is in effect may transiently fail new opens after the restore until its
// descriptors drain — an extreme-concurrency edge that self-heals. This
// changes no key, no lock file and no acquisition order — only how many
// descriptors the existing whole-tree-EX protocol may hold. A tree that still
// cannot be admitted (beyond the raiseable hard limit or the OS per-process
// file cap) fails with ownershipTreeFileLimitError instead of a raw
// per-process descriptor-exhaustion (EMFILE) error; system-wide exhaustion
// (ENFILE) keeps its raw identity.
var ownershipFileLimitHold = struct {
	sync.Mutex
	active   int
	raised   bool
	original uint64
}{}

// holdOwnershipFileLimit raises the process soft file-descriptor limit toward
// the hard limit while at least one holder is active and restores the
// original soft limit when the last holder releases. The returned release
// function is safe to call more than once. On platforms without a soft/hard
// descriptor-limit model it is a no-op.
func holdOwnershipFileLimit() func() {
	ownershipFileLimitHold.Lock()
	if ownershipFileLimitHold.active == 0 {
		// A restore that failed on an earlier release stays owed (raised
		// remains true with the original remembered). Retry it before
		// re-raising; only a successful restore clears the debt, so the
		// original soft limit is never silently lost.
		if ownershipFileLimitHold.raised {
			if err := setOwnershipSoftFileLimit(ownershipFileLimitHold.original); err == nil {
				ownershipFileLimitHold.raised = false
			}
		}
		if !ownershipFileLimitHold.raised {
			if soft, hard, ok := currentOwnershipFileLimit(); ok && soft < hard {
				if err := setOwnershipSoftFileLimit(hard); err == nil {
					ownershipFileLimitHold.raised = true
					ownershipFileLimitHold.original = soft
				}
			}
		}
	}
	ownershipFileLimitHold.active++
	ownershipFileLimitHold.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			ownershipFileLimitHold.Lock()
			ownershipFileLimitHold.active--
			if ownershipFileLimitHold.active == 0 && ownershipFileLimitHold.raised {
				// Retry the restore once rather than swallowing a transient
				// failure. If it still fails, raised stays true with the
				// original remembered: the process keeps the raised soft
				// limit, and the next release (or the next hold's pre-raise
				// retry) attempts the restore again.
				err := setOwnershipSoftFileLimit(ownershipFileLimitHold.original)
				if err != nil {
					err = setOwnershipSoftFileLimit(ownershipFileLimitHold.original)
				}
				if err == nil {
					ownershipFileLimitHold.raised = false
				}
			}
			ownershipFileLimitHold.Unlock()
		})
	}
}

// ownershipTreeFileLimitError turns descriptor exhaustion during whole-tree
// admission into an actionable failure that names the observed limit, the
// producing realm's per-directory lock-file cost, and the remedy. The
// original error stays wrapped, so callers keep every existing identity check
// (busy, not-exist) and no raw EMFILE ever surfaces alone. cacheOwned selects
// that cost from the realm's directory-key shape (see ownershipDirectoryRequests
// and nodeDirectoryKeys): two lock files per directory in the cache realm
// (its path and entry dimensions share one key after normalization, plus the
// children key) and three in the flat realm (the path, entry and object keys
// of an existing directory).
func ownershipTreeFileLimitError(cacheOwned bool, held int, err error) error {
	mode, kinds, multiple, perDirectory := "flat", "path, entry and object", "three times", 3
	if cacheOwned {
		mode, kinds, multiple, perDirectory = "cache", "entry and children", "twice", 2
	}
	soft, hard, ok := currentOwnershipFileLimit()
	if !ok {
		return fmt.Errorf("%s tree admission exhausted the operating-system file-handle limit: whole-tree exclusion keeps %d open lock files per directory (%s) until removal finishes; free file handles in this process and retry: %w", mode, perDirectory, kinds, err)
	}
	heldClause := ""
	if held > 0 {
		heldClause = fmt.Sprintf(", with roughly %d lock files already held across %d directories", perDirectory*held, held)
	}
	return fmt.Errorf("%s tree admission exhausted the process file-descriptor limit: whole-tree exclusion keeps %d open lock files per directory (%s) until removal finishes%s, but no more could be opened with the soft limit at %d and the hard limit at %d; raise the soft limit (ulimit -n, or LimitNOFILE= for a service — the hard limit may need raising first with ulimit -Hn — and on some systems a per-process OS file cap such as kern.maxfilesperproc also bounds it) so it covers roughly %s this tree's directory count plus slack: %w",
		mode, perDirectory, kinds, heldClause, soft, hard, multiple, err)
}
