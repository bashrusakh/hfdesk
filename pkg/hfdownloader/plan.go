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
)

// unsafeRepoPath reports whether a relative path returned by the repo tree API
// would escape the repository root if joined onto a local directory. The path
// list is remote-controlled (and the endpoint is operator-configurable via
// --endpoint, so a malicious or MITM'd mirror can return anything), and the
// path flows unchecked into file writes (filepath.Join(base, rel)) and symlink
// creation. Anything absolute, containing a "\\" (Windows separator / drive
// escape), or normalising to "" / "." / ".." / a "../" prefix is rejected to
// prevent arbitrary-file-write.
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
	return false
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
func PathInside(base, target string) bool {
	base = filepath.Clean(base)
	target = filepath.Clean(target)
	if target == base {
		return true
	}
	return strings.HasPrefix(target+string(filepath.Separator), base+string(filepath.Separator))
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

// PlanItem represents a single file in the download plan.
type PlanItem struct {
	RelativePath string `json:"path"`
	URL          string `json:"url"`
	LFS          bool   `json:"lfs"`
	SHA256       string `json:"sha256,omitempty"`
	Size         int64  `json:"size"`
	AcceptRanges bool   `json:"acceptRanges"`
	// Subdir holds the matched filter (if any) used when --append-filter-subdir is set.
	Subdir string `json:"subdir,omitempty"`
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
	httpc := buildHTTPClientWithProxy(cfg.Proxy)
	return scanRepo(ctx, httpc, cfg.Token, job, cfg)
}

// scanRepo walks the repo tree and builds a download plan.
func scanRepo(ctx context.Context, httpc *http.Client, token string, job Job, cfg Settings) (*Plan, error) {
	var items []PlanItem
	seen := make(map[string]struct{})      // ensure each relative path appears once in the plan
	seenLower := make(map[string]struct{}) // ensure each path appears at most once modulo case

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
		if n.Type == "file" || n.Type == "blob" {
			// Reject path-traversal entries before they reach any filesystem
			// operation downstream. Fail the whole plan rather than silently
			// skipping so a tampered tree is loud, not partial.
			if unsafeRepoPath(n.Path) {
				return fmt.Errorf("refusing unsafe path from repo tree: %q", n.Path)
			}
			fileNodes = append(fileNodes, n)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// GGUF-only mode: filters are set and at least one matches a .gguf file. A
	// GGUF file embeds its own tokenizer and config, so the download should be
	// just the chosen quant shards (plus any mmproj filter) — not the repo's
	// config/tokenizer JSON, README, .gitattributes, or the fp16/ transformers
	// metadata that ships alongside many GGUF repos. For non-GGUF downloads
	// (e.g. a "safetensors" filter) those companion files are still required,
	// so the original behavior is kept.
	baseNames := make([]string, 0, len(fileNodes))
	for _, n := range fileNodes {
		baseNames = append(baseNames, strings.ToLower(filepath.Base(n.Path)))
	}
	ggufMode := isGGUFFilterDownload(baseNames, job.Filters, job.ExactMatch)

	for _, n := range fileNodes {
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

		// Check excludes first - if file matches any exclude pattern, skip it
		// Credits: Exclude feature suggested by jeroenkroese (#41)
		excluded := false
		for _, ex := range job.Excludes {
			exLower := strings.ToLower(ex)
			if strings.Contains(nameLower, exLower) || strings.Contains(relLower, exLower) {
				excluded = true
				break
			}
		}
		if excluded {
			continue
		}

		// Determine which filter (if any) matches this file name, prefer the longest match
		// Filter matching is case-insensitive (e.g., q4_0 matches Q4_0)
		matchedFilter := ""
		if ggufMode {
			// Keep only files that match a filter: the selected quant's shards
			// and any mmproj companion. Everything else is skipped.
			for _, f := range job.Filters {
				if filterMatches(nameLower, strings.ToLower(f), job.ExactMatch) {
					if len(f) > len(matchedFilter) {
						matchedFilter = f
					}
				}
			}
			if matchedFilter == "" {
				continue
			}
		} else if isLFS && len(job.Filters) > 0 {
			for _, f := range job.Filters {
				fLower := strings.ToLower(f)
				if filterMatches(nameLower, fLower, job.ExactMatch) {
					if len(f) > len(matchedFilter) {
						matchedFilter = f
					}
				}
			}
			// If filters provided and none matched, skip typical large LFS blobs
			if matchedFilter == "" {
				ln := strings.ToLower(name)
				ext := strings.ToLower(filepath.Ext(name))
				if ext == ".bin" || ext == ".act" || ext == ".safetensors" || ext == ".zip" || strings.HasSuffix(ln, ".gguf") || strings.HasSuffix(ln, ".ggml") {
					continue
				}
			}
		}

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

		items = append(items, PlanItem{
			RelativePath: rel,
			URL:          urlStr,
			LFS:          isLFS,
			SHA256:       sha,
			Size:         size,
			AcceptRanges: acceptRanges,
			Subdir:       matchedFilter, // empty when no filter matched
		})
	}
	return &Plan{Items: items, Commit: commitSHA}, nil
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

// isFilterDelimiter reports whether r separates segments for exact-match
// filtering. Underscores are intentionally NOT delimiters because quantization
// names contain them (e.g. Q6_K, Q4_K_M).
func isFilterDelimiter(r rune) bool {
	return r == '-' || r == '.' || r == ' '
}

// isGGUFFilterDownload reports whether the given filters target a .gguf file in
// the supplied set of file base names (all expected lowercased). When true the
// download is treated as GGUF-only: because a GGUF file is self-contained, the
// plan keeps just the filter-matched files (the chosen quant's shards plus any
// mmproj companion) and drops config/tokenizer JSON, README, .gitattributes and
// fp16/ transformers metadata. For non-GGUF filters (e.g. "safetensors") this
// returns false so those companion files are still downloaded.
func isGGUFFilterDownload(baseNames, filters []string, exact bool) bool {
	if len(filters) == 0 {
		return false
	}
	for _, base := range baseNames {
		if !strings.HasSuffix(base, ".gguf") {
			continue
		}
		for _, f := range filters {
			if filterMatches(base, strings.ToLower(f), exact) {
				return true
			}
		}
	}
	return false
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
	// The repo-derived folder segment is joined onto the configured output
	// root and becomes the root of every downstream path, so it must stay
	// local. validate() enforces IsValidModelName(job.Repo) at the Download
	// boundary, but job.LocalRepo reaches this join unchecked; a "../" (or
	// absolute) segment would move the whole destination outside
	// cfg.OutputDir. filepath.IsLocal is the CodeQL-modeled barrier for
	// exactly this containment property.
	if !filepath.IsLocal(repoForPath) {
		return "", fmt.Errorf("destination folder %q must be a local path", repoForPath)
	}
	// Normalize the configured output root to an absolute, cleaned path
	// BEFORE the containment proof. The legacy OutputDir API accepts
	// cwd-shaped roots (".", "./", "./Models"): filepath.Join cleans a root
	// of "." away entirely, so proving against the raw configured string
	// rejected those legitimate roots as escapes even though they never
	// leave the working directory. Abs resolves every relative form —
	// including "" and roots containing ".." segments — against the current
	// working directory, giving the proof one stable root shape for
	// relative, absolute, and traversal-containing configs alike. The
	// preserved property is "destination never outside the effective
	// (resolved) root".
	root, err := filepath.Abs(cfg.OutputDir)
	if err != nil {
		return "", fmt.Errorf("resolve output root %q: %w", cfg.OutputDir, err)
	}
	base := filepath.Join(root, repoForPath)
	// Prove the joined result stays under the resolved output root before
	// returning it: this dominates every use of the result — including
	// os.MkdirAll at the download entry and SafeJoin's base — so neither a
	// crafted folder segment nor taint in cfg.OutputDir itself can escape
	// the root unproven. A local repoForPath always satisfies this, so
	// legitimate destinations are unaffected.
	if !PathInside(root, base) {
		return "", fmt.Errorf("destination %q escapes output root %q", base, root)
	}
	// Restate the containment PathInside just proved against the resolved
	// root with strings.HasPrefix, the primitive code scanning models as a
	// sanitizer guard: this check is evaluated on `base` itself and
	// dominates the return below, so taint carried in the configured root
	// cannot flow out of destinationBase unnoticed — the same restatement
	// SafeJoin applies to its result. Acceptance is identical to
	// PathInside's (root is already cleaned by Abs, and base is a cleaned
	// join of root), so no input that passed before is rejected.
	if !strings.HasPrefix(base, root) {
		return "", fmt.Errorf("destination %q escapes output root %q", base, root)
	}
	return base, nil
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
