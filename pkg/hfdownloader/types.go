// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"fmt"
	"time"
)

// Job defines what to download from the HuggingFace Hub.
//
// A Job specifies the repository, revision, and optional filters for selecting
// which files to download. The Repo field is required and must be in
// "owner/name" format (e.g., "TheBloke/Mistral-7B-Instruct-v0.2-GGUF").
//
// Example:
//
//	job := hfdownloader.Job{
//	    Repo:     "TheBloke/Mistral-7B-Instruct-v0.2-GGUF",
//	    Revision: "main",
//	    Filters:  []string{"q4_k_m"},
//	}
type Job struct {
	// Repo is the repository ID in "owner/name" format.
	// This field is required.
	//
	// Examples:
	//   - "TheBloke/Mistral-7B-Instruct-v0.2-GGUF"
	//   - "meta-llama/Llama-2-7b"
	//   - "facebook/flores" (dataset)
	Repo string

	// IsDataset indicates this is a dataset repo, not a model.
	// When true, the HuggingFace datasets API is used instead of the models API.
	IsDataset bool

	// Revision is the branch, tag, or commit SHA to download.
	// If empty, defaults to "main".
	//
	// Examples:
	//   - "main" (default branch)
	//   - "v1.0" (tag)
	//   - "abc123" (commit SHA)
	Revision string

	// Filters define case-insensitive direct selection matches against the full
	// repository-relative path for any file type (not only LFS blobs). Recognized
	// dataset group filters may also select files by dataset membership. If empty,
	// all files are downloaded.
	//
	// Each filter is matched against repository-relative paths: a file is a direct
	// match if its path contains any filter string (substring mode, the default) or,
	// when ExactMatch is set, if the filter matches the path by the exact-mode
	// component and path forms described on ExactMatch. Exact filters can use
	// "./name" to select only the literal root-level basename; nested artifacts
	// use their full relative path. Excludes are applied before selection: a file
	// that matches a filter but is excluded does not confirm selection. After
	// Excludes are applied, at least one file must directly match or be selected
	// by recognized dataset membership, or the scan fails with ErrNoFilterMatch.
	//
	// The planner may additionally retain companion/metadata files in the plan
	// after a successful direct selection (e.g., mmproj companions for GGUF,
	// or config/tokenizer JSON for non-GGUF downloads).
	//
	// Examples:
	//   - []string{"q4_0"} matches "model.Q4_0.gguf"
	//   - []string{"q4_k_m", "q5_k_m"} matches either quantization
	//   - []string{"config.json"} matches a plain (non-LFS) config file
	Filters []string

	// Excludes specify patterns to exclude from download, matched case-insensitively.
	// Files matching any exclude pattern will be skipped, even if they match a filter.
	// Excludes are applied after filters.
	//
	// Examples:
	//   - []string{".md"} excludes all markdown files
	//   - []string{"fp16", "fp32"} excludes full precision models
	//   - []string{"onnx"} excludes ONNX format files
	//
	// Credits: Feature suggested by jeroenkroese (#41)
	Excludes []string

	// ExactMatch controls how Filters are matched. When false (the default),
	// each filter is matched as a case-insensitive substring of the complete
	// repository-relative path (see Filters). When true, filters select by
	// repository-relative path form:
	//
	//   - a filter ending in "/" selects descendants of that directory at a
	//     path-component boundary;
	//   - a filter beginning with "." and containing no "/" is an extension
	//     selector (for example ".safetensors");
	//   - "./name" is an additive selector for the literal root-level basename,
	//     so it does not match a same-named file in a subdirectory;
	//   - a filter containing "/" is a complete repository-relative path;
	//   - any other filter matches a directory component or the basename as a
	//     whole delimiter-bounded segment (segments are split on '-', '.', and
	//     spaces; underscores stay within a segment).
	//
	// The segment form lets a filter like "q6_k" select "model-Q6_K.gguf"
	// without also matching "model-UD-Q6_K_XL.gguf". A directory-component
	// filter such as "unet" selects files below a unet/ directory, and a full
	// slash path such as "weights/model.gguf" matches only that path. The "./"
	// form is reserved for exact mode; without ExactMatch, filters retain their
	// existing substring behavior.
	//
	// Excludes are always matched as substrings, regardless of this setting.
	ExactMatch bool

	// AppendFilterSubdir puts each filter's matched files in a subdirectory
	// named after the filter. Useful for organizing multiple quantizations.
	//
	// When true, a file matching filter "q4_0" would be saved as:
	//   <output>/<repo>/q4_0/<filename>
	// Instead of:
	//   <output>/<repo>/<filename>
	//
	// Only single-component group/token filters are supported because the
	// filter becomes a destination directory name. Path-shaped filters (any
	// filter containing "/" or "\", including a "./" prefix or trailing
	// separator, or the "."/".." components) must use
	// AppendFilterSubdir=false and cause PlanRepo and Download to return
	// ErrAppendFilterSubdirPathFilter before any network or plan work.
	AppendFilterSubdir bool

	// LocalRepo overrides the destination folder path when set.
	// Files are fetched from Repo but stored as if they belong to LocalRepo.
	// Use when downloading from an upstream repo (e.g. mmproj from a base
	// model) but wanting the files saved alongside a different model's files.
	// When empty, Repo is used for the destination path (default behaviour).
	LocalRepo string
}

