// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// progressReader wraps an io.Reader and emits progress events during reads.
type progressReader struct {
	reader     io.Reader
	total      int64
	downloaded int64
	path       string
	emit       func(ProgressEvent)
	lastEmit   time.Time
	interval   time.Duration
}

func newProgressReader(r io.Reader, total int64, path string, emit func(ProgressEvent)) *progressReader {
	return &progressReader{
		reader:   r,
		total:    total,
		path:     path,
		emit:     emit,
		lastEmit: time.Now(),
		interval: 200 * time.Millisecond, // Emit at most 5 times per second
	}
}

func (pr *progressReader) Read(p []byte) (n int, err error) {
	n, err = pr.reader.Read(p)
	if n > 0 {
		pr.downloaded += int64(n)
		// Throttle emissions to avoid flooding
		if time.Since(pr.lastEmit) >= pr.interval || err == io.EOF {
			pr.emit(ProgressEvent{
				Event:      "file_progress",
				Path:       pr.path,
				Downloaded: pr.downloaded,
				Total:      pr.total,
			})
			pr.lastEmit = time.Now()
		}
	}
	return n, err
}

type multipartResumeLayout struct {
	Size        int64                `json:"size"`
	Concurrency int                  `json:"concurrency"`
	Parts       []multipartPartRange `json:"parts"`
}

type multipartPartRange struct {
	Index int   `json:"index"`
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

func multipartLayoutMetaPath(dst string) string {
	return dst + ".parts.json"
}

// buildMultipartResumeLayout is the legacy .parts.json layout builder. The
// ranged downloader no longer writes it, but the one-time upgrade migration
// (migrateLegacyMultipart) reads layouts in exactly this shape, and the
// legacy cleanup family still enumerates its artifacts.
func buildMultipartResumeLayout(size int64, n int, chunk int64) multipartResumeLayout {
	layout := multipartResumeLayout{
		Size:        size,
		Concurrency: n,
		Parts:       make([]multipartPartRange, n),
	}
	for i := 0; i < n; i++ {
		start := int64(i) * chunk
		end := start + chunk - 1
		if i == n-1 {
			end = size - 1
		}
		layout.Parts[i] = multipartPartRange{Index: i, Start: start, End: end}
	}
	return layout
}

// writeMultipartResumeLayout persists a legacy .parts.json layout. The
// ranged downloader no longer writes these records; this helper remains so
// the one-time upgrade migration and the legacy-state tests share the exact
// on-disk shape a pre-#140 version produced.
func writeMultipartResumeLayout(dst string, layout multipartResumeLayout) error {
	data, err := json.Marshal(layout)
	if err != nil {
		return err
	}
	metaPath := multipartLayoutMetaPath(dst)
	tmpPath := metaPath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpPath, metaPath)
}

// multipartPartFiles lists the legacy per-connection payload files for dst:
// dst.part-NN for a non-empty run of digits, matching the pre-#140 %02d/%d
// part naming. Non-digit suffixes are not download artifacts and are left
// alone.
func multipartPartFiles(dst string) ([]string, error) {
	dir := filepath.Dir(dst)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	prefix := filepath.Base(dst) + ".part-"
	files := make([]string, 0)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		// Only the legacy %02d/%d part index family; a user file such as
		// "model.bin.part-notes" must not be swept up as a download artifact.
		if !isAllDigits(strings.TrimPrefix(entry.Name(), prefix)) {
			continue
		}
		files = append(files, filepath.Join(dir, entry.Name()))
	}
	return files, nil
}

