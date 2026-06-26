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
		if sha == "" && n.LFS != nil {
			// LFS files have SHA256 in either Sha256 field or Oid field (LFS spec uses oid)
			sha = n.LFS.Sha256
			if sha == "" {
				sha = n.LFS.Oid
			}
		}

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
func destinationBase(job Job, cfg Settings) string {
	// LocalRepo overrides the folder name: use it when the files are fetched
	// from an upstream repo but should be stored alongside another model's files
	// (e.g. mmproj from a base model saved next to the current model's quants).
	repoForPath := job.Repo
	if job.LocalRepo != "" {
		repoForPath = job.LocalRepo
	}
	return filepath.Join(cfg.OutputDir, repoForPath)
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
