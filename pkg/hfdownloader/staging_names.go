// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Classification of the names the downloader writes into a cache (issue #140
// follow-up, group 2).
//
// A cache's blobs/ directory holds two different kinds of entries: PUBLISHED
// content and the downloader's own STAGING artifacts. Treating the second as
// the first is a defect: an unfinished single download or a legacy multipart
// residue is counted as ready content, listed as a cached file and copied by a
// mirror.
//
// The staging shapes below are derived from the code that creates them:
//
//   - tmp-<name>            the single-request staging destination (a content
//     digest, or a path-derived name for an item without a known digest) and
//     everything derived from it: tmp-<name>.part (the single-request
//     partial), tmp-<name>.part.meta (its identity sidecar) and
//     tmp-<name>.lock (the destination domain's persistent lock);
//   - <name>.incomplete      the ranged staging payload;
//   - <name>.incomplete.meta its resume record;
//   - <name>.incomplete.meta.tmp / <name>.meta.tmp
//                            the atomic record temp;
//   - <name>.lock            the persistent ownership lock beside a staging
//     payload;
//   - <name>.part            the single-request partial (the flat-mode
//     destination's partial);
//   - <name>.part.meta       the single-request identity sidecar;
//   - <name>.part-NN         the legacy multipart family;
//   - <name>.parts.json      the legacy layout metadata;
//   - <name>.tmp-<random>    the atomic copy temp of the cache store.
//
// A published blob is named by its content: a canonical 64-hex SHA-256
// digest. The public storage API also accepts other names, so an entry that is
// neither a staging artifact nor a canonical digest is an UNUSUAL published
// name: it is content, and it is copied, verified and counted like any other.
// Only the staging artifacts are excluded - and only inside blobs/, which is
// the namespace the downloader writes them into.

import (
	"runtime"
	"strings"
)

// BlobEntryKind classifies a cache blobs/ entry name.
type BlobEntryKind int

const (
	// BlobEntryStaging is a downloader staging artifact: a payload, a resume
	// record, an identity sidecar, an ownership lock or an atomic-write temp.
	// It is not published content and is excluded from the content surfaces.
	BlobEntryStaging BlobEntryKind = iota
	// BlobEntryCanonical is a ready, published blob: a canonical 64-hex
	// SHA-256 digest, the cache's blob naming.
	BlobEntryCanonical
	// BlobEntryUnusual is a published blob whose name is not the canonical
	// digest form. The public storage API accepts such names, so they are
	// content: the cache surfaces count them, the mirror copies and verifies
	// them, and the deletion path may delete them.
	BlobEntryUnusual
)

// IsCanonicalBlobName reports whether name is the canonical 64-hex SHA-256
// digest form the cache uses to name a published blob. It accepts upper or
// lower case, exactly like the canonical digest check the downloader uses for
// the blob keys it writes.
func IsCanonicalBlobName(name string) bool {
	return canonicalSHA256(name)
}

// IsStagingArtifactName reports whether name is one of the staging artifact
// shapes the downloader creates. It is a NAME check: the decision applies to
// the blobs/ namespace, where the downloader writes these artifacts. A name
// outside that namespace is content even when it has one of these shapes.
func IsStagingArtifactName(name string) bool {
	if name == "" {
		return false
	}
	folded := name
	if runtime.GOOS == "windows" {
		// The filesystem's own case semantics decide: a staging artifact
		// keeps its shape when the name's case changes.
		folded = strings.ToLower(name)
	}

	// The single-request staging destination and everything derived from it:
	// tmp-<name>, tmp-<name>.part, tmp-<name>.part.meta, tmp-<name>.lock.
	if strings.HasPrefix(folded, "tmp-") && len(folded) > len("tmp-") {
		return true
	}
	// The ranged staging family: the payload, its resume record and the
	// atomic record temp.
	if strings.HasSuffix(folded, ".incomplete") ||
		strings.HasSuffix(folded, ".incomplete.meta") ||
		strings.HasSuffix(folded, ".incomplete.meta.tmp") ||
		strings.HasSuffix(folded, ".meta.tmp") {
		return true
	}
	// The persistent ownership lock beside a staging payload.
	if strings.HasSuffix(folded, ".lock") {
		return true
	}
	// The single-request partial and its identity sidecar.
	if strings.HasSuffix(folded, ".part") || strings.HasSuffix(folded, ".part.meta") {
		return true
	}
	// The legacy multipart family: the numbered parts and the layout metadata.
	if strings.HasSuffix(folded, ".parts.json") {
		return true
	}
	if idx := strings.LastIndex(folded, ".part-"); idx >= 0 && isAllDigits(folded[idx+len(".part-"):]) {
		return true
	}
	// The atomic-write temp family: the cache store's copy temp.
	if isAtomicCopyStagingName(name, runtime.GOOS == "windows") {
		return true
	}
	return false
}

// ClassifyBlobEntryName classifies a cache blobs/ entry name. The staging
// shapes win over the published ones: an artifact is never content, whatever
// it is named.
func ClassifyBlobEntryName(name string) BlobEntryKind {
	if IsStagingArtifactName(name) {
		return BlobEntryStaging
	}
	if IsCanonicalBlobName(name) {
		return BlobEntryCanonical
	}
	return BlobEntryUnusual
}

// IsPublishedBlobName reports whether name is published content: a canonical
// digest, or an unusual published name the public storage API accepts. Only a
// downloader staging artifact is not content.
func IsPublishedBlobName(name string) bool {
	return ClassifyBlobEntryName(name) != BlobEntryStaging
}
