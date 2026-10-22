// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// BranchVariantServiceDirName is the application-owned subtree used for EXL
// branch payloads and their separate completion evidence. It is a path
// component, not an ownership test: ownership is established by the durable
// record created when the service directory is claimed.
const BranchVariantServiceDirName = ".hfdesk-variant-store"

const (
	branchVariantStoreOwnerFilename    = ".hfdesk-owner.json"
	branchVariantArtifactOwnerFilename = ".hfdesk-owner.json"
	branchVariantStoreVersion          = 1
)

var errInvalidBranchVariantOwnership = errors.New("invalid branch variant service ownership")

// errNoBranchVariantOwnershipEvidence reports that a service-root candidate
// carries no application ownership evidence at all. Such a candidate is
// ordinary data: it is never adopted, and it never blocks unrelated ordinary
// work in its root.
var errNoBranchVariantOwnershipEvidence = errors.New("no branch variant service ownership evidence")

// branchVariantServiceState classifies one concrete service-root candidate.
// A component name alone never classifies a directory: the state comes from
// the durable application evidence inside the candidate.
type branchVariantServiceState int

const (
	// branchVariantServiceOrdinary: the candidate carries no application
	// ownership evidence and stays ordinary data. Unrelated ordinary work in
	// the same root is unaffected, and a claim refuses it without adopting or
	// changing it.
	branchVariantServiceOrdinary branchVariantServiceState = iota
	// branchVariantServiceUnverifiable: the candidate carries application-shaped
	// ownership evidence that could not be established. Writes that physically
	// intersect it fail closed, and the data is neither adopted nor deleted.
	branchVariantServiceUnverifiable
	// branchVariantServiceOwned: the candidate's durable logical ownership
	// evidence validated. Normal ownership rules apply.
	branchVariantServiceOwned
)

type branchVariantWriteLockSet struct {
	held     map[string]struct{}
	releases []func()
}

type branchVariantStoreOwner struct {
	Version int    `json:"version"`
	StoreID string `json:"storeId"`
	Purpose string `json:"purpose"`
	// FileID records the physical service-root object when the logical store
	// was first claimed. It is not the runtime lease key; a copied root can have
	// a different current FileID while retaining its validated StoreID.
	FileID string `json:"fileId"`
}

type branchVariantArtifactOwner struct {
	Version  int      `json:"version"`
	StoreID  string   `json:"storeId"`
	RepoType RepoType `json:"repoType"`
	Repo     string   `json:"repo"`
	Revision string   `json:"revision"`
	Commit   string   `json:"commit"`
}

// BranchVariantArtifact identifies the app-owned payload and its separate
// control record for one resolved branch artifact.
type BranchVariantArtifact struct {
	ServiceRoot  string
	PayloadRoot  string
	ControlRoot  string
	ManifestPath string
	RepoType     RepoType
	Repo         string
	Revision     string
	Commit       string
	storeID      string
}

// BranchVariantServiceRoot returns the physical service-root spelling below
// the selected destination. Existing parent symlinks are resolved so route,
// localDir, and configured-root aliases identify the same store.
func BranchVariantServiceRoot(destinationRoot string) (string, error) {
	root, err := resolvedPathForCreate(destinationRoot)
	if err != nil {
		return "", fmt.Errorf("resolve branch variant destination root: %w", err)
	}
	return filepath.Join(root, BranchVariantServiceDirName), nil
}

// BranchVariantServiceOwned reports whether the selected root contains a
// valid application ownership record. An absent service tree is a normal
// miss; a candidate that carries application ownership evidence which cannot
// be established is an ownership failure.
func BranchVariantServiceOwned(destinationRoot string) (bool, error) {
	_, _, exists, err := readBranchVariantServiceRoot(destinationRoot)
	return exists, err
}

// RebindBranchVariantServiceRootUnderLease refreshes the stored physical
// binding after a structurally valid whole-root copy has been recognized.
// Callers must hold an exclusive destination-root data or admission lease. A
// missing, malformed, foreign, or incomplete service tree is left untouched.
func RebindBranchVariantServiceRootUnderLease(destinationRoot string) error {
	_, err := rebindBranchVariantServiceRoot(destinationRoot)
	return err
}

func branchVariantServiceBindingNeedsRebind(destinationRoot string) (bool, error) {
	serviceRoot, err := BranchVariantServiceRoot(destinationRoot)
	if err != nil {
		return false, err
	}
	state, owner, err := resolveBranchVariantServiceOwnership(serviceRoot)
	if err != nil || state != branchVariantServiceOwned {
		// Ordinary and unverifiable candidates have no established logical
		// binding to refresh. Ordinary work must never depend on claiming the
		// service location, so an unreadable or foreign candidate never blocks
		// unrelated ordinary downloads in its root.
		return false, nil
	}
	currentID, err := branchVariantFilesystemIdentity(serviceRoot)
	if err != nil {
		return false, err
	}
	return owner.FileID != currentID, nil
}

func rebindBranchVariantServiceRoot(destinationRoot string) (bool, error) {
	serviceRoot, err := BranchVariantServiceRoot(destinationRoot)
	if err != nil {
		return false, err
	}
	state, owner, err := resolveBranchVariantServiceOwnership(serviceRoot)
	if err != nil || state != branchVariantServiceOwned {
		// A missing, malformed, foreign, or incomplete service tree is left
		// untouched.
		return false, nil
	}
	currentID, err := branchVariantFilesystemIdentity(serviceRoot)
	if err != nil {
		return false, err
	}
	if owner.FileID == currentID {
		return false, nil
	}
	owner.FileID = currentID
	data, err := json.Marshal(owner)
	if err != nil {
		return false, err
	}
	ownerPath := filepath.Join(serviceRoot, branchVariantStoreOwnerFilename)
	if err := writeFileAtomic(ownerPath, data); err != nil {
		return false, fmt.Errorf("refresh copied branch variant service binding: %w", err)
	}
	return true, nil
}

// IsOwnedBranchVariantServicePath checks a concrete directory path and treats
// malformed/unmatched owner data as unowned. Permission and I/O errors remain
// errors so scanners can fail closed without reserving a name globally.
func IsOwnedBranchVariantServicePath(serviceRoot string) (bool, error) {
	state, _, err := resolveBranchVariantServiceOwnership(serviceRoot)
	if errors.Is(err, errInvalidBranchVariantOwnership) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return state == branchVariantServiceOwned, nil
}

