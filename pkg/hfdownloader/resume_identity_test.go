// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import "testing"

// Exercise the shared pinned identity comparison independently of either
// persisted-record adapter.
func TestResumeIdentityMatches(t *testing.T) {
	base := resumeIdentity{
		Repo:   "owner/model",
		Commit: "0123456789abcdef",
		Path:   "weights.bin",
		Size:   4096,
		Pinned: true,
	}
	with := func(mutate func(*resumeIdentity)) resumeIdentity {
		id := base
		mutate(&id)
		return id
	}

	tests := []struct {
		name      string
		recorded  resumeIdentity
		current   resumeIdentity
		wantMatch bool
	}{
		{"same identity", base, base, true},
		{"different repository", with(func(id *resumeIdentity) { id.Repo = "owner/other" }), base, false},
		{"different commit", with(func(id *resumeIdentity) { id.Commit = "fedcba9876543210" }), base, false},
		{"different path", with(func(id *resumeIdentity) { id.Path = "other.bin" }), base, false},
		{"different size", with(func(id *resumeIdentity) { id.Size = 4097 }), base, false},
		{"recorded identity unpinned", with(func(id *resumeIdentity) { id.Pinned = false }), base, false},
		{"current identity unpinned", base, with(func(id *resumeIdentity) { id.Pinned = false }), false},
		{"both identities unpinned", with(func(id *resumeIdentity) { id.Pinned = false }), with(func(id *resumeIdentity) { id.Pinned = false }), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.recorded.matches(tt.current); got != tt.wantMatch {
				t.Fatalf("matches() = %v, want %v (recorded=%+v current=%+v)", got, tt.wantMatch, tt.recorded, tt.current)
			}
		})
	}
}

// Both persisted formats must expose the same five identity fields while
// retaining their separate lifecycle state.
func TestResumeIdentityAdapters(t *testing.T) {
	want := resumeIdentity{
		Repo:   "owner/model",
		Commit: "0123456789abcdef",
		Path:   "weights.bin",
		Size:   4096,
		Pinned: true,
	}

	single := singlePartIdentity{
		Repo:            want.Repo,
		Commit:          want.Commit,
		Path:            want.Path,
		Size:            want.Size,
		Pinned:          want.Pinned,
		FramingComplete: true, // lifecycle proof, not identity
		Version:         singleIdentityVersion,
	}
	if got := single.identity(); got != want {
		t.Errorf("singlePartIdentity.identity() = %+v, want %+v", got, want)
	}

	ranged := IncompleteMeta{
		Repo:         want.Repo,
		Commit:       want.Commit,
		Path:         want.Path,
		Size:         want.Size,
		CommitPinned: want.Pinned,
		Ranges:       []RangeProgress{{Start: 0, End: want.Size - 1, Committed: want.Size, Done: true}},
		ProofPending: true, // lifecycle state, not identity
		LockFenced:   true, // ownership state, not identity
		Version:      stagingMetaVersion,
	}
	if got := ranged.identity(); got != want {
		t.Errorf("IncompleteMeta.identity() = %+v, want %+v", got, want)
	}
}
