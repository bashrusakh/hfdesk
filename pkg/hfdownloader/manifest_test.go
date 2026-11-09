// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import "testing"

// TestRecordDeletionAllVersionsAbsorbing pins the documented empty==all
// tombstone semantics: the empty Versions state is absorbing.
func TestRecordDeletionAllVersionsAbsorbing(t *testing.T) {
	t.Run("specific into existing all is a no-op", func(t *testing.T) {
		m := &DownloadManifest{}
		if !m.RecordDeletion("model-Q4_K_M.gguf", nil) {
			t.Fatal("initial all-version record must report a change")
		}
		if changed := m.RecordDeletion("model-Q4_K_M.gguf", []string{"version-a"}); changed {
			t.Fatal("recording a specific version into an all-version tombstone must be a no-op")
		}
		if len(m.Deleted) != 1 {
			t.Fatalf("Deleted=%v, want one tombstone", m.Deleted)
		}
		if len(m.Deleted[0].Versions) != 0 {
			t.Fatalf("Versions=%v, want empty (all versions) after no-op record", m.Deleted[0].Versions)
		}
		if !m.IsDeleted("model-Q4_K_M.gguf", "version-a") || !m.IsDeleted("model-Q4_K_M.gguf", "other-version") {
			t.Fatal("all-version tombstone must cover every version")
		}
	})

	t.Run("all upgrades an existing specific tombstone", func(t *testing.T) {
		m := &DownloadManifest{}
		if !m.RecordDeletion("model-Q4_K_M.gguf", []string{"version-a"}) {
			t.Fatal("initial specific record must report a change")
		}
		if !m.IsDeleted("model-Q4_K_M.gguf", "version-a") {
			t.Fatal("specific tombstone must cover its version")
		}
		if m.IsDeleted("model-Q4_K_M.gguf", "version-b") {
			t.Fatal("specific tombstone must not cover other versions")
		}
		if changed := m.RecordDeletion("model-Q4_K_M.gguf", []string{}); !changed {
			t.Fatal("recording an all-version deletion into a specific tombstone must report a change")
		}
		if len(m.Deleted) != 1 {
			t.Fatalf("Deleted=%v, want one tombstone", m.Deleted)
		}
		if len(m.Deleted[0].Versions) != 0 {
			t.Fatalf("Versions=%v, want empty (all versions) after upgrade", m.Deleted[0].Versions)
		}
		if !m.IsDeleted("model-Q4_K_M.gguf", "version-a") || !m.IsDeleted("model-Q4_K_M.gguf", "version-b") {
			t.Fatal("upgraded tombstone must cover every version")
		}
		if changed := m.RecordDeletion("model-Q4_K_M.gguf", nil); changed {
			t.Fatal("re-recording all into an all-version tombstone must be a no-op")
		}
	})

	t.Run("specific into specific still merges", func(t *testing.T) {
		m := &DownloadManifest{}
		if !m.RecordDeletion("model-Q4_K_M.gguf", []string{"version-a"}) {
			t.Fatal("initial specific record must report a change")
		}
		if !m.RecordDeletion("model-Q4_K_M.gguf", []string{"version-b"}) {
			t.Fatal("new specific version must report a change")
		}
		if changed := m.RecordDeletion("model-Q4_K_M.gguf", []string{"version-a"}); changed {
			t.Fatal("duplicate specific version must be a no-op")
		}
		if len(m.Deleted) != 1 || len(m.Deleted[0].Versions) != 2 {
			t.Fatalf("Deleted=%v, want one tombstone with two versions", m.Deleted)
		}
		if !m.IsDeleted("model-Q4_K_M.gguf", "version-a") || !m.IsDeleted("model-Q4_K_M.gguf", "version-b") {
			t.Fatal("merged tombstone must cover both versions")
		}
		if m.IsDeleted("model-Q4_K_M.gguf", "version-c") {
			t.Fatal("merged tombstone must not cover unrecorded versions")
		}
	})
}