// BranchVariantControlRepoPath returns one repository's control namespace in
// the service tree.
func BranchVariantControlRepoPath(destinationRoot string, repoType RepoType, repoID string) (string, error) {
	root, err := BranchVariantServiceRoot(destinationRoot)
	if err != nil {
		return "", err
	}
	return branchVariantRepoPath(root, "control", repoType, repoID)
}

// BranchVariantPayloadRepoPath returns one repository's payload namespace in
// the service tree.
func BranchVariantPayloadRepoPath(destinationRoot string, repoType RepoType, repoID string) (string, error) {
	root, err := BranchVariantServiceRoot(destinationRoot)
	if err != nil {
		return "", err
	}
	return branchVariantRepoPath(root, "payloads", repoType, repoID)
}

// BranchVariantRevisionPayloadPath returns the revision-specific payload
// parent. It is a planning/display path only; it does not claim or create the
// application-owned service tree.
func BranchVariantRevisionPayloadPath(destinationRoot string, repoType RepoType, repoID, revision string) (string, error) {
	root, err := BranchVariantServiceRoot(destinationRoot)
	if err != nil {
		return "", err
	}
	repoPath, err := branchVariantRepoPath(root, "payloads", repoType, repoID)
	if err != nil {
		return "", err
	}
	return SafeJoin(repoPath, BranchVariantRevisionDirName(revision))
}

// BranchVariantRevisionPath returns the revision-specific control parent.
// Completion records never share the repository payload namespace.
func BranchVariantRevisionPath(destinationRoot string, repoType RepoType, repoID, revision string) (string, error) {
	root, err := BranchVariantServiceRoot(destinationRoot)
	if err != nil {
		return "", err
	}
	repoPath, err := branchVariantRepoPath(root, "control", repoType, repoID)
	if err != nil {
		return "", err
	}
	return SafeJoin(repoPath, BranchVariantRevisionDirName(revision))
}

// BranchVariantPath returns the payload-files root for one requested branch
// and its download-time resolved commit, inside the selected root's service
// tree rather than under the ordinary repo-relative destination.
func BranchVariantPath(destinationRoot string, repoType RepoType, repoID, revision, commit string) (string, error) {
	revisionRoot, err := BranchVariantRevisionPayloadPath(destinationRoot, repoType, repoID, revision)
	if err != nil {
		return "", err
	}
	artifactRoot, err := SafeJoin(revisionRoot, BranchVariantCommitDirName(commit))
	if err != nil {
		return "", err
	}
	return SafeJoin(artifactRoot, "files")
}

// BranchVariantMetadataRevisionPath returns the revision-specific control
// parent for one repository.
func BranchVariantMetadataRevisionPath(destinationRoot string, repoType RepoType, repoID, revision string) (string, error) {
	return BranchVariantRevisionPath(destinationRoot, repoType, repoID, revision)
}

// BranchVariantMetadataPath returns the control directory for one resolved
// artifact. A Hub-provided file named hfd.yaml or manifest remains model data
// under files/ and cannot replace this completion record.
func BranchVariantMetadataPath(destinationRoot string, repoType RepoType, repoID, revision, commit string) (string, error) {
	revisionRoot, err := BranchVariantRevisionPath(destinationRoot, repoType, repoID, revision)
	if err != nil {
		return "", err
	}
	return SafeJoin(revisionRoot, BranchVariantCommitDirName(commit))
}

// BranchVariantRevisionDirName returns a portable path component for a
// requested revision. The revision is represented by a readable slug plus its
// full digest; it is never used as a filesystem path.
func BranchVariantRevisionDirName(revision string) string {
	digest := sha256.Sum256([]byte(revision))
	return "branch-" + branchVariantSlug(revision) + "-" + hex.EncodeToString(digest[:])
}

// BranchVariantCommitDirName returns the portable path component for a
// download-time resolved commit.
func BranchVariantCommitDirName(commit string) string {
	digest := sha256.Sum256([]byte(commit))
	return "commit-" + hex.EncodeToString(digest[:])
}

// ClaimBranchVariantArtifact establishes or reopens the service-root and
// per-artifact ownership records before any branch payload is written. Existing
// unclaimed files at either location are refused, never adopted or removed.
func ClaimBranchVariantArtifact(destinationRoot string, repoType RepoType, repoID, revision, commit string) (*BranchVariantArtifact, error) {
	dataRelease, err := acquireRebuildCoordinationLeases(context.Background(), []string{destinationRoot}, false, nil)
	if err != nil {
		return nil, fmt.Errorf("lease branch variant destination: %w", err)
	}
	defer dataRelease()
	admissionRelease, err := acquireRebuildAdmissionLeases(context.Background(), []string{destinationRoot}, true, nil)
	if err != nil {
		return nil, fmt.Errorf("lease branch variant service claim: %w", err)
	}
	defer admissionRelease()
	if err := RebindBranchVariantServiceRootUnderLease(destinationRoot); err != nil {
		return nil, err
	}
	artifact, release, err := claimBranchVariantArtifact(destinationRoot, repoType, repoID, revision, commit, false)
	if release != nil {
		release()
	}
	return artifact, err
}

// claimBranchVariantArtifact retains destination/artifact leases when the
// caller is beginning a download. Claim publication and the following payload
// mutation therefore have no unprotected delete/rebuild window.
func claimBranchVariantArtifactForDownload(destinationRoot string, repoType RepoType, repoID, revision, commit string) (*BranchVariantArtifact, func(), error) {
	return claimBranchVariantArtifact(destinationRoot, repoType, repoID, revision, commit, true)
}