func removeMultipartResumeFiles(dst string) error {
	files, err := multipartPartFiles(dst)
	if err != nil {
		return err
	}
	for _, p := range files {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := os.Remove(multipartLayoutMetaPath(dst)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// removeLegacySinglePart removes the pre-#140 single-download prefix
// (dst+".part") once it has either been migrated into the staging record or
// become obsolete because a valid .part-NN layout won the migration. The
// identity sidecar of that partial goes with it: no identity record may
// outlive the partial it describes.
func removeLegacySinglePart(dst string) error {
	if err := os.Remove(dst + ".part"); err != nil && !os.IsNotExist(err) {
		return err
	}
	removeSinglePartIdentity(singlePartIdentityPath(dst))
	return nil
}

// skippedBlobLink records an HF-cache item that was skipped while another
// writer owned or completed the shared blob. A later writer may have created
// snapshot/friendly entries only for its own repository path, so Download
// reconciles every skipped path once all files have finished.
type skippedBlobLink struct {
	commit       string
	relativePath string
	filterSubdir string
	sha256       string
}

// Download scans and downloads files from a HuggingFace repo.
//
// v3.0+: Files are stored in HuggingFace Hub cache structure by default:
//   - Blobs: hub/models--{owner}--{repo}/blobs/{sha256}
//   - Snapshots: hub/models--{owner}--{repo}/snapshots/{commit}/{path} (symlinks)
//   - Friendly: models/{owner}/{repo}/{path} (symlinks)
//
// Legacy mode (OutputDir set): Falls back to flat directory structure.
//
// Cancellation: all loops/sleeps/requests are tied to ctx for fast abort.
func Download(ctx context.Context, job Job, cfg Settings, progress ProgressFunc) (err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// The outcome of the whole job is recorded exactly once, here: every
	// return path of the function passes through this deferred record. A
	// cancelled job is not a failure, so it is reported separately; the
	// successful outcome is reported at the end of the function, where the
	// file counters are final.
	defer func() {
		if err == nil {
			return
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			downloadLog.Info("download cancelled", "repo", job.Repo, "error", err)
			return
		}
		downloadLog.Error("download failed", "repo", job.Repo, "error", err)
	}()
	if err := validate(job, cfg); err != nil {
		return err
	}

	// Apply defaults
	if job.Revision == "" {
		job.Revision = "main"
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 8
	}
	if cfg.MaxActiveDownloads <= 0 {
		cfg.MaxActiveDownloads = runtime.GOMAXPROCS(0)
	}

	// Resolve the shared speed limiter. A caller-supplied SpeedLimiter (the
	// server's process-wide limiter) wins; otherwise build a per-download one
	// from MaxSpeed. When neither is set, cfg.SpeedLimiter stays nil and the
	// copy sites read at full speed.
	if cfg.SpeedLimiter == nil {
		if bps := ParseSize(cfg.MaxSpeed); bps > 0 {
			cfg.SpeedLimiter = NewRateLimiter(bps)
		}
	}

	// Determine storage mode: HF cache (new) vs flat directory (legacy)
	// Use HF cache mode when:
	// 1. --cache-dir is explicitly set, OR
	// 2. --output is NOT set (default to HF cache)
	useHFCache := cfg.CacheDir != "" || cfg.OutputDir == ""
	var hfCache *HFCache
	var repoDir *RepoDir

	if useHFCache {
		var err error
		hfCache, err = cfg.BuildHFCache()
		if err != nil {
			return fmt.Errorf("build hf cache: %w", err)
		}
		repoType := RepoTypeModel
		if job.IsDataset {
			repoType = RepoTypeDataset
		}
		// LocalRepo overrides the cache folder name: files fetched from an
		// upstream repo are stored under the target model's cache directory
		// instead of the upstream repo's own directory.
		cacheRepoID := job.Repo
		if job.LocalRepo != "" {
			cacheRepoID = job.LocalRepo
		}
		repoDir, err = hfCache.Repo(cacheRepoID, repoType)
		if err != nil {
			return fmt.Errorf("create repo dir: %w", err)
		}
		if err := repoDir.EnsureDirs(); err != nil {
			return fmt.Errorf("ensure repo dirs: %w", err)
		}
	} else {
		// Legacy mode: use OutputDir
		if cfg.OutputDir == "" {
			cfg.OutputDir = "Storage"
		}
	}

	thresholdBytes, err := parseSizeString(cfg.MultipartThreshold, 256<<20)
	if err != nil {
		return fmt.Errorf("invalid multipart-threshold: %w", err)
	}
	httpc := buildHTTPClientWithProxy(cfg.Proxy)

	emit := func(ev ProgressEvent) {
		if progress != nil {
			if ev.Time.IsZero() {
				ev.Time = time.Now()
			}
			if ev.Repo == "" {
				ev.Repo = job.Repo
			}
			if ev.Revision == "" {
				ev.Revision = job.Revision
			}
			progress(ev)
		}
	}

	emit(ProgressEvent{Event: "scan_start", Message: "scanning repo"})

	downloadLog.Info("download started", "repo", job.Repo, "revision", job.Revision)

	plan, err := scanRepo(ctx, httpc, cfg.Token, job, cfg)
	if err != nil {
		return err
	}
	var plannedBytes int64
	for _, item := range plan.Items {
		plannedBytes += item.Size
	}
	downloadLog.Info("download planned",
		"repo", job.Repo,
		"files", len(plan.Items),
		"bytes", plannedBytes,
	)
	if cfg.OnPlanComplete != nil {
		completedPlan := *plan
		completedPlan.Items = append([]PlanItem(nil), plan.Items...)
		cfg.OnPlanComplete(completedPlan)
	}

	// Emit ALL plan_item events upfront so TUI knows total size immediately
	for _, item := range plan.Items {
		displayRel := item.RelativePath
		if job.AppendFilterSubdir && item.DestinationSubdir != "" {
			displayRel = filepath.ToSlash(filepath.Join(item.DestinationSubdir, item.RelativePath))
		}
		emit(ProgressEvent{Event: "plan_item", Path: displayRel, Total: item.Size})
	}

	// Ensure destination root exists (only for legacy mode)
	// HF cache mode already created directories via repoDir.EnsureDirs()
	if !useHFCache {
		base, err := destinationBase(job, cfg)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(base, 0o755); err != nil {
			return err
		}
	}

	// Overall concurrency limiter (ctx-aware acquisition). One token is one
	// TRANSFER slot (see download_slot.go): an item that waits for another
	// writer's ownership domain returns its token while it waits and takes it
	// back before it transfers.
	lim := make(chan struct{}, cfg.MaxActiveDownloads)

	var wg sync.WaitGroup
	errCh := make(chan error, len(plan.Items))

	// Job-scoped cancellable context. Every per-file context derives from it so
	// that a permanent (fail-fast) file/part error — or a job-fatal security
	// rejection — can terminate the whole job promptly instead of letting
	// sibling files/parts keep downloading, retrying, or waiting until
	// wg.Wait() returns. The cancel carries the original error as its cause, so
	// a sibling that exits via its ctx path can tell an internal abort
	// (preserve resumable bytes) apart from a user cancel/pause.
	jobCtx, jobCancel := context.WithCancelCause(ctx)
	defer jobCancel(nil)

	// fatalErr records the first job-fatal error. It is surfaced to the caller
	// in preference to the errCh drain so a sibling goroutine's "context
	// canceled" (produced while unwinding from the abort) can never mask or
	// precede the original permanent error.
	var (
		fatalMu  sync.Mutex
		fatalErr error
	)
	setFatalErr := func(err error) {
		fatalMu.Lock()
		if fatalErr == nil {
			fatalErr = err
		}
		fatalMu.Unlock()
		jobCancel(abortError(err))
	}

	// To print "skip" only once per final path per run
	var skipOnce sync.Map

	// Skipped HF-cache items with a known blob SHA. Their snapshot/friendly
	// entries are reconciled after every file finishes, because a shared blob
	// winner only creates entries for its own path.
	var (
		skippedLinksMu  sync.Mutex
		skippedLinks    []skippedBlobLink
		skippedLinkSeen = map[string]bool{}
	)
	recordSkippedBlobLink := func(it PlanItem, finalRel, filterSubdir string) {
		if !useHFCache || it.SHA256 == "" {
			return
		}
		key := plan.Commit + "\x00" + finalRel + "\x00" + filterSubdir
		skippedLinksMu.Lock()
		if !skippedLinkSeen[key] {
			skippedLinkSeen[key] = true
			skippedLinks = append(skippedLinks, skippedBlobLink{
				commit:       plan.Commit,
				relativePath: it.RelativePath,
				filterSubdir: filterSubdir,
				sha256:       it.SHA256,
			})
		}
		skippedLinksMu.Unlock()
	}

	var skippedCount int64
	var downloadedCount int64

	// Build manifest during download (thread-safe)
	// Manifest is always written unless explicitly disabled with NoManifest
	var manifestBuilder *ManifestBuilder
	var manifestMu sync.Mutex
	if useHFCache && !cfg.NoManifest {
		manifestBuilder = NewManifestBuilder(job, cfg.Command)
		manifestBuilder.SetCommit(plan.Commit)
		// RepoPath describes the selected physical repository, relative to the
		// friendly root when possible (absolute on different Windows volumes).
		rel, err := filepath.Rel(hfCache.Root, repoDir.Path())
		if err != nil {
			rel = repoDir.Path()
		}
		manifestBuilder.manifest.RepoPath = filepath.ToSlash(rel)
	}

LOOP:
	for _, item := range plan.Items {
		// Stop scheduling more work once canceled
		select {
		case <-jobCtx.Done():
			break LOOP
		default:
		}

		it := item // capture for goroutine

		// Acquire a slot or abort if canceled
		select {
		case lim <- struct{}{}:
		case <-jobCtx.Done():
			break LOOP
		}

		wg.Add(1)
		go func() {
			defer wg.Done()

			// Per-file context; ensures all inner loops stop on cancellation.
			// It derives from jobCtx so a fatal error in a sibling file also
			// stops this one promptly.
			fileCtx, fileCancel := context.WithCancel(jobCtx)
			// The item owns the slot acquired above through a LEASE, so it can
			// return the slot while it WAITS for another writer's ownership
			// domain and take it back before it transfers. The wait is
			// signalled by the domain acquisition through this context (see
			// download_slot.go and acquireBlobDomainWithin).
			lease := newDownloadSlotLease(fileCtx, lim)
			defer fileCancel()
			defer lease.release()
			fileCtx = withDownloadSlotWaitNotice(fileCtx, lease)

			finalRel := it.RelativePath
			filterSubdir := ""
			if job.AppendFilterSubdir && it.DestinationSubdir != "" {
				filterSubdir = it.DestinationSubdir
				finalRel = filepath.ToSlash(filepath.Join(it.DestinationSubdir, it.RelativePath))
			}

			var dst string
			var skipCheck func() (bool, string, error)

			if useHFCache {
				// HF Cache mode: check blob existence
				skipCheck = func() (bool, string, error) {
					if it.SHA256 != "" {
						status, _, err := repoDir.CheckBlob(it.SHA256)
						if err != nil {
							// The ownership of an existing incomplete
							// download could not be determined (the
							// OS-lock probe failed, or this platform
							// cannot represent the lock). No live owner
							// is proven, so this must NOT become a
							// successful skip: the error travels the
							// normal error channel and the incomplete
							// payload is preserved.
							return false, "", fmt.Errorf("check %s: %w", finalRel, err)
						}
						if status == BlobComplete {
							// The blob exists, but the requested path must also
							// be REACHABLE through its snapshot entry (and its
							// friendly entry). A link-creation failure is an
							// actionable error for this item, exactly like the
							// same failure on the transfer path: the two paths
							// must agree inside one contract, and a job that
							// reports success means the requested files are
							// present AND reachable.
							if err := cacheEntryLinkCreate(fileCtx, repoDir, plan.Commit, it.RelativePath, filterSubdir, it.SHA256, cfg.NoFriendlyView); err != nil {
								return false, "", err
							}
							return true, "blob exists", nil
						}
						if status == BlobDownloading {
							// A PROVEN live writer owns this blob key, so
							// the file is NOT present. This must NOT
							// become a successful skip: a job that
							// reports success must mean the requested
							// files are present. The item proceeds to the
							// blob key's ownership domain, waits for the
							// live owner (bounded) and re-checks the
							// published blob afterwards, so a blob the
							// owner publishes is reused instead of
							// transferred a second time.
							return false, "", nil
						}
					}
					return false, "", nil
				}
				// Download to temp location, will be moved to blob later.
				// Use SHA256 as temp name to avoid collisions (e.g., multiple
				// config.json files). Note: for an LFS blob with a canonical
				// SHA the ranged multipart path does NOT use this dst as its
				// staging location — it writes directly into
				// blobs/<sha>.incomplete (the future blob) and publishes
				// with a rename, so no tmp-<sha> restage ever runs for it.
				// The dst below remains the caller-visible destination key
				// (OnPartialFile registration, cleanup) and the staging
				// target for the single-request path and non-LFS files.
				tmpName := "tmp-" + it.SHA256
				if it.SHA256 == "" {
					// Path-derived names are STRUCTURALLY NON-CANONICAL: the
					// "path-" prefix can never be a 64-hex digest, so this
					// staging name can never look like a content-addressed
					// tmp-<sha> name and its name-based destination domain
					// (blobs/path-<sanitized>) can never reach a blob key or
					// lock. Sanitize path separators to avoid collisions.
					tmpName = "tmp-path-" + strings.ReplaceAll(it.RelativePath, "/", "_")
				}
				// Guard: SafeJoin so the remote-controlled temp name (SHA256
				// or path-derived) cannot escape the blobs directory — the
				// same containment the legacy branch applies to finalRel.
				// SafeJoin's filepath.IsLocal check also makes this barrier
				// visible to CodeQL's go/path-injection analysis.
				safeDst, err := SafeJoin(repoDir.BlobsDir(), tmpName)
				if err != nil {
					fatal := fmt.Errorf("path traversal: %q would escape blobs directory", tmpName)
					setFatalErr(fatal)
					select {
					case errCh <- fatal:
					default:
					}
					return
				}
				dst = safeDst
			} else {
				// Legacy mode: flat directory structure
				base, err := destinationBase(job, cfg)
				if err != nil {
					fatal := fmt.Errorf("path traversal: %w", err)
					setFatalErr(fatal)
					select {
					case errCh <- fatal:
					default:
					}
					return
				}
				// Guard: SafeJoin so finalRel (from HF Hub) cannot escape the
				// output directory via path traversal.
				safeDst, err := SafeJoin(base, finalRel)
				if err != nil {
					fatal := fmt.Errorf("path traversal: %q would escape output directory", finalRel)
					setFatalErr(fatal)
					select {
					case errCh <- fatal:
					default:
					}
					return
				}
				dst = safeDst
				skipCheck = func() (bool, string, error) {
					return shouldSkipLocalCtx(fileCtx, it, dst)
				}
			}

			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				select {
				case errCh <- err:
				default:
				}
				return
			}

			// Check if we can skip
			alreadyOK, reason, err := skipCheck()
			if err != nil {
				select {
				case errCh <- err:
				default:
				}
				return
			}
			if alreadyOK {
				recordSkippedBlobLink(it, finalRel, filterSubdir)
				if _, loaded := skipOnce.LoadOrStore(finalRel, struct{}{}); !loaded {
					emit(ProgressEvent{Event: "file_done", Path: finalRel, Message: "skip (" + reason + ")"})
					atomic.AddInt64(&skippedCount, 1)
					// Add to manifest (skipped files are still part of the download job)
					if manifestBuilder != nil {
						manifestMu.Lock()
						manifestBuilder.AddFile(it.RelativePath, it.SHA256, it.Size, it.LFS)
						manifestMu.Unlock()
					}
				}
				return
			}

			// Register the per-file destination with the caller's in-flight
			// tracker before any bytes hit disk. If the goroutine exits
			// (cancel, pause, error) before the matching finalize call below,
			// the dst stays in the caller's set and is the exact set of
			// partial files the caller may safely remove.
			if cfg.OnPartialFile != nil {
				cfg.OnPartialFile(dst, false)
			}

			emit(ProgressEvent{Event: "file_start", Path: finalRel, Total: it.Size})
			downloadLog.Info("file download started",
				"repo", job.Repo,
				"path", finalRel,
				"bytes", it.Size,
			)

			// Create a copy with updated RelativePath for progress display
			itForIO := it
			itForIO.RelativePath = finalRel

			// Choose single/multipart path. In HF cache mode an LFS file
			// with a canonical blob SHA stages its ranged download directly
			// in blobs/<sha>.incomplete — the future blob itself — so
			// finalization is a rename into blobs/<sha> plus link work,
			// never a tmp-<sha> restage or an assembly copy. Non-LFS files
			// and files without a canonical SHA keep the tmp-<sha> staging
			// and the store path.
			blobDirect := useHFCache && canonicalSHA256(it.SHA256)
			var mpOpts multipartOpts
			// The resolved plan commit is part of the staging record's resume
			// identity. The requested branch name is not enough: a moved
			// branch could otherwise resume old bytes in Verify=size mode.
			// The pinned flag says whether that commit was actually resolved;
			// an unresolved revision pins nothing, so no bytes may be
			// inherited from it.
			mpOpts.Commit = plan.Commit
			mpOpts.CommitPinned = plan.CommitPinned
			versionedStaging := dst + ".incomplete"
			if blobDirect {
				mpOpts.Staging = repoDir.IncompletePath(it.SHA256)
				mpOpts.Publish = repoDir.BlobPath(it.SHA256)
				// Only a blob-keyed target proves completion by existence; an
				// arbitrary destination keeps the caller's pre-download skip
				// decision authoritative.
				mpOpts.ContentAddressed = true
				versionedStaging = mpOpts.Staging
			}
			// Existing multipart resume state must not be ignored (and later
			// deleted) just because the current threshold routes a small item to
			// the single-request path. Detect it before choosing the route; a
			// ranged-capable item keeps the multipart route so migration/resume
			// consumes it. A fresh download with no state is unchanged.
			resumeState := multipartResumeState{
				legacy: hasUsableLegacyMultipartState(dst, it.Size),
			}
			if hasResumableStagingState(versionedStaging, versionedStaging+".meta", itForIO, job, plan.Commit, plan.CommitPinned) {
				resumeState.versionedStaging = versionedStaging
			}
			multipartRan := it.Size >= thresholdBytes && it.AcceptRanges
			if !multipartRan && it.AcceptRanges && (resumeState.legacy || resumeState.versionedStaging != "") {
				multipartRan = true
			}
			// A server/plan without range support cannot consume multipart
			// state. Do not silently delete it: warn, download the full file
			// through the single path, and remove the obsolete versioned pair
			// only after the final state is durable (claim-checked). The legacy
			// family is removed by the single-path success cleanup.
			obsoleteVersionedStaging := ""
			if !it.AcceptRanges && resumeState.versionedStaging != "" {
				obsoleteVersionedStaging = resumeState.versionedStaging
			}
			if !it.AcceptRanges && (resumeState.legacy || resumeState.versionedStaging != "") {
				emit(ProgressEvent{Level: "warn", Event: "warning", Message: fmt.Sprintf(
					"resume state for %s cannot be used without HTTP range support; downloading the full file and attempting to discard the obsolete state after success",
					finalRel)})
			}

			// The DESTINATION's ownership domain.
			//
			// Every cache destination owns exactly ONE name-based domain:
			// a destination is blobs/tmp-<name>, its domain key is
			// blobs/<name> and its domain lock is the persistent sibling
			// blobs/tmp-<name>.lock. The writer and the cleanup of that
			// destination derive the same identity.
			//
			// The two item classes are kept in separate staging SHAPES, so
			// the name-based derivation cannot collide with a blob key:
			//
			//   - a content-addressed item stages at blobs/tmp-<sha>; its
			//     name is the canonical SHA-256, so the domain is EXACTLY the
			//     blob key's domain (key blobs/<sha>, lock
			//     blobs/tmp-<sha>.lock) and the publication's holdsBlobKey
			//     check keeps working unchanged.
			//
			//   - a path-derived item (no supplied digest, which the planner
			//     allows) stages at blobs/tmp-path-<sanitized path>. "path-"
			//     is not a hex digest, so its domain key is
			//     blobs/path-<sanitized path> and its lock is
			//     blobs/tmp-path-<sanitized path>.lock. Neither identity can
			//     be reached by a canonical blob key/lock, and the staging
			//     name can never equal a content-addressed tmp-<sha> name.
			//     That is what prevents two digest-less items whose paths are
			//     each other's content digests from holding the blob domains
			//     each other's store needs, and it removes the staging-name
			//     collision by construction.
			//
			// The domain is held for the whole operation on the destination:
			// the partial namespace (so no other writer can adopt, migrate or
			// delete this item's LIVE partial) and the publication (so the
			// existence decision and the placement that follows it cannot be
			// overtaken).
			//
			// A PROVEN live owner of the destination is WAITED FOR (bounded
			// and context-aware), not skipped: the file is not present while
			// the owner holds the destination, so a skip would report a
			// success the cache does not have.
			//
			// A FLAT/output-mode destination owns a domain as well. That
			// namespace has no blobs/-style identity of its own, so the
			// domain borrows the MULTIPART staging name: the destination's
			// domain lock is the persistent sibling of
			// <destination>.incomplete, which is the very lock the multipart
			// route's staging claim already takes. Both routes and the
			// cleanup therefore resolve ONE lock file, and no new artifact
			// name appears next to the user's model file.
			var (
				dlErr  error
				domain *blobDomain
			)
			if useHFCache {
				key, keyed := cacheDstDomainKey(dst)
				if !keyed {
					// A cache destination always has a domain. A destination
					// that cannot be mapped to one would run without the
					// exclusion its cleanup relies on, so the item fails
					// instead of writing unprotected bytes.
					dlErr = fmt.Errorf("resolve the ownership domain of %s: the destination is not a cache staging name", finalRel)
				} else {
					dom, derr := blobDomainAcquire(fileCtx, key, dst)
					switch {
					case derr == nil:
						domain = dom
						defer domain.release()
						if blobDirect {
							// The ranged publication needs the domain of the blob key
							// it publishes: the domain-exclusive placement tier
							// (a filesystem without hard links) is only exclusive to
							// the domain while it is held.
							mpOpts.Domain = domain
						}
						// The published content-addressed target is the completion
						// proof, so a blob published while this writer waited is
						// reused instead of transferred again.
						//
						// A path-derived destination has no such proof: its
						// completed bytes are moved into a content-addressed blob
						// whose name is the digest computed at store time, which
						// this writer cannot know before it transfers the file.
						// The item therefore OBTAINS the file itself (these are
						// small non-LFS files), and the store then reuses the
						// already published blob instead of replacing it.
						if it.SHA256 != "" {
							blobPath := repoDir.BlobPath(it.SHA256)
							if _, serr := os.Stat(blobPath); serr == nil {
								dlErr = errBlobAlreadyComplete
							}
						}
					case errors.Is(derr, errStagingOwnershipBusy):
						// A PROVEN live writer still holds the destination's
						// ownership domain after the bounded wait. The file is
						// NOT present, so this is an actionable failure for this
						// item - never a successful skip and never a silent
						// success.
						dlErr = blobLiveOwnerError(it.SHA256, finalRel)
					default:
						// The lifetime ownership of this destination is
						// unavailable: the OS lock is unsupported, denied, or the
						// filesystem failed. The operation fails HERE, before the
						// transfer writes any payload. An operation that cannot
						// own its destination must not write bytes for it, and
						// the partial data of an earlier run is preserved
						// untouched.
						dlErr = fmt.Errorf("acquire the ownership domain of %s: %w", finalRel, derr)
					}
				}
			} else {
				// Flat/output mode: the destination owns ONE domain too, and
				// both routes and the cleanup resolve that ONE identity.
				//
				// The domain's lifetime lock is the persistent sibling of the
				// MULTIPART staging name: <destination>.incomplete.lock - the
				// very lock the multipart route's staging claim already takes.
				// Reusing it is what excludes the two routes from each other
				// (and the cleanup from both) without a SECOND artifact next
				// to the user's model file: the lock file is the one the
				// multipart route already leaves there.
				flatDom := newFlatDestinationDomain(dst)
				// The multipart route needs the domain only for the hand-off
				// to the single-request path, which happens before it claims
				// the staging.
				mpOpts.FlatDomain = flatDom
				//
				// DEADLOCK RULE: the domain is taken HERE only for the route
				// that does NOT take it internally. The multipart route's
				// claim takes <destination>.incomplete.lock from inside
				// downloadMultipart, and an exclusive lock conflicts even
				// within one process when a second descriptor is used, so
				// taking the same lock here would make the writer contend
				// with ITSELF. The multipart route therefore keeps relying on
				// its staging claim - which is the same lock file - and
				// single-vs-multipart and multipart-vs-multipart are excluded
				// by that one file.
				//
				// A PROVEN live owner of the destination is WAITED FOR
				// (bounded and context-aware), never skipped: the file is not
				// present while the owner holds the destination, so a skip
				// would report a success the output directory does not have.
				if !multipartRan {
					dom, derr := blobDomainAcquire(fileCtx, flatDom.key, flatDom.lockSrc)
					switch {
					case derr == nil:
						domain = dom
						defer domain.release()
					case errors.Is(derr, errStagingOwnershipBusy):
						// A PROVEN live writer still holds the destination's
						// ownership domain after the bounded wait. The file
						// is NOT present, so this is an actionable failure for
						// this item - never a successful skip and never a
						// silent success.
						dlErr = blobLiveOwnerError(it.SHA256, finalRel)
					default:
						// The lifetime ownership of this destination is
						// unavailable: the OS lock is unsupported, denied, or
						// the filesystem failed. The operation fails HERE,
						// before the transfer writes any payload, and the
						// partial data of an earlier run is preserved.
						dlErr = fmt.Errorf("acquire the ownership domain of %s: %w", finalRel, derr)
					}
				}
			}

			directBlobPublished := blobDirect
			if blobDirect && multipartRan {
				mpOpts.DirectBlobPublished = &directBlobPublished
			}
			// contentVerified reports that the downloader already proved the
			// payload against it.SHA256 inside this call, before the publish
			// rename (one pass at most): the ranged path's hash-on-write/final/
			// legacy proof, or the single path's pre-publication proof. For a
			// known digest this is always true on success in Verify=sha256 and
			// Verify=etag modes, so the caller must not repeat the hash (a
			// post-publication hash could only observe corrupt published
			// bytes). HEAD-ETag-only verification with no known digest remains
			// the documented post-publication exception.
			var contentVerified bool
			if dlErr == nil {
				if multipartRan {
					contentVerified, dlErr = downloadMultipart(fileCtx, httpc, cfg.Token, job, cfg, itForIO, dst, mpOpts, emit)
				} else {
					contentVerified, dlErr = downloadSingle(fileCtx, httpc, cfg.Token, job, cfg, itForIO, dst,
						singleOpts{Commit: plan.Commit, CommitPinned: plan.CommitPinned}, emit)
				}
			}
			if blobDirect && multipartRan && !directBlobPublished {
				// downloadMultipart delegated an unknown-size item to the
				// single-request temp path, so storage must move that temp into
				// the blob and create this item's links.
				multipartRan = false
			}

			// A published content-addressed blob is the completion proof:
			// finish this item as an ordinary skip, exactly like the
			// pre-download skip check does. The links for THIS item's path
			// are created, so the requested file is present in the cache.
			if errors.Is(dlErr, errBlobAlreadyComplete) {
				if useHFCache && it.SHA256 != "" {
					// The blob is complete; make sure this file's snapshot
					// and friendly entries exist for its own path (a
					// cross-repo dedup winner may have created entries only
					// for its path). A link-creation failure is an actionable
					// error for this item - the blob is present but the
					// requested path is not reachable - and never a silent
					// success.
					if err := cacheEntryLinkCreate(fileCtx, repoDir, plan.Commit, it.RelativePath, filterSubdir, it.SHA256, cfg.NoFriendlyView); err != nil {
						fatal := fmt.Errorf("download %s: %w", finalRel, err)
						select {
						case errCh <- fatal:
						default:
						}
						return
					}
				}
				recordSkippedBlobLink(it, finalRel, filterSubdir)
				if cfg.OnPartialFile != nil {
					cfg.OnPartialFile(dst, true)
				}
				if _, loaded := skipOnce.LoadOrStore(finalRel, struct{}{}); !loaded {
					emit(ProgressEvent{Event: "file_done", Path: finalRel, Message: "skip (blob exists)"})
					atomic.AddInt64(&skippedCount, 1)
					if manifestBuilder != nil {
						manifestMu.Lock()
						manifestBuilder.AddFile(it.RelativePath, it.SHA256, it.Size, it.LFS)
						manifestMu.Unlock()
					}
				}
				return
			}
			// A live writer that still owns this item's staging is NOT a
			// successful skip: the file is not present, so the item fails
			// below with an actionable error instead of reporting a
			// completion the cache does not have.
			//
			// An UNPROVEN record (a legacy/unfenced one, or one that cannot
			// be read) keeps its own actionable message: no live owner is
			// proven for it, so it must not be reported as "being
			// downloaded by another process", and its message names the
			// staging pair the operator has to remove to retry.
			if errors.Is(dlErr, errBlobIncompleteOwned) && !errors.Is(dlErr, ErrStagingOwnershipUnproven) {
				dlErr = blobLiveOwnerError(it.SHA256, finalRel)
			}
			if dlErr != nil {
				fatal := fmt.Errorf("download %s: %w", finalRel, dlErr)
				if errors.Is(dlErr, context.Canceled) || errors.Is(dlErr, context.DeadlineExceeded) {
					downloadLog.Info("file download cancelled", "repo", job.Repo, "path", finalRel)
				} else {
					downloadLog.Error("file download failed",
						"repo", job.Repo,
						"path", finalRel,
						"error", dlErr,
					)
				}
				if isFailFastError(dlErr) {
					setFatalErr(fatal)
				}
				select {
				case errCh <- fatal:
				default:
				}
				return
			}

			// The bytes are on disk. A strict verification or storing into
			// the cache can still take local I/O time for a large file,
			// during which no file_progress arrives. Mark the file as
			// finalizing so the job doesn't look stuck at 100% (the
			// post-loop "finalizing" event only covers work after every
			// file is done).
			emit(ProgressEvent{Event: "file_finalizing", Path: finalRel, Message: "verifying"})

			// Post-download verification. Settings.Verify controls LFS
			// verification too (issue #140 — deliberate semantics change),
			// scoped to the normal transfer/finalization path:
			//
			//   none   — no post-download content hash; HTTP/range
			//            protocol correctness still applied during transfer.
			//   size   — (default) the exact expected-size invariant, plus
			//            the complete validated transfer coverage both paths
			//            already enforced; NO full-file SHA reread on the
			//            normal path. Three pre-existing, documented
			//            exceptions may still hash: the pre-download skip
			//            check of an already-present file, the one-time
			//            legacy-migration content proof, and the proof of
			//            inherited single-path .part bytes — all before
			//            publication.
			//   sha256 — strict full-file SHA-256 against the known digest,
			//            at most one pass, always before publication for a
			//            known digest: the engine verified hash-on-write
			//            (fresh sequential) or with one pass before the
			//            rename. A mismatch never reaches dst/blobs/<sha>.
			//   etag   — historical compatibility: a known SHA-256 digest is
			//            proven with one pass before publication (the engine
			//            reports contentVerified, so it is not repeated here).
			//            Non-LFS ETag-only files have no known digest and keep
			//            their historical no-post-check behavior. (The
			//            HEAD-ETag fallback exists only in "sha256" mode; with
			//            no known digest it remains a post-publication check
			//            and can report a mismatch then.)
			//
			// Other values keep their historical compatible behavior for
			// non-LFS files below.
			verifyPath := dst
			if multipartRan && blobDirect {
				verifyPath = mpOpts.Publish
			}
			if it.LFS && it.SHA256 != "" {
				switch cfg.Verify {
				case "none":
					// No post-download content hash.
				case "sha256", "etag":
					// Known-digest proof is a pre-publication engine gate.
					// contentVerified should therefore be true on success;
					// the fallback stays for direct callers of unusual legacy
					// configurations, but it must never be the normal path for
					// a known digest (a post-publication hash cannot undo a
					// published mismatch).
					if !contentVerified {
						if err := verifySHA256Ctx(fileCtx, verifyPath, it.SHA256); err != nil {
							downloadLog.Warn("verification failed",
								"repo", job.Repo,
								"path", finalRel,
								"method", "sha256",
								"error", err,
							)
							select {
							case errCh <- fmt.Errorf("sha256 verify failed: %s: %w", finalRel, err):
							default:
							}
							return
						}
					}
				default:
					// "size" (the default) and any other legacy value: the
					// exact-size invariant. A preallocated staging file can
					// never satisfy this by size alone — the multipart path
					// only reaches here with every range committed, and the
					// single path wrote the file sequentially.
					if it.Size > 0 {
						fi, err := os.Stat(verifyPath)
						if err != nil || fi.Size() != it.Size {
							select {
							case errCh <- fmt.Errorf("size mismatch for %s", finalRel):
							default:
							}
							return
						}
					}
				}
			} else if cfg.Verify == "size" && it.Size > 0 {
				fi, err := os.Stat(verifyPath)
				if err != nil || fi.Size() != it.Size {
					downloadLog.Warn("verification failed",
						"repo", job.Repo,
						"path", finalRel,
						"method", "size",
						"expected_bytes", it.Size,
					)
					select {
					case errCh <- fmt.Errorf("size mismatch for %s", finalRel):
					default:
					}
					return
				}
			} else if cfg.Verify == "sha256" && !contentVerified {
				_, remoteSha, herr := headForETag(fileCtx, httpc, cfg.Token, itForIO)
				if herr != nil {
					// headForETag maps every non-2xx status (401/403
					// included) to an optional-metadata miss with no error,
					// so the ordinary miss path falls through and the actual
					// file request stays authoritative. Only a genuine
					// caller cancellation surfaces here, and it must fail
					// this file promptly instead of pretending verification
					// succeeded; a fail-fast status error, should one ever
					// surface, is escalated job-fatal below.
					downloadLog.Warn("verification probe failed",
						"repo", job.Repo,
						"path", finalRel,
						"error", herr,
					)
					if isFailFastError(herr) {
						setFatalErr(fmt.Errorf("verify head %s: %w", finalRel, herr))
					}
					select {
					case errCh <- fmt.Errorf("verify head %s: %w", finalRel, herr):
					default:
					}
					return
				}
				if remoteSha != "" {
					if err := verifySHA256Ctx(fileCtx, verifyPath, remoteSha); err != nil {
						downloadLog.Warn("verification failed",
							"repo", job.Repo,
							"path", finalRel,
							"method", "sha256",
							"error", err,
						)
						select {
						case errCh <- fmt.Errorf("sha256 verify failed: %s: %w", finalRel, err):
						default:
						}
						return
					}
				}
			}

			// Move the completed bytes into the cache and create the
			// snapshot/friendly entries. The store may hash a large file,
			// so pass the per-file context: a sibling's fail-fast abort must
			// not wait for it to finish.
			var finalSHA256 string
			switch {
			case blobDirect && multipartRan:
				// The ranged downloader already renamed blobs/<sha>.incomplete
				// onto blobs/<sha> (rename-only publish). Only the entries
				// remain — metadata work, no payload copy.
				//
				// The entries are created through the SAME helper the skip
				// paths use, so a link-creation failure carries the exported
				// ErrCacheLinkUnreachable sentinel on every path that creates
				// the cache entries, and the item fails the same way
				// everywhere: the payload may be in blobs/, but the requested
				// path is not reachable through it. The friendly entry stays
				// deliberately absent when the friendly view is disabled,
				// which is not an error.
				if err := cacheEntryLinkCreate(fileCtx, repoDir, plan.Commit, it.RelativePath, filterSubdir, it.SHA256, cfg.NoFriendlyView); err != nil {
					select {
					case errCh <- fmt.Errorf("store file %s: %w", finalRel, err):
					default:
					}
					return
				}
				finalSHA256 = it.SHA256
			case useHFCache:
				sha := it.SHA256
				// The store publishes into the blob key's ownership domain,
				// which this item already holds: the existence decision and
				// the placement that follows it are one serialized step.
				result, err := repoDir.storeDownloadedFileInDomain(fileCtx, dst, it.RelativePath, plan.Commit, sha, filterSubdir, cfg.NoFriendlyView, domain)
				if err != nil {
					select {
					case errCh <- fmt.Errorf("store file %s: %w", finalRel, err):
					default:
					}
					return
				}
				finalSHA256 = result.SHA256 // Use computed SHA256 from store result
			default:
				finalSHA256 = it.SHA256
			}

			// The single path may have been chosen because the server/plan has
			// no range support while usable versioned staging state existed. The
			// final file is now durable (and, for cache mode, stored); remove the
			// obsolete pair claim-checked so a live owner's state is never deleted
			// and no stale pair survives a successful download.
			if obsoleteVersionedStaging != "" {
				if domain.holdsStagingLock(obsoleteVersionedStaging) {
					// The caller already holds this staging pair's lifetime
					// lock - in flat/output mode the destination's domain
					// lock IS that lock - so it must not be taken again: a
					// second exclusive open of the same lock file conflicts
					// even inside one process, and the acquisition would
					// report the caller's OWN hold as a live owner and skip
					// the removal.
					removeStagingArtifactsIfUnclaimedLocked(obsoleteVersionedStaging, obsoleteVersionedStaging+".meta")
				} else {
					removeStagingArtifactsIfUnclaimed(obsoleteVersionedStaging, obsoleteVersionedStaging+".meta")
				}
			}

			// File is now at its final location; the partial (dst in HF
			// cache, dst+".part*" in legacy) is gone. Deregister so the
			// caller's in-flight set reflects "this dst is no longer
			// partial". Errors below this point do not un-finalize; the
			// file is on disk and the only thing at risk is metadata.
			if cfg.OnPartialFile != nil {
				cfg.OnPartialFile(dst, true)
			}

			// Add to manifest with actual LFS info from API and final SHA256
			if manifestBuilder != nil {
				manifestMu.Lock()
				manifestBuilder.AddFile(it.RelativePath, finalSHA256, it.Size, it.LFS)
				manifestMu.Unlock()
			}

			emit(ProgressEvent{Event: "file_done", Path: finalRel})
			downloadLog.Info("file download complete",
				"repo", job.Repo,
				"path", finalRel,
				"bytes", it.Size,
			)
			atomic.AddInt64(&downloadedCount, 1)
		}()
	}

	wg.Wait()

	// Reconcile skipped shared-blob paths once every writer has finished. A
	// same-SHA/different-path item that skipped via BlobDownloading or a lost
	// staging claim only becomes linkable after the winning writer publishes;
	// create its own snapshot and friendly entries now so a shared blob does
	// not leave later plan paths without cache entries.
	//
	// A link that cannot be created leaves the requested path unreachable, so
	// the reconciliation reports it as a JOB ERROR instead of only a warning:
	// a job must not finish "successful" with a missing path. The other duties
	// of the reconciliation are unchanged: a link whose blob is not published
	// yet is still skipped, and the remaining links are still reconciled.
	var reconcileErr error
	if useHFCache && repoDir != nil {
		skippedLinksMu.Lock()
		links := append([]skippedBlobLink(nil), skippedLinks...)
		skippedLinksMu.Unlock()
		for _, link := range links {
			status, _, cerr := repoDir.CheckBlob(link.sha256)
			if cerr != nil || status != BlobComplete {
				continue
			}
			if err := cacheEntryLinkCreate(jobCtx, repoDir, link.commit, link.relativePath, link.filterSubdir, link.sha256, cfg.NoFriendlyView); err != nil {
				if reconcileErr == nil {
					reconcileErr = err
				}
				continue
			}
		}
	}

	close(errCh)

	// Drain errors, but remember every error so the job-fatal error (if any)
	// can take precedence over a sibling goroutine's "context canceled" that
	// happened to land in errCh first.
	drainedErr := error(nil)
	for e := range errCh {
		if e != nil && drainedErr == nil {
			drainedErr = e
		}
	}

	fatalMu.Lock()
	jobFatal := fatalErr
	fatalMu.Unlock()

	// The original permanent (fail-fast) error is surfaced in preference to
	// anything a sibling produced while unwinding from the abort.
	if jobFatal != nil {
		emit(ProgressEvent{Level: "error", Event: "error", Message: jobFatal.Error()})
		return jobFatal
	}
	if drainedErr != nil {
		emit(ProgressEvent{Level: "error", Event: "error", Message: drainedErr.Error()})
		return drainedErr
	}
	// A link the end-of-job reconciliation could not create leaves the
	// requested path unreachable, so the job fails instead of reporting
	// success with a missing path.
	if reconcileErr != nil {
		emit(ProgressEvent{Level: "error", Event: "error", Message: reconcileErr.Error()})
		return reconcileErr
	}

	if ctx.Err() != nil {
		return ctx.Err()
	}

	// Post-download finalization (refs, building the friendly view, rebuild
	// script, manifest) can take a while — copying the friendly view on
	// filesystems without symlink support is the slow part — so signal the
	// phase to the UI instead of leaving the job stuck at 100%.
	if useHFCache && repoDir != nil && !cfg.NoFriendlyView {
		emit(ProgressEvent{Event: "finalizing", Message: "finalizing"})
	}

	// For HF Cache mode: write ref file and ensure friendly directory exists
	if useHFCache && repoDir != nil {
		// Write refs/main (or the revision used)
		ref := job.Revision
		if ref == "" {
			ref = "main"
		}
		if err := repoDir.WriteRef(ref, plan.Commit); err != nil {
			emit(ProgressEvent{Level: "warn", Event: "warning", Message: fmt.Sprintf("failed to write ref: %v", err)})
		}
		// Ensure friendly directory structure exists (unless disabled)
		if !cfg.NoFriendlyView {
			if err := repoDir.EnsureFriendlyDir(); err != nil {
				emit(ProgressEvent{Level: "warn", Event: "warning", Message: fmt.Sprintf("failed to create friendly dir: %v", err)})
			}
		}
	}

	// Write/update the rebuild shell script if using HF cache (unless friendly view disabled)
	if hfCache != nil && !cfg.NoFriendlyView {
		if _, err := hfCache.WriteRebuildScript(); err != nil {
			emit(ProgressEvent{Level: "warn", Event: "warning", Message: fmt.Sprintf("failed to write rebuild script: %v", err)})
		}
	}

	// Write manifest file (hfd.yaml) if using HF cache (unless friendly view disabled)
	if manifestBuilder != nil && repoDir != nil && !cfg.NoFriendlyView {
		manifest := manifestBuilder.Build()
		if _, err := manifest.Write(repoDir.FriendlyPath()); err != nil {
			emit(ProgressEvent{Level: "warn", Event: "warning", Message: fmt.Sprintf("failed to write manifest: %v", err)})
		}
	}

	emit(ProgressEvent{
		Event:   "done",
		Message: fmt.Sprintf("download complete (downloaded %d, skipped %d)", downloadedCount, skippedCount),
	})
	downloadLog.Info("download complete",
		"repo", job.Repo,
		"downloaded", downloadedCount,
		"skipped", skippedCount,
	)
	return nil
}

// cleanupPartialsOnCancel removes the partial download artifacts for dst
// when the caller's cancel-cleanup callback reports that the context was
// cancelled for a non-resumable reason (explicit cancel, not pause). For
// the single-file path (staging == "") that is dst+".part" plus any legacy
// multipart artifacts; for the ranged path it also removes the staging
// file and its resume metadata, so an explicitly-cancelled job leaves no
// resumable bytes behind. The ranged path uses applyCancelCleanup instead so
// the staging pair is removed only while the claim record still names this
// process instance.
func cleanupPartialsOnCancel(cfg Settings, dst, staging string) {
	if cfg.CleanupPartialsOnCancel == nil || !cfg.CleanupPartialsOnCancel() {
		return
	}
	// dst is derived upstream in Download() through SafeJoin in both modes:
	// legacy mode joins the remote file path to the output directory, and HF
	// cache mode joins the tmp-<sha> name to the blobs directory. SafeJoin
	// rejects non-local values via filepath.IsLocal and proves containment
	// with PathInside, so dst cannot name a file outside those roots. The
	// staging path is either dst+".incomplete" or a RepoDir blob path
	// derived from the canonical SHA the plan validated.
	cleanupLegacyPartials(dst)
	if staging != "" {
		removeStagingArtifacts(staging, staging+".meta")
	}
}

// cleanupLegacyPartials removes the pre-#140 multipart family and the single
// dst+".part" prefix for dst. It never touches the ranged staging pair. The
// single-request identity sidecar goes with the partial it describes.
func cleanupLegacyPartials(dst string) {
	removePartialWithIdentity(dst)
	if err := removeMultipartResumeFiles(dst); err != nil {
		downloadLog.Warn("cleanup multipart resume files failed", "path", dst, "error", err)
	}
}

// removePartialWithIdentity removes the single-request partial of dst and the
// identity sidecar that describes it.
//
// The sidecar goes only when the partial is actually GONE. An identity record
// must not outlive the bytes it describes, and a partial that SURVIVES must
// keep its record: without it the next run cannot prove which object the
// bytes on disk belong to, so it discards them and the resumable progress is
// lost even though the bytes are still there.
func removePartialWithIdentity(dst string) {
	// lgtm[go/path-injection]
	if err := os.Remove(dst + ".part"); err != nil && !os.IsNotExist(err) {
		downloadLog.Warn("cleanup partial file failed", "path", dst+".part", "error", err)
		return
	}
	removeSinglePartIdentity(singlePartIdentityPath(dst))
}

// CleanupJobPartFiles removes the partial download artifacts
// associated with the given list of per-file destinations. The
// caller is responsible for tracking which dsts belong to a given
// run via Settings.OnPartialFile; this helper then removes exactly
// those dsts' partial files without scanning the repo, so a
// concurrent job downloading into the same blobs directory or
// output subtree is not disturbed.
//
// For each dst, the helper removes the partial files the downloader
// would have produced for that dst, covering both artifact families:
//
//   - single path: dst+".part";
//   - legacy multipart (pre-#140): dst+".part-NN" (any digit run) and
//     dst+".parts.json";
//   - ranged path (issue #140): dst+".incomplete" and
//     dst+".incomplete.meta" — and, in HF cache mode where the ranged
//     path stages an LFS blob under its blob key instead of the dst
//     name, the derived blobs/<sha>.incomplete and .meta for a
//     tmp-<sha> dst.
//
// In HF cache mode, dst is the temporary tmp-<sha> path under blobs/, so
// the helper also removes dst itself for the window after the single
// downloader has renamed the completed bytes to tmp-<sha> but before
// StoreDownloadedFile has moved them into the final blob.
//
// OWNERSHIP: in HF cache mode every artifact above belongs to ONE cache
// destination, and the destination's ownership domain is what a writer of
// that destination holds while it works on it. The artifacts are therefore
// removed only while that domain is held: a cleanup that removed them
// without the domain could delete the LIVE partial of another writer (a
// cancelled job whose destination meanwhile became owned by another job),
// and that writer would keep writing to an unlinked inode and fail its later
// publication with ENOENT. When a live owner holds the destination - in this
// process or in another one - the artifacts are left alone and the contention
// is reported. The job that owns the destination can still clean up its own
// artifacts: its own writer releases the domain when it returns, and the
// downloader's own cancel cleanup removes its staging while it still holds
// the domain.
//
// The domain identity is the SAME for the writer and for the cleanup: it is
// derived from the destination name (blobs/tmp-<name> -> domain key
// blobs/<name>, lock file blobs/tmp-<name>.lock). For a content-addressed
// destination the name is the SHA-256, so the domain is the blob key's domain.
// For a path-derived destination the downloader allocates the structurally
// non-canonical staging name blobs/tmp-path-<sanitized path>, so the key is
// blobs/path-<sanitized path>: "path-" cannot be a canonical digest, so this
// domain can never equal a blob key or lock, and the staging name can never
// equal a content-addressed tmp-<sha> name. The cleanup therefore protects a
// path-derived destination exactly like a content-addressed one while keeping
// ONE name-based identity per destination.
//
// A legacy/output-mode destination is not cache-scoped: it has no blobs/
// style internal identity of its own. It nevertheless owns ONE domain, and
// that domain borrows the MULTIPART staging name: the destination's domain
// lock is the persistent sibling of <destination>.incomplete, which is the
// very lock the multipart route's staging claim already takes. Both routes
// and the cleanup therefore resolve the SAME lock file, so no new artifact
// name appears next to the user's model file, and the cleanup protects a
// flat destination exactly like a cache one: a live owner of the destination
// keeps its .part/.incomplete/.meta artifacts, and the job that owns the
// destination can still clean up its own.
//
// settings determines the mode (HF cache vs legacy) the same way
// Download() does: HF cache is the default when neither CacheDir nor
// OutputDir is set.
//
// All operations are best-effort: missing files are not an error, and
// individual file removal failures are logged and skipped.
func CleanupJobPartFiles(settings Settings, dsts []string) error {
	useHFCache := settings.CacheDir != "" || settings.OutputDir == ""
	root := cleanupRoot(settings, useHFCache)
	for _, dst := range dsts {
		if !pathWithinRoot(root, dst) {
			downloadLog.Warn("skip cleanup for out-of-root partial destination", "path", dst)
			continue
		}
		if useHFCache {
			// The destination namespace: the staging name, its single-request
			// .part, the legacy .part-NN parts and the blob-keyed staging
			// pair all belong to ONE destination. They are removed only while
			// that destination's ownership domain is held - the SAME
			// name-based domain its writer holds: destination
			// blobs/tmp-<name> means key blobs/<name> and lock
			// blobs/tmp-<name>.lock. A path-derived destination is staged at
			// blobs/tmp-path-<sanitized path>, so its key is
			// blobs/path-<sanitized path>; "path-" cannot be a canonical
			// digest, so the writer and this cleanup never derive a blob key.
			if key, ok := cacheDstDomainKey(dst); ok {
				domain, derr := blobDomainCleanupAcquire(context.Background(), key, dst)
				if derr != nil {
					// A live owner holds the destination (in this process or
					// in another one), or its ownership could not be
					// established. The artifacts are left alone: not
					// deleting is the safe choice, because the owner may
					// be writing them right now.
					downloadLog.Warn("skip cleanup of a destination owned by another live writer",
						"path", dst, "error", derr)
					continue
				}
				removePartialArtifacts(dst)
				removeHFCacheTemp(dst)
				removeBlobStagingForTemp(dst)
				domain.release()
				continue
			}
		} else {
			// Flat/output mode: the destination's artifacts are removed only
			// while THAT destination's ownership domain is held - the same
			// domain its writer holds, and the same lock file
			// (<destination>.incomplete.lock) that excludes the single-request
			// route from the multipart route.
			//
			// Without the domain, a cleanup could delete the LIVE partial of
			// another writer (a cancelled job whose destination meanwhile
			// became owned by another job), and that writer would keep writing
			// to an unlinked inode and fail its later publication with ENOENT.
			// When a live owner holds the destination - in this process or in
			// another one - the artifacts are left alone and the contention is
			// reported.
			//
			// The job that owns the destination can still clean up its own
			// artifacts: its own writer releases the domain when it returns,
			// and the downloader's own cancel cleanup removes its staging
			// while it still holds the domain.
			flatDom := newFlatDestinationDomain(dst)
			domain, derr := blobDomainCleanupAcquire(context.Background(), flatDom.key, flatDom.lockSrc)
			if derr != nil {
				downloadLog.Warn("skip cleanup of a destination owned by another live writer",
					"path", dst, "error", derr)
				continue
			}
			if err := removePartialArtifactsOwned(dst); err != nil {
				downloadLog.Warn("cleanup partials failed", "path", dst, "error", err)
			}
			domain.release()
			continue
		}
		if err := removePartialArtifacts(dst); err != nil {
			downloadLog.Warn("cleanup partials failed", "path", dst, "error", err)
		}
		if useHFCache {
			removeHFCacheTemp(dst)
			removeBlobStagingForTemp(dst)
		}
	}
	return nil
}

// removeBlobStagingForTemp removes the blob-keyed staging artifacts the
// ranged downloader creates for an LFS item whose HF-cache dst is the
// tmp-<sha> temporary: blobs/<sha>.incomplete and its .meta. The staging
// file lives under the blob key (so it IS the future blob), while the
// caller-tracked dst keeps the tmp-<sha> name; this derivation bridges the
// two without widening the OnPartialFile protocol. Dsts that are not
// tmp-<canonical-sha> temporaries are left alone — a coincidentally named
// file cannot contribute a canonical SHA, so nothing outside the
// downloader's own naming scheme is ever touched.
//
// The pair is removed only when no live claim remains (in-process registry,
// then the lifetime OS lock and the record owner evidence), so cancelling
// one job can never delete another live job's staging file.
func removeBlobStagingForTemp(dst string) {
	base := filepath.Base(dst)
	sha, ok := strings.CutPrefix(base, "tmp-")
	if !ok || sha == "" || !canonicalSHA256(sha) {
		return
	}
	// lgtm[go/path-injection]
	staging := filepath.Join(filepath.Dir(dst), sha+".incomplete")
	removeStagingArtifactsIfUnclaimed(staging, staging+".meta")
}

func cleanupRoot(settings Settings, useHFCache bool) string {
	if useHFCache {
		cache, err := settings.BuildHFCache()
		if err != nil {
			return "" // Invalid settings must never widen cleanup's root.
		}
		return cache.HubDir()
	}
	return settings.OutputDir
}

func pathWithinRoot(root, path string) bool {
	if root == "" || path == "" {
		return false
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	if resolvedRoot, err := filepath.EvalSymlinks(absRoot); err == nil {
		absRoot = resolvedRoot
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	if resolvedParent, err := filepath.EvalSymlinks(filepath.Dir(absPath)); err == nil {
		absPath = filepath.Join(resolvedParent, filepath.Base(absPath))
	}
	rel, err := filepath.Rel(absRoot, absPath)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// removeHFCacheTemp removes the completed-but-not-yet-stored tmp-<sha>
// file at dst. The downloader normally writes partial bytes to
// dst+".part" / dst+".part-NN" first, then renames assembled bytes to
// dst before StoreDownloadedFile moves them into the final blob. dst is
// allocated in Download() via SafeJoin(blobsDir, tmpName), which rejects
// non-local values (filepath.IsLocal) and proves containment with
// PathInside, and scanRepo rejects malformed remote SHAs before they can
// become a tmpName at all.
func removeHFCacheTemp(dst string) {
	// lgtm[go/path-injection]
	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		downloadLog.Warn("cleanup partial file failed", "path", dst, "error", err)
	}
}

// removePartialArtifacts removes all partial-file artifacts associated
// with a single downloader dst, covering every artifact family the
// downloader produces:
//
//   - the single-path partial (dst+".part") and its identity sidecar
//     (dst+".part.meta");
//   - legacy multipart parts (dst+".part-NN" for N any non-empty run of
//     digits, matching the pre-#140 downloader's %02d/%d format) and the
//     legacy layout metadata (dst+".parts.json");
//   - the ranged staging file (dst+".incomplete") and its resume metadata
//     (dst+".incomplete.meta").
//
// The blob-keyed staging an LFS ranged download uses in HF cache mode
// (blobs/<sha>.incomplete) is derived separately by
// removeBlobStagingForTemp, which CleanupJobPartFiles invokes.
//
// dst is already validated as residing inside the output subtree at
// allocation time in Download() (via SafeJoin), so removing the
// derived partial paths cannot escape the output directory. The
// glob-style match for .part-NN is intentionally narrow: only the
// recognized suffix family and a digit run, so a coincidentally
// named "weights.part" or similar user file with a different base
// (which would not appear in dsts) is not touched.
func removePartialArtifacts(dst string) error {
	// The single-path partial and the identity sidecar that describes it. The
	// sidecar goes only when the partial is actually gone, so a partial that
	// survives keeps the record that makes its bytes inheritable.
	removePartialWithIdentity(dst)
	// Ranged staging file and resume metadata (issue #140). The pair is
	// removed only when no live claim remains; the metadata goes first inside
	// removeStagingArtifacts so a crash cannot leave a record claiming a
	// deleted payload.
	removeStagingArtifactsIfUnclaimed(dst+".incomplete", dst+".incomplete.meta")
	return removeLegacyMultipartParts(dst)
}

// removePartialArtifactsOwned removes the partial artifacts of dst while the
// caller HOLDS the destination's ownership domain.
//
// It is the flat/output-mode counterpart of removePartialArtifacts. In HF
// cache mode the domain's lifetime lock and the staging pair's lock are
// DIFFERENT files (blobs/tmp-<name>.lock vs
// blobs/tmp-<name>.incomplete.lock), so the staging pair can be removed
// through the unclaimed-claim path. In
// flat mode they are the SAME file (<destination>.incomplete.lock), so the
// pair must be removed through the body that does NOT take the lock again: a
// second exclusive open of the same lock file conflicts even inside one
// process, so re-acquiring it would report the caller's OWN hold as a live
// owner and skip the removal.
func removePartialArtifactsOwned(dst string) error {
	removePartialWithIdentity(dst)
	removeStagingArtifactsIfUnclaimedLocked(dst+".incomplete", dst+".incomplete.meta")
	return removeLegacyMultipartParts(dst)
}

// removeLegacyMultipartParts removes the legacy multipart artifact family of
// one downloader dst: the pre-#140 parts (dst+".part-NN" for N any non-empty
// run of digits, matching the pre-#140 downloader's %02d/%d format) and the
// legacy layout metadata (dst+".parts.json").
func removeLegacyMultipartParts(dst string) error {
	// Multipart parts: glob the dst's directory for files matching
	// dst+".part-NNN" pattern. This matches what the legacy downloader
	// produced (the parts have indices 0..n-1).
	dir := filepath.Dir(dst)
	base := filepath.Base(dst)
	prefix := base + ".part-"
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		suffix := name[len(prefix):]
		if !isAllDigits(suffix) {
			continue
		}
		full := filepath.Join(dir, name)
		if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
			downloadLog.Warn("cleanup partial file failed", "path", full, "error", err)
		}
	}
	// Legacy multipart layout metadata.
	if err := os.Remove(dst + ".parts.json"); err != nil && !os.IsNotExist(err) {
		downloadLog.Warn("cleanup partial file failed", "path", dst+".parts.json", "error", err)
	}
	return nil
}

// isAllDigits reports whether s is a non-empty run of ASCII digits.
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// RepoTypeFromJob returns the RepoType matching job.IsDataset.
func RepoTypeFromJob(job Job) RepoType {
	if job.IsDataset {
		return RepoTypeDataset
	}
	return RepoTypeModel
}

// isFailFastError reports whether err is a permanent file/part error (an
// *APIError with a fail-fast status: 401/403/404). It is used to distinguish
// "abort the whole job" from ordinary retryable/network failures when
// registering the fatal error for a file.
func isFailFastError(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.IsFailFast()
}

// abortCause wraps an internally-generated fail-fast error used as a context
// cancellation cause. It marks the cancel as an internal abort (preserve
// resumable bytes) independent of the wrapped error's concrete type, which may
// be an *APIError (401/403/404) or a plain error such as the job-level
// path-traversal rejection. Testing a dedicated sentinel rather than the
// error's HTTP status also keeps it distinct from a user cancel/pause.
type abortCause struct{ err error }

func (a *abortCause) Error() string { return a.err.Error() }
func (a *abortCause) Unwrap() error { return a.err }

// abortError wraps err so it can be used as an internal-abort cancel cause.
func abortError(err error) error { return &abortCause{err: err} }

// abortPartAttempt reports whether ctx was canceled by an internal fail-fast
// abort (see abortCause) rather than a user cancel/pause/shutdown. Returning
// without calling cleanupPartialsOnCancel preserves the resumable
// .part/.incomplete bytes (and their resume metadata) for a later retry.
func abortPartAttempt(ctx context.Context) bool {
	var a *abortCause
	return errors.As(context.Cause(ctx), &a)
}

// maxAttemptsPerRetry scales the configured retry budget into the absolute
// per-file/per-part attempt ceiling below. It is deliberately generous: a
// legitimately flaky long download that keeps advancing past many stalls must
// still complete, so the ceiling tolerates many progress refunds. It is a
// package variable (not a public API) so tests can lower it deterministically.
var maxAttemptsPerRetry = 100

// absoluteAttemptCeiling bounds retry attempts over the lifetime of one file or
// part, including retries enabled by progress refunds. The limit is checked
// after each request attempt returns; it does not bound a request that keeps
// delivering data without returning. The high-water-mark rule refunds only
// after non-truncating progress strictly beyond the previous mark. An ignored-
// Range restart can record a higher mark but never refunds the budget, and a
// later bodyless failure earns no refund from that mark. retries<0 is treated
// as 0.
func absoluteAttemptCeiling(retries int) int {
	if retries < 0 {
		retries = 0
	}
	if maxAttemptsPerRetry <= 0 {
		return 0
	}
	maxInt := int(^uint(0) >> 1)
	if retries >= maxInt/maxAttemptsPerRetry {
		return maxInt
	}
	return (retries + 1) * maxAttemptsPerRetry
}

// downloadSingle downloads a file in a single request.
//
// Resume behavior: the bytes already on disk in <dst>.part are inherited only
// when the identity sidecar <dst>.part.meta proves they belong to THIS item:
// the record must exist, be readable and well-formed, name the same
// repository, resolved commit, item path and expected size, and be PINNED
// (the commit was actually resolved). A partial without a usable record, or
// one recorded for another object or another revision, is discarded: the
// transfer restarts from zero. When the record matches, the HTTP request
// uses a Range header to fetch only the remaining bytes. An exactly-sized
// partial is published without a request only when the matching sidecar also
// records a cleanly-framed completion (io.Copy saw EOF at the exact expected
// byte count); otherwise the final byte is reserved and re-requested as the
// non-empty range [size-1, size), because written bytes alone do not prove
// that the response body ended cleanly. Every ranged (206)
// response is validated exactly like the multipart worker path: the status
// must be 206, Content-Range must name the requested [pos, size-1] start/end
// (and the expected total), Content-Length must match the requested length,
// and a bounded writer refuses any byte outside that range. A short body is
// rejected after the copy. If the server ignores the Range header and
// responds with 200 (full body), the .part file is truncated and the download
// restarts from zero. No resume is attempted when the expected size is
// unknown (the range layout cannot be validated).
//
// The path inherits the same trust model as the ranged path. Before the rename,
// the file must have exactly the expected size, and a known digest is proven
// before publication: in Verify=sha256 and Verify=etag modes every single-path
// transfer (fresh or resumed) gets one full-file SHA-256 pass, and in Verify=size
// mode inherited .part bytes are proven once (the same one-time migration
// exception the legacy import gets). A mismatch removes the partial and never
// renames it onto the destination. Verify="none" inherits that mode's no-hash
// semantics, and bytes written entirely by this run in Verify="size" mode need
// no reread. Verification with no known digest, where the caller falls back to a
// HEAD ETag, remains a post-publication check (historical behavior).
//
// The returned bool reports whether this call already proved the payload
// against it.SHA256 before the rename, so the caller does not repeat the
// full-file pass.
func downloadSingle(ctx context.Context, httpc *http.Client, token string, job Job, cfg Settings, it PlanItem, dst string, opts singleOpts, emit func(ProgressEvent)) (bool, error) {
	const unboundedBody = int64(^uint64(0) >> 1)

	tmp := dst + ".part"
	out, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return false, err
	}
	defer out.Close()

	// The identity of the object this transfer is about. The sidecar next to
	// the partial records it, so the next run can tell whether the bytes on
	// disk belong to this object or to another one that shares the staging
	// name (an item without a known digest is staged under a name derived
	// from its path, and two revisions share that name too).
	identity := singleIdentityFor(job, it, opts)
	sidecar := singlePartIdentityPath(dst)
	recorded, rerr := readSinglePartIdentity(sidecar)
	if rerr != nil {
		// A record that cannot be read or understood authorizes nothing: the
		// bytes on disk stay unproven and are discarded below.
		downloadLog.Warn("single-part identity record is unusable",
			"repo", job.Repo,
			"path", it.RelativePath,
			"record", sidecar,
			"error", rerr,
		)
		recorded = nil
	}

	fi, err := out.Stat()
	if err != nil {
		return false, err
	}
	pos := fi.Size()
	// inherited marks bytes this run did not write. A .part is a sequential
	// prefix, so those bytes need the content proof below before publication
	// unless the identity record proves which object they belong to.
	inherited := pos > 0

	// Bytes are inherited only when the identity record proves they belong
	// to this exact object. Otherwise they are discarded: a size match is
	// not an identity, and publishing another object's bytes (or another
	// revision's) would corrupt the cache silently.
	if pos > 0 && !singlePartialInheritable(recorded, identity, pos) {
		downloadLog.Info("discarding unproven single-path partial",
			"repo", job.Repo,
			"path", it.RelativePath,
			"bytes", pos,
			"identity_recorded", recorded != nil,
		)
		if err := out.Truncate(0); err != nil {
			return false, err
		}
		pos = 0
		inherited = false
	}

	// The record is (re)written before any inherited byte is trusted, and
	// after the unproven bytes are gone: it must describe exactly the partial
	// that is on disk. A partial left by an interrupted run keeps this
	// record, which is what makes the next run's decision possible.
	//
	// The clean-framing completion proof travels with the bytes it describes
	// and only with them: it is carried over only for an exactly-sized partial
	// that the recorded identity authorizes, and it is dropped for a fresh
	// transfer, for an incomplete restart (the proof described a full body)
	// and for a partial that is about to be discarded. It is not part of the
	// identity comparison - a matching record still resumes the prefix - but
	// an exactly-sized partial without it is not published without a request
	// (see the exact-size branch below).
	sizeValid := !((it.Size > 0 && pos > it.Size) || (it.Size <= 0 && pos > 0))
	identity.FramingComplete = it.Size > 0 && pos == it.Size && recorded != nil &&
		recorded.FramingComplete && recorded.inheritableBy(identity)
	if err := writeSinglePartIdentity(sidecar, identity); err != nil {
		return false, fmt.Errorf("write single-part identity %s: %w", sidecar, err)
	}

	// An oversized partial is stale/corrupt. A partial with no known expected
	// size cannot be range-validated at all, so it is discarded rather than
	// guessed at (downloader correctness rule: never resume unknown layout).
	if !sizeValid {
		if err := out.Truncate(0); err != nil {
			return false, err
		}
		pos = 0
		inherited = false
	}

	// forceLastByteRange makes the clean-framing revalidation an explicit
	// ranged request even when the reserved final byte starts at offset zero
	// (a one-byte file), where the ordinary resume rule would send a full GET.
	forceLastByteRange := false

	// If the partial already holds exactly the expected number of bytes, its
	// bytes are still unproven: publish only after the exact-size check and,
	// for inherited bytes, the content proof. Emit the full-progress reading
	// before the finalization event so the UI does not sit below 100% through
	// the proof/rename.
	if it.Size > 0 && pos == it.Size {
		if identity.FramingComplete {
			emit(ProgressEvent{Event: "file_progress", Path: it.RelativePath, Downloaded: it.Size, Total: it.Size})
			return finalizeSinglePartial(ctx, out, tmp, dst, it, cfg, inherited, opts, emit)
		}
		// The file is exactly full, but no run recorded that the response
		// body ended cleanly at that byte count. Written bytes alone are not
		// proof of clean framing: a process can exit after the last WriteAt
		// and before io.Copy observes EOF, leaving a torn final byte. Reserve
		// that byte so the normal transfer path re-requests the non-empty
		// range [size-1, size) and only a clean response commits it.
		downloadLog.Info("revalidating unframed single-path final byte",
			"repo", job.Repo,
			"path", it.RelativePath,
			"bytes", it.Size,
		)
		if err := out.Truncate(it.Size - 1); err != nil {
			return false, err
		}
		pos = it.Size - 1
		// For a one-byte file the reserved range starts at zero, where the
		// normal resume rule would send a full GET. Force the explicit
		// non-empty [0, 1) range so the revalidation is always one cleanly
		// framed ranged response, exactly like every other size.
		forceLastByteRange = pos == 0
	}
	if pos > 0 {
		emit(ProgressEvent{Event: "file_progress", Path: it.RelativePath, Downloaded: pos, Total: it.Size})
		// The bytes already on disk are reused: the next request asks for
		// the remaining range only.
		downloadLog.Info("resuming download",
			"repo", job.Repo,
			"path", it.RelativePath,
			"offset", pos,
			"bytes", it.Size,
		)
	}

	retry := newRetry(cfg)
	stall := stallTimeout(cfg)
	attempts := cfg.Retries
	if attempts < 0 {
		attempts = 0
	}
	remaining := attempts
	attempt := 0
	ceiling := absoluteAttemptCeiling(attempts)
	var lastErr error
	// The high-water mark prevents non-progress and ignored-Range restarts from
	// refunding the budget. The separate lifetime retry ceiling limits retries
	// once attempts return; it does not time-bound a request that keeps delivering
	// data. An ignored-Range restart may record a higher mark but cannot earn a
	// refund, so a following bodyless failure also earns none.
	// hwm is the highest offset ever reached for this file, seeded with the
	// initial on-disk offset.
	hwm := pos
	// countedUpTo is the absolute end of the bytes already reported as
	// progress, seeded with the inherited prefix. Progress therefore counts
	// durable unique bytes: a failed attempt's written prefix is not counted
	// again when the retry resumes from it, and an overflowing attempt's
	// accepted bytes are not counted twice when the range is re-requested.
	countedUpTo := pos
	var lastEmit time.Time

	for {
		select {
		case <-ctx.Done():
			out.Close()
			// An internal fail-fast abort must not delete resumable bytes.
			if !abortPartAttempt(ctx) {
				cleanupPartialsOnCancel(cfg, dst, "")
			}
			return false, ctx.Err()
		default:
		}

		// Each attempt gets its own cancellable context so the stall
		// watchdog can abort just this attempt (closing the connection or
		// HTTP/2 stream) without cancelling the whole job.
		attemptCtx, attemptCancel := context.WithCancel(ctx)

		req, _ := http.NewRequestWithContext(attemptCtx, "GET", it.URL, nil)
		addAuth(req, token)
		if it.Size > 0 && (pos > 0 || forceLastByteRange) {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", pos, it.Size-1))
		}

		// waitResp carries a throttling response (429/503) whose Retry-After /
		// RateLimit header must shape the next wait.
		var waitResp *http.Response
		// truncatedRestart is set when the server ignored our Range and
		// answered 200 while resumable bytes were present, forcing a restart
		// from zero. That is destructive rather than progress, so it must not
		// refund the budget (and it bounds the ignored-Range growing-prefix
		// server, which would otherwise advance the high-water mark on every
		// attempt).
		truncatedRestart := false
		// preHwm is the high-water mark as of the start of this attempt; the
		// refund decision compares the final pos against it (see below).
		preHwm := hwm

		resp, err := httpc.Do(req)
		if err != nil {
			lastErr = err
		} else {
			status := resp.StatusCode
			if apiErr := classifyFileResponse(resp, it.URL); apiErr != nil {
				lastErr = apiErr
				resp.Body.Close()
				if apiErr.IsFailFast() {
					// Permanent for the whole job: no retry, no request
					// storm. Remove any empty partial this attempt created
					// before returning the actionable error.
					//
					// The identity sidecar goes ONLY with the partial it
					// describes. A NON-EMPTY partial holds resumable bytes
					// that the next run may inherit, and that run can only
					// prove they belong to this object while the sidecar
					// survives. Removing the record while the bytes stay
					// would silently discard the progress a permanent (but
					// eventually fixable) error made.
					attemptCancel()
					out.Close()
					if removeEmptyPartialFile(tmp) {
						removeSinglePartIdentity(sidecar)
					}
					return false, lastErr
				}
				if !apiErr.IsRetryable() {
					attemptCancel()
					out.Close()
					return false, lastErr
				}
				waitResp = resp
			} else if status != http.StatusOK && status != http.StatusPartialContent {
				// A 2xx other than 200/206 is a protocol violation, not a
				// valid body.
				lastErr = fmt.Errorf("unexpected status %s for file request", resp.Status)
				resp.Body.Close()
			} else {
				base := int64(0)
				limit := unboundedBody
				valid := true
				if status == http.StatusPartialContent {
					// A ranged response is accepted only with an exact
					// Content-Range for the requested interval. This path
					// never resumes without a known size, so an unvalidatable
					// 206 is a protocol violation.
					if it.Size <= 0 {
						lastErr = fmt.Errorf("ranged response without an expected size (bytes %d-)", pos)
						valid = false
					} else if verr := validateRangeResponse(resp, pos, it.Size-1, it.Size); verr != nil {
						lastErr = verr
						valid = false
					} else {
						base = pos
						limit = it.Size - pos
					}
					if !valid {
						resp.Body.Close()
					}
				} else {
					// Full body: if we asked for a range and got 200, existing
					// partial bytes are thrown away and the download restarts
					// from zero. It is a destructive restart, not progress.
					if pos > 0 {
						truncatedRestart = true
						if err := out.Truncate(0); err != nil {
							resp.Body.Close()
							attemptCancel()
							return false, err
						}
						pos = 0
						// The inherited bytes were just discarded: this run will
						// write every byte from zero, so Verify=size must not
						// perform the one-time inherited-bytes proof on the
						// restarted transfer.
						inherited = false
						countedUpTo = 0
						lastEmit = time.Time{}
					}
					base = 0
					if it.Size > 0 {
						limit = it.Size
					}
				}

				if valid {
					// The stall watchdog wraps the raw network body BEFORE the
					// speed limiter's pacing, so deliberate throttling is not
					// mistaken for a stalled peer.
					body := newStallReader(resp.Body, stall, attemptCancel)
					paced := cfg.SpeedLimiter.Reader(attemptCtx, body)
					w := newBoundedAtWriter(out, base, limit)
					w.onWrite = func([]byte) {
						end := base + w.written
						if end <= countedUpTo {
							return
						}
						countedUpTo = end
						if now := time.Now(); now.Sub(lastEmit) >= 200*time.Millisecond {
							lastEmit = now
							emit(ProgressEvent{Event: "file_progress", Path: it.RelativePath,
								Downloaded: end, Total: it.Size})
						}
					}
					_, cerr := io.Copy(w, paced)
					resp.Body.Close()
					if cerr == nil {
						// Exact body length: the bounded writer refuses an
						// overlong body; a short body is rejected here.
						switch {
						case status == http.StatusPartialContent && w.written != limit:
							cerr = fmt.Errorf("ranged body short: got %d of %d bytes for bytes %d-%d",
								w.written, limit, base, it.Size-1)
						case status == http.StatusOK && it.Size > 0 && w.written != it.Size:
							cerr = fmt.Errorf("single body short: got %d of %d bytes for %s",
								w.written, it.Size, it.RelativePath)
						}
					}
					if cerr == nil {
						// io.Copy observed a clean end of the body AND every
						// exact length check passed, so this run holds the
						// clean-framing proof the partial lacked. Persist it
						// before publication: a later restart that lands on an
						// exactly-sized partial may then finalize without a
						// request. A persistence failure only loses that
						// restart optimization - this run's bytes are framed
						// now - so it is a warning, not a publication error.
						identity.FramingComplete = true
						if werr := writeSinglePartIdentity(sidecar, identity); werr != nil {
							downloadLog.Warn("cannot persist single-part clean-framing completion",
								"repo", job.Repo,
								"path", it.RelativePath,
								"record", sidecar,
								"error", werr,
							)
						}
						// The file now has exactly the expected bytes (a ranged
						// body covers [pos, size) and a full body covers
						// [0, size)); finalizeSinglePartial re-checks the size
						// and runs the inherited-bytes content proof.
						attemptCancel()
						emit(ProgressEvent{Event: "file_progress", Path: it.RelativePath,
							Downloaded: countedUpTo, Total: it.Size})
						return finalizeSinglePartial(ctx, out, tmp, dst, it, cfg, inherited, opts, emit)
					}
					attemptCancel()
					lastErr = classifyStall(ctx, body, cerr)
					if errors.Is(cerr, errRangedBodyOverflow) {
						// Protocol violation: never advance the durable offset
						// for an overlong body. Remove every byte from this
						// invalid attempt before a later invocation can mistake
						// the exact-sized partial for valid transport progress.
						if terr := out.Truncate(base); terr != nil {
							return false, fmt.Errorf("discard invalid ranged response: %w", terr)
						}
						pos = base
						if countedUpTo > base {
							countedUpTo = base
						}
					} else {
						// Advance by bytes actually written so the next retry
						// resumes from the durable offset instead of
						// duplicating them. Reaching the requested byte count is
						// not proof of clean body framing: reserve the last byte
						// and truncate it so retry/restart requests a non-empty range.
						accepted := w.written
						if it.Size > 0 && accepted == limit {
							accepted--
							pos = base + accepted
							if err := out.Truncate(pos); err != nil {
								return false, fmt.Errorf("retain unframed response prefix: %w", err)
							}
						} else {
							pos = base + accepted
						}
					}
				}
			}
		}
		attemptCancel()

		// Refund the retry budget only when this attempt moved STRICTLY above
		// the high-water mark captured BEFORE the attempt AND did not truncate
		// previously-resumable bytes (an ignored-Range 200 restart is
		// destructive, not progress). preHwm is read before the attempt because
		// hwm is otherwise updated unconditionally below: a restart that writes
		// past the old mark must be recorded (so it cannot earn a later free
		// refund from a bodyless failure) without itself granting a refund.
		// The ceiling above still bounds how far refunds may extend the budget.
		advanced := pos > preHwm && !truncatedRestart
		// Always record the highest offset reached, regardless of refund
		// eligibility, so a truncating restart's higher offset is never lost.
		if pos > hwm {
			hwm = pos
		}
		if advanced {
			retry.reset()
			remaining = attempts
		}

		if remaining == 0 || attempt >= ceiling {
			if ctx.Err() != nil {
				out.Close()
				if !abortPartAttempt(ctx) {
					cleanupPartialsOnCancel(cfg, dst, "")
				}
				return false, ctx.Err()
			}
			break
		}
		remaining--
		attempt++
		emit(ProgressEvent{Event: "retry", Path: it.RelativePath, Attempt: attempt, Message: lastErr.Error()})
		downloadLog.Warn("download retry",
			"repo", job.Repo,
			"path", it.RelativePath,
			"attempt", attempt,
			"error", lastErr,
		)
		if d := NextRetryWait(waitResp, retry.Next(), DefaultMaxRetryAfter, time.Now()); !sleepCtx(ctx, d) {
			out.Close()
			if !abortPartAttempt(ctx) {
				cleanupPartialsOnCancel(cfg, dst, "")
			}
			return false, ctx.Err()
		}
	}
	return false, lastErr
}

// singleProofStartHook is a test seam invoked at the very start of the
// single-path known-digest content proof, immediately before
// computeSHA256Ctx. It is nil in production. A regression test installs it to
// cancel the context while the partial's writable handle is still open, so it
// can prove that the explicit-cancel cleanup closes that handle before it
// removes anything (Windows cannot unlink an open file).
var singleProofStartHook func(out *os.File)

// finalizeSinglePartial publishes a .part that already holds exactly the
// expected number of bytes. It is called only when this run observed a clean
// end of the response body at that byte count, or when a matching sidecar
// already recorded that clean-framing completion for the exact-sized partial;
// written bytes alone never reach this gate. It is the single path's
// publication gate: the
// exact-size check runs first, then a known digest is proven with one
// full-file SHA-256 pass before the rename whenever Verify is "sha256" or
// "etag" (fresh or resumed), whenever the partial contains inherited bytes and
// Verify is not "none" (the one-time migration exception for size mode),
// whenever the identity is UNPINNED and Verify is not "none" (an unpinned
// identity names a revision the endpoint did not resolve, so the bytes are not
// bound to an immutable object and only the content proof can detect a moved
// branch), or whenever the expected size is unknown but the item carries a
// known digest (only the digest can prove an unconstrained stream). A mismatch
// removes the partial and never renames it onto dst. The returned bool reports
// whether that proof ran, so the caller does not repeat it. file_finalizing is
// emitted before the slow proof so the UI sees the local-finalization window.
func finalizeSinglePartial(ctx context.Context, out *os.File, tmp, dst string, it PlanItem, cfg Settings, inherited bool, opts singleOpts, emit func(ProgressEvent)) (bool, error) {
	emit(ProgressEvent{Event: "file_finalizing", Path: it.RelativePath, Message: "verifying"})
	cancelBeforePublish := func() error {
		if err := ctx.Err(); err != nil {
			_ = out.Close()
			if !abortPartAttempt(ctx) {
				cleanupPartialsOnCancel(cfg, dst, "")
			}
			return err
		}
		return nil
	}
	if err := cancelBeforePublish(); err != nil {
		return false, err
	}
	if it.Size > 0 {
		fi, err := out.Stat()
		if err != nil {
			return false, err
		}
		if fi.Size() != it.Size {
			return false, fmt.Errorf("single download %s: got %d bytes on disk, want %d", it.RelativePath, fi.Size(), it.Size)
		}
	}
	verified := false
	// Known-digest modes always prove before publication (fresh or resumed);
	// size mode proves inherited bytes or an unknown-size stream, neither of
	// which can be trusted from size alone. An UNPINNED identity proves in
	// every non-"none" mode as well: it names a revision the endpoint did not
	// resolve to a commit, so the transferred bytes are not bound to an
	// immutable object and a moved branch can only be caught by the content
	// proof. Verify="none" is the opt-out.
	proveKnownDigest := it.SHA256 != "" &&
		(cfg.Verify == "sha256" || cfg.Verify == "etag" ||
			(cfg.Verify != "none" && (it.Size <= 0 || inherited || !opts.CommitPinned)))
	if proveKnownDigest {
		if singleProofStartHook != nil {
			singleProofStartHook(out)
		}
		sum, err := computeSHA256Ctx(ctx, tmp)
		if err != nil {
			if cerr := ctx.Err(); cerr != nil {
				// The content proof is the only pre-publication cancellation
				// branch that reached the cleanup with the partial's writable
				// handle still open. Close it before the explicit-cancel
				// cleanup: Windows cannot unlink a file while this handle
				// (opened without FILE_SHARE_DELETE) is open, so a cancelled
				// proof would otherwise retain the whole .part and its identity
				// sidecar. A double Close from the deferred close is harmless.
				// Pause/internal aborts keep their bytes exactly as before; they
				// only skip the cleanup below.
				_ = out.Close()
				if !abortPartAttempt(ctx) {
					cleanupPartialsOnCancel(cfg, dst, "")
				}
				return false, cerr
			}
			return false, err
		}
		if err := cancelBeforePublish(); err != nil {
			return false, err
		}
		if !strings.EqualFold(sum, it.SHA256) {
			// Unproven bytes must never be published: the partial is removed so
			// a retry starts from a clean prefix, and dst is left untouched.
			_ = out.Close()
			if rerr := os.Remove(tmp); rerr == nil || os.IsNotExist(rerr) {
				// The identity record described the partial that is now gone:
				// it must not outlive it.
				removeSinglePartIdentity(tmp + ".meta")
			}
			return false, fmt.Errorf("sha256 verify failed: %s: expected %s got %s", it.RelativePath, it.SHA256, sum)
		}
		verified = true
	}
	if err := out.Close(); err != nil {
		return false, err
	}
	if err := cancelBeforePublish(); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return false, err
	}
	// The identity record described the partial that has just been published.
	// It must not outlive its partial: the published file is the content now,
	// and a stale record would describe bytes that no longer exist.
	removeSinglePartIdentity(tmp + ".meta")
	// Obsolete pre-#140 multipart artifacts for this destination must not
	// survive a successful single-path finalization. Finalization already
	// succeeded, so a cleanup failure is logged and does not unpublish the file.
	if err := removeMultipartResumeFiles(dst); err != nil {
		downloadLog.Warn("cleanup legacy multipart artifacts failed", "path", dst, "error", err)
	}
	return verified, nil
}