// Settings configures download behavior.
//
// All fields have sensible defaults. CacheDir specifies where the
// HuggingFace cache structure will be created.
//
// Example with defaults:
//
//	cfg := hfdownloader.Settings{
//	    CacheDir: "~/.cache/huggingface",
//	}
//
// Example with full configuration:
//
//	cfg := hfdownloader.Settings{
//	    CacheDir:           "~/.cache/huggingface",
//	    Concurrency:        8,
//	    MaxActiveDownloads: 4,
//	    MultipartThreshold: "32MiB",
//	    Verify:             "sha256",
//	    Retries:            4,
//	    Token:              os.Getenv("HF_TOKEN"),
//	}
type Settings struct {
	// CacheDir is the HuggingFace cache root directory.
	// Downloads use the official HF Hub cache structure:
	//   <CacheDir>/hub/models--<owner>--<repo>/blobs/
	//   <CacheDir>/hub/models--<owner>--<repo>/snapshots/<commit>/
	// With a friendly view at:
	//   <CacheDir>/models/<owner>/<repo>/
	//
	// If empty, defaults to ~/.cache/huggingface (or HF_HOME env var).
	CacheDir string
	// HubDir freezes the exact Hub storage directory when nonempty. Empty
	// retains normal ENV resolution at cache construction, even with CacheDir.
	HubDir string

	// StaleTimeout is the duration after which an incomplete download
	// with no writes is considered stale and can be taken over by
	// another process. Accepts duration strings: "5m", "10m", "1h".
	// If empty, defaults to "5m".
	StaleTimeout string

	// OutputDir is DEPRECATED. Use CacheDir instead.
	// If set, falls back to legacy flat directory structure for compatibility.
	// Will be removed in a future version.
	OutputDir string

	// Concurrency is the number of parallel HTTP connections per file
	// when using multipart downloads. Higher values can improve speed
	// on fast networks but increase memory usage.
	// If <= 0, defaults to 8.
	Concurrency int

	// MaxActiveDownloads limits how many files TRANSFER simultaneously.
	// This controls overall parallelism across all files in a job. The
	// limit bounds concurrent transfers, not waiters: a file that waits for
	// another writer's destination ownership domain, or for a multipart
	// staging claim, does not consume a slot. Both the domain acquisition
	// (the in-process domain slot and the cross-process OS lock) and the
	// staging claim (the in-process staging owner, the cross-process staging
	// OS lock and the local metadata gate) return the slot for a wait and
	// take it back before returning a domain/claim the caller will transfer
	// under; a failed claim keeps the slot returned.
	// If <= 0, defaults to GOMAXPROCS (number of CPU cores).
	MaxActiveDownloads int

	// MultipartThreshold is the minimum file size to use multipart downloads.
	// Files smaller than this are downloaded in a single request.
	// Accepts human-readable sizes: "32MiB", "256MB", "1GiB", etc.
	// If empty, defaults to "256MiB".
	MultipartThreshold string

	// Verify specifies how to verify files after download. It controls LFS
	// files too (issue #140):
	//
	//   - "none": No post-download content hash (fastest). HTTP and ranged
	//     response validation still apply during the transfer.
	//   - "size": (default, fast) Verify the exact expected size and complete
	//     validated transfer coverage on both the single-request and ranged
	//     paths: the ranged path requires every expected byte range to be
	//     validated and written, and the single path requires an exact 206
	//     Content-Range/body-length match and checks the on-disk size before
	//     the rename. On the normal transfer path no full-file SHA-256 reread
	//     is performed for LFS files. Deliberate exceptions may still hash:
	//     the pre-download skip check over an already-present destination,
	//     the content proof of inherited single-path .part bytes, an
	//     unpinned-identity proof, and a single-request transfer with a known
	//     digest but no known expected size (see below). A resumed
	//     ProofPending record can also force the proof after a crash or mode
	//     change (see the durable proof gate below).
	//   - "sha256": Full SHA-256 hash verification (most secure, slower).
	//     At most one full-file read is performed per file, always before
	//     publication when the item carries a known digest: only an eligible
	//     fresh sequential LFS ranged transfer (one task covers the whole file)
	//     hashes while writing; every other transfer
	//     (including the single-request path, fresh or resumed) performs one
	//     sequential pass before the destination/blob is renamed into place, so
	//     a mismatch never publishes corrupt bytes.
	//   - "etag": Historical compatibility mode. A known SHA-256 digest is
	//     proven with one full-file pass before publication, reusing the same
	//     engine proof as "sha256" (a ranged or single-path proof is never
	//     repeated). Files without a known digest keep the historical
	//     behavior: non-LFS ETag files perform no post-download content check.
	//     (The HEAD-ETag fallback exists only in "sha256" mode; with no known
	//     digest it remains a post-publication check.)
	//
	// Partial downloads from old or unsupported formats are discarded, not
	// migrated: their bytes are never read into staging and the file is
	// re-downloaded from zero (already-published blobs, completed files and
	// snapshot/friendly entries are untouched). A resumed single-request
	// .part contributed bytes before this run, and since a moved commit or a
	// corrupt file can produce a size-correct prefix, inherited .part bytes are
	// content-proven once before the rename. A fresh single-request transfer in
	// "sha256"/"etag" mode is likewise proven before the rename, and a known
	// digest with no known expected size is proven too (only the digest can
	// validate an unconstrained stream). A mismatch discards the
	// partial/staging data and never publishes. With Verify "none" these
	// inherit the mode's no-hash semantics; without a known digest only size
	// and complete transfer coverage can be checked.
	//
	// Durable proof gate: a staging (multipart) record carries a persisted
	// ProofPending flag that is set and flushed before any known-digest proof
	// starts and cleared and flushed only after the proof succeeds, immediately
	// before publication. On resume it forces the proof in every mode except
	// "none" (including "size") whenever the item has a known digest, so a
	// crash mid-proof, a mismatch whose cleanup fails,
	// or a later Verify-mode change cannot publish unproven all-Done ranges. On
	// a mismatch the flag stays set, poison is attempted only while the claim is
	// still owned under lifetime exclusion and metadata serialization (an
	// unfenced writer must not truncate another owner's payload), and the owned discard
	// is attempted; if either cannot verify ownership, the durable flag remains
	// the guard and the next run re-proves or discards. Publish refuses while the
	// flag is still set for a known digest. Verify "none" is the explicit
	// no-hash opt-out. The single-request .part has no separate flag: when a
	// proof is required (sha256/etag, inherited bytes, or a known digest with
	// unknown size) it is renamed only after that proof, so every restart
	// re-proves it; a fresh size transfer with a known size follows the size
	// contract and may rename without a hash.
	Verify string

	// Retries is the maximum number of retry attempts per HTTP request.
	// Each retry uses exponential backoff with jitter.
	// If <= 0, defaults to 4.
	Retries int

	// BackoffInitial is the initial delay before the first retry.
	// Accepts duration strings: "400ms", "1s", "2s", etc.
	// If empty, defaults to "400ms".
	BackoffInitial string

	// BackoffMax is the maximum delay between retries.
	// The actual delay grows exponentially but caps at this value.
	// If empty, defaults to "10s".
	BackoffMax string

	// StallTimeout aborts a body-read attempt that stops delivering bytes for
	// this long, then retries from the current on-disk offset. Without it a
	// stalled-but-open connection hangs the download forever.
	//
	// Accepts duration strings: "60s", "2m", etc. An empty value (library
	// callers that leave it unset) uses the protective default of 60s; an
	// explicit "0" or "0s" disables the watchdog.
	StallTimeout string

	// Token is the HuggingFace access token for private or gated repos.
	// Get yours at: https://huggingface.co/settings/tokens
	// The Settings struct does not read the environment itself; library
	// callers pass the token explicitly (e.g. os.Getenv("HF_TOKEN")). The
	// hfdesk server resolves tokens in this order: --token flag, HF_TOKEN
	// environment variable, then config-file token.
	Token string

	// Endpoint is the base URL for HuggingFace Hub API.
	// Use this to specify a custom mirror or enterprise endpoint.
	// If empty, defaults to "https://huggingface.co".
	//
	// Examples:
	//   - "https://huggingface.co" (default)
	//   - "https://hf-mirror.com" (China mirror)
	//   - "https://your-enterprise.com/hf" (enterprise)
	//
	// Credits: Feature suggested by windtail (#38)
	Endpoint string

	// NoManifest disables writing the hfd.yaml manifest file after download.
	// By default, a manifest is always written to the friendly view directory.
	NoManifest bool

	// NoFriendlyView disables creating the friendly view symlinks (models/, datasets/).
	// When set, only the HF cache structure (hub/) is created.
	// This also skips writing the rebuild.sh script since there's nothing to rebuild.
	NoFriendlyView bool

	// Command is the CLI command string used to initiate this download.
	// If set, it will be included in the manifest file (hfd.yaml).
	// The token is automatically stripped from the command for security.
	// Used internally by the CLI; library users typically don't set this.
	Command string

	// Proxy configures HTTP/HTTPS/SOCKS5 proxy settings for downloads.
	// If nil or empty, falls back to environment variables (HTTP_PROXY, etc).
	//
	// Example configurations:
	//   - HTTP proxy: &ProxyConfig{URL: "http://proxy:8080"}
	//   - SOCKS5 proxy: &ProxyConfig{URL: "socks5://proxy:1080"}
	//   - With auth: &ProxyConfig{URL: "http://proxy:8080", Username: "user", Password: "pass"}
	//   - Disable env proxy: &ProxyConfig{NoEnvProxy: true}
	Proxy *ProxyConfig

	// MaxSpeed caps the aggregate download throughput, as a human-readable
	// bytes-per-second value: "2MB", "500KB", "1.5MiB", etc. Empty or "0"
	// means unlimited. The cap is global across every file and connection in
	// the download, so concurrent transfers share the budget instead of each
	// getting the full rate.
	MaxSpeed string

	// SpeedLimiter, when non-nil, is the shared token-bucket used to pace
	// downloads, taking precedence over MaxSpeed. The server sets this to a
	// single process-wide limiter so the cap applies across all running jobs;
	// library callers can leave it nil and just set MaxSpeed.
	SpeedLimiter *RateLimiter

	// CleanupPartialsOnCancel, when non-nil, is called when the download
	// context is cancelled. If it returns true, the downloader deletes the
	// partial files it produced before returning: dst+".part" (single
	// path), the legacy dst+".part-NN"/dst+".parts.json" family, and the
	// ranged staging file with its resume metadata
	// (dst+".incomplete"/".incomplete.meta", or blobs/<sha>.incomplete in
	// HF cache mode). The persistent `<staging>.lock` ownership file is not
	// removed: it carries no payload and is reused by later claims. When nil
	// (default), partial files are preserved for resume (useful for
	// pause/resume workflows).
	CleanupPartialsOnCancel func() bool

	// OnPartialFile, when non-nil, is called for every per-file
	// destination the downloader touches during a run. The downloader
	// calls it with finalize=false once a per-file goroutine has
	// computed its destination path and decided to download (not
	// skip), and again with finalize=true once the file is fully
	// assembled and renamed to its final location. A cancel/pause that
	// lands between those two calls leaves the dst in the caller's
	// tracking set, which is exactly the set of partial files that
	// belong to this run and are safe to remove if the run ends in a
	// terminal non-resumable state.
	//
	// The callback runs from per-file downloader goroutines. Callers
	// are responsible for serializing access to whatever state they
	// maintain.
	//
	// The downloader does not interpret the callback's return; it is
	// fire-and-forget. The downloader still calls
	// CleanupPartialsOnCancel for the in-flight direct-cancel cleanup
	// path; OnPartialFile is for callers that need per-job scoping
	// after the run ends (e.g. the server's pause → cancel cleanup).
	OnPartialFile func(dst string, finalize bool)

	// OnPlanComplete is called once after the repository plan has been
	// successfully built and before any planned file is skipped or modified.
	// The callback receives an independent copy, including the resolved commit
	// used for snapshot paths. It is intended for callers that must reserve the
	// concrete destinations for the full plan lifecycle.
	OnPlanComplete func(plan Plan)
}