func claimBranchVariantArtifact(destinationRoot string, repoType RepoType, repoID, revision, commit string, retain bool) (*BranchVariantArtifact, func(), error) {
	if commit == "" {
		return nil, nil, fmt.Errorf("branch variant requires a resolved commit")
	}
	if err := rejectOwnedCoordinationMutationTargets(true, destinationRoot); err != nil {
		return nil, nil, err
	}
	serviceRoot, err := BranchVariantServiceRoot(destinationRoot)
	if err != nil {
		return nil, nil, err
	}
	var locks branchVariantWriteLockSet
	if err := locks.Acquire(serviceRoot); err != nil {
		return nil, nil, err
	}
	keepLocks := false
	defer func() {
		if !keepLocks {
			locks.Close()
		}
	}()

	var storeID string
	serviceRoot, storeID, err = ensureBranchVariantServiceRoot(destinationRoot)
	if err != nil {
		return nil, nil, err
	}
	artifact, err := branchVariantArtifactPaths(serviceRoot, storeID, repoType, repoID, revision, commit)
	if err != nil {
		return nil, nil, err
	}

	if _, err := os.Lstat(artifact.ControlRoot); err == nil {
		if err := verifyBranchVariantArtifactOwner(artifact); err != nil {
			return nil, nil, fmt.Errorf("refuse unowned branch variant control path: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, nil, fmt.Errorf("inspect branch variant control path: %w", err)
	} else {
		if _, payloadErr := os.Lstat(artifact.PayloadRoot); payloadErr == nil {
			return nil, nil, fmt.Errorf("refuse unowned branch variant payload path %q", artifact.PayloadRoot)
		} else if !errors.Is(payloadErr, os.ErrNotExist) {
			return nil, nil, fmt.Errorf("inspect branch variant payload path: %w", payloadErr)
		}
		if err := makeOwnedDirectory(serviceRoot, artifact.ControlRoot); err != nil {
			return nil, nil, fmt.Errorf("create branch variant control path: %w", err)
		}
		owner := branchVariantArtifactOwner{
			Version: branchVariantStoreVersion, StoreID: storeID, RepoType: repoType,
			Repo: repoID, Revision: revision, Commit: commit,
		}
		if err := writeExclusiveJSON(filepath.Join(artifact.ControlRoot, branchVariantArtifactOwnerFilename), owner); err != nil {
			return nil, nil, fmt.Errorf("claim branch variant artifact: %w", err)
		}
	}
	if err := makeOwnedDirectory(serviceRoot, artifact.PayloadRoot); err != nil {
		return nil, nil, fmt.Errorf("create branch variant payload path: %w", err)
	}
	if !retain {
		return artifact, nil, nil
	}

	// The root hierarchy prevents a recursive delete/clean of an aliased
	// destination. The artifact lock remains narrow so separate variants can
	// download concurrently.
	locks.Close()
	keepLocks = true
	rootRelease, err := acquireBranchVariantDirectoryScopes(nil, branchVariantDirectoryScope{
		path: destinationRoot, mode: branchVariantLeaseShared,
	})
	if err != nil {
		return nil, nil, err
	}
	controlRelease, err := acquireExactBranchVariantDirectoryLease(artifact.ControlRoot, branchVariantLeaseExclusive)
	if err != nil {
		rootRelease()
		return nil, nil, err
	}
	return artifact, func() {
		controlRelease()
		rootRelease()
	}, nil
}

// ReadBranchVariantArtifact returns the path mapping only when both the
// selected-root service claim and the exact repo/revision/commit claim match.
// A missing claim is an ordinary cache miss, not proof inferred from a
// completion-shaped manifest.
func ReadBranchVariantArtifact(destinationRoot string, repoType RepoType, repoID, revision, commit string) (*BranchVariantArtifact, error) {
	serviceRoot, storeID, exists, err := readBranchVariantServiceRoot(destinationRoot)
	if err != nil || !exists {
		return nil, err
	}
	artifact, err := branchVariantArtifactPaths(serviceRoot, storeID, repoType, repoID, revision, commit)
	if err != nil {
		return nil, err
	}
	if err := verifyBranchVariantArtifactOwner(artifact); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return artifact, nil
}

// ReadBranchVariantArtifactAtControl validates a control directory discovered
// while enumerating a revision. The control owner record, not directory names
// or completion-manifest contents alone, supplies the repo/revision/commit
// identity.
func ReadBranchVariantArtifactAtControl(destinationRoot string, repoType RepoType, repoID, revision, controlRoot string) (*BranchVariantArtifact, error) {
	serviceRoot, storeID, exists, err := readBranchVariantServiceRoot(destinationRoot)
	if err != nil || !exists {
		return nil, err
	}
	controlAbs, err := filepath.Abs(filepath.Clean(controlRoot))
	if err != nil {
		return nil, err
	}
	if !PathInside(serviceRoot, controlAbs) || !sameBranchVariantPath(controlAbs, controlRoot) {
		return nil, fmt.Errorf("branch variant control path is outside the selected service tree")
	}
	if !ownedPathComponentsSafe(serviceRoot, controlAbs) {
		return nil, fmt.Errorf("branch variant control path contains an unsafe component")
	}
	data, err := readRegularJSON(filepath.Join(controlAbs, branchVariantArtifactOwnerFilename))
	if err != nil {
		return nil, err
	}
	var owner branchVariantArtifactOwner
	if err := json.Unmarshal(data, &owner); err != nil {
		return nil, err
	}
	if owner.Version != branchVariantStoreVersion || owner.StoreID != storeID || owner.RepoType != repoType ||
		!strings.EqualFold(owner.Repo, repoID) || owner.Revision != revision ||
		filepath.Base(controlAbs) != BranchVariantCommitDirName(owner.Commit) {
		return nil, fmt.Errorf("branch variant control ownership does not match its selected location")
	}
	artifact, err := branchVariantArtifactPaths(serviceRoot, storeID, owner.RepoType, owner.Repo, owner.Revision, owner.Commit)
	if err != nil {
		return nil, err
	}
	if !sameBranchVariantPath(artifact.ControlRoot, controlAbs) {
		return nil, fmt.Errorf("branch variant control path does not match its owner record")
	}
	return artifact, nil
}

// ReadBranchVariantArtifactAtControlAnyRevision reads an app-owned artifact
// identity from a control directory without taking its revision from the
// directory name or manifest.
func ReadBranchVariantArtifactAtControlAnyRevision(destinationRoot string, repoType RepoType, repoID, controlRoot string) (*BranchVariantArtifact, error) {
	serviceRoot, storeID, exists, err := readBranchVariantServiceRoot(destinationRoot)
	if err != nil || !exists {
		return nil, err
	}
	return readBranchVariantArtifactAtControlAnyRevision(serviceRoot, storeID, repoType, repoID, controlRoot)
}

// RejectOwnedBranchVariantWrites refuses ordinary entry mutations whose
// physical directory entry is inside an actually claimed service tree. The
// final symlink is deliberately not followed: rename/unlink replaces that
// entry, not its referent. A matching component alone is not ownership.
func RejectOwnedBranchVariantWrites(targets ...string) error {
	return RejectOwnedBranchVariantEntryWrites(targets...)
}

// RejectOwnedBranchVariantEntryWrites checks leaf-entry replacements/unlinks
// without following the final symlink.
func RejectOwnedBranchVariantEntryWrites(targets ...string) error {
	return rejectOwnedBranchVariantEffects(false, targets...)
}

// RejectOwnedBranchVariantContentWrites refuses ordinary content writes that
// follow a leaf symlink into an owned service tree.
func RejectOwnedBranchVariantContentWrites(targets ...string) error {
	return rejectOwnedBranchVariantEffects(true, targets...)
}

// RejectOwnedBranchVariantDirectoryWrites checks directory effects that may
// descend through a final directory symlink.
func RejectOwnedBranchVariantDirectoryWrites(targets ...string) error {
	return RejectOwnedBranchVariantContentWrites(targets...)
}

func rejectOwnedBranchVariantEffects(followLeaf bool, targets ...string) error {
	if err := rejectOwnedCoordinationMutationTargets(followLeaf, targets...); err != nil {
		return err
	}
	seen := make(map[string]struct{})
	for _, target := range targets {
		if target == "" {
			continue
		}
		abs, err := filepath.Abs(filepath.Clean(target))
		if err != nil {
			return err
		}
		spellings := []string{abs}
		parent, parentErr := resolvedPathForCreate(filepath.Dir(abs))
		if parentErr == nil {
			spellings = append(spellings, filepath.Join(parent, filepath.Base(abs)))
		} else if pathMayResolveIntoBranchVariantService(abs) || pathMayResolveIntoBranchVariantService(filepath.Dir(abs)) {
			return fmt.Errorf("resolve ordinary service-tree mutation target %q: %w", target, parentErr)
		}
		if followLeaf {
			physical, physicalErr := resolvedPathForCreate(abs)
			if physicalErr == nil {
				spellings = append(spellings, physical)
			} else if pathMayResolveIntoBranchVariantService(abs) || pathMayResolveIntoBranchVariantService(filepath.Dir(abs)) {
				return fmt.Errorf("resolve ordinary service-tree content target %q: %w", target, physicalErr)
			}
		}
		for _, spelling := range spellings {
			for current := filepath.Clean(spelling); ; current = filepath.Dir(current) {
				if isBranchVariantServiceComponent(filepath.Base(current)) {
					if _, ok := seen[current]; !ok {
						seen[current] = struct{}{}
						state, _, ownershipErr := resolveBranchVariantServiceOwnership(current)
						switch {
						case ownershipErr != nil && errors.Is(ownershipErr, errInvalidBranchVariantOwnership):
							// The candidate carries application ownership evidence
							// that could not be established. A write that physically
							// intersects it fails closed; the data is neither adopted
							// nor deleted.
							return fmt.Errorf("ordinary download path %q intersects a branch variant service tree whose ownership could not be established", target)
						case ownershipErr != nil:
							return fmt.Errorf("cannot verify branch variant storage ownership at %q: %w", current, ownershipErr)
						case state == branchVariantServiceOwned:
							return fmt.Errorf("ordinary download path %q conflicts with the app-owned EXL branch variant service tree", target)
						}
					}
				}
				parent := filepath.Dir(current)
				if parent == current {
					break
				}
			}
		}
	}
	return nil
}

// RejectBranchVariantTreeEffects preflights recursive deletion/cleanup roots.
// It refuses only descendant service roots whose logical owner record
// validates; a same-spelled unowned directory remains ordinary data.
func RejectBranchVariantTreeEffects(targets ...string) error {
	for _, target := range targets {
		if target == "" {
			continue
		}
		if err := rejectOwnedCoordinationTreeEffects(target); err != nil {
			return err
		}
		info, err := os.Lstat(target)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect recursive mutation scope %q: %w", target, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			if err := RejectOwnedBranchVariantEntryWrites(target); err != nil {
				return err
			}
			continue
		}
		if err := RejectOwnedBranchVariantDirectoryWrites(target); err != nil {
			return err
		}
		err = filepath.WalkDir(target, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if isBranchVariantServiceComponent(entry.Name()) {
				state, _, ownerErr := resolveBranchVariantServiceOwnership(path)
				switch {
				case ownerErr != nil && errors.Is(ownerErr, errInvalidBranchVariantOwnership):
					// Application ownership evidence exists but could not be
					// established. The candidate's data is protected: it is not
					// deleted by a recursive effect that only proved a name.
					return fmt.Errorf("recursive mutation scope %q contains a branch variant service root at %q whose ownership could not be established", target, path)
				case ownerErr != nil:
					return fmt.Errorf("verify recursive service-root ownership at %q: %w", path, ownerErr)
				case state == branchVariantServiceOwned:
					return fmt.Errorf("recursive mutation scope %q contains an owned branch variant service root at %q", target, path)
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// acquireBranchVariantEntryMutationLease pairs a no-follow leaf-entry check
// with the directory that owns the entry, closing the app-controlled
// check/replace window for symlink aliases.
func acquireBranchVariantEntryMutationLease(path string) (func(), error) {
	return acquireOrdinaryEntryMutationLease(path)
}

func acquireOrdinaryEntryMutationLease(path string) (func(), error) {
	return acquireOrdinaryMutationLease([]string{path}, nil)
}

func acquireOrdinaryContentMutationLease(paths ...string) (func(), error) {
	return acquireOrdinaryMutationLease(nil, paths)
}

func acquireOrdinaryFileMutationLease(entryPath string, contentPaths ...string) (func(), error) {
	return acquireOrdinaryMutationLease([]string{entryPath}, contentPaths)
}

func acquireOrdinaryMutationLease(entryPaths, contentPaths []string) (func(), error) {
	var scopes []branchVariantDirectoryScope
	for _, path := range entryPaths {
		if path == "" {
			continue
		}
		abs, err := filepath.Abs(filepath.Clean(path))
		if err != nil {
			return nil, err
		}
		parent, resolveErr := resolvedPathForCreate(filepath.Dir(abs))
		if resolveErr != nil {
			if pathMayResolveIntoBranchVariantService(abs) || pathMayResolveIntoBranchVariantService(filepath.Dir(abs)) {
				return nil, fmt.Errorf("resolve ordinary entry mutation parent %q: %w", path, resolveErr)
			}
			continue
		}
		physicalEntry := filepath.Join(parent, filepath.Base(abs))
		if pathHasBranchVariantServiceComponent(abs) || pathHasBranchVariantServiceComponent(physicalEntry) {
			scopes = append(scopes, branchVariantDirectoryScope{path: parent, mode: branchVariantLeaseExclusive})
		}
	}
	for _, path := range contentPaths {
		if path == "" {
			continue
		}
		abs, err := filepath.Abs(filepath.Clean(path))
		if err != nil {
			return nil, err
		}
		physical, resolveErr := resolvedPathForCreate(abs)
		if resolveErr != nil {
			if pathMayResolveIntoBranchVariantService(abs) || pathMayResolveIntoBranchVariantService(filepath.Dir(abs)) {
				return nil, fmt.Errorf("resolve ordinary content mutation target %q: %w", path, resolveErr)
			}
			continue
		}
		if !pathHasBranchVariantServiceComponent(abs) && !pathHasBranchVariantServiceComponent(physical) {
			continue
		}
		scopes = append(scopes, branchVariantDirectoryScope{path: filepath.Dir(physical), mode: branchVariantLeaseExclusive})
	}
	if len(scopes) == 0 {
		if err := RejectOwnedBranchVariantEntryWrites(entryPaths...); err != nil {
			return nil, err
		}
		return func() {}, RejectOwnedBranchVariantContentWrites(contentPaths...)
	}
	release, err := acquireBranchVariantDirectoryScopes(nil, scopes...)
	if err != nil {
		return nil, err
	}
	if err := RejectOwnedBranchVariantEntryWrites(entryPaths...); err != nil {
		release()
		return nil, err
	}
	if err := RejectOwnedBranchVariantContentWrites(contentPaths...); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

// Acquire takes exclusive physical directory leases only for target paths that
// pass through a service-root component. Ordinary downloads outside an EXL
// service tree remain fully concurrent.
func (locks *branchVariantWriteLockSet) Acquire(targets ...string) error {
	keys, err := branchVariantWriteLockKeys(targets)
	if err != nil {
		return err
	}
	if locks.held == nil {
		locks.held = make(map[string]struct{})
	}
	for _, key := range keys {
		if _, ok := locks.held[key]; ok {
			continue
		}
		release, err := acquireBranchVariantPathLock(key)
		if err != nil {
			return err
		}
		locks.held[key] = struct{}{}
		locks.releases = append(locks.releases, release)
	}
	return nil
}

func (locks *branchVariantWriteLockSet) Close() {
	if locks == nil {
		return
	}
	for i := len(locks.releases) - 1; i >= 0; i-- {
		locks.releases[i]()
	}
	locks.releases = nil
	locks.held = nil
}

func branchVariantWriteLockKeys(targets []string) ([]string, error) {
	keys := make(map[string]struct{})
	for _, target := range targets {
		if target == "" {
			continue
		}
		abs, err := filepath.Abs(filepath.Clean(target))
		if err != nil {
			return nil, err
		}
		spellings := []string{abs}
		if physical, err := resolvedPathForCreate(target); err == nil {
			spellings = append(spellings, physical)
		} else if pathHasBranchVariantServiceComponent(abs) {
			return nil, fmt.Errorf("resolve ordinary service-tree write target %q: %w", target, err)
		}
		for _, spelling := range spellings {
			for current := filepath.Clean(spelling); ; current = filepath.Dir(current) {
				if isBranchVariantServiceComponent(filepath.Base(current)) {
					candidate := current
					if info, statErr := os.Lstat(current); statErr == nil && !info.IsDir() {
						// The component spelling alone does not reserve an ordinary
						// file. Its parent is still the admission point if a branch
						// claim could create a directory at this path concurrently.
						candidate = filepath.Dir(current)
					}
					resolved, err := resolvedPathForCreate(candidate)
					if err != nil {
						return nil, err
					}
					key, err := branchVariantPathLockKey(resolved)
					if err != nil {
						return nil, err
					}
					keys[key] = struct{}{}
				}
				parent := filepath.Dir(current)
				if parent == current {
					break
				}
			}
		}
	}
	ordered := make([]string, 0, len(keys))
	for key := range keys {
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)
	return ordered, nil
}

func pathHasBranchVariantServiceComponent(path string) bool {
	for current := filepath.Clean(path); ; current = filepath.Dir(current) {
		if isBranchVariantServiceComponent(filepath.Base(current)) {
			return true
		}
		parent := filepath.Dir(current)
		if parent == current {
			return false
		}
	}
}

func pathMayResolveIntoBranchVariantService(path string) bool {
	if pathHasBranchVariantServiceComponent(path) {
		return true
	}
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return false
	}
	for current := abs; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			link, readErr := os.Readlink(current)
			if readErr != nil {
				return true
			}
			if !filepath.IsAbs(link) {
				link = filepath.Join(filepath.Dir(current), link)
			}
			if pathHasBranchVariantServiceComponent(link) {
				return true
			}
		}
		if parent := filepath.Dir(current); parent == current {
			return false
		}
	}
}

func acquireBranchVariantPathLock(path string) (func(), error) {
	return acquireBranchVariantCreationLease(path)
}

func branchVariantPathLockKey(path string) (string, error) {
	key, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	if runtimeGOOSWindows() {
		key = strings.ToLower(key)
	}
	return key, nil
}

// RejectBranchVariantServiceOverlap prevents HF cache internals from being
// configured inside the selected root's own branch service tree. Branch
// payloads intentionally live there; Hub blobs/snapshots and service payloads
// must remain distinct physical owners.
func RejectBranchVariantServiceOverlap(destinationRoot string, targets ...string) error {
	serviceRoot, err := BranchVariantServiceRoot(destinationRoot)
	if err != nil {
		return err
	}
	for _, target := range targets {
		if target == "" {
			continue
		}
		physical, err := resolvedPathForCreate(target)
		if err != nil {
			return fmt.Errorf("resolve branch variant storage path %q: %w", target, err)
		}
		if PathInside(serviceRoot, physical) || PathInside(physical, serviceRoot) {
			return fmt.Errorf("branch variant Hub path %q overlaps its app-owned service tree", target)
		}
	}
	return nil
}

// RemoveOwnedBranchVariantRepo removes only artifacts with matching app-owned
// control records for this exact repository and selected destination.
func RemoveOwnedBranchVariantRepo(destinationRoot string, repoType RepoType, repoID string) ([]string, error) {
	coordinationRelease, err := acquireCompleteRebuildCoordinationLeases(context.Background(), []string{destinationRoot}, true, nil)
	if err != nil {
		return nil, fmt.Errorf("lease branch variant destination for deletion: %w", err)
	}
	defer coordinationRelease()
	release, err := AcquireBranchVariantDirectoryLease(destinationRoot, true)
	if err != nil {
		return nil, fmt.Errorf("lease branch variant destination for deletion: %w", err)
	}
	defer release()
	return RemoveOwnedBranchVariantRepoUnderLease(destinationRoot, repoType, repoID)
}

// RemoveOwnedBranchVariantRepoUnderLease removes exact-repository service
// artifacts while the caller holds an exclusive lease for destinationRoot.
// It exists so an encompassing cache DELETE can hold one root lease across its
// recursive preflight, Hub/friendly removal, and owned-artifact cleanup.
func RemoveOwnedBranchVariantRepoUnderLease(destinationRoot string, repoType RepoType, repoID string) ([]string, error) {
	if err := RebindBranchVariantServiceRootUnderLease(destinationRoot); err != nil {
		return nil, err
	}
	serviceRoot, storeID, exists, err := readBranchVariantServiceRoot(destinationRoot)
	if err != nil || !exists {
		return nil, err
	}
	if _, err := branchVariantRepoPath(serviceRoot, "control", repoType, repoID); err != nil {
		return nil, err
	}
	controlRepo, err := branchVariantRepoPath(serviceRoot, "control", repoType, repoID)
	if err != nil {
		return nil, err
	}
	revisions, err := os.ReadDir(controlRepo)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	removed := make([]string, 0)
	var errs []error
	for _, revisionEntry := range revisions {
		if !revisionEntry.IsDir() {
			continue
		}
		revisionRoot := filepath.Join(controlRepo, revisionEntry.Name())
		commits, readErr := os.ReadDir(revisionRoot)
		if readErr != nil {
			errs = append(errs, readErr)
			continue
		}
		for _, commitEntry := range commits {
			if !commitEntry.IsDir() {
				continue
			}
			controlRoot := filepath.Join(revisionRoot, commitEntry.Name())
			artifact, ownerErr := readBranchVariantArtifactAtControlAnyRevision(serviceRoot, storeID, repoType, repoID, controlRoot)
			if ownerErr != nil {
				if !errors.Is(ownerErr, os.ErrNotExist) {
					errs = append(errs, fmt.Errorf("verify ownership for %s: %w", controlRoot, ownerErr))
				}
				continue // Unowned or malformed artifacts are preserved.
			}
			if artifact == nil {
				continue // Unowned, malformed, or foreign artifacts are preserved.
			}
			payloadRepoPath, pathErr := branchVariantRepoPath(serviceRoot, "payloads", repoType, repoID)
			if pathErr != nil {
				errs = append(errs, pathErr)
				continue
			}
			payloadDir, pathErr := SafeJoin(payloadRepoPath, filepath.Join(BranchVariantRevisionDirName(artifact.Revision), BranchVariantCommitDirName(artifact.Commit)))
			if pathErr != nil || !ownedPathComponentsSafe(serviceRoot, controlRoot) || !ownedPathComponentsSafe(serviceRoot, payloadDir) {
				errs = append(errs, fmt.Errorf("refuse unsafe owned branch variant path for %s/%s", repoID, artifact.Revision))
				continue
			}
			if err := os.RemoveAll(payloadDir); err != nil {
				errs = append(errs, err)
				continue
			}
			if err := os.RemoveAll(controlRoot); err != nil {
				errs = append(errs, err)
				continue
			}
			removed = append(removed, payloadDir, controlRoot)
		}
	}
	if len(errs) > 0 {
		return removed, fmt.Errorf("remove owned branch variant artifacts: %v", errs)
	}
	return removed, nil
}

func readBranchVariantArtifactAtControlAnyRevision(serviceRoot, storeID string, repoType RepoType, repoID, controlRoot string) (*BranchVariantArtifact, error) {
	if !ownedPathComponentsSafe(serviceRoot, controlRoot) {
		return nil, fmt.Errorf("branch variant control path contains an unsafe component")
	}
	data, err := readRegularJSON(filepath.Join(controlRoot, branchVariantArtifactOwnerFilename))
	if err != nil {
		return nil, err
	}
	var owner branchVariantArtifactOwner
	if err := json.Unmarshal(data, &owner); err != nil {
		return nil, err
	}
	if owner.Version != branchVariantStoreVersion || owner.StoreID != storeID || owner.RepoType != repoType ||
		!strings.EqualFold(owner.Repo, repoID) || owner.Revision == "" || owner.Commit == "" ||
		filepath.Base(filepath.Dir(controlRoot)) != BranchVariantRevisionDirName(owner.Revision) ||
		filepath.Base(controlRoot) != BranchVariantCommitDirName(owner.Commit) {
		return nil, fmt.Errorf("branch variant owner record does not match its location")
	}
	artifact, err := branchVariantArtifactPaths(serviceRoot, storeID, owner.RepoType, owner.Repo, owner.Revision, owner.Commit)
	if err != nil || !sameBranchVariantPath(artifact.ControlRoot, controlRoot) {
		return nil, fmt.Errorf("branch variant owner path is invalid")
	}
	return artifact, nil
}

func ensureBranchVariantServiceRoot(destinationRoot string) (string, string, error) {
	root, err := resolvedPathForCreate(destinationRoot)
	if err != nil {
		return "", "", fmt.Errorf("resolve branch variant destination: %w", err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", "", fmt.Errorf("create branch variant destination: %w", err)
	}
	root, err = resolvedPathForCreate(root)
	if err != nil {
		return "", "", fmt.Errorf("resolve created branch variant destination: %w", err)
	}
	serviceRoot := filepath.Join(root, BranchVariantServiceDirName)
	info, err := os.Lstat(serviceRoot)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(serviceRoot, 0o755); err != nil {
			return "", "", fmt.Errorf("claim branch variant service tree: %w", err)
		}
		storeID, err := newBranchVariantStoreID()
		if err != nil {
			_ = os.Remove(serviceRoot)
			return "", "", err
		}
		fileID, err := branchVariantFilesystemIdentity(serviceRoot)
		if err != nil {
			_ = os.Remove(serviceRoot)
			return "", "", fmt.Errorf("capture branch variant service identity: %w", err)
		}
		owner := branchVariantStoreOwner{Version: branchVariantStoreVersion, StoreID: storeID, Purpose: "exl-branch-variants", FileID: fileID}
		if err := writeExclusiveJSON(filepath.Join(serviceRoot, branchVariantStoreOwnerFilename), owner); err != nil {
			_ = os.Remove(filepath.Join(serviceRoot, branchVariantStoreOwnerFilename))
			_ = os.Remove(serviceRoot)
			return "", "", fmt.Errorf("record branch variant service ownership: %w", err)
		}
		return serviceRoot, storeID, nil
	}
	if err != nil {
		return "", "", fmt.Errorf("inspect branch variant service tree: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", "", fmt.Errorf("refuse unowned branch variant service path %q", serviceRoot)
	}
	state, owner, ownershipErr := resolveBranchVariantServiceOwnership(serviceRoot)
	if ownershipErr != nil {
		return "", "", fmt.Errorf("refuse unowned branch variant service tree %q: %w", serviceRoot, ownershipErr)
	}
	if state != branchVariantServiceOwned {
		return "", "", fmt.Errorf("refuse unowned branch variant service tree %q: %w", serviceRoot, errNoBranchVariantOwnershipEvidence)
	}
	return serviceRoot, owner.StoreID, nil
}

func readBranchVariantServiceRoot(destinationRoot string) (string, string, bool, error) {
	serviceRoot, err := BranchVariantServiceRoot(destinationRoot)
	if err != nil {
		return "", "", false, err
	}
	return readBranchVariantServiceRootAt(serviceRoot)
}

// readBranchVariantServiceRootAt resolves one concrete service-root candidate
// through the shared ownership normalization point.
func readBranchVariantServiceRootAt(serviceRoot string) (string, string, bool, error) {
	state, owner, err := resolveBranchVariantServiceOwnership(serviceRoot)
	if err != nil {
		return "", "", false, err
	}
	switch state {
	case branchVariantServiceOwned:
		return serviceRoot, owner.StoreID, true, nil
	case branchVariantServiceUnverifiable:
		return "", "", false, fmt.Errorf("%w: %s", errInvalidBranchVariantOwnership, serviceRoot)
	default:
		return serviceRoot, "", false, nil
	}
}

// resolveBranchVariantServiceOwnership is the single normalization point for
// service-root ownership. It separates the states that a plain owned/not-owned
// answer cannot express:
//
//   - ordinary: the candidate shares the service component name but carries no
//     application ownership evidence. It stays ordinary data, so unrelated
//     ordinary work in its root is unaffected and a claim refuses it without
//     adopting or changing it.
//   - unverifiable: the candidate carries application-shaped ownership evidence
//     that could not be established. Writes that physically intersect it fail
//     closed, and the data is neither adopted nor deleted.
//   - owned: the candidate's durable logical ownership evidence validated.
//
// The returned error describes why application evidence could not be
// established. It is non-nil only for an unverifiable candidate; callers apply
// their own policy to the resolved state.
func resolveBranchVariantServiceOwnership(serviceRoot string) (branchVariantServiceState, *branchVariantStoreOwner, error) {
	info, err := os.Lstat(serviceRoot)
	if errors.Is(err, os.ErrNotExist) {
		return branchVariantServiceOrdinary, nil, nil
	}
	if err != nil {
		return branchVariantServiceUnverifiable, nil, fmt.Errorf("inspect branch variant service path %q: %w", serviceRoot, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		// A plain file or symlink that merely shares the component name is not
		// a service tree.
		return branchVariantServiceOrdinary, nil, nil
	}
	owner, err := readBranchVariantStoreOwner(serviceRoot)
	switch {
	case err == nil:
		return branchVariantServiceOwned, owner, nil
	case errors.Is(err, errNoBranchVariantOwnershipEvidence):
		return branchVariantServiceOrdinary, nil, nil
	default:
		return branchVariantServiceUnverifiable, nil, err
	}
}

// branchVariantOwnerRecordShaped reports whether a decoded record uses the
// application's ownership record fields. The application's own record names
// are what make a candidate application evidence; an unrelated file that
// merely shares the record's path stays ordinary data.
func branchVariantOwnerRecordShaped(record map[string]json.RawMessage) bool {
	for _, field := range []string{"version", "storeId", "purpose", "fileId"} {
		if _, ok := record[field]; ok {
			return true
		}
	}
	return false
}

// readBranchVariantStoreOwner reads the durable ownership record of a service
// root. It returns errNoBranchVariantOwnershipEvidence when the candidate
// carries no application ownership record, and errInvalidBranchVariantOwnership
// when a record exists but the ownership it claims could not be established.
func readBranchVariantStoreOwner(serviceRoot string) (*branchVariantStoreOwner, error) {
	recordPath := filepath.Join(serviceRoot, branchVariantStoreOwnerFilename)
	info, err := os.Lstat(recordPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, errNoBranchVariantOwnershipEvidence
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: ownership record %q is not a regular file", errInvalidBranchVariantOwnership, recordPath)
	}
	data, err := os.ReadFile(recordPath)
	if err != nil {
		return nil, err
	}
	var record map[string]json.RawMessage
	if err := json.Unmarshal(data, &record); err != nil || record == nil || !branchVariantOwnerRecordShaped(record) {
		// The candidate holds a file at the application's record path whose
		// content is not shaped like an application ownership record. It
		// carries no application ownership evidence and stays ordinary data.
		return nil, errNoBranchVariantOwnershipEvidence
	}
	var owner branchVariantStoreOwner
	if err := json.Unmarshal(data, &owner); err != nil {
		return nil, fmt.Errorf("%w: %v", errInvalidBranchVariantOwnership, err)
	}
	if owner.Version != branchVariantStoreVersion || owner.Purpose != "exl-branch-variants" || owner.StoreID == "" {
		return nil, fmt.Errorf("%w: invalid service ownership record", errInvalidBranchVariantOwnership)
	}
	fileID, err := branchVariantFilesystemIdentity(serviceRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve service directory identity: %w", err)
	}
	if owner.FileID == "" {
		return nil, fmt.Errorf("%w: service ownership record has no creation identity", errInvalidBranchVariantOwnership)
	}
	if owner.FileID != fileID {
		// The recorded physical binding no longer matches this directory, so
		// the store may be a copy. Copy recognition is per artifact: a
		// complete, self-consistent artifact keeps its identity regardless of
		// its siblings, and an artifact that is not complete means that
		// artifact is incomplete rather than that the store is foreign.
		if err := recognizeCopiedBranchVariantStore(serviceRoot, &owner); err != nil {
			return nil, fmt.Errorf("%w: copied service root is not structurally valid: %v", errInvalidBranchVariantOwnership, err)
		}
	}
	return &owner, nil
}

func branchVariantArtifactPaths(serviceRoot, storeID string, repoType RepoType, repoID, revision, commit string) (*BranchVariantArtifact, error) {
	if revision == "" || commit == "" {
		return nil, fmt.Errorf("branch variant requires a revision and resolved commit")
	}
	controlRepo, err := branchVariantRepoPath(serviceRoot, "control", repoType, repoID)
	if err != nil {
		return nil, err
	}
	payloadRepo, err := branchVariantRepoPath(serviceRoot, "payloads", repoType, repoID)
	if err != nil {
		return nil, err
	}
	controlRevision, err := SafeJoin(controlRepo, BranchVariantRevisionDirName(revision))
	if err != nil {
		return nil, err
	}
	controlRoot, err := SafeJoin(controlRevision, BranchVariantCommitDirName(commit))
	if err != nil {
		return nil, err
	}
	payloadRevision, err := SafeJoin(payloadRepo, BranchVariantRevisionDirName(revision))
	if err != nil {
		return nil, err
	}
	payloadArtifact, err := SafeJoin(payloadRevision, BranchVariantCommitDirName(commit))
	if err != nil {
		return nil, err
	}
	payloadRoot, err := SafeJoin(payloadArtifact, "files")
	if err != nil {
		return nil, err
	}
	return &BranchVariantArtifact{
		ServiceRoot: serviceRoot, PayloadRoot: payloadRoot, ControlRoot: controlRoot,
		ManifestPath: filepath.Join(controlRoot, ManifestFilename), RepoType: repoType,
		Repo: repoID, Revision: revision, Commit: commit, storeID: storeID,
	}, nil
}

func branchVariantRepoPath(root, area string, repoType RepoType, repoID string) (string, error) {
	if repoType != RepoTypeModel && repoType != RepoTypeDataset {
		return "", fmt.Errorf("invalid branch variant repository type %q", repoType)
	}
	if !IsValidModelName(repoID) {
		return "", fmt.Errorf("invalid branch variant repository ID %q", repoID)
	}
	parts := strings.SplitN(repoID, "/", 2)
	repoPath, err := SafeJoin(root, filepath.Join(area, string(repoType)+"s", parts[0], parts[1]))
	if err != nil {
		return "", err
	}
	return repoPath, nil
}

func verifyBranchVariantArtifactOwner(artifact *BranchVariantArtifact) error {
	if artifact == nil {
		return fmt.Errorf("missing branch variant artifact")
	}
	if !ownedPathComponentsSafe(artifact.ServiceRoot, artifact.ControlRoot) {
		return fmt.Errorf("branch variant control path contains an unsafe component")
	}
	data, err := readRegularJSON(filepath.Join(artifact.ControlRoot, branchVariantArtifactOwnerFilename))
	if err != nil {
		return err
	}
	var owner branchVariantArtifactOwner
	if err := json.Unmarshal(data, &owner); err != nil {
		return err
	}
	if owner.Version != branchVariantStoreVersion || owner.StoreID != artifact.storeID || owner.RepoType != artifact.RepoType ||
		!strings.EqualFold(owner.Repo, artifact.Repo) || owner.Revision != artifact.Revision || owner.Commit != artifact.Commit {
		return fmt.Errorf("branch variant artifact ownership does not match the selected artifact")
	}
	return nil
}

func writeExclusiveJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return nil
}

func readRegularJSON(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: ownership record %q is not a regular file", errInvalidBranchVariantOwnership, path)
	}
	return os.ReadFile(path)
}

func makeOwnedDirectory(root, target string) error {
	rootAbs, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return err
	}
	targetAbs, err := filepath.Abs(filepath.Clean(target))
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(rootAbs, targetAbs)
	if err != nil || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path %q is outside owned root %q", target, root)
	}
	if err := ensureDirectoryComponent(rootAbs); err != nil {
		return err
	}
	current := rootAbs
	if rel == "." {
		return nil
	}
	for _, component := range strings.Split(rel, string(filepath.Separator)) {
		if component == "" || component == "." || component == ".." {
			return fmt.Errorf("invalid owned directory component %q", component)
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(current, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			info, err = os.Lstat(current)
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("owned directory component %q is not a plain directory", current)
		}
	}
	return nil
}

func ensureDirectoryComponent(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("owned root %q is not a plain directory", path)
	}
	return nil
}

func ownedPathComponentsSafe(root, target string) bool {
	rootAbs, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return false
	}
	targetAbs, err := filepath.Abs(filepath.Clean(target))
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(rootAbs, targetAbs)
	if err != nil || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	current := rootAbs
	if err := ensureDirectoryComponent(current); err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	for _, component := range strings.Split(rel, string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return true
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return false
		}
		if current != targetAbs && !info.IsDir() {
			return false
		}
	}
	return true
}

