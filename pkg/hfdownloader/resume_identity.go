// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// The PINNED part of the resume identity (issue #140 follow-up).
//
// A resume inherits bytes that an earlier run staged under the same name.
// That is only sound when the name identifies ONE immutable object. The
// repository, the item path and the expected size are necessary but not
// sufficient: the plan's commit may be the REQUESTED REVISION (a branch
// name) instead of a RESOLVED commit, and the content behind a branch name
// can move between two runs. The identity is therefore PINNED only when the
// commit was actually resolved to an immutable commit - and it must be
// pinned on BOTH sides: the record that describes the staged bytes and the
// current plan.
//
// Both transfer routes ask this ONE rule, so they cannot drift apart:
//
//   - the single-request path, through singlePartIdentity.inheritableBy;
//   - the ranged staging path, through stagingMetaResumable.
//
// The same reasoning applies to the other identity fields: a record written
// without the pinned flag (or with it false) is not inheritable. That is the
// safe direction - unproven bytes are never published - and no released
// version ever wrote these records.
//
// ONE identity rule, TWO record formats. The single-request sidecar
// (version 1, <dst>.part.meta) and the ranged staging record (version 4,
// <staging>.incomplete.meta) keep their own formats, versions and
// lifecycle-specific fields, because they describe different staging states.
// They do NOT duplicate the identity decision: each exposes its object
// identity as a resumeIdentity value, and both routes compare it with the
// single resumeIdentity.matches rule.

// resumeIdentity is the in-memory identity of a staged payload: the object a
// persisted record or the current plan names. It is deliberately not a record
// format - the version, the range layout, the framing proof and the ownership
// diagnostics stay in the lifecycle-specific records.
type resumeIdentity struct {
	Repo   string
	Commit string
	Path   string
	Size   int64
	Pinned bool
}

// matches reports whether current names the same immutable object as id.
//
// Both routes call this one comparison, so the identity rule cannot drift
// between the single-request and the ranged path. An unpinned identity pins
// nothing: it names a revision whose content may have moved, so a record is
// inheritable only when BOTH sides are pinned, however well the repository,
// commit string, path and size match.
func (id resumeIdentity) matches(current resumeIdentity) bool {
	if !resumeIdentityPinned(id.Pinned, current.Pinned) {
		return false
	}
	if id.Repo != current.Repo || id.Commit != current.Commit || id.Path != current.Path {
		return false
	}
	return id.Size == current.Size
}

// identity returns the shared resume identity of the single-request sidecar.
// Version, the framing-complete proof and diagnostics are not part of the
// object identity and stay in the record.
func (id singlePartIdentity) identity() resumeIdentity {
	return resumeIdentity{
		Repo:   id.Repo,
		Commit: id.Commit,
		Path:   id.Path,
		Size:   id.Size,
		Pinned: id.Pinned,
	}
}

// identity returns the shared resume identity of the ranged staging record.
// The range layout, ProofPending, LockFenced and diagnostics are not part of
// the object identity and stay in the record.
func (meta IncompleteMeta) identity() resumeIdentity {
	return resumeIdentity{
		Repo:   meta.Repo,
		Commit: meta.Commit,
		Path:   meta.Path,
		Size:   meta.Size,
		Pinned: meta.CommitPinned,
	}
}

// resumeIdentityPinned reports whether the recorded identity and the current
// plan both name a PINNED (resolved, immutable) commit.
//
// An unpinned identity pins nothing: it names a revision whose content may
// have moved, so no bytes may be inherited from it, no matter how well the
// repository, path and size match.
//
// It is a variable so the negative control can install the historical rule
// (ignore the pinned flag) and prove that the identity regressions are
// discriminating.
var resumeIdentityPinned = func(recorded, current bool) bool {
	return recorded && current
}
