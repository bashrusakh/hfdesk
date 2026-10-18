// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"fmt"
	"net/http"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/bashrusakh/hfdesk/internal/datasetselection"
)

// unsafeRepoPath reports whether a relative path returned by the repo tree API
// would escape the repository root if joined onto a local directory, or is not
// in canonical form. The path list is remote-controlled (and the endpoint is
// operator-configurable via --endpoint, so a malicious or MITM'd mirror can
// return anything), and the path flows unchecked into file writes
// (filepath.Join(base, rel)) and symlink creation. Anything absolute,
// containing a "\\" (Windows separator / drive escape), or normalising to ""
// / "." / ".." / a "../" prefix is rejected to prevent arbitrary-file-write.
// Beyond escape safety, the accepted domain is canonical spellings only: a
// path whose raw spelling differs from its path.Clean form ("." segment,
// empty "//" segment, in-root ".." segment, or trailing "/") would be silently
// rewritten by the downstream SafeJoin/Join into a different physical
// spelling, so two raw tree paths could plan duplicate writes of one cache
// entry. Callers fail the plan loudly on the dirty spelling instead.
func unsafeRepoPath(rel string) bool {
	if rel == "" {
		return true
	}
	if strings.ContainsRune(rel, '\\') || strings.HasPrefix(rel, "/") || filepath.IsAbs(rel) {
		return true
	}
	cleaned := path.Clean(rel)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return true
	}
	// Canonical-form check: rel must already be its own cleaned spelling.
	return cleaned != rel
}

