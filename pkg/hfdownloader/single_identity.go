// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Resume identity of the single-request path (issue #140 follow-up).
//
// The single-request path stages its bytes in <dst>.part. For an item without
// a known digest the destination name is derived from the item's path
// (tmp-path-<sanitized path>), so two different objects - or two revisions of
// one object - can share one staging name, and the destination's ownership
// domain excludes concurrent writers without establishing WHICH object the
// staged bytes belong to. A size match is not an identity: a partial left by
// an earlier run of a different file or a different revision could otherwise be
// published as this item's content.
//
// The sidecar <dst>.part.meta carries that identity next to the partial: the
// repository, the RESOLVED commit, the item's relative path, the expected size
// and whether the commit was actually resolved (PINNED). Bytes already on disk
// are inherited only when the sidecar exists, is readable and well-formed,
// matches the CURRENT item exactly, and the identity is pinned on both sides.
// Otherwise the inherited bytes are discarded and the transfer starts from
// zero. The sidecar is written before any inherited byte is trusted, is
// removed with the partial it describes, and survives an interrupted run -
// which is exactly what makes the next run's decision possible.
//
// The same sidecar records a second, independent fact: whether the run that
// produced the partial saw the response body end cleanly at the exact expected
// byte count (FramingComplete). Object identity does not prove clean framing:
// a process can exit after the last WriteAt but before io.Copy observes EOF.
// An exactly-sized partial is therefore published without a request only when
// the identity matches AND that completion proof is present; otherwise the
// final byte is reserved and revalidated with a non-empty range request.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// singleIdentityVersion is the version written into the single-request
// identity sidecar. A record with any other version is not understood and
// therefore never authorizes inheriting bytes.
const singleIdentityVersion = 1

// singleOpts carries the resolved-object identity the single-request path
// binds its partial payload to. It mirrors multipartOpts.Commit: the plan's
// resolved commit, plus whether that commit was actually resolved.
type singleOpts struct {
	// Commit is the plan's resolved commit identity.
	Commit string
	// CommitPinned reports whether Commit is an immutable, resolved commit
	// (the endpoint returned it) rather than the requested-revision fallback
	// the planner uses when the revision could not be resolved. An unpinned
	// identity pins nothing: the branch may have moved, so no bytes may be
	// inherited from it.
	CommitPinned bool
	// FramingComplete is the persisted clean-framing completion proof of the
	// sidecar: it is set only after a response body ended cleanly at the
	// exact expected byte count, and it is NOT part of the object identity
	// (inheritableBy ignores it). A fresh identity carries it false.
	FramingComplete bool
}

// singlePartIdentity is the recorded identity of a single-request partial.
// It uses the same identity vocabulary as the ranged staging record
// (IncompleteMeta): Repo, Commit, Path and Size, plus the pinned flag that
// says whether the commit is an immutable, resolved commit.
type singlePartIdentity struct {
	// Version is the sidecar format version.
	Version int `json:"version"`
	// Repo is the repository the staged bytes belong to.
	Repo string `json:"repo"`
	// Commit is the resolved commit the staged bytes belong to. It is empty
	// for an unpinned identity.
	Commit string `json:"commit,omitempty"`
	// Path is the item's repository-relative path.
	Path string `json:"path"`
	// Size is the expected size of the object.
	Size int64 `json:"size"`
	// Pinned reports whether Commit was actually resolved to an immutable
	// commit. An unpinned identity pins nothing, so a partial carrying it is
	// never inherited.
	Pinned bool `json:"pinned"`
	// FramingComplete records that the run which produced this partial
	// observed a clean end of the response body at the exact expected byte
	// count. It is not part of the object identity (inheritableBy ignores
	// it): a matching, pinned record may resume the staged prefix without it,
	// but an exactly-sized partial is not published without a request until
	// this proof exists. A fresh transfer records false before any byte is
	// trusted and sets it only after io.Copy returns nil and every exact body
	// length check has passed.
	FramingComplete bool `json:"framing_complete,omitempty"`
}

