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