// unsafeBlobName reports whether a SHA256 value taken from the remote tree
// API is not acceptable as a canonical SHA-256 hash for the plan. The SHA is
// remote-controlled (see the unsafeRepoPath rationale above) and is used
// verbatim both as a filesystem name (blobs/<sha> storage and the tmp-<sha>
// staging file) and as the expected digest in verification, so it must be
// either empty (a file without a hash) or exactly 64 hex characters -- the
// canonical SHA-256 form. The separator / absolute / traversal checks below
// keep the path-safety property stated on its own guard; the length+hex check
// subsumes them but is deliberately not the only check. Hex is accepted
// case-insensitively; scanRepo lowercases accepted values before they enter
// the plan. Everything else -- including 40-hex git-OID-shaped values and
// "sha256:"-prefixed LFS pointer oids -- is rejected: neither can ever match
// a computed 64-hex digest at verify time, so accepting them would only
// defer a guaranteed verification failure while writing non-canonical cache
// entries. Callers must still check for the empty shape before using the
// value as a path.
func unsafeBlobName(sha string) bool {
	if sha == "" {
		return false
	}
	if strings.ContainsAny(sha, `/\`) || filepath.IsAbs(sha) || filepath.VolumeName(sha) != "" {
		return true
	}
	cleaned := path.Clean(sha)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return true
	}
	return !canonicalSHA256(sha)
}

// canonicalSHA256 reports whether sha is exactly 64 hexadecimal characters,
// the canonical SHA-256 digest representation, accepting upper or lower case.
func canonicalSHA256(sha string) bool {
	if len(sha) != 64 {
		return false
	}
	for i := 0; i < len(sha); i++ {
		c := sha[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// PathInside reports whether target resolves to base itself or to a location
// nested within base. Both paths are cleaned first, so "../" segments are
// resolved before the comparison. This is the single home for the containment
// guard that the cache, mirror, and downloader call sites previously
// open-coded as inline strings.HasPrefix checks.
//
// The separator-strict prefix below keeps a sibling that shares the base's
// name (e.g. /tmp/models2 versus base /tmp/models) from matching: appending
// a separator to both sides forces the comparison onto a component boundary.
// A filesystem/volume root — "/" on Unix, a drive root such as "C:\" or a
// UNC share root on Windows (filepath.VolumeName names its volume; Clean
// keeps the root's trailing separator) — already ends in a separator after
// filepath.Clean, so it must not receive a second one: base+sep would be
// "//" (or "\\"), a prefix no path can ever match, which rejected every
// destination legitimately inside the root as an escape. For a root the
// cleaned root itself is the strict prefix: a root has no shallower sibling,
// so any cleaned target carrying that prefix is inside it (relative targets
// still fail the prefix, as before). filepath.Clean never leaves a trailing
// separator on a non-root path, so non-root bases take the unchanged
// separator-strict branch and keep byte-identical acceptance.
func PathInside(base, target string) bool {
	base = filepath.Clean(base)
	target = filepath.Clean(target)
	if target == base {
		return true
	}
	sep := string(filepath.Separator)
	if strings.HasSuffix(base, sep) {
		return strings.HasPrefix(target, base)
	}
	return strings.HasPrefix(target+sep, base+sep)
}

// SafeJoin joins rel onto base and verifies the result stays within base,
// returning an error if rel would escape (via "../", an absolute component,
// etc.). Use this instead of a bare filepath.Join wherever rel is remote- or
// user-influenced.
func SafeJoin(base, rel string) (string, error) {
	// Reject absolute or volume-qualified rel: filepath.Join would otherwise
	// silently rewrite them under base (or, on Windows, splice in a drive
	// letter), hiding what is really an absolute-path input.
	if filepath.IsAbs(rel) || filepath.VolumeName(rel) != "" {
		return "", fmt.Errorf("path %q must be relative", rel)
	}
	// Reject non-local values ("", "..", escaping segments, and the platform's
	// reserved names) before joining. filepath.IsLocal is the standard-library
	// primitive CodeQL models as a go/path-injection sanitizer barrier, so this
	// check makes the containment below legible to code scanning; the
	// PathInside check remains the containment proof.
	if !filepath.IsLocal(rel) {
		return "", fmt.Errorf("path %q must be local", rel)
	}
	dst := filepath.Clean(filepath.Join(base, rel))
	if !PathInside(base, dst) {
		return "", fmt.Errorf("path %q escapes %q", rel, base)
	}
	// Restate the containment PathInside just proved against the cleaned base
	// with strings.HasPrefix, the primitive code scanning models as a
	// sanitizer guard: this check dominates the return below, so taint
	// carried in `base` itself cannot flow out of SafeJoin unnoticed.
	// Acceptance is identical to PathInside's (its separator-strict prefix
	// implies this plain prefix), so no input that passed before is rejected.
	if !strings.HasPrefix(dst, filepath.Clean(base)) {
		return "", fmt.Errorf("path %q escapes %q", rel, base)
	}
	return dst, nil
}

// DestinationBase joins a user-selected repository folder to its configured
// root and proves the resulting path remains under that root. Unlike SafeJoin,
// repository folder names may contain normalizing segments such as a/../b;
// only absolute paths and paths that actually escape the root are rejected.
func DestinationBase(root, folder string) (string, error) {
	// Keep the platform-aware component rules used by the download path: these
	// reject reserved names/characters in the repo component on platforms where
	// they are not valid local paths, while still allowing in-root a/../b.
	if !filepath.IsLocal(folder) {
		return "", fmt.Errorf("destination folder %q must be local", folder)
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve output root %q: %w", root, err)
	}
	base := filepath.Clean(filepath.Join(rootAbs, folder))
	if !PathInside(rootAbs, base) {
		return "", fmt.Errorf("destination %q escapes output root %q", base, rootAbs)
	}
	// Preserve the modeled containment barrier used by the previous download
	// destination check, after the component-boundary proof above.
	if !strings.HasPrefix(base, rootAbs) {
		return "", fmt.Errorf("destination %q escapes output root %q", base, rootAbs)
	}
	return base, nil
}

// PlanItem represents a single file in the download plan.
type PlanItem struct {
	RelativePath string `json:"path"`
	URL          string `json:"url"`
	LFS          bool   `json:"lfs"`
	SHA256       string `json:"sha256,omitempty"`
	Size         int64  `json:"size"`
	AcceptRanges bool   `json:"acceptRanges"`
	// Subdir holds the matched filter attribution (if any).
	Subdir string `json:"subdir,omitempty"`
	// DestinationSubdir is the relative prefix applied to this file when the
	// request enables AppendFilterSubdir. It is empty when the file remains at
	// its repository-relative path. This is distinct from Subdir attribution.
	DestinationSubdir string `json:"destinationSubdir,omitempty"`
}

// Plan contains the list of files to download.
type Plan struct {
	Items  []PlanItem `json:"items"`
	Commit string     `json:"commit,omitempty"` // Commit hash for this plan (for HF cache snapshots)
}

// PlanRepo builds the file list without downloading.
func PlanRepo(ctx context.Context, job Job, cfg Settings) (*Plan, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := validate(job, cfg); err != nil {
		return nil, err
	}
	if job.Revision == "" {
		job.Revision = "main"
	}
	httpc, err := BuildHTTPClient(cfg.Proxy)
	if err != nil {
		return nil, fmt.Errorf("build planning HTTP client: %w", err)
	}
	return scanRepo(ctx, httpc, cfg.Token, job, cfg)
}

// scanRepo walks the repo tree and builds a download plan.
func scanRepo(ctx context.Context, httpc *http.Client, token string, job Job, cfg Settings) (*Plan, error) {
	var items []PlanItem
	seen := make(map[string]struct{})      // ensure each relative path appears once in the plan
	seenLower := make(map[string]struct{}) // ensure each path appears at most once modulo case
	// directFilterMatch is the selection proof for non-empty Filters: at least
	// one non-excluded file in the tree must directly match a requested filter.
	// It is tracked independently of plan inclusion so companion/metadata
	// files that enter the plan below can never make a filtered request
	// succeed on their own.
	directFilterMatch := false
	datasetMembershipMatch := false

	// Fetch actual commit SHA for the revision
	repoInfo, err := fetchRepoInfo(ctx, httpc, token, cfg.Endpoint, job)
	if err != nil {
		// Fall back to revision name if API call fails (e.g., some mirrors)
		repoInfo = &RepoInfo{SHA: job.Revision}
	}
	commitSHA := repoInfo.SHA
	if commitSHA == "" {
		commitSHA = job.Revision // fallback
	}

	// Collect every file node first. We need the full list before building the
	// plan so we can detect a GGUF-only download and skip companion files that
	// a self-contained GGUF doesn't need.
	var fileNodes []hfNode
	err = walkTree(ctx, httpc, token, cfg.Endpoint, job, "", func(n hfNode) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if n.Type == "file" || n.Type == "blob" {
			// Reject path-traversal entries before they reach any filesystem
			// operation downstream. Fail the whole plan rather than silently
			// skipping so a tampered tree is loud, not partial. A non-canonical
			// but in-root spelling (".", "//", an in-root "..", or a trailing
			// "/") is reported as such, naming the canonical form the caller
			// would have rewritten it to: SafeJoin cleans before joining, so
			// two spellings of one remote file would both be planned and the
			// two transfers would race to write a single cache entry. The
			// plan's accepted domain is exactly the canonical spelling.
			if unsafeRepoPath(n.Path) {
				if cleaned := path.Clean(n.Path); cleaned != n.Path && cleaned != "." && cleaned != ".." && !strings.HasPrefix(cleaned, "../") {
					return fmt.Errorf("refusing non-canonical path from repo tree: %q: want %q", n.Path, cleaned)
				}
				return fmt.Errorf("refusing unsafe path from repo tree: %q", n.Path)
			}
			fileNodes = append(fileNodes, n)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	baseNames := make([]string, 0, len(fileNodes))
	datasetFiles := make([]datasetselection.File, len(fileNodes))
	for _, n := range fileNodes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		baseNames = append(baseNames, strings.ToLower(n.Path))
		datasetFiles[len(baseNames)-1] = datasetselection.File{Path: n.Path}
	}
	datasetMembership := datasetselection.Recognize(datasetFiles)
	// Evidence is input-indexed; project definite data roles once instead of
	// rescanning the complete evidence slice for every planned file.
	datasetDataRoles := make([]bool, len(fileNodes))
	for _, evidence := range datasetMembership.Evidence {
		if evidence.InputIndex >= 0 && evidence.InputIndex < len(datasetDataRoles) && evidence.DefiniteRole == datasetselection.EvidenceRoleData {
			datasetDataRoles[evidence.InputIndex] = true
		}
	}
	ggufMode := isGGUFFilterDownloadWithExcludes(baseNames, job.Filters, job.Excludes, job.ExactMatch)
	var datasetEvaluation datasetDecisionEvaluation
	// A recognized dataset group remains the governing selection when its files
	// coexist with GGUFs that happen to match the same filter. GGUF mode is the
	// fallback only when no dataset group is recognized (e.g. a dataset repo
	// containing a GGUF-only quant selection).
	datasetGroupRequest := job.IsDataset && len(job.Filters) > 0
	var recognizedDatasetGroup bool
	if datasetGroupRequest {
		membership := datasetMembership
		// The group evaluator is reached only when a filter is recognized as a
		// dataset label; do not let unrelated GGUF matches disable its membership
		// and attribution model.
		context := compileDatasetDecisionContextWithContext(ctx, fileNodes, membership, job.Filters, job.Excludes, job.ExactMatch, job.IsDataset, false)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		datasetGroupRequest = false
		for _, filter := range job.Filters {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			normalized := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(filter), "\\", "/"))
			if containsString(context.RecognizedLabels, normalized) || !job.ExactMatch && datasetFilterMatchesMembership(context, filter) {
				datasetGroupRequest = true
				break
			}
		}
		recognizedDatasetGroup = datasetGroupRequest
		// Even ordinary filename/path filters use the shared dataset evidence
		// evaluator so recognized metadata with data/companion alternatives can
		// be retained or refused based on its actual final emitted-set outcomes.
		if datasetGroupRequest || job.IsDataset && len(job.Filters) > 0 {
			context.RetainMetadata = true
			// Ordinary non-GGUF dataset selectors also use the shared evaluator,
			// but retain their distinct path-filter semantics and ambiguity scope.
			datasetGroupRequest = true
			// Whole-inventory refusal proofs run after a concrete selection is
			// established and only for paths whose uncertainty can trigger refusal.
			world := datasetDecisionWorld{DataLabels: make(map[int]string), Companion: make(map[int]bool), SharedToken: make(map[uint]bool)}
			for bit := range collisionTokenBits(context) {
				world.SharedToken[bit] = false
			}
			for i, input := range context.Inputs {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				if len(input.Alternatives) == 0 {
					return nil, fmt.Errorf("dataset selection has no membership evidence for %q", input.Path)
				}
				alternative := input.Alternatives[0]
				if alternative.Role == datasetselection.EvidenceRoleCompanion {
					world.Companion[i] = true
				} else {
					world.DataLabels[i] = alternative.Label
				}
			}
			var evalErr error
			datasetEvaluation, evalErr = evaluateDatasetDecisionWorld(context, world)
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if evalErr != nil {
				return nil, fmt.Errorf("evaluate dataset selection: %w", evalErr)
			}
			if recognizedDatasetGroup {
				for i, record := range datasetEvaluation.Records {
					if record.Selected && world.DataLabels[i] != "" && !world.Companion[i] {
						datasetMembershipMatch = true
						break
					}
				}
				if !datasetMembershipMatch {
					witness := queryDatasetGroupSelection(context)
					if witness.Err != nil {
						return nil, witness.Err
					}
					if witness.Possible {
						datasetMembershipMatch = true
						datasetEvaluation, evalErr = evaluateDatasetDecisionWorld(context, witness.World)
						if evalErr != nil {
							return nil, fmt.Errorf("evaluate dataset selection witness: %w", evalErr)
						}
						world = witness.World
					}
				}
			}
			if len(job.Filters) > 0 && !directFilterMatch && !datasetMembershipMatch {
				return nil, fmt.Errorf("%w: filters=%q exact=%t", ErrNoFilterMatch, job.Filters, job.ExactMatch)
			}
			groupSelectionAmbiguous := false
			for _, filter := range job.Filters {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				normalized := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(filter), "\\", "/"))
				if containsString(context.RecognizedLabels, normalized) || !job.ExactMatch && datasetFilterMatchesMembership(context, filter) {
					groupSelectionAmbiguous = true
					break
				}
			}
			for i, n := range fileNodes {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				if !groupSelectionAmbiguous {
					continue
				}
				can, cannot := datasetDecisionRefusal(context, i)
				if can.Err != nil {
					return nil, can.Err
				}
				if cannot.Err != nil {
					return nil, cannot.Err
				}
				if can.Possible && cannot.Possible {
					return nil, fmt.Errorf("dataset filter selection has ambiguous emitted set for %q: %w", n.Path, ErrAmbiguousSelection)
				}
			}
		}
	}
	for nodeIndex, n := range fileNodes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rel := n.Path

		// Deduplicate by relative path
		if _, ok := seen[rel]; ok {
			continue
		}
		seen[rel] = struct{}{}

		name := filepath.Base(rel)
		nameLower := strings.ToLower(name)
		relLower := strings.ToLower(rel)
		isLFS := n.LFS != nil
		var datasetRecord datasetDecisionRecord
		if datasetGroupRequest {
			datasetRecord = datasetEvaluation.Records[nodeIndex]
			if !datasetRecord.Selected {
				_, caseWinner := seenLower[relLower]
				caseWinner = caseWinner && datasetRecord.Eligible
				if !caseWinner {
					continue
				}
			}
			// Ordinary dataset selectors can share the evaluator with dataset
			// groups, but its companion-role result must not broaden a GGUF-only
			// quant selection to unrelated model weights. Group selections retain
			// their established role-aware membership; the filtered GGUF route
			// retains recognized model and dataset service metadata.
			directDatasetMatch := false
			for _, filter := range job.Filters {
				if filterMatchesPath(rel, filter, job.ExactMatch) {
					directDatasetMatch = true
					break
				}
			}
			if !recognizedDatasetGroup && ggufMode && !strings.HasSuffix(relLower, ".gguf") &&
				!directDatasetMatch && !isRecognizedDatasetMetadataPath(rel) {
				continue
			}
		}
		// Check excludes first - if file matches any exclude pattern, skip it
		// Credits: Exclude feature suggested by jeroenkroese (#41)
		excluded := false
		for _, ex := range job.Excludes {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			exLower := strings.ToLower(ex)
			if strings.Contains(nameLower, exLower) || strings.Contains(relLower, exLower) {
				excluded = true
				break
			}
		}
		if excluded {
			continue
		}

		// Direct selection match: proof that a requested filter actually
		// matched this file. Computed for every non-excluded file before any
		// GGUF/LFS inclusion decision and regardless of whether the file later
		// enters the plan, so an excluded file never confirms selection while
		// a valid direct match of e.g. a plain non-LFS config.json is not
		// missed. Prefer the longest matching filter (same rule as the
		// inclusion branches below, which reuse this value). Filters match
		// against the complete repository-relative path.
		directMatchedFilter := ""
		for _, f := range job.Filters {
			if filterMatchesPath(rel, f, job.ExactMatch) {
				if len(f) > len(directMatchedFilter) {
					directMatchedFilter = f
				}
			}
		}
		if directMatchedFilter != "" {
			directFilterMatch = true
		}

		// matchedFilter feeds PlanItem.Subdir and is only set by the inclusion
		// branches below; it is not the selection proof (see directMatchedFilter).
		matchedFilter := ""
		matchedFilterLength := 0
		if datasetGroupRequest {
			matchedFilter = datasetRecord.Subdir
		}
		// This shared rule narrows only GGUF entries. In GGUF mode, retain the
		// original filter behavior for non-GGUF files, including explicitly
		// requested companions.
		if !datasetGroupRequest && ggufMode && strings.HasSuffix(nameLower, ".gguf") && len(job.Filters) > 0 &&
			!GGUFPathSelected(rel, job.Filters, job.Excludes, job.ExactMatch) {
			continue
		}
		if datasetGroupRequest {
			// Inclusion and attribution come from one concrete dataset world.
		} else if ggufMode {
			// GGUF-only selection is matched-only: keep selected GGUF/mmproj and
			// directly matched files, but do not infer metadata companions.
			for _, f := range job.Filters {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				if filterMatchesPath(rel, f, job.ExactMatch) {
					if len(f) > matchedFilterLength {
						matchedFilter = f
						matchedFilterLength = len(f)
					}
				}
			}
			matchedFilter = directMatchedFilter
			if matchedFilter == "" {
				metadata := job.IsDataset && isRecognizedDatasetMetadataPath(rel)
				if isLFS || !metadata || datasetDataRoles[nodeIndex] {
					continue
				}
			}
		} else if len(job.Filters) > 0 {
			for _, f := range job.Filters {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				if filterMatchesPath(rel, f, job.ExactMatch) {
					if len(f) > matchedFilterLength {
						matchedFilter = f
						matchedFilterLength = len(f)
					}
				}
			}
			matchedFilter = directMatchedFilter
			// Filters select payloads by their full repo-relative path. An unmatched
			// LFS entry is payload for planning purposes, regardless of extension:
			// mirrors may store configs and other metadata as LFS pointers too.
			if matchedFilter == "" {
				if isLFS {
					continue
				}
				// Ordinary model selection follows the established upstream
				// companion policy: retain every non-LFS file, regardless of
				// basename or directory. Preserve dataset service-metadata rules.
				if job.IsDataset {
					if !isRecognizedDatasetMetadataPath(rel) {
						continue
					}
				}
			}
		}
		// Ordinary non-LFS model files keep their repo-relative destination.
		// matchedFilter is used by AppendFilterSubdir to alter that destination;
		// only dataset membership attribution and the established GGUF/LFS
		// branches provide a destination subdirectory.
		if !job.IsDataset && !isLFS && !strings.HasSuffix(nameLower, ".gguf") {
			matchedFilter = ""
		}
		// Normalize attribution once after the inclusion branch has chosen its
		// winning filter. Dataset feasibility retains the original selector for
		// matching/precedence, but its public Subdir must not expose the identity
		// marker any more than the direct model-selection branches do.
		matchedFilter = filterAttribution(matchedFilter, job.ExactMatch)

		// Build URL and file size
		var urlStr string
		if isLFS {
			urlStr = lfsURL(cfg.Endpoint, job, rel)
		} else {
			urlStr = rawURL(cfg.Endpoint, job, rel)
		}
		// For LFS files, ALWAYS use LFS.Size (n.Size is the pointer file size, not actual)
		var size int64
		if n.LFS != nil && n.LFS.Size > 0 {
			size = n.LFS.Size
		} else {
			size = n.Size
		}

		// Assume LFS files support range requests (HuggingFace always does)
		// Don't block with HEAD requests during planning - too slow for large repos
		acceptRanges := isLFS

		sha := n.Sha256
		shaField := "sha256"
		if sha == "" && n.LFS != nil {
			// LFS files have SHA256 in either Sha256 field or Oid field (LFS spec uses oid)
			sha = n.LFS.Sha256
			shaField = "lfs.sha256"
			if sha == "" {
				sha = n.LFS.Oid
				shaField = "lfs.oid"
			}
		}

		// The SHA is remote-controlled too, and unlike n.Path it flows into
		// filesystem paths verbatim (tmp-<sha> staging, blobs/<sha> storage)
		// and into verification as the expected digest. Only the canonical
		// SHA-256 form may reach the plan: empty (a file without a hash) or
		// 64 hex characters. Reject anything else before it reaches the
		// plan, failing the whole plan rather than silently skipping so a
		// tampered tree is loud, not partial. The error names the offending
		// field so a malformed mirror response is diagnosable.
		if unsafeBlobName(sha) {
			return nil, fmt.Errorf("refusing non-canonical sha256 from repo tree for %q (field %s): %q: want empty or 64 hex characters", rel, shaField, sha)
		}
		// Store the canonical lowercase spelling so every consumer of
		// PlanItem.SHA256 (tmp-<sha> staging, blobs/<sha>, CheckBlob,
		// verify, manifest) sees "" or lowercase hex no matter how the
		// remote spelled the digest. ToLower is a no-op for lowercase input,
		// so canonical values pass through byte-identical.
		sha = strings.ToLower(sha)

		// Case-insensitive dedup: filterMatches is intentionally case-insensitive
		// (q4_k_m matches Q4_K_M), so two files that differ only in case both
		// pass the same filter and would otherwise both end up in the plan.
		// Treat them as the same logical file and keep the first occurrence,
		// matching what the HF API returns first. The exact-path dedup above
		// doesn't catch this because the two paths differ only in case.
		if _, dup := seenLower[relLower]; dup {
			continue
		}
		seenLower[relLower] = struct{}{}

		destinationSubdir := matchedFilter
		if job.IsDataset && !isLFS && !strings.HasSuffix(nameLower, ".gguf") &&
			(!ggufMode || datasetDataRoles[nodeIndex]) {
			destinationSubdir = ""
		}
		items = append(items, PlanItem{
			RelativePath:      rel,
			URL:               urlStr,
			LFS:               isLFS,
			SHA256:            sha,
			Size:              size,
			AcceptRanges:      acceptRanges,
			Subdir:            matchedFilter, // empty when no filter matched
			DestinationSubdir: destinationSubdir,
		})
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// A non-empty filter set must have produced at least one direct selection
	// match across the scanned tree (excludes applied). Companion/metadata
	// files alone never make a filtered request succeed. An empty repo
	// without filters still returns a successful empty plan.
	if len(job.Filters) > 0 && !directFilterMatch {
		return nil, fmt.Errorf("%w: filters=%q exact=%t", ErrNoFilterMatch, job.Filters, job.ExactMatch)
	}
	return &Plan{Items: items, Commit: commitSHA}, nil
}

// GGUFPathSelected is the direct per-path predicate for a known relative GGUF
// filename in matched-only contexts, including GGUF-only planning and the
// dataset GGUF projection. It does not describe the complete ordinary-model
// emission policy: an unmatched non-LFS file can still be emitted by an
// ordinary model plan.
func GGUFPathSelected(relativePath string, filters, excludes []string, exact bool) bool {
	name := strings.ToLower(filepath.Base(relativePath))
	if !strings.HasSuffix(name, ".gguf") {
		return false
	}
	if isExcludedPath(relativePath, excludes) {
		return false
	}
	if len(filters) == 0 {
		return true
	}
	for _, filter := range filters {
		if filterMatchesPath(relativePath, filter, exact) {
			return true
		}
	}
	return false
}

func datasetFilterMatchesMembership(c datasetDecisionContext, filter string) bool {
	for _, label := range c.RecognizedLabels {
		if filterMatchesPath(label, filter, false) {
			return true
		}
	}
	return false
}

func containsPath(dirs map[string]struct{}, dir string) bool {
	for current := dir; current != "." && current != "/"; current = path.Dir(current) {
		if _, ok := dirs[current]; ok {
			return true
		}
	}
	return false
}

// matchedFilterDirectories returns the directories that contain a matched file
// or an ancestor of one. They define the companion scope for active non-GGUF
// filters: root files remain shared companions, while unrelated component
// metadata does not leak into the plan. GGUF mode intentionally has no
// companion scope because it is matched-only.
func matchedFilterDirectories(fileNodes []hfNode, job Job, ggufMode bool) map[string]struct{} {
	dirs := make(map[string]struct{})
	if len(job.Filters) == 0 || ggufMode {
		return dirs
	}
	for _, n := range fileNodes {
		if isExcludedPath(n.Path, job.Excludes) {
			continue
		}
		matched := false
		for _, filter := range job.Filters {
			if filterMatchesPath(n.Path, filter, job.ExactMatch) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		for dir := path.Dir(strings.ReplaceAll(n.Path, "\\", "/")); dir != "." && dir != "/"; dir = path.Dir(dir) {
			dirs[dir] = struct{}{}
		}
	}
	return dirs
}

func isExcludedPath(rel string, excludes []string) bool {
	nameLower := strings.ToLower(filepath.Base(rel))
	relLower := strings.ToLower(rel)
	for _, ex := range excludes {
		exLower := strings.ToLower(ex)
		if strings.Contains(nameLower, exLower) || strings.Contains(relLower, exLower) {
			return true
		}
	}
	return false
}

// GGUFPathMayBeEmitted is a conservative per-path projection for use when a
// complete plan is not yet available; it is not a substitute for PlanRepo or
// its completed destinations. Ordinary model selection retains every non-LFS
// file, so without the remote tree the storage type and selection mode are
// unknown and any non-excluded path may be emitted. Dataset selection retains
// its narrower role-aware policy. Server admission scopes this projection to
// reachable writable namespaces and prefers complete frozen destinations
// when available; the projection does not imply a global conflict.
func GGUFPathMayBeEmitted(relativePath string, job Job) bool {
	for _, exclude := range job.Excludes {
		if strings.Contains(strings.ToLower(relativePath), strings.ToLower(exclude)) {
			return false
		}
	}
	if !job.IsDataset {
		return true
	}
	return GGUFPathSelected(relativePath, job.Filters, job.Excludes, job.ExactMatch)
}

// isRecognizedModelMetadata uses narrow, established model-context names. It
// does not classify arbitrary JSON/TXT files or claim that every such name is
// metadata in every repository.
func isRecognizedModelMetadata(relativePath string) bool {
	base := strings.ToLower(path.Base(strings.ReplaceAll(relativePath, "\\", "/")))
	switch base {
	case "config.json", "tokenizer.json", "tokenizer_config.json", "special_tokens_map.json", "added_tokens.json", "vocab.json", "merges.txt", "vocab.txt":
		return true
	}
	return isREADMEPath(relativePath)
}

// Dataset metadata keeps its established recognition domain. A basename that
// is a model tokenizer asset (notably vocab.txt) is not thereby dataset metadata.
func isRecognizedDatasetMetadataPath(relativePath string) bool {
	base := strings.ToLower(path.Base(strings.ReplaceAll(relativePath, "\\", "/")))
	return base != "vocab.txt" && isRecognizedMetadataPath(relativePath)
}

func isRecognizedMetadataPath(relativePath string) bool {
	return isRecognizedModelMetadata(relativePath) || isDatasetServiceMetadata(relativePath)
}

func hasDatasetRoleUncertainty(input datasetDecisionInput) bool {
	data, companion := false, false
	for _, alternative := range input.Alternatives {
		data = data || alternative.Role == datasetselection.EvidenceRoleData
		companion = companion || alternative.Role == datasetselection.EvidenceRoleCompanion
	}
	return data && companion
}

func isDatasetServiceMetadata(relativePath string) bool {
	return strings.EqualFold(path.Base(strings.ReplaceAll(relativePath, "\\", "/")), "dataset_info.json")
}

// filterMatches reports whether filter fLower matches the file name nameLower
// (both already lowercased). In substring mode (the default) it uses a plain
// substring check. In exact mode it matches only when fLower equals a whole
// delimiter-bounded segment of the name, so "q6_k" matches "...-Q6_K.gguf" but
// not "...-Q6_K_XL.gguf". See Settings.ExactMatch (github issue #78).
func filterMatches(nameLower, fLower string, exact bool) bool {
	if !exact {
		return strings.Contains(nameLower, fLower)
	}
	for _, seg := range strings.FieldsFunc(nameLower, isFilterDelimiter) {
		if seg == fLower {
			return true
		}
	}
	if strings.Contains(fLower, "-") || strings.Contains(fLower, ".") || strings.Contains(fLower, " ") {
		start := 0
		for {
			idx := strings.Index(nameLower[start:], fLower)
			if idx < 0 {
				break
			}
			idx += start
			beforeOK := idx == 0 || isFilterDelimiter(rune(nameLower[idx-1]))
			afterIdx := idx + len(fLower)
			afterOK := afterIdx == len(nameLower) || isFilterDelimiter(rune(nameLower[afterIdx]))
			if beforeOK && afterOK {
				return true
			}
			start = idx + 1
		}
	}
	return false
}

// filterMatchesPath matches a filter against the complete repository-relative
// path. Substring mode intentionally retains the historical broad matching,
// while exact mode treats directory components, filename segments, extensions,
// and explicit directory prefixes as separate forms.
func filterMatchesPath(rel, filter string, exact bool) bool {
	rel = strings.ToLower(strings.ReplaceAll(rel, "\\", "/"))
	filter = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(filter), "\\", "/"))
	if rel == "" || filter == "" {
		return false
	}
	if !exact {
		return strings.Contains(rel, filter)
	}

	// A trailing slash is an explicit directory selection. It must start at a
	// path component boundary, so "unet/" does not select "myunet/".
	if strings.HasSuffix(filter, "/") {
		dir := strings.TrimSuffix(filter, "/")
		return strings.HasPrefix(rel, dir+"/") || strings.Contains(rel, "/"+dir+"/")
	}
	// Extension filters are written as ".safetensors" and select that exact
	// extension without making the leading dot a filename delimiter.
	if strings.HasPrefix(filter, ".") && !strings.Contains(filter[1:], "/") {
		return strings.HasSuffix(rel, filter)
	}
	// "./name" is the additive exact-only spelling for a literal root basename.
	// Canonical repository paths cannot contain a dot segment, so this spelling
	// is distinct from every pre-existing path or basename filter.
	if strings.HasPrefix(filter, "./") {
		name := strings.TrimPrefix(filter, "./")
		return name != "" && name != "." && name != ".." && !strings.Contains(name, "/") && rel == name
	}
	// A slash-containing filter is a complete relative path form.
	if strings.Contains(filter, "/") {
		return rel == filter
	}
	parts := strings.Split(rel, "/")
	for _, part := range parts[:len(parts)-1] {
		if filterMatches(part, filter, true) {
			return true
		}
	}
	return filterMatches(parts[len(parts)-1], filter, true)
}

// filterAttribution removes the root-selector marker before a matched filter
// becomes a public Subdir or DestinationSubdir. Selection identity and output
// attribution are separate: the marker must never become part of a destination.
func filterAttribution(filter string, exact bool) string {
	if !exact {
		return filter
	}
	filter = strings.ReplaceAll(strings.TrimSpace(filter), "\\", "/")
	if strings.HasPrefix(filter, "./") {
		name := strings.TrimPrefix(filter, "./")
		if name != "" && name != "." && name != ".." && !strings.Contains(name, "/") {
			return name
		}
	}
	return filter
}

// isFilterDelimiter reports whether r separates segments for exact-match
// filtering. Underscores are intentionally NOT delimiters because quantization
// names contain them (e.g. Q6_K, Q4_K_M).
func isFilterDelimiter(r rune) bool {
	return r == '-' || r == '.' || r == ' '
}

// isGGUFFilterDownload reports whether the given filters select only .gguf files
// in the supplied set of file paths (all expected lowercased). When true the
// download is treated as GGUF-only: because a GGUF file is self-contained, the
// plan keeps just the filter-matched files (the chosen quant's shards plus any
// mmproj companion) and drops config/tokenizer JSON, README, .gitattributes and
// fp16/ transformers metadata. A filter can match both GGUF and non-GGUF files
// (for example, a selected directory), so mixed selections retain the normal
// companion scope instead.
func isGGUFFilterDownload(baseNames, filters []string, exact bool) bool {
	return isGGUFFilterDownloadWithExcludes(baseNames, filters, nil, exact)
}

func isGGUFFilterDownloadWithExcludes(baseNames, filters, excludes []string, exact bool) bool {
	if len(filters) == 0 {
		return false
	}
	matchedGGUF := false
	matchedNonGGUF := false
	for _, filePath := range baseNames {
		if isExcludedPath(filePath, excludes) {
			continue
		}
		for _, f := range filters {
			if !filterMatchesPath(filePath, f, exact) {
				continue
			}
			if strings.HasSuffix(filePath, ".gguf") {
				matchedGGUF = true
			} else if isModelPayloadPath(filePath) {
				matchedNonGGUF = true
			}
			break
		}
	}
	return matchedGGUF && !matchedNonGGUF
}

// isModelPayloadPath is positive evidence that a directly matched non-GGUF
// entry is another model payload. Unknown files and recognized companions do
// not change GGUF selection mode; they remain downloadable only when directly
// selected. Keep this deliberately separate from storage/LFS status.
func isModelPayloadPath(filePath string) bool {
	base := strings.ToLower(path.Base(strings.ReplaceAll(filePath, "\\", "/")))
	// Match the established weight-file exclusions from smartdl: these are
	// recognized training artifacts, not evidence of another model payload.
	if strings.Contains(base, "optimizer") || strings.Contains(base, "training") {
		return false
	}
	switch path.Ext(base) {
	case ".safetensors", ".onnx", ".pt", ".pth":
		return true
	case ".bin":
		return !strings.Contains(base, "tokenizer")
	}
	// These exact Hugging Face/framework filenames are documented parameter
	// artifacts. Extension alone is not sufficient: arbitrary files such as
	// custom.msgpack or checkpoint.h5 remain unknown and do not activate mixed
	// model fallback. Numbered shard forms are accepted only for the established
	// Transformers Flax and TensorFlow names.
	if base == "flax_model.msgpack" || base == "diffusion_flax_model.msgpack" ||
		isNumberedArtifactShard(base, "flax_model", ".msgpack") ||
		base == "tf_model.h5" || isNumberedArtifactShard(base, "tf_model", ".h5") ||
		base == "model.tflite" || base == "rust_model.ot" {
		return true
	}
	return false
}

func isNumberedArtifactShard(base, stem, extension string) bool {
	prefix := stem + "-"
	if !strings.HasPrefix(base, prefix) || !strings.HasSuffix(base, extension) {
		return false
	}
	shard := strings.TrimSuffix(strings.TrimPrefix(base, prefix), extension)
	parts := strings.Split(shard, "-of-")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return false
	}
	for _, part := range parts {
		for _, r := range part {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

// isREADMEPath identifies README documents that may carry a selected GGUF
// quant token in their filename. They are descriptive companions, not evidence
// that a filter also selected a non-GGUF payload.
func isREADMEPath(filePath string) bool {
	base := strings.ToLower(path.Base(strings.ReplaceAll(filePath, "\\", "/")))
	if !strings.HasPrefix(base, "readme") {
		return false
	}
	switch path.Ext(base) {
	case ".md", ".markdown", ".rst", ".txt":
		return true
	default:
		return false
	}
}

// destinationBase returns the base output directory for a job.
func destinationBase(job Job, cfg Settings) (string, error) {
	// LocalRepo overrides the folder name: use it when the files are fetched
	// from an upstream repo but should be stored alongside another model's files
	// (e.g. mmproj from a base model saved next to the current model's quants).
	repoForPath := job.Repo
	if job.LocalRepo != "" {
		repoForPath = job.LocalRepo
	}
	return DestinationBase(cfg.OutputDir, repoForPath)
}

// ScanPlan scans a repository and emits plan_item events via the progress callback.
// This is useful for dry-run/preview functionality.
func ScanPlan(ctx context.Context, job Job, cfg Settings, progress ProgressFunc) error {
	plan, err := PlanRepo(ctx, job, cfg)
	if err != nil {
		return err
	}

	if progress != nil {
		for _, item := range plan.Items {
			progress(ProgressEvent{
				Time:     time.Now().UTC(),
				Event:    "plan_item",
				Repo:     job.Repo,
				Revision: job.Revision,
				Path:     item.RelativePath,
				Total:    item.Size,
				IsLFS:    item.LFS,
			})
		}
	}

	return nil
}

// Run is an alias for Download for API compatibility.
func Run(ctx context.Context, job Job, cfg Settings, progress ProgressFunc) error {
	return Download(ctx, job, cfg, progress)
}