// ProgressEvent represents a progress update during download.
//
// Events are emitted throughout the download process to allow for
// progress display, logging, or integration with other systems.
//
// The Event field indicates the type of event:
//   - "scan_start": Repository scanning has begun
//   - "plan_item": A file has been added to the download plan
//   - "file_start": Download of a file has started
//   - "file_progress": Periodic progress update during download
//   - "file_done": File download complete (check Message for "skip" info)
//   - "retry": A retry attempt is being made
//   - "error": An error occurred
//   - "done": All downloads complete
type ProgressEvent struct {
	// Time is when the event occurred (UTC).
	Time time.Time `json:"time"`

	// Level is the log level: "debug", "info", "warn", "error".
	// Empty defaults to "info".
	Level string `json:"level,omitempty"`

	// Event is the event type identifier.
	Event string `json:"event"`

	// Repo is the repository being processed.
	Repo string `json:"repo,omitempty"`

	// Revision is the branch/tag/commit being downloaded.
	Revision string `json:"revision,omitempty"`

	// Path is the relative file path within the repository.
	Path string `json:"path,omitempty"`

	// Bytes is the number of bytes in the current progress update.
	// Used in "file_progress" events.
	Bytes int64 `json:"bytes,omitempty"`

	// Total is the total expected size in bytes.
	Total int64 `json:"total,omitempty"`

	// Downloaded is the cumulative bytes downloaded so far.
	Downloaded int64 `json:"downloaded,omitempty"`

	// Attempt is the retry attempt number (1-based).
	// Only set in "retry" events.
	Attempt int `json:"attempt,omitempty"`

	// Message contains additional context or error details.
	// For "file_done" events, may contain "skip (reason)" if skipped.
	Message string `json:"message,omitempty"`

	// IsLFS indicates whether this file is stored in Git LFS.
	IsLFS bool `json:"isLfs,omitempty"`
}

