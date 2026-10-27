// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"strings"
	"testing"
)

func TestStagingArtifactNameCoversEveryProducerShape(t *testing.T) {
	sha := strings.Repeat("a", 64)
	other := strings.Repeat("b", 64)

	staging := []string{
		// Single-request staging names and derived artifacts.
		"tmp-" + sha,
		"tmp-" + sha + ".part",
		"tmp-" + sha + ".part.meta",
		"tmp-" + sha + ".lock",
		"tmp-model_config.json",
		"tmp-" + sha + ".part-00",
		"tmp-" + sha + ".parts.json",
		// The ranged staging family.
		sha + ".incomplete",
		sha + ".incomplete.meta",
		sha + ".incomplete.meta.tmp",
		sha + ".incomplete.lock",
		sha + ".incomplete.meta.lock",
		// The legacy multipart family.
		"model.part-00",
		"model.part-17",
		"model.parts.json",
		// The atomic copy temp of the cache store.
		sha + ".tmp-123456789",
		"opaque-key.tmp-123456789",
	}
	for _, name := range staging {
		if !IsStagingArtifactName(name) {
			t.Errorf("producer shape %q was not classified as a staging artifact", name)
		}
		if got := ClassifyBlobEntryName(name); got != BlobEntryStaging {
			t.Errorf("producer shape %q classified as %v, want staging", name, got)
		}
		if IsPublishedBlobName(name) {
			t.Errorf("producer shape %q was reported as published content", name)
		}
	}

	published := []string{
		sha,
		strings.ToUpper(sha),
		other,
		// Unusual published names the public storage API accepts.
		"opaque-key",
		"unusual-published-name.bin",
		"tokenizer.model",
		"weights.safetensors",
	}
	for _, name := range published {
		if IsStagingArtifactName(name) {
			t.Errorf("published name %q was classified as a staging artifact", name)
		}
		if !IsPublishedBlobName(name) {
			t.Errorf("published name %q was not reported as content", name)
		}
	}

	// A canonical digest is the normal published name; anything else that is
	// not a staging artifact is an unusual published name.
	if got := ClassifyBlobEntryName(sha); got != BlobEntryCanonical {
		t.Errorf("canonical digest classified as %v, want canonical", got)
	}
	if got := ClassifyBlobEntryName("opaque-key"); got != BlobEntryUnusual {
		t.Errorf("unusual published name classified as %v, want unusual", got)
	}
	if !IsCanonicalBlobName(sha) || IsCanonicalBlobName("opaque-key") {
		t.Error("the canonical blob name check does not recognize the canonical digest form")
	}

	// Incomplete staging-name shapes remain ordinary published names.
	for _, name := range []string{"tmp-", "key.tmp-", ".tmp-tail", "abc123.gguf"} {
		if IsStagingArtifactName(name) {
			t.Errorf("non-producer name %q was classified as a staging artifact", name)
		}
	}
}
