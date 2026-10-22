// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// The content-addressed publication primitive (issue #140 / PR #151).
//
// Publishing a content-addressed blob has two rules that the historical
// Stat-then-rename placement broke:
//
//  1. The decision "the blob does not exist yet" and the placement that
//     follows it must not be overtaken by a concurrent publisher of the same
//     blob key.
//
//  2. An existing blobs/<sha> is completion proof and is not replaced by a
//     writer of the same blob key. The winner's proven bytes stay; the loser
//     discards its own.
//
// The primitive refuses to overwrite in one of two tiers. The tier is chosen
// by the FILESYSTEM, not by the operating system, and the two tiers guarantee
// DIFFERENT things:
//
//  - ATOMIC NO-REPLACE (a filesystem that can create hard links). The payload
//    is placed with a hard link, which creates the final name and fails when
//    the name already exists. The existence decision and the placement are
//    ONE filesystem operation, so a concurrent publisher cannot be overtaken
//    between them, and an existing blobs/<sha> is never replaced: not by
//    another writer of the same blob key, and not by a writer outside the
//    blob key's domain either. A hard link is a directory operation, not a
//    payload copy, so the normal path stays rename-only in cost.
//
//  - DOMAIN-EXCLUSIVE (a filesystem that cannot create hard links). The Go
//    standard library exposes NO atomic no-replace rename on any supported
//    platform, so there is no stronger primitive to fall back to:
//
//      - os.Rename REPLACES an existing target on Unix (rename(2)) and on
//        Windows (MoveFileEx with MOVEFILE_REPLACE_EXISTING);
//      - the syscall package exports neither renameat2(RENAME_NOREPLACE)
//        (on Linux the wrapper is unexported and SYS_RENAMEAT2 is not even
//        defined for linux/amd64) nor renamex_np(RENAME_EXCL) (Darwin).
//
//    The fallback is therefore a check-then-rename performed while the blob
//    key's ownership domain is held EXCLUSIVELY, so no other participant of
//    that domain can create the target between the check and the rename. The
//    property that holds there is "an existing blobs/<sha> is never replaced
//    by a domain participant", NOT "never replaced" unconditionally: a
//    writer outside the domain (the mirror writer) is not fenced by it.
//
// The placement refuses to run the second tier without the ownership domain
// of the blob key it publishes, so the weaker guarantee is never reached by
// accident.
//
// The primitive is shared by the ranged publish, the cache store and the blob
// finalization, so every route that writes blobs/<sha> agrees on it.

import (
	"errors"
	"fmt"
	"log"
	"os"
)

// errBlobTargetExists reports that the content-addressed publish target
// already exists. The existing blob is the completion proof; the caller
// discards its own bytes instead of replacing them.
var errBlobTargetExists = errors.New("content-addressed blob already exists")

// errBlobPublicationDomainRequired reports that the publication could not use
// the atomic no-replace primitive and that the domain-exclusive fallback
// needs the ownership domain of the blob key, which this caller does not
// hold. It is an actionable error, not a silent downgrade: the caller must
// take the domain of the blob key it publishes.
var errBlobPublicationDomainRequired = errors.New("publishing without replacing an existing blob needs the ownership domain of the blob key")

// blobLink is the hard-link operation of the atomic no-replace placement. It
// is a package variable so a regression test can force the "this filesystem
// cannot create hard links" branch and pin what the fallback guarantees.
var blobLink = os.Link

// blobPublishPlace is the no-replace placement primitive. It is a package
// variable so a regression test can inject the historical Stat-then-rename
// behavior and prove that the publication tests actually detect it.
var blobPublishPlace = placeBlobNoReplace

// blobPublishWindowHook is called by the domain-exclusive tier between its
// existence check and its rename: the window in which the target could
// appear. It is a package variable so a regression test can observe that
// window and prove that the blob key's ownership domain excludes a second
// domain participant from it.
//
// The hook runs while the caller still holds the blob key's ownership
// domain, which is the only reason the window is safe: the rename REPLACES
// an existing target, and a domain participant is the only writer the
// domain excludes. A writer OUTSIDE the domain (the mirror writer) is not
// fenced by it.
var blobPublishWindowHook func(src, dst string)

// placeBlobNoReplace places src at dst without replacing an existing dst.
//
// It returns errBlobTargetExists when dst already exists.
//
// dom is the ownership domain of the blob key dst belongs to. The atomic
// no-replace tier does not need it. The domain-exclusive tier does, because
// its existence check and its placement are only exclusive to the domain
// while the domain of THAT blob key is held: a rename alone would replace
// whatever appeared at the target in between. The placement therefore refuses
// to run the domain-exclusive tier without the domain of this very blob key
// instead of silently losing the guarantee.
func placeBlobNoReplace(src, dst string, dom *blobDomain) error {
	err := blobLink(src, dst)
	switch {
	case err == nil:
		// The payload now carries its final name. The staging name is dropped
		// so the published blob is the only name left for those bytes.
		if rmErr := os.Remove(src); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			log.Printf("warning: could not remove staging file %s after publishing %s: %v", src, dst, rmErr)
		}
		return nil
	case errors.Is(err, os.ErrExist):
		// Somebody published this blob first. Its bytes are the completion
		// proof; ours must not replace them.
		return errBlobTargetExists
	}
	// The filesystem cannot create hard links (or the link crossed a device
	// boundary). The atomic no-replace tier is unavailable, so the placement
	// falls back to the domain-exclusive tier: the existence check and the
	// rename that follows it run while the blob key's ownership domain is
	// held exclusively, so no other participant of that domain can create the
	// target in between.
	if !dom.holdsBlobKey(dst) {
		return fmt.Errorf("publish %s: %w", dst, errBlobPublicationDomainRequired)
	}
	if _, statErr := os.Stat(dst); statErr == nil {
		// The blob is already there: it is the completion proof, and this
		// payload must not replace it.
		return errBlobTargetExists
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	// The window between the existence check and the rename. The rename
	// REPLACES an existing target, so this window is safe only because the
	// caller holds the blob key's ownership domain: no other participant of
	// that domain can create the target in here.
	if blobPublishWindowHook != nil {
		blobPublishWindowHook(src, dst)
	}
	if renameErr := os.Rename(src, dst); renameErr != nil {
		return renameErr
	}
	return nil
}