// removeEmptyPartialFile removes path when it exists and is empty. It is used
// to clean up a zero-byte .part file that a fail-fast attempt created, without
// touching a partial that predates the attempt and may hold resumable bytes.
//
// It reports whether the file is GONE afterwards: the caller uses it to decide
// whether the identity sidecar that describes the partial may be removed as
// well. A sidecar must not outlive its partial, and a partial that SURVIVES
// must keep its sidecar, or the next run cannot prove the identity of the
// bytes on disk and discards them.
func removeEmptyPartialFile(path string) bool {
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			// The file is already gone: there is nothing left for a sidecar
			// to describe.
			return true
		}
		// The stat FAILED, so whether the partial still exists is UNKNOWN
		// (a permission error, a symlink loop, an I/O error, ...). The caller
		// uses this answer to decide whether the identity sidecar may be
		// removed together with the partial. Removing a sidecar whose partial
		// may still be on disk would make the next run unable to prove the
		// identity of the surviving bytes and discard them, so the
		// conservative answer is false: the sidecar survives.
		downloadLog.Warn("cannot stat partial file; keeping its identity sidecar", "path", path, "error", err)
		return false
	}
	if fi.Size() != 0 {
		return false
	}
	// lgtm[go/path-injection]
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		downloadLog.Warn("cleanup empty partial file failed", "path", path, "error", err)
		return false
	}
	return true
}
