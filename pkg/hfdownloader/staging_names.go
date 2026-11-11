// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// This classifier defines the reserved staging shapes inside blobs/; every other name, including non-canonical
// published names, remains content. Keep the producer list in sync with the artifact patterns in doc.go.

import (
	"runtime"
	"strings"
)

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

	// The single-request staging destination and its legacy lock-residue shape:
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
	// Legacy payload-side lock residue; current ownership locks are external.
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