func resolvedPathForCreate(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("empty path")
	}
	absolute, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	var missing []string
	for current := absolute; ; current = filepath.Dir(current) {
		if _, err := os.Lstat(current); err == nil {
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil {
				return "", err
			}
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return filepath.Clean(resolved), nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("could not resolve path %q", absolute)
		}
		missing = append(missing, filepath.Base(current))
	}
}

func newBranchVariantStoreID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", fmt.Errorf("create branch variant service identity: %w", err)
	}
	return hex.EncodeToString(id[:]), nil
}

func sameBranchVariantPath(a, b string) bool {
	a, errA := filepath.Abs(filepath.Clean(a))
	b, errB := filepath.Abs(filepath.Clean(b))
	if errA != nil || errB != nil {
		return false
	}
	if runtimeGOOSWindows() {
		a, b = strings.ToLower(a), strings.ToLower(b)
	}
	return a == b
}

func runtimeGOOSWindows() bool { return filepath.Separator == '\\' }

func isBranchVariantServiceComponent(component string) bool {
	if runtimeGOOSWindows() {
		return strings.EqualFold(component, BranchVariantServiceDirName)
	}
	return component == BranchVariantServiceDirName
}

func branchVariantSlug(revision string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(revision) {
		if b.Len() >= 32 {
			break
		}
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash && b.Len() > 0 {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}
