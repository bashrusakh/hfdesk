// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Resumed bytes are reusable only when repo, resolved commit, path and size
// match and both identities are pinned. The version-1 single sidecar and
// version-4 ranged record retain separate lifecycle state but use this shared
// identity comparison; framing, ownership and format version are not identity.

type resumeIdentity struct {
	Repo   string
	Commit string
	Path   string
	Size   int64
	Pinned bool
}

// matches requires equal object fields and pinned identities on both sides.
func (id resumeIdentity) matches(current resumeIdentity) bool {
	if !resumeIdentityPinned(id.Pinned, current.Pinned) {
		return false
	}
	if id.Repo != current.Repo || id.Commit != current.Commit || id.Path != current.Path {
		return false
	}
	return id.Size == current.Size
}

func (id singlePartIdentity) identity() resumeIdentity {
	return resumeIdentity{
		Repo:   id.Repo,
		Commit: id.Commit,
		Path:   id.Path,
		Size:   id.Size,
		Pinned: id.Pinned,
	}
}

func (meta IncompleteMeta) identity() resumeIdentity {
	return resumeIdentity{
		Repo:   meta.Repo,
		Commit: meta.Commit,
		Path:   meta.Path,
		Size:   meta.Size,
		Pinned: meta.CommitPinned,
	}
}

// resumeIdentityPinned is replaceable so tests can falsify the shared rule.
var resumeIdentityPinned = func(recorded, current bool) bool {
	return recorded && current
}
