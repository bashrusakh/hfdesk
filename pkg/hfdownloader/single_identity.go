// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Single-request partials use a sidecar to bind bytes to repository, resolved
// commit, path, and size; only matching pinned identities inherit bytes. The
// sidecar separately records clean response framing, so an exactly-sized
// partial without that proof must revalidate its final byte.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const singleIdentityVersion = 1

// singleOpts carries single-route identity, framing and caller-held ownership.
type singleOpts struct {
	// Commit is the plan's resolved commit identity.
	Commit string
	// CommitPinned distinguishes an endpoint-resolved immutable commit from a
	// requested-revision fallback, which cannot authorize inherited bytes.
	CommitPinned bool
	// FramingComplete records clean response completion, independently of object
	// identity; fresh transfers start false.
	FramingComplete bool
	// Domain is held by the caller; this route does not acquire it. Explicit-cancel
	// cleanup requires it and leaves artifacts untouched when it is nil.
	Domain *blobDomain
}

// singlePartIdentity records the object identity and framing state of a partial.
type singlePartIdentity struct {
	Version int    `json:"version"`
	Repo    string `json:"repo"`
	// Commit is the resolved commit the staged bytes belong to. It is empty
	// for an unpinned identity.
	Commit string `json:"commit,omitempty"`
	// Path is the item's repository-relative path.
	Path string `json:"path"`
	// Size is the expected size of the object.
	Size int64 `json:"size"`
	// Pinned is required for reuse; unpinned records never authorize inherited bytes.
	Pinned bool `json:"pinned"`
	// FramingComplete is separate from identity: a matching pinned prefix may
	// resume without it, but an exact-size partial needs it to skip a request.
	// Persist false before transfer and true only after clean EOF and exact length.
	FramingComplete bool `json:"framing_complete,omitempty"`
}

func singlePartIdentityPath(dst string) string {
	return dst + ".part.meta"
}

// readSinglePartIdentity reads the sidecar. Missing state is not inheritable;
// malformed or unsupported state returns an error.
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

// writeSinglePartIdentity atomically replaces the sidecar via a sibling temp.
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

// removeSinglePartIdentity removes the sidecar; a missing file is harmless.
func removeSinglePartIdentity(path string) {
	// lgtm[go/path-injection]
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		downloadLog.Warn("cleanup single-part identity failed", "path", path, "error", err)
	}
}

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

// inheritableBy uses the shared identity rule and requires a pinned match.
// FramingComplete is separate: it permits skipping final-byte revalidation,
// but does not determine whether a staged prefix belongs to this object.
func (id *singlePartIdentity) inheritableBy(current singlePartIdentity) bool {
	if id == nil {
		return false
	}
	return id.identity().matches(current.identity())
}

// singlePartialInheritable is replaceable so tests can disable identity enforcement.
var singlePartialInheritable = func(recorded *singlePartIdentity, current singlePartIdentity, staged int64) bool {
	return recorded.inheritableBy(current)
}
