// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// This classifier defines reserved staging shapes inside blobs/. Other names,
// including non-canonical published names accepted by the storage API, remain
// content. Keep the producer list in sync with the artifact patterns in doc.go.

import (
	"runtime"
	"strings"
)

// BlobEntryKind classifies a cache blobs/ entry name.
type BlobEntryKind int

const (
	// BlobEntryStaging denotes a reserved staging-name shape, excluded from
	// published-content surfaces regardless of the entry's actual provenance.
	BlobEntryStaging BlobEntryKind = iota
	// BlobEntryCanonical denotes a canonical 64-hex SHA-256 blob name.
	BlobEntryCanonical
	// BlobEntryUnusual denotes a noncanonical, nonreserved name accepted as
	// published content by the storage API and cache surfaces.
	BlobEntryUnusual
)

// IsCanonicalBlobName accepts exactly 64 hexadecimal SHA-256 characters,
// in upper or lower case.
func IsCanonicalBlobName(name string) bool {
	return canonicalSHA256(name)
}

// IsStagingArtifactName checks reserved staging-name shapes. Callers must
// apply the exclusion only inside blobs/, not to same-shaped content elsewhere.
func IsStagingArtifactName(name string) bool {
	if name == "" {
		return false
	}
	folded := name
	if runtime.GOOS == "windows" {
		// Fold staging-name case on Windows.
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

// ClassifyBlobEntryName classifies blobs/ names, with reserved staging shapes
// taking precedence over published-name classes.
func ClassifyBlobEntryName(name string) BlobEntryKind {
	if IsStagingArtifactName(name) {
		return BlobEntryStaging
	}
	if IsCanonicalBlobName(name) {
		return BlobEntryCanonical
	}
	return BlobEntryUnusual
}

// IsPublishedBlobName reports whether name is nonreserved: canonical digests
// and unusual names are content; staging-shaped names are excluded.
func IsPublishedBlobName(name string) bool {
	return ClassifyBlobEntryName(name) != BlobEntryStaging
}