// ProgressFunc is a callback for receiving progress events.
//
// Implement this to display progress in a UI, log events, or track downloads.
// The callback is invoked from multiple goroutines and should be thread-safe.
//
// Example:
//
//	progress := func(e hfdownloader.ProgressEvent) {
//	    switch e.Event {
//	    case "file_start":
//	        fmt.Printf("Downloading: %s\n", e.Path)
//	    case "file_done":
//	        fmt.Printf("Complete: %s\n", e.Path)
//	    case "error":
//	        fmt.Printf("Error: %s\n", e.Message)
//	    }
//	}
type ProgressFunc func(ProgressEvent)

// DefaultSettings returns Settings with sensible defaults filled in.
//
// Use this as a starting point and override specific fields:
//
//	cfg := hfdownloader.DefaultSettings()
//	cfg.CacheDir = "/custom/path"
//	cfg.Token = os.Getenv("HF_TOKEN")
func DefaultSettings() Settings {
	return Settings{
		CacheDir:           "", // Empty = use DefaultCacheDir()
		StaleTimeout:       "5m",
		Concurrency:        8,
		MaxActiveDownloads: 4,
		MultipartThreshold: "256MiB",
		Verify:             "size",
		Retries:            4,
		BackoffInitial:     "400ms",
		BackoffMax:         "10s",
		StallTimeout:       "60s",
	}
}

// BuildHFCache creates an HFCache from Settings.
// This is the recommended way to get a cache instance.
func (s Settings) BuildHFCache() (*HFCache, error) {
	staleTimeout := DefaultStaleTimeout
	if s.StaleTimeout != "" {
		var err error
		staleTimeout, err = time.ParseDuration(s.StaleTimeout)
		if err != nil {
			return nil, fmt.Errorf("invalid stale-timeout %q: %w", s.StaleTimeout, err)
		}
	}
	if s.HubDir != "" {
		root := s.CacheDir
		if root == "" {
			root = DefaultCacheDir()
		}
		return NewHFCacheResolved(root, s.HubDir, staleTimeout), nil
	}
	return NewHFCache(s.CacheDir, staleTimeout), nil
}
