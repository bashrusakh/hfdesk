// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// multipartLayoutMetaPath names the obsolete pre-#140 layout record, used only for cleanup.
func multipartLayoutMetaPath(dst string) string {
	return dst + ".parts.json"
}

// multipartPartFiles lists legacy dst.part-NN files with non-empty digit suffixes;
// non-digit suffixes are not download artifacts.
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

// skippedBlobLink tracks completed shared-blob items whose per-path cache links
// are reconciled after all transfers finish.
type skippedBlobLink struct {
	commit       string
	relativePath string
	filterSubdir string
	sha256       string
}

// Download scans and downloads repository files into the HF cache by default.
// OutputDir selects flat mode only when CacheDir is unset. Requests honor ctx.
func Download(ctx context.Context, job Job, cfg Settings, progress ProgressFunc) (err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// Record one terminal outcome; cancellation is distinct from failure.
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

	if job.Revision == "" {
		job.Revision = "main"
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 8
	}
	if cfg.MaxActiveDownloads <= 0 {
		cfg.MaxActiveDownloads = runtime.GOMAXPROCS(0)
	}

	// Respect a caller's shared limiter; otherwise construct one from MaxSpeed.
	if cfg.SpeedLimiter == nil {
		if bps := ParseSize(cfg.MaxSpeed); bps > 0 {
			cfg.SpeedLimiter = NewRateLimiter(bps)
		}
	}

	// An explicit CacheDir wins; otherwise OutputDir selects flat mode.
	useHFCache := cfg.CacheDir != "" || cfg.OutputDir == ""
	var hfCache *HFCache
	var repoDir *RepoDir
	var ownership *ownershipRealm
	var friendlyOwnership *ownershipRealm

	if useHFCache {
		var err error
		hfCache, err = cfg.BuildHFCache()
		if err != nil {
			return fmt.Errorf("build hf cache: %w", err)
		}
		ownership, err = hfCache.ownershipRealmForDataContext(ctx)
		if err != nil {
			return fmt.Errorf("resolve ownership realm: %w", err)
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
		if !job.BranchVariant {
			if err := repoDir.EnsureDirs(); err != nil {
				return fmt.Errorf("ensure repo dirs: %w", err)
			}
		}
	} else {
		if cfg.OutputDir == "" {
			cfg.OutputDir = "Storage"
		}
		ownerDir, err := resolveOwnershipDir(cfg.OwnershipDir)
		if err != nil {
			return fmt.Errorf("resolve ownership realm: %w", err)
		}
		cfg.OwnershipDir = ownerDir
		ownership, err = newOwnershipRealm(ownerDir)
		if err != nil {
			return fmt.Errorf("open ownership realm: %w", err)
		}
		if err := validateOwnershipRealmOutside(ownership, cfg.OutputDir); err != nil {
			return err
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
	failBranchVariant := func(err error) error {
		emit(ProgressEvent{Level: "error", Event: "error", Message: err.Error()})
		return err
	}

	emit(ProgressEvent{Event: "scan_start", Message: "scanning repo"})

	downloadLog.Info("download started", "repo", job.Repo, "revision", job.Revision)

	plan, err := scanRepo(ctx, httpc, cfg.Token, job, cfg)
	if err != nil {
		if job.BranchVariant {
			return failBranchVariant(err)
		}
		return err
	}
	var localSpace *BranchVariantLocalSpace
	if job.BranchVariant {
		if !plan.CommitPinned || !plan.CommitResolved {
			return failBranchVariant(fmt.Errorf("branch variant %q requires a pinned resolved commit", job.Revision))
		}
		if useHFCache {
			if err := repoDir.EnsureDirs(); err != nil {
				return failBranchVariant(fmt.Errorf("ensure repo dirs: %w", err))
			}
		} else {
			space, err := NewBranchVariantLocalSpace(cfg.OutputDir, job.Repo)
			if err != nil {
				return failBranchVariant(fmt.Errorf("resolve branch variant space: %w", err))
			}
			localSpace = &space
			if err := invalidateBranchVariantRecordsWithOwnership(ctx, ownership, space, job.Revision); err != nil {
				return failBranchVariant(fmt.Errorf("invalidate prior branch variant completion records: %w", err))
			}
			if err := space.ensureStagingTree(job.Revision, plan.Commit); err != nil {
				return failBranchVariant(fmt.Errorf("unsafe branch variant layout: %w", err))
			}
		}
	}
	friendlyView := !cfg.NoFriendlyView && !job.BranchVariant
	if useHFCache && friendlyView {
		friendlyOwnership, err = hfCache.ownershipRealmForFriendlyContext(ctx)
		if err != nil {
			return fmt.Errorf("resolve friendly-view ownership realm: %w", err)
		}
	}
	if !plan.CommitPinned {
		emit(ProgressEvent{Level: "warn", Event: "warning", Message: "revision is not pinned to an immutable commit; partial bytes will not be resumed across calls and cache metadata is revision-addressed"})
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

	// Emit plan events up front so the UI knows the total before transfer.
	for _, item := range plan.Items {
		displayRel := item.RelativePath
		if job.AppendFilterSubdir && item.DestinationSubdir != "" {
			displayRel = filepath.ToSlash(filepath.Join(item.DestinationSubdir, item.RelativePath))
		}
		emit(ProgressEvent{Event: "plan_item", Path: displayRel, Total: item.Size})
	}
	if !useHFCache {
		base, err := localDownloadBase(job, cfg, localSpace)
		if err != nil {
			return err
		}
		if err := ownership.ensureDirectories(ctx, []string{base}, blobOwnershipWaitBudget); err != nil {
			return err
		}
	}

	// A slot limits active transfers, not items waiting for destination ownership.
	lim := make(chan struct{}, cfg.MaxActiveDownloads)

	var wg sync.WaitGroup
	errCh := make(chan error, len(plan.Items))

	// A fatal item error cancels siblings; the cause distinguishes internal abort
	// (preserve resumable bytes) from user cancellation or pause.
	jobCtx, jobCancel := context.WithCancelCause(ctx)
	defer jobCancel(nil)

	// Prefer the original fatal error over sibling cancellation during unwind.
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

	var skipOnce sync.Map

	// Reconcile per-path links of skipped shared blobs after all transfers finish.
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

	var manifestBuilder *ManifestBuilder
	var manifestMu sync.Mutex
	if job.BranchVariant || (useHFCache && !cfg.NoManifest) {
		manifestBuilder = NewManifestBuilder(job, cfg.Command)
		if plan.CommitPinned {
			manifestBuilder.SetCommit(plan.Commit)
		}
		if useHFCache {
			// RepoPath describes the selected physical repository, relative to the
			// friendly root when possible (absolute on different Windows volumes).
			rel, err := filepath.Rel(hfCache.Root, repoDir.Path())
			if err != nil {
				rel = repoDir.Path()
			}
			manifestBuilder.manifest.RepoPath = filepath.ToSlash(rel)
		} else if job.BranchVariant {
			repoPath, err := localSpace.PayloadRepoPath(job.Revision)
			if err != nil {
				return fmt.Errorf("branch variant revision %q: %w", job.Revision, err)
			}
			manifestBuilder.manifest.RepoPath = repoPath
		}
	}

LOOP:
	for _, item := range plan.Items {
		select {
		case <-jobCtx.Done():
			break LOOP
		default:
		}

		it := item // capture for goroutine

		select {
		case lim <- struct{}{}:
		case <-jobCtx.Done():
			break LOOP
		}

		wg.Add(1)
		go func() {
			defer wg.Done()

			fileCtx, fileCancel := context.WithCancel(jobCtx)
			// Return the transfer slot while waiting for ownership; reacquire it
			// before transferring.
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
			var publishPath string
			var skipCheck func() (bool, string, error)

			if useHFCache {
				// HF Cache mode: check blob existence
				skipCheck = func() (bool, string, error) {
					if it.SHA256 != "" {
						status, _, err := repoDir.CheckBlob(it.SHA256)
						if err != nil {
							// Unknown ownership is an error, not a skip; preserve the partial.
							return false, "", fmt.Errorf("check %s: %w", finalRel, err)
						}
						if status == BlobComplete {
							return true, "blob exists", nil
						}
						if status == BlobDownloading {
							// Wait for the proven owner and re-check its target; do not skip yet.
							return false, "", nil
						}
					}
					return false, "", nil
				}
				// Use digest-keyed temp names when available; canonical multipart transfers
				// stage directly at the blob instead.
				tmpName := "tmp-" + it.SHA256
				if it.SHA256 == "" {
					// The reserved path- prefix keeps this key distinct from SHA domains.
					tmpName = "tmp-path-" + strings.ReplaceAll(it.RelativePath, "/", "_")
					if job.BranchVariant {
						tmpName = branchVariantStagingName(job.Revision, plan.Commit, it)
					}
				}
				// Confine the remote-derived temp name to blobs/.
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
				base, err := localDownloadBase(job, cfg, localSpace)
				if err != nil {
					fatal := fmt.Errorf("path traversal: %w", err)
					setFatalErr(fatal)
					select {
					case errCh <- fatal:
					default:
					}
					return
				}
				// Confine the Hub-provided path to the output directory.
				publishPath, err = SafeJoin(base, finalRel)
				if err != nil {
					fatal := fmt.Errorf("path traversal: %q would escape output directory", finalRel)
					setFatalErr(fatal)
					select {
					case errCh <- fatal:
					default:
					}
					return
				}
				if job.BranchVariant {
					dst, err = localSpace.stagingPath(job.Revision, plan.Commit, finalRel)
					if err != nil {
						select {
						case errCh <- fmt.Errorf("resolve branch variant staging path: %w", err):
						default:
						}
						return
					}
				} else {
					dst = publishPath
				}
				skipCheck = func() (bool, string, error) {
					if job.BranchVariant {
						return shouldSkipLocalByHashCtx(fileCtx, it, publishPath)
					}
					return shouldSkipLocalCtx(fileCtx, it, publishPath)
				}
			}

			alreadyOK, reason, err := skipCheck()
			if err != nil {
				select {
				case errCh <- err:
				default:
				}
				return
			}
			if alreadyOK && !useHFCache {
				recordSkippedBlobLink(it, finalRel, filterSubdir)
				if _, loaded := skipOnce.LoadOrStore(finalRel, struct{}{}); !loaded {
					emit(ProgressEvent{Event: "file_done", Path: finalRel, Message: "skip (" + reason + ")"})
					atomic.AddInt64(&skippedCount, 1)
					if manifestBuilder != nil {
						manifestMu.Lock()
						blobHash := it.SHA256
						if !useHFCache {
							blobHash = ""
						}
						manifestBuilder.AddFile(it.RelativePath, blobHash, it.Size, it.LFS)
						manifestMu.Unlock()
					}
				}
				return
			}

			itForIO := it
			itForIO.RelativePath = finalRel

			blobDirect := useHFCache && canonicalSHA256(it.SHA256)
			var mpOpts multipartOpts
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
			var (
				dlErr  error
				domain *blobDomain
			)
			key, domainDst, proofPath, ownershipPaths, pathErr := downloadOwnershipPaths(useHFCache, repoDir, dst, publishPath, it)
			if pathErr != nil {
				dlErr = pathErr
			} else {
				domain, dlErr = acquireBlobDomainScoped(fileCtx, key, domainDst, ownership, ownershipPaths, proofPath)
				if errors.Is(dlErr, errStagingOwnershipBusy) || errors.Is(dlErr, errStagingOwnershipUncertain) {
					dlErr = blobOwnershipScopeError(it.SHA256, finalRel, dlErr)
				} else if dlErr != nil {
					dlErr = fmt.Errorf("acquire the ownership domain of %s: %w", finalRel, dlErr)
				} else {
					defer domain.release()
					mpOpts.Domain = domain
				}
			}

			if dlErr == nil && useHFCache && it.SHA256 != "" {
				if _, statErr := os.Stat(repoDir.BlobPath(it.SHA256)); statErr == nil {
					dlErr = errBlobAlreadyComplete
				} else if !errors.Is(statErr, os.ErrNotExist) {
					dlErr = fmt.Errorf("re-check blob %s: %w", it.SHA256, statErr)
				}
			}
			if alreadyOK && dlErr == nil {
				// A cache skip candidate that vanished before admission must transfer.
				alreadyOK = false
			}

			if errors.Is(dlErr, errBlobAlreadyComplete) {
				if err := createCacheEntryLinksScoped(fileCtx, repoDir, plan.Commit, it.RelativePath, filterSubdir, it.SHA256, !friendlyView, ownership, friendlyOwnership); err != nil {
					select {
					case errCh <- fmt.Errorf("download %s: %w", finalRel, err):
					default:
					}
					return
				}
				recordSkippedBlobLink(it, finalRel, filterSubdir)
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
			if dlErr != nil {
				if errors.Is(dlErr, errBlobIncompleteOwned) && !errors.Is(dlErr, ErrStagingOwnershipUnproven) {
					dlErr = blobLiveOwnerError(it.SHA256, finalRel)
				}
				fatal := fmt.Errorf("download %s: %w", finalRel, dlErr)
				if errors.Is(dlErr, context.Canceled) || errors.Is(dlErr, context.DeadlineExceeded) {
					downloadLog.Info("file download cancelled", "repo", job.Repo, "path", finalRel)
				} else {
					downloadLog.Error("file download failed", "repo", job.Repo, "path", finalRel, "error", dlErr)
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

			// The domain is held before resume inspection, HEAD, or claim access.
			resumeStaging := ""
			if hasResumableStagingState(versionedStaging, versionedStaging+".meta", itForIO, job, plan.Commit, plan.CommitPinned) {
				resumeStaging = versionedStaging
			}
			multipartRan := plan.CommitPinned && it.Size >= thresholdBytes && it.AcceptRanges
			if !multipartRan && it.AcceptRanges && resumeStaging != "" {
				multipartRan = true
			}
			obsoleteVersionedStaging := ""
			if !it.AcceptRanges && resumeStaging != "" {
				obsoleteVersionedStaging = resumeStaging
				emit(ProgressEvent{Level: "warn", Event: "warning", Message: fmt.Sprintf(
					"resume state for %s cannot be used without HTTP range support; downloading the full file and attempting to discard the obsolete state after success",
					finalRel)})
			}

			if cfg.OnPartialFile != nil {
				cfg.OnPartialFile(dst, false)
			}
			emit(ProgressEvent{Event: "file_start", Path: finalRel, Total: it.Size})
			downloadLog.Info("file download started", "repo", job.Repo, "path", finalRel, "bytes", it.Size)

			directBlobPublished := blobDirect
			if blobDirect && multipartRan {
				mpOpts.DirectBlobPublished = &directBlobPublished
			}
			// contentVerified means a known digest was proven before publication;
			// avoid a second hash. A HEAD-ETag-only check has no known digest and
			// remains the post-publication exception.
			var contentVerified bool
			if dlErr == nil {
				if multipartRan {
					contentVerified, dlErr = downloadMultipart(fileCtx, httpc, cfg.Token, job, cfg, itForIO, dst, mpOpts, emit)
				} else {
					contentVerified, dlErr = downloadSingle(fileCtx, httpc, cfg.Token, job, cfg, itForIO, dst,
						singleOpts{Commit: plan.Commit, CommitPinned: plan.CommitPinned, Domain: domain}, emit)
				}
			}
			if blobDirect && multipartRan && !directBlobPublished {
				// Unknown size fell back to the single-path temp-and-store route.
				multipartRan = false
			}

			if errors.Is(dlErr, errBlobAlreadyComplete) {
				if useHFCache && it.SHA256 != "" {
					if err := createCacheEntryLinksScoped(fileCtx, repoDir, plan.Commit, it.RelativePath, filterSubdir, it.SHA256, !friendlyView, ownership, friendlyOwnership); err != nil {
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
						blobHash := it.SHA256
						if !useHFCache {
							blobHash = ""
						}
						manifestBuilder.AddFile(it.RelativePath, blobHash, it.Size, it.LFS)
						manifestMu.Unlock()
					}
				}
				return
			}
			// Report proven live ownership as transient, but preserve the unproven-record
			// error and its staging-pair recovery instructions.
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

			// Final verification/store may be long local I/O with no progress
			// events; expose the finalizing phase instead of appearing stuck.
			emit(ProgressEvent{Event: "file_finalizing", Path: finalRel, Message: "verifying"})

			// Settings.Verify also applies to LFS. contentVerified avoids repeating a
			// required pre-publication proof; this layer checks size or the no-digest
			// HEAD-ETag fallback. See doc.go for mode exceptions.
			verifyPath := dst
			if multipartRan && blobDirect {
				verifyPath = mpOpts.Publish
			}
			if it.LFS && it.SHA256 != "" {
				switch cfg.Verify {
				case "none":
				case "sha256", "etag":
					// Avoid a duplicate hash when the transfer already reported content proof.
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
					// The engine validates transfer coverage; check the final size.
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
					// Non-2xx HEAD responses are metadata misses; cancellation and
					// fail-fast errors still stop this file.
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

			// Pass fileCtx through cache hashing/store so a sibling's fatal error can
			// interrupt long local I/O.
			var finalSHA256 string
			switch {
			case blobDirect && multipartRan:
				// The ranged route already published the blob; create its required cache
				// links. NoFriendlyView omits only the friendly entry, not the snapshot.
				if err := createCacheEntryLinksScoped(fileCtx, repoDir, plan.Commit, it.RelativePath, filterSubdir, it.SHA256, !friendlyView, ownership, friendlyOwnership); err != nil {
					select {
					case errCh <- fmt.Errorf("store file %s: %w", finalRel, err):
					default:
					}
					return
				}
				finalSHA256 = it.SHA256
			case useHFCache:
				sha := it.SHA256
				// The store reuses this domain when it covers the blob key, or acquires the
				// computed blob's domain for a path-derived item before placement.
				result, err := repoDir.storeDownloadedFileInDomain(fileCtx, dst, it.RelativePath, plan.Commit, sha, filterSubdir, !friendlyView, domain)
				if err != nil {
					select {
					case errCh <- fmt.Errorf("store file %s: %w", finalRel, err):
					default:
					}
					return
				}
				finalSHA256 = result.SHA256 // Use computed SHA256 from store result
			default:
				if job.BranchVariant {
					if err := publishBranchVariantFile(dst, publishPath); err != nil {
						select {
						case errCh <- fmt.Errorf("publish branch variant file %s: %w", finalRel, err):
						default:
						}
						return
					}
				}
				finalSHA256 = it.SHA256
			}

			// After successful single-path fallback, attempt claim-checked cleanup of
			// obsolete ranged state while the destination domain is still held.
			if obsoleteVersionedStaging != "" {
				removeStagingArtifactsIfUnowned(obsoleteVersionedStaging, obsoleteVersionedStaging+".meta", domain)
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
				blobHash := finalSHA256
				if !useHFCache {
					blobHash = ""
				}
				manifestBuilder.AddFile(it.RelativePath, blobHash, it.Size, it.LFS)
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

	// Reconcile each skipped shared blob's own snapshot/friendly paths after all
	// transfers finish. Success requires every skipped blob to still be complete
	// and reachable: a vanished blob, a failed availability/ownership check, and
	// a link failure are distinct job errors. Every link is still checked; a
	// cancelled job reports its cancellation, not a reconcile side effect.
	var reconcileErr error
	recordReconcile := func(err error) {
		if reconcileErr == nil && jobCtx.Err() == nil {
			reconcileErr = err
		}
	}
	if useHFCache && repoDir != nil {
		skippedLinksMu.Lock()
		links := append([]skippedBlobLink(nil), skippedLinks...)
		skippedLinksMu.Unlock()
		for _, link := range links {
			if jobCtx.Err() != nil {
				break
			}
			status, _, cerr := repoDir.CheckBlob(link.sha256)
			if cerr != nil {
				recordReconcile(fmt.Errorf("%w: the blob of skipped %s could not be re-confirmed (%s): %w",
					ErrCacheBlobCheckFailed, link.relativePath, status, cerr))
				continue
			}
			if status != BlobComplete {
				recordReconcile(fmt.Errorf("%w: blob %s of skipped %s is %s at final reconciliation; run the download again to fetch it",
					ErrCacheBlobUnavailable, link.sha256, link.relativePath, status))
				continue
			}
			blobPath := repoDir.BlobPath(link.sha256)
			domainDst, derr := repoDir.blobDomainDst(link.sha256)
			if derr != nil {
				recordReconcile(fmt.Errorf("%w: resolve the ownership scope of blob %s for skipped %s: %w",
					ErrCacheBlobCheckFailed, link.sha256, link.relativePath, derr))
				continue
			}
			domain, derr := acquireBlobDomainScoped(jobCtx, blobPath, domainDst, ownership, []string{blobPath}, blobPath)
			if derr != nil {
				recordReconcile(fmt.Errorf("%w: acquire the ownership scope of blob %s for skipped %s: %w",
					ErrCacheBlobCheckFailed, link.sha256, link.relativePath, derr))
				continue
			}
			if _, statErr := os.Stat(blobPath); statErr != nil {
				derr = fmt.Errorf("%w: blob %s disappeared before link reconciliation: %w", ErrCacheBlobUnavailable, link.sha256, statErr)
			} else {
				derr = createCacheEntryLinksScoped(jobCtx, repoDir, link.commit, link.relativePath, link.filterSubdir, link.sha256, !friendlyView, ownership, friendlyOwnership)
			}
			domain.release()
			if derr != nil {
				recordReconcile(derr)
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
	// A cancellation landing after a reconcile error was recorded still reports
	// the cancellation: it is not a failure of a cancelled job, and jobCtx can
	// only be cancelled here by an abort (handled above) or the caller's context.
	if jobCtx.Err() != nil {
		return jobCtx.Err()
	}
	if reconcileErr != nil {
		emit(ProgressEvent{Level: "error", Event: "error", Message: reconcileErr.Error()})
		return reconcileErr
	}

	// Post-download finalization (refs, building the friendly view, rebuild
	// script, manifest) can take a while — copying the friendly view on
	// filesystems without symlink support is the slow part — so signal the
	// phase to the UI instead of leaving the job stuck at 100%.
	if useHFCache && repoDir != nil && friendlyView {
		emit(ProgressEvent{Event: "finalizing", Message: "finalizing"})
	}

	// For HF Cache mode: write ref file and ensure friendly directory exists
	if useHFCache && repoDir != nil {
		// Write refs/main (or the revision used)
		ref := job.Revision
		if ref == "" {
			ref = "main"
		}
		if plan.CommitPinned {
			refPath, err := repoDir.RefPath(ref)
			if err == nil {
				err = withExternalWriter(jobCtx, ownership, refPath, func() error {
					return repoDir.WriteRef(ref, plan.Commit)
				})
			}
			if err != nil {
				if job.BranchVariant {
					return failBranchVariant(fmt.Errorf("write ref %q: %w", ref, err))
				}
				emit(ProgressEvent{Level: "warn", Event: "warning", Message: fmt.Sprintf("failed to write ref: %v", err)})
			}
		}
		// Ensure friendly directory structure exists (unless disabled)
		if friendlyView {
			if err := repoDir.EnsureFriendlyDir(); err != nil {
				emit(ProgressEvent{Level: "warn", Event: "warning", Message: fmt.Sprintf("failed to create friendly dir: %v", err)})
			}
		}
	}

	// Write/update the rebuild shell script if using HF cache (unless friendly
	// view disabled). WriteRebuildScript admits the script entry itself, so
	// the write must not arrive already holding those same keys; the job
	// context still bounds its wait.
	if hfCache != nil && friendlyView {
		if _, err := hfCache.writeRebuildScriptCtx(jobCtx); err != nil {
			emit(ProgressEvent{Level: "warn", Event: "warning", Message: fmt.Sprintf("failed to write rebuild script: %v", err)})
		}
	}

	// Write manifest file (hfd.yaml) if using HF cache (unless friendly view disabled)
	if manifestBuilder != nil && repoDir != nil && friendlyView {
		manifest := manifestBuilder.Build()
		manifestPath := filepath.Join(repoDir.FriendlyPath(), ManifestFilename)
		err := withExternalWriter(jobCtx, friendlyOwnership, manifestPath, func() error {
			_, err := manifest.Write(repoDir.FriendlyPath())
			return err
		})
		if err != nil {
			emit(ProgressEvent{Level: "warn", Event: "warning", Message: fmt.Sprintf("failed to write manifest: %v", err)})
		}
	}
	if job.BranchVariant {
		manifest := manifestBuilder.Build()
		var manifestDir string
		var err error
		if useHFCache {
			manifestDir, err = BranchVariantManifestDir(repoDir, job.Revision, plan.Commit)
		} else {
			manifestDir, err = localSpace.RecordDir(job.Revision, plan.Commit)
		}
		if err == nil {
			manifestPath := filepath.Join(manifestDir, ManifestFilename)
			err = withExternalWriter(jobCtx, ownership, manifestPath, func() error {
				_, err := manifest.Write(manifestDir)
				return err
			})
		}
		if err != nil {
			return failBranchVariant(fmt.Errorf("write branch variant completion manifest: %w", err))
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

func invalidateBranchVariantRecordsWithOwnership(ctx context.Context, realm *ownershipRealm, space BranchVariantLocalSpace, revision string) error {
	recordDir, err := space.recordRevisionDir(revision)
	if err != nil {
		return err
	}
	for {
		info, statErr := os.Lstat(recordDir)
		if errors.Is(statErr, os.ErrNotExist) {
			preparation, err := realm.prepareWriter(ctx, []string{filepath.Join(recordDir, ".hfdesk-record-admission")}, 0)
			if err != nil {
				return err
			}
			_, secondStatErr := os.Lstat(recordDir)
			if secondStatErr == nil {
				preparation.Release()
				continue
			}
			if !errors.Is(secondStatErr, os.ErrNotExist) {
				preparation.Release()
				return secondStatErr
			}
			err = space.InvalidateRecords(revision)
			preparation.Release()
			return err
		}
		if statErr != nil {
			return statErr
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("unsafe branch variant record directory %s", recordDir)
		}
		lease, err := realm.acquireTreeRemoval(ctx, recordDir, blobOwnershipWaitBudget)
		if err != nil {
			return err
		}
		err = space.InvalidateRecords(revision)
		lease.Release()
		return err
	}
}

func withExternalWriter(ctx context.Context, realm *ownershipRealm, path string, write func() error) error {
	lease, err := realm.acquireWriterWithWait(ctx, []string{path}, blobOwnershipWaitBudget, nil)
	if err != nil {
		return err
	}
	defer lease.Release()
	return write()
}

func branchVariantStagingName(revision, commit string, item PlanItem) string {
	if canonicalSHA256(item.SHA256) {
		return "tmp-" + item.SHA256
	}
	sum := sha256.Sum256([]byte(revision + "\x00" + commit + "\x00" + item.RelativePath))
	return "tmp-path-" + hex.EncodeToString(sum[:])
}

func publishBranchVariantFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.Rename(src, dst)
}

// cleanupPartialsOnCancel removes single/legacy partials after explicit cancel.
// The caller holds the destination domain; ranged cleanup occurs under its claim.
func cleanupPartialsOnCancel(cfg Settings, dst string, domain *blobDomain) {
	if cfg.CleanupPartialsOnCancel == nil || !cfg.CleanupPartialsOnCancel() {
		return
	}
	if domain == nil || domain.released || !domain.coversStaging(dst+".incomplete") {
		downloadLog.Warn("skip cancel cleanup: the destination ownership domain is not held", "path", dst)
		return
	}
	// dst was confined to its destination root by SafeJoin in Download.
	cleanupLegacyPartials(dst)
}

// cleanupLegacyPartials removes single-request and legacy multipart artifacts.
func cleanupLegacyPartials(dst string) {
	removePartialWithIdentity(dst)
	if err := removeMultipartResumeFiles(dst); err != nil {
		downloadLog.Warn("cleanup multipart resume files failed", "path", dst, "error", err)
	}
}

// removePartialWithIdentity removes the sidecar only after removing the partial;
// a surviving partial must retain the identity needed to resume it.
func removePartialWithIdentity(dst string) {
	// lgtm[go/path-injection]
	if err := os.Remove(dst + ".part"); err != nil && !os.IsNotExist(err) {
		downloadLog.Warn("cleanup partial file failed", "path", dst+".part", "error", err)
		return
	}
	removeSinglePartIdentity(singlePartIdentityPath(dst))
}

// CleanupJobPartFiles removes artifacts for the exact destinations tracked by
// the caller (typically via OnPartialFile); it does not scan the repository.
// It acquires each destination domain and leaves artifacts untouched if
// ownership cannot be established. Removal is best-effort. See doc.go for the
// cache/flat artifact families and ownership contract.
func CleanupJobPartFiles(settings Settings, dsts []string) error {
	useHFCache := settings.CacheDir != "" || settings.OutputDir == ""
	root := cleanupRoot(settings, useHFCache)
	var realm *ownershipRealm
	if useHFCache {
		cache, err := settings.BuildHFCache()
		if err != nil {
			return err
		}
		realm, err = cache.ownershipRealmForData()
		if err != nil {
			return err
		}
	} else {
		ownerDir, err := resolveOwnershipDir(settings.OwnershipDir)
		if err != nil {
			return err
		}
		realm, err = newOwnershipRealm(ownerDir)
		if err != nil {
			return err
		}
		if err := validateOwnershipRealmOutside(realm, settings.OutputDir); err != nil {
			return err
		}
	}
	for _, dst := range dsts {
		if !pathWithinRoot(root, dst) {
			downloadLog.Warn("skip cleanup for out-of-root partial destination", "path", dst)
			continue
		}
		// Use the writer's domain so cleanup cannot unlink a live partial.
		var key, domainDst, proofPath string
		var paths []string
		if useHFCache {
			k, ok := cacheDstDomainKey(dst)
			if !ok {
				downloadLog.Warn("skip cleanup of a cache destination with no derivable ownership domain", "path", dst)
				continue
			}
			key, domainDst, proofPath = k, dst, dst
			paths = appendPartialOwnershipPaths(paths, dst)
			if sha, found := strings.CutPrefix(filepath.Base(dst), "tmp-"); found && canonicalSHA256(sha) {
				blobPath := filepath.Join(filepath.Dir(dst), sha)
				incomplete := blobPath + ".incomplete"
				meta := incomplete + ".meta"
				paths = appendUniqueOwnershipPaths(paths, blobPath, incomplete, meta, meta+stagingRecordTempSuffix)
				proofPath = blobPath
			}
		} else {
			flatDom := newFlatDestinationDomain(dst)
			key, domainDst, proofPath = flatDom.key, flatDom.lockSrc, dst
			paths = appendPartialOwnershipPaths(paths, dst)
		}
		domain, derr := acquireBlobDomainScopedForCleanup(context.Background(), key, domainDst, realm, paths, proofPath)
		if derr != nil {
			// Preserve artifacts if an owner is live or ownership is uncertain.
			downloadLog.Warn("skip cleanup of a destination owned by another live writer",
				"path", dst, "error", derr)
			continue
		}
		if err := removePartialArtifacts(dst, domain); err != nil {
			downloadLog.Warn("cleanup partials failed", "path", dst, "error", err)
		}
		if useHFCache {
			removeHFCacheTemp(dst)
			removeBlobStagingForTemp(dst, domain)
		}
		domain.release()
	}
	return nil
}

// removeBlobStagingForTemp derives and removes blob-keyed ranged state for a
// canonical tmp-<sha> destination. It requires the covering domain and uses
// record-checked cleanup; other names are untouched.
func removeBlobStagingForTemp(dst string, domain *blobDomain) {
	base := filepath.Base(dst)
	sha, ok := strings.CutPrefix(base, "tmp-")
	if !ok || sha == "" || !canonicalSHA256(sha) {
		return
	}
	// lgtm[go/path-injection]
	staging := filepath.Join(filepath.Dir(dst), sha+".incomplete")
	if !domain.coversStaging(staging) {
		downloadLog.Warn("skip cleanup of blob staging not covered by the destination ownership domain",
			"path", dst, "staging", staging)
		return
	}
	removeStagingArtifactsIfUnowned(staging, staging+".meta", domain)
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

func downloadOwnershipPaths(useHFCache bool, repoDir *RepoDir, dst, publishPath string, it PlanItem) (key, domainDst, proofPath string, paths []string, err error) {
	paths = appendPartialOwnershipPaths(paths, dst)
	if useHFCache {
		var ok bool
		key, ok = cacheDstDomainKey(dst)
		if !ok {
			return "", "", "", nil, fmt.Errorf("the destination is not a cache staging name")
		}
		domainDst = dst
		proofPath = dst
		if canonicalSHA256(it.SHA256) {
			blobPath := repoDir.BlobPath(it.SHA256)
			incomplete := repoDir.IncompletePath(it.SHA256)
			meta := repoDir.IncompleteMetaPath(it.SHA256)
			paths = appendUniqueOwnershipPaths(paths, blobPath, incomplete, meta, meta+stagingRecordTempSuffix)
			proofPath = blobPath
		}
		return key, domainDst, proofPath, paths, nil
	}
	flat := newFlatDestinationDomain(dst)
	key, domainDst, proofPath = flat.key, flat.lockSrc, dst
	if publishPath != dst {
		paths = appendUniqueOwnershipPaths(paths, appendPartialOwnershipPaths(nil, publishPath)...)
	}
	return key, domainDst, proofPath, paths, nil
}

func appendPartialOwnershipPaths(paths []string, dst string) []string {
	meta := dst + ".incomplete.meta"
	return appendUniqueOwnershipPaths(paths,
		dst, dst+".part", singlePartIdentityPath(dst),
		dst+".incomplete", meta, meta+stagingRecordTempSuffix,
		dst+".parts.json")
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

// removePartialArtifacts removes single, legacy and ranged artifacts under
// the caller-held domain, with record checks for ranged state. Blob-keyed
// ranged state is handled separately by removeBlobStagingForTemp.
func removePartialArtifacts(dst string, domain *blobDomain) error {
	removePartialWithIdentity(dst)
	removeStagingArtifactsIfUnowned(dst+".incomplete", dst+".incomplete.meta", domain)
	return removeLegacyMultipartParts(dst)
}

// removeLegacyMultipartParts removes dst.part-NN (non-empty digit suffix)
// and the obsolete dst.parts.json layout record.
func removeLegacyMultipartParts(dst string) error {
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

// abortPartAttempt distinguishes internal fail-fast cancellation from user
// cancellation, so resumable payloads and metadata survive sibling failure.
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

// syncSinglePartPayload orders previously written partial bytes durable before
// any sidecar persist publishes the record that makes them inheritable; tests
// may replace it to observe the ordering.
var syncSinglePartPayload = func(f *os.File) error { return f.Sync() }

// persistSinglePartIdentity orders the partial's written bytes durable, then
// writes the sidecar: every persisted record, whatever its claim state, sits
// behind durable bytes, matching the ranged family's unconditional checkpoint
// sync. Checkpoint boundary only, never per write chunk.
func persistSinglePartIdentity(out *os.File, sidecar string, id singlePartIdentity) error {
	if err := syncSinglePartPayload(out); err != nil {
		return fmt.Errorf("sync single-part payload of %s before its sidecar persist: %w", id.Path, err)
	}
	return writeSinglePartIdentity(sidecar, id)
}

// downloadSingle resumes dst.part only with a matching pinned sidecar and
// validates response framing. An exact-size partial without clean-framing
// proof revalidates its final byte. Required digest proofs run before rename;
// the returned bool prevents a duplicate proof. See doc.go for Verify modes.
func downloadSingle(ctx context.Context, httpc *http.Client, token string, job Job, cfg Settings, it PlanItem, dst string, opts singleOpts, emit func(ProgressEvent)) (bool, error) {
	const unboundedBody = int64(^uint64(0) >> 1)

	tmp := dst + ".part"
	out, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return false, err
	}
	defer out.Close()

	// Bind the partial to the object; path-derived names can be shared across
	// objects and revisions.
	identity := singleIdentityFor(job, it, opts)
	sidecar := singlePartIdentityPath(dst)
	recorded, rerr := readSinglePartIdentity(sidecar)
	if rerr != nil {
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
	stat := fi.Size()

	// pos is the record's durable offset, never the stat-visible size: bytes
	// above it were written after the last checkpoint, so a size-preserving
	// media loss could hide holes below the stat size. They are discarded here
	// and re-fetched below; only known-digest proof is not what bounds them.
	pos := int64(0)
	if stat > 0 {
		switch {
		case !singlePartialInheritable(recorded, identity, stat):
			// A size match is not identity; discard prefixes without a matching record.
			if recorded == nil {
				downloadLog.Warn("discarding old or unproven single-path partial; re-downloading from zero",
					"repo", job.Repo,
					"path", it.RelativePath,
					"bytes", stat,
					"record", sidecar,
				)
			} else {
				downloadLog.Info("discarding unproven single-path partial",
					"repo", job.Repo,
					"path", it.RelativePath,
					"bytes", stat,
					"identity_recorded", recorded != nil,
				)
			}
			if err := out.Truncate(0); err != nil {
				return false, err
			}
		case recorded.Offset > stat:
			// The record claims durable bytes this partial does not hold;
			// neither side can prove which survived, so restart rather than
			// trust a prefix the durability claim cannot back.
			downloadLog.Warn("discarding single-path partial below its durable offset; re-downloading from zero",
				"repo", job.Repo,
				"path", it.RelativePath,
				"bytes", stat,
				"durable_offset", recorded.Offset,
				"record", sidecar,
			)
			if err := out.Truncate(0); err != nil {
				return false, err
			}
		default:
			pos = recorded.Offset
			if stat > pos {
				downloadLog.Info("discarding single-path partial bytes above the durable offset",
					"repo", job.Repo,
					"path", it.RelativePath,
					"bytes", stat-pos,
					"durable_offset", pos,
				)
				if err := out.Truncate(pos); err != nil {
					return false, err
				}
			}
		}
	}
	// inherited marks the on-disk prefix that this run did not write.
	inherited := pos > 0

	// Persist identity before trusting bytes. Framing proof carries only with a
	// matching exact-size partial; without it the prefix may resume, but a full
	// partial must revalidate its final byte. The claim carries the durable
	// offset just derived, or zero when the discard below reclaims those bytes,
	// so it never outlasts the prefix it describes.
	sizeValid := !((it.Size > 0 && pos > it.Size) || (it.Size <= 0 && pos > 0))
	identity.FramingComplete = it.Size > 0 && pos == it.Size && recorded != nil &&
		recorded.FramingComplete && recorded.inheritableBy(identity)
	identity.Offset = pos
	if !sizeValid {
		identity.Offset = 0
	}
	if err := persistSinglePartIdentity(out, sidecar, identity); err != nil {
		return false, fmt.Errorf("write single-part identity %s: %w", sidecar, err)
	}

	// Discard oversized partials and any partial with unknown expected size.
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

	// An exact-size partial may finalize only with clean-framing proof.
	if it.Size > 0 && pos == it.Size {
		if identity.FramingComplete {
			emit(ProgressEvent{Event: "file_progress", Path: it.RelativePath, Downloaded: it.Size, Total: it.Size})
			return finalizeSinglePartial(ctx, out, tmp, dst, it, cfg, inherited, opts, emit)
		}
		// Written size does not prove clean EOF. Reserve and re-request the final
		// byte so a cleanly framed response commits the partial. The reservation
		// is the request offset only: the record already claims the full prefix
		// durable and framing_complete — not the stat size — still gates
		// publication, while truncating here would leave the record overclaiming
		// the partial until the next checkpoint.
		downloadLog.Info("revalidating unframed single-path final byte",
			"repo", job.Repo,
			"path", it.RelativePath,
			"bytes", it.Size,
		)
		pos = it.Size - 1
		forceLastByteRange = pos == 0
	}
	if pos > 0 {
		emit(ProgressEvent{Event: "file_progress", Path: it.RelativePath, Downloaded: pos, Total: it.Size})
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
	// hwm tracks progress refunds; ignored-Range restarts update it without a
	// refund. The lifetime ceiling applies only after requests return.
	hwm := pos
	// Count unique durable bytes, including the inherited prefix, only once.
	countedUpTo := pos
	var lastEmit time.Time

	for {
		unpinnedRestart := !opts.CommitPinned && attempt > 0
		if unpinnedRestart {
			// A mutable branch identity cannot authorize stitching bytes from
			// separate responses. Retry the complete file from byte zero.
			if err := out.Truncate(0); err != nil {
				return false, err
			}
			pos, countedUpTo = 0, 0
			forceLastByteRange = false
			identity.FramingComplete = false
			identity.Offset = 0
			if err := persistSinglePartIdentity(out, sidecar, identity); err != nil {
				return false, fmt.Errorf("write single-part identity %s: %w", sidecar, err)
			}
		}
		select {
		case <-ctx.Done():
			out.Close()
			// An internal fail-fast abort must not delete resumable bytes.
			if !abortPartAttempt(ctx) {
				cleanupPartialsOnCancel(cfg, dst, opts.Domain)
			}
			return false, ctx.Err()
		default:
		}

		// The stall watchdog cancels this request without canceling the job.
		attemptCtx, attemptCancel := context.WithCancel(ctx)

		req, _ := http.NewRequestWithContext(attemptCtx, "GET", it.URL, nil)
		addAuth(req, token)
		if it.Size > 0 && (pos > 0 || forceLastByteRange) {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", pos, it.Size-1))
		}

		// waitResp carries a throttling response (429/503) whose Retry-After /
		// RateLimit header must shape the next wait.
		var waitResp *http.Response
		// Ignored-Range 200 restarts are destructive, not progress refunds.
		truncatedRestart := unpinnedRestart
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
					// Fail the job without retry. Remove only an empty partial; retain the
					// sidecar whenever resumable bytes remain, so a later run keeps their identity.
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
					// An ignored Range restarts the partial from zero.
					if pos > 0 {
						truncatedRestart = true
						if err := out.Truncate(0); err != nil {
							resp.Body.Close()
							attemptCancel()
							return false, err
						}
						pos = 0
						// The restart discarded inherited bytes, so they no longer require a proof.
						inherited = false
						countedUpTo = 0
						lastEmit = time.Time{}
					}
					// Correct the persisted claim before any byte it authorizes is
					// rewritten: a crash mid-rewrite must leave a record claiming
					// nothing, or a later resume would trust the rewritten prefix
					// without a payload sync ever covering it.
					if identity.Offset > 0 {
						identity.FramingComplete = false
						identity.Offset = 0
						if err := persistSinglePartIdentity(out, sidecar, identity); err != nil {
							resp.Body.Close()
							attemptCancel()
							return false, fmt.Errorf("write single-part identity %s: %w", sidecar, err)
						}
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
					w.measure = payloadIOMeasurementFromContext(ctx)
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
						// Clean EOF and exact length establish framing. Persist that proof
						// before publication: the claim must be durable, so a failed payload
						// or record sync fails the item instead of publishing bytes whose
						// durability ordering is known to be broken. The whole written
						// range becomes the durable offset with this same ordering.
						identity.FramingComplete = true
						identity.Offset = base + w.written
						werr := persistSinglePartIdentity(out, sidecar, identity)
						attemptCancel()
						if werr != nil {
							return false, fmt.Errorf("persist single-part framing checkpoint %s: %w", sidecar, werr)
						}
						// Framing is complete; finalization checks any known size and required digest.
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
						// Resume from bytes actually written, but roll back the final byte when an
						// error prevents clean framing so the next attempt requests a non-empty range.
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
					// Attempt-boundary checkpoint: make the bytes actually on disk
					// durable in the sidecar before the next attempt, mirroring the
					// ranged family's attempt-boundary flush — bounded to attempt
					// boundaries (payload sync, then record sync, then rename),
					// never per write chunk. It fires only when the claim must
					// move, so a no-progress failure costs no sync, and only for
					// pinned transfers: an unpinned record never authorizes
					// inherited bytes, so its offset is never consumed.
					if opts.CommitPinned && pos != identity.Offset {
						identity.Offset = pos
						if err := persistSinglePartIdentity(out, sidecar, identity); err != nil {
							return false, fmt.Errorf("persist single-part resume checkpoint %s: %w", sidecar, err)
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
					cleanupPartialsOnCancel(cfg, dst, opts.Domain)
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
				cleanupPartialsOnCancel(cfg, dst, opts.Domain)
			}
			return false, ctx.Err()
		}
	}
	return false, lastErr
}

// singleProofStartHook lets tests cancel before hashing while the writable
// partial is still open, exercising Windows close-before-cleanup ordering.
var singleProofStartHook func(out *os.File)

// finalizeSinglePartial requires clean response framing (from this run or a
// matching exact-size sidecar), checks any known size, and publishes the partial.
// With a known digest, sha256/etag always prove before rename; other non-none
// modes prove inherited bytes, unpinned identities and unknown-size streams.
// A mismatch leaves dst untouched and attempts partial cleanup. The bool reports
// completed proof; file_finalizing precedes the slow read.
func finalizeSinglePartial(ctx context.Context, out *os.File, tmp, dst string, it PlanItem, cfg Settings, inherited bool, opts singleOpts, emit func(ProgressEvent)) (bool, error) {
	emit(ProgressEvent{Event: "file_finalizing", Path: it.RelativePath, Message: "verifying"})
	cancelBeforePublish := func() error {
		if err := ctx.Err(); err != nil {
			_ = out.Close()
			if !abortPartAttempt(ctx) {
				cleanupPartialsOnCancel(cfg, dst, opts.Domain)
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
				// Close before explicit-cancel cleanup: Windows cannot unlink this open
				// partial. Pause/internal abort still preserves payload and sidecar.
				_ = out.Close()
				if !abortPartAttempt(ctx) {
					cleanupPartialsOnCancel(cfg, dst, opts.Domain)
				}
				return false, cerr
			}
			return false, err
		}
		if err := cancelBeforePublish(); err != nil {
			return false, err
		}
		if !strings.EqualFold(sum, it.SHA256) {
			// A mismatch must leave dst untouched; attempt to discard the bad partial.
			_ = out.Close()
			if rerr := os.Remove(tmp); rerr == nil || os.IsNotExist(rerr) {
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
	// The published partial no longer needs its identity sidecar.
	removeSinglePartIdentity(tmp + ".meta")
	// Obsolete pre-#140 multipart artifacts for this destination must not
	// survive a successful single-path finalization. Finalization already
	// succeeded, so a cleanup failure is logged and does not unpublish the file.
	if err := removeMultipartResumeFiles(dst); err != nil {
		downloadLog.Warn("cleanup obsolete multipart artifacts failed", "path", dst, "error", err)
	}
	return verified, nil
}

// removeEmptyPartialFile removes an empty partial and reports confirmed absence.
// A false result preserves its identity sidecar for any surviving resume bytes.
func removeEmptyPartialFile(path string) bool {
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return true
		}
		// A failed stat does not prove absence; preserve the sidecar for possible
		// surviving bytes.
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