// singlePartIdentityPath returns the identity sidecar path of the
// single-request partial staged at dst.
func singlePartIdentityPath(dst string) string {
	return dst + ".part.meta"
}

// readSinglePartIdentity reads and parses the identity sidecar at path.
//
// A missing sidecar yields (nil, nil): the partial it would describe has no
// recorded identity and can never be inherited. A malformed or unsupported
// record yields an error, because an unreadable record can never authorize
// inheriting bytes either.
func readSinglePartIdentity(path string) (*singlePartIdentity, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var id singlePartIdentity
	if err := json.Unmarshal(data, &id); err != nil {
		return nil, fmt.Errorf("parse single-part identity: %w", err)
	}
	if id.Version != singleIdentityVersion {
		return nil, fmt.Errorf("unsupported single-part identity version %d", id.Version)
	}
	if id.Repo == "" || id.Path == "" {
		return nil, errors.New("incomplete single-part identity")
	}
	if id.Pinned && id.Commit == "" {
		return nil, errors.New("pinned single-part identity without a commit")
	}
	return &id, nil
}

// writeSinglePartIdentity writes the identity sidecar atomically. The record
// is staged in a sibling temp file and renamed into place, so an interrupted
// write never leaves a half-written record that a later run could misread.
func writeSinglePartIdentity(path string, id singlePartIdentity) error {
	id.Version = singleIdentityVersion
	data, err := json.Marshal(id)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// The staged copy is removed when it was not renamed into place; after a
	// successful rename the removal is a no-op.
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// lgtm[go/path-injection]
	return os.Rename(tmpName, path)
}

// removeSinglePartIdentity removes the identity sidecar at path. A missing
// sidecar is not an error: the record is an artifact, not state that must
// exist.
func removeSinglePartIdentity(path string) {
	// lgtm[go/path-injection]
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		downloadLog.Warn("cleanup single-part identity failed", "path", path, "error", err)
	}
}

// singleIdentityFor derives the identity of the item the single-request path
// is about to transfer. The path is the item's repository-relative path as
// the transfer sees it, the same identity vocabulary the ranged staging record
// uses.
func singleIdentityFor(job Job, it PlanItem, opts singleOpts) singlePartIdentity {
	id := singlePartIdentity{
		Version:         singleIdentityVersion,
		Repo:            job.Repo,
		Path:            it.RelativePath,
		Size:            it.Size,
		Pinned:          opts.CommitPinned,
		FramingComplete: opts.FramingComplete,
	}
	if opts.CommitPinned {
		id.Commit = opts.Commit
	}
	return id
}

// inheritableBy reports whether the recorded identity authorizes inheriting
// the staged bytes for the current item.
//
// The bytes may be inherited only when the record was written for exactly
// this object: the same repository, the same resolved commit, the same item
// path and the same expected size - and when the identity is PINNED on both
// sides. An unpinned identity names a revision whose content may have moved,
// so nothing under that name may be inherited, no matter how well the names
// match. The pinned rule is the shared one (resumeIdentityPinned), so this
// route and the ranged staging route cannot drift apart.
//
// FramingComplete is deliberately NOT compared here: it is completion proof
// for the response body, not part of the object identity. A matching record
// without it still authorizes resuming the staged prefix; it only forbids
// publishing an exactly-sized partial without revalidating the final byte.
func (id *singlePartIdentity) inheritableBy(current singlePartIdentity) bool {
	if id == nil {
		return false
	}
	if !resumeIdentityPinned(id.Pinned, current.Pinned) {
		return false
	}
	if id.Repo != current.Repo || id.Commit != current.Commit || id.Path != current.Path {
		return false
	}
	return id.Size == current.Size
}

// singlePartialInheritable decides whether the bytes already staged at a
// destination may be inherited for the current item.
//
// It is a seam: the negative control installs the historical rule (inherit the
// staged bytes on their size alone) to prove that the identity regressions are
// discriminating. The production path always enforces the identity record.
var singlePartialInheritable = func(recorded *singlePartIdentity, current singlePartIdentity, staged int64) bool {
	return recorded.inheritableBy(current)
}
