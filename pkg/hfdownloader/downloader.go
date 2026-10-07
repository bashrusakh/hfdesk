// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
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

func removeUnexpectedMultipartPartFiles(dst string, layout multipartResumeLayout) error {
	files, err := multipartPartFiles(dst)
	if err != nil {
		return err
	}
	expected := make(map[string]struct{}, len(layout.Parts))
	for _, part := range layout.Parts {
		expected[fmt.Sprintf("%s.part-%02d", dst, part.Index)] = struct{}{}
	}
	for _, p := range files {
		if _, ok := expected[p]; ok {
			continue
		}
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

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

func prepareMultipartResume(dst string, layout multipartResumeLayout) error {
	files, err := multipartPartFiles(dst)
	if err != nil {
		return err
	}
	if len(files) > 0 {
		data, err := os.ReadFile(multipartLayoutMetaPath(dst))
		if err != nil {
			if !os.IsNotExist(err) {
				return err
			}
			if err := removeMultipartResumeFiles(dst); err != nil {
				return err
			}
			return writeMultipartResumeLayout(dst, layout)
		}

		var existing multipartResumeLayout
		if err := json.Unmarshal(data, &existing); err != nil || !multipartLayoutsEqual(existing, layout) {
			if err := removeMultipartResumeFiles(dst); err != nil {
				return err
			}
			return writeMultipartResumeLayout(dst, layout)
		}
		if err := removeUnexpectedMultipartPartFiles(dst, layout); err != nil {
			return err
		}
	}

	return writeMultipartResumeLayout(dst, layout)
}

func multipartLayoutsEqual(a, b multipartResumeLayout) bool {
	if a.Size != b.Size || a.Concurrency != b.Concurrency || len(a.Parts) != len(b.Parts) {
		return false
	}
	for i := range a.Parts {
		if a.Parts[i] != b.Parts[i] {
			return false
		}
	}
	return true
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
func Download(ctx context.Context, job Job, cfg Settings, progress ProgressFunc) error {
	if ctx == nil {
		ctx = context.Background()
	}
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

	plan, err := scanRepo(ctx, httpc, cfg.Token, job, cfg)
	if err != nil {
		return err
	}

	// Emit ALL plan_item events upfront so TUI knows total size immediately
	for _, item := range plan.Items {
		displayRel := item.RelativePath
		if job.AppendFilterSubdir && item.Subdir != "" {
			displayRel = filepath.ToSlash(filepath.Join(item.Subdir, item.RelativePath))
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

	// Overall concurrency limiter (ctx-aware acquisition)
	type token struct{}
	lim := make(chan token, cfg.MaxActiveDownloads)

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
		case lim <- token{}:
		case <-jobCtx.Done():
			break LOOP
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-lim }()

			// Per-file context; ensures all inner loops stop on cancellation.
			// It derives from jobCtx so a fatal error in a sibling file also
			// stops this one promptly.
			fileCtx, fileCancel := context.WithCancel(jobCtx)
			defer fileCancel()

			finalRel := it.RelativePath
			filterSubdir := ""
			if job.AppendFilterSubdir && it.Subdir != "" {
				filterSubdir = it.Subdir
				finalRel = filepath.ToSlash(filepath.Join(it.Subdir, it.RelativePath))
			}

			var dst string
			var skipCheck func() (bool, string, error)

			if useHFCache {
				// HF Cache mode: check blob existence
				skipCheck = func() (bool, string, error) {
					if it.SHA256 != "" {
						status, _, err := repoDir.CheckBlob(it.SHA256)
						if err != nil {
							return false, "", err
						}
						if status == BlobComplete {
							// Blob exists, but ensure symlinks are in place
							if err := repoDir.createSnapshotSymlink(plan.Commit, it.RelativePath, it.SHA256); err == nil {
								if !cfg.NoFriendlyView {
									repoDir.CreateFriendlySymlink(plan.Commit, it.RelativePath, filterSubdir)
								}
							}
							return true, "blob exists", nil
						}
						if status == BlobDownloading {
							return true, "downloading by another process", nil
						}
					}
					return false, "", nil
				}
				// Download to temp location, will be moved to blob later
				// Use SHA256 as temp name to avoid collisions (e.g., multiple config.json files)
				tmpName := "tmp-" + it.SHA256
				if it.SHA256 == "" {
					// Fallback: sanitize path to avoid collisions
					tmpName = "tmp-" + strings.ReplaceAll(it.RelativePath, "/", "_")
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

			// Create a copy with updated RelativePath for progress display
			itForIO := it
			itForIO.RelativePath = finalRel

			// Choose single/multipart path
			var dlErr error
			if it.Size >= thresholdBytes && it.AcceptRanges {
				dlErr = downloadMultipart(fileCtx, httpc, cfg.Token, job, cfg, itForIO, dst, emit)
			} else {
				dlErr = downloadSingle(fileCtx, httpc, cfg.Token, job, cfg, itForIO, dst, emit)
			}
			if dlErr != nil {
				fatal := fmt.Errorf("download %s: %w", finalRel, dlErr)
				if isFailFastError(dlErr) {
					setFatalErr(fatal)
				}
				select {
				case errCh <- fatal:
				default:
				}
				return
			}

			// The bytes are on disk, but SHA-256 verification rereads the
			// whole file and storing it into the cache may copy it again —
			// minutes of local I/O for a large model, during which no
			// file_progress arrives. Mark the file as finalizing so the job
			// doesn't look stuck at 100% (the post-loop "finalizing" event
			// only covers work after every file is done).
			emit(ProgressEvent{Event: "file_finalizing", Path: finalRel, Message: "verifying"})

			// Verify after download
			if it.LFS && it.SHA256 != "" {
				if err := verifySHA256Ctx(fileCtx, dst, it.SHA256); err != nil {
					select {
					case errCh <- fmt.Errorf("sha256 verify failed: %s: %w", finalRel, err):
					default:
					}
					return
				}
			} else if cfg.Verify == "size" && it.Size > 0 {
				fi, err := os.Stat(dst)
				if err != nil || fi.Size() != it.Size {
					select {
					case errCh <- fmt.Errorf("size mismatch for %s", finalRel):
					default:
					}
					return
				}
			} else if cfg.Verify == "sha256" {
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
					if err := verifySHA256Ctx(fileCtx, dst, remoteSha); err != nil {
						select {
						case errCh <- fmt.Errorf("sha256 verify failed: %s: %w", finalRel, err):
						default:
						}
						return
					}
				}
			}

			// For HF Cache mode: move to blob and create symlinks. The store
			// re-hashes/copies a large file, so pass the per-file context: a
			// sibling's fail-fast abort must not wait for it to finish.
			var finalSHA256 string
			if useHFCache {
				sha := it.SHA256
				result, err := repoDir.storeDownloadedFileCtx(fileCtx, dst, it.RelativePath, plan.Commit, sha, filterSubdir, cfg.NoFriendlyView)
				if err != nil {
					select {
					case errCh <- fmt.Errorf("store file %s: %w", finalRel, err):
					default:
					}
					return
				}
				finalSHA256 = result.SHA256 // Use computed SHA256 from store result
			} else {
				finalSHA256 = it.SHA256
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
			atomic.AddInt64(&downloadedCount, 1)
		}()
	}

	wg.Wait()
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
	return nil
}

// cleanupPartialsOnCancel removes partial download artifacts (.part,
// .part-NN, .parts.json) for dst when the caller's cancel-cleanup
// callback reports that the context was cancelled for a non-resumable
// reason (explicit cancel, not pause).
func cleanupPartialsOnCancel(cfg Settings, dst string) {
	if cfg.CleanupPartialsOnCancel == nil || !cfg.CleanupPartialsOnCancel() {
		return
	}
	// dst is derived upstream in Download() through SafeJoin in both modes:
	// legacy mode joins the remote file path to the output directory, and HF
	// cache mode joins the tmp-<sha> name to the blobs directory. SafeJoin
	// rejects non-local values via filepath.IsLocal and proves containment
	// with PathInside, so dst cannot name a file outside those roots.
	// lgtm[go/path-injection]
	if err := os.Remove(dst + ".part"); err != nil && !os.IsNotExist(err) {
		log.Printf("warning: cleanup partial file %s: %v", dst+".part", err)
	}
	if err := removeMultipartResumeFiles(dst); err != nil {
		log.Printf("warning: cleanup multipart resume files for %s: %v", dst, err)
	}
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
// would have produced for that dst: dst+".part", dst+".part-NN"
// (any digit run), and dst+".parts.json". In HF cache mode, dst is
// the temporary tmp-<sha> path under blobs/, so the helper also removes
// dst itself for the window after the single/multipart downloader has
// renamed the completed bytes to tmp-<sha> but before StoreDownloadedFile
// has moved them into the final blob.
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
			log.Printf("warning: skip cleanup for out-of-root partial dst %s", dst)
			continue
		}
		if err := removePartialArtifacts(dst); err != nil {
			log.Printf("warning: cleanup partials for %s: %v", dst, err)
		}
		if useHFCache {
			removeHFCacheTemp(dst)
		}
	}
	return nil
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
		log.Printf("warning: cleanup partial file %s: %v", dst, err)
	}
}

// removePartialArtifacts removes all partial-file artifacts associated
// with a single downloader dst: the single-part partial (dst+".part"),
// any multipart parts (dst+".part-NN" for N being any non-empty run of
// digits, to match the downloader's %02d / %d format), and the
// multipart layout metadata (dst+".parts.json").
//
// dst is already validated as residing inside the output subtree at
// allocation time in Download() (via SafeJoin), so removing the
// derived partial paths cannot escape the output directory. The
// glob-style match for .part-NN is intentionally narrow: only the
// recognized suffix family and a digit run, so a coincidentally
// named "weights.part" or similar user file with a different base
// (which would not appear in dsts) is not touched.
func removePartialArtifacts(dst string) error {
	// lgtm[go/path-injection]
	if err := os.Remove(dst + ".part"); err != nil && !os.IsNotExist(err) {
		log.Printf("warning: cleanup partial file %s: %v", dst+".part", err)
	}
	// Multipart parts: glob the dst's directory for files matching
	// dst+".part-NNN" pattern. This matches what downloadMultipart
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
			log.Printf("warning: cleanup partial file %s: %v", full, err)
		}
	}
	// Multipart layout metadata.
	if err := os.Remove(dst + ".parts.json"); err != nil && !os.IsNotExist(err) {
		log.Printf("warning: cleanup partial file %s: %v", dst+".parts.json", err)
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
// .part/.part-NN/.parts.json bytes for a later retry.
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
// Resume behavior: if a .part file already exists from a previous interrupted
// run, its bytes are preserved and the HTTP request uses a Range header to
// fetch only the remaining bytes. If the server ignores the Range header and
// responds with 200 (full body), the .part file is truncated and the download
// restarts from zero.
func downloadSingle(ctx context.Context, httpc *http.Client, token string, job Job, cfg Settings, it PlanItem, dst string, emit func(ProgressEvent)) error {
	tmp := dst + ".part"
	out, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()

	fi, err := out.Stat()
	if err != nil {
		return err
	}
	pos := fi.Size()

	// If the partial is already exactly the right size, finalize without network.
	if it.Size > 0 && pos == it.Size {
		out.Close()
		return os.Rename(tmp, dst)
	}
	// If the partial is larger than expected (stale/corrupt), start over.
	if it.Size > 0 && pos > it.Size {
		if err := out.Truncate(0); err != nil {
			return err
		}
		pos = 0
	}
	if _, err := out.Seek(pos, io.SeekStart); err != nil {
		return err
	}
	if pos > 0 {
		emit(ProgressEvent{Event: "file_progress", Path: it.RelativePath, Downloaded: pos, Total: it.Size})
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

	for {
		select {
		case <-ctx.Done():
			out.Close()
			// An internal fail-fast abort must not delete resumable bytes.
			if !abortPartAttempt(ctx) {
				cleanupPartialsOnCancel(cfg, dst)
			}
			return ctx.Err()
		default:
		}

		// Each attempt gets its own cancellable context so the stall
		// watchdog can abort just this attempt (closing the connection or
		// HTTP/2 stream) without cancelling the whole job.
		attemptCtx, attemptCancel := context.WithCancel(ctx)

		req, _ := http.NewRequestWithContext(attemptCtx, "GET", it.URL, nil)
		addAuth(req, token)
		if pos > 0 {
			if it.Size > 0 {
				req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", pos, it.Size-1))
			} else {
				req.Header.Set("Range", fmt.Sprintf("bytes=%d-", pos))
			}
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
			// If we asked for a range but the server returned the whole body,
			// throw away any existing partial bytes and start fresh.
			if pos > 0 && resp.StatusCode == http.StatusOK {
				truncatedRestart = true
				if err := out.Truncate(0); err != nil {
					resp.Body.Close()
					attemptCancel()
					return err
				}
				if _, err := out.Seek(0, io.SeekStart); err != nil {
					resp.Body.Close()
					attemptCancel()
					return err
				}
				pos = 0
			}
			if apiErr := classifyFileResponse(resp, it.URL); apiErr != nil {
				lastErr = apiErr
				resp.Body.Close()
				if apiErr.IsFailFast() {
					// Permanent for the whole job: no retry, no request
					// storm. Remove any empty partial this attempt created
					// before returning the actionable error.
					attemptCancel()
					out.Close()
					removeEmptyPartialFile(tmp)
					return lastErr
				}
				if !apiErr.IsRetryable() {
					attemptCancel()
					out.Close()
					return lastErr
				}
				waitResp = resp
			} else {
				// The stall watchdog wraps the raw network body BEFORE the
				// speed limiter's pacing, so deliberate throttling is not
				// mistaken for a stalled peer.
				body := newStallReader(resp.Body, stall, attemptCancel)
				pr := newProgressReader(cfg.SpeedLimiter.Reader(attemptCtx, body), it.Size, it.RelativePath, emit)
				pr.downloaded = pos // emitted progress reflects cumulative bytes
				_, cerr := io.Copy(out, pr)
				resp.Body.Close()
				attemptCancel()
				if cerr == nil {
					out.Close()
					return os.Rename(tmp, dst)
				}
				// If this attempt's stall watchdog fired, the failure is a
				// stall, not a caller cancellation — classifyStall keeps a
				// done parent context (genuine cancel) unchanged.
				lastErr = classifyStall(ctx, body, cerr)
				// Update pos to current file position so the next retry issues
				// a Range request for the remaining bytes instead of duplicating.
				if cur, serr := out.Seek(0, io.SeekCurrent); serr == nil {
					pos = cur
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
					cleanupPartialsOnCancel(cfg, dst)
				}
				return ctx.Err()
			}
			break
		}
		remaining--
		attempt++
		emit(ProgressEvent{Event: "retry", Path: it.RelativePath, Attempt: attempt, Message: lastErr.Error()})
		if d := NextRetryWait(waitResp, retry.Next(), DefaultMaxRetryAfter, time.Now()); !sleepCtx(ctx, d) {
			out.Close()
			if !abortPartAttempt(ctx) {
				cleanupPartialsOnCancel(cfg, dst)
			}
			return ctx.Err()
		}
	}
	return lastErr
}

// removeEmptyPartialFile removes path when it exists and is empty. It is used
// to clean up a zero-byte .part file that a fail-fast attempt created, without
// touching a partial that predates the attempt and may hold resumable bytes.
func removeEmptyPartialFile(path string) {
	fi, err := os.Stat(path)
	if err != nil {
		return
	}
	if fi.Size() != 0 {
		return
	}
	// lgtm[go/path-injection]
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		log.Printf("warning: cleanup empty partial file %s: %v", path, err)
	}
}

// downloadMultipart downloads a file using multiple parallel range requests.
func downloadMultipart(ctx context.Context, httpc *http.Client, token string, job Job, cfg Settings, it PlanItem, dst string, emit func(ProgressEvent)) error {
	// HEAD to resolve size, reissued only when the server throttles (429/503)
	// and bounded by the same attempt/backoff conventions as the transfer
	// loops: wait the larger of the server-requested Retry-After and the
	// local backoff (capped at DefaultMaxRetryAfter) with a context-aware
	// sleep so pause/cancel/shutdown stays prompt, then reissue the HEAD
	// within the configured retry budget.
	retry := newRetry(cfg)
	remaining := cfg.Retries
	if remaining < 0 {
		remaining = 0
	}
	attempt := 0
	for {
		req, _ := http.NewRequestWithContext(ctx, "HEAD", it.URL, nil)
		addAuth(req, token)
		resp, err := httpc.Do(req)
		if err != nil {
			return err
		}
		// A HEAD response carries headers only, so its body is closed
		// immediately instead of being held open (via defer) for the whole
		// parallel transfer. Status and headers stay readable after Close
		// for the classification and size resolution below.
		resp.Body.Close()

		apiErr := classifyFileResponse(resp, it.URL)

		// A permanent gated/private/missing response on HEAD fails the whole
		// job immediately with the actionable message, before any part
		// requests are issued.
		if apiErr != nil && apiErr.IsFailFast() {
			removeEmptyPartialFile(dst + ".part")
			return apiErr
		}

		// A throttling 429/503 must not be accepted as the resolved HEAD.
		// Every other status keeps the previous behavior (proceed and let
		// the part loop classify it).
		if resp.StatusCode != http.StatusTooManyRequests &&
			resp.StatusCode != http.StatusServiceUnavailable {
			if it.Size == 0 {
				if clen := resp.Header.Get("Content-Length"); clen != "" {
					var n int64
					fmt.Sscan(clen, &n)
					it.Size = n
				}
			}
			break
		}

		// Throttled: wait out the server-requested Retry-After (or the local
		// backoff, whichever is larger) and reissue the HEAD. When the retry
		// budget is exhausted, surface the transient error like an exhausted
		// transfer retry instead of silently proceeding with a throttled
		// response.
		if remaining == 0 {
			return apiErr
		}
		remaining--
		attempt++
		emit(ProgressEvent{Event: "retry", Path: it.RelativePath, Attempt: attempt, Message: apiErr.Error()})
		if d := NextRetryWait(resp, retry.Next(), DefaultMaxRetryAfter, time.Now()); !sleepCtx(ctx, d) {
			return ctx.Err()
		}
	}
	if it.Size == 0 {
		return downloadSingle(ctx, httpc, token, job, cfg, it, dst, emit)
	}

	// Plan parts
	n := cfg.Concurrency
	chunk := it.Size / int64(n)
	if chunk <= 0 {
		chunk = it.Size
		n = 1
	}
	layout := buildMultipartResumeLayout(it.Size, n, chunk)
	if err := prepareMultipartResume(dst, layout); err != nil {
		return err
	}

	tmpParts := make([]string, n)
	for i := 0; i < n; i++ {
		tmpParts[i] = fmt.Sprintf("%s.part-%02d", dst, i)
	}

	// Download parts in parallel
	var wg sync.WaitGroup
	errCh := make(chan error, n)

	// Part-scoped cancellable context: a permanent (fail-fast) error on any one
	// part cancels the whole file so sibling parts stop promptly instead of
	// downloading/retrying/waiting until wg.Wait() returns. The cancel cause is
	// an abortCause wrapping the original error (see abortError), which lets a
	// sibling part that exits via its ctx path distinguish an internal abort
	// from a user cancel/pause.
	partCtx, partCancel := context.WithCancelCause(ctx)
	defer partCancel(nil)

	// partErr records the first permanent part error so the caller surfaces the
	// original error rather than a sibling's "context canceled" from errCh.
	var (
		partErrMu sync.Mutex
		partErr   error
	)

	for i := 0; i < n; i++ {
		i := i
		start := int64(i) * chunk
		end := start + chunk - 1
		if i == n-1 {
			end = it.Size - 1
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			tmp := tmpParts[i]
			expected := end - start + 1

			// Open or create the part file without truncating; we may be
			// resuming from a previous interrupted run.
			out, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE, 0o644)
			if err != nil {
				select {
				case errCh <- err:
				default:
				}
				return
			}
			defer out.Close()

			fi, err := out.Stat()
			if err != nil {
				select {
				case errCh <- err:
				default:
				}
				return
			}
			pos := fi.Size()
			// Already fully downloaded.
			if pos == expected {
				return
			}
			// Oversize (stale/corrupt) — reset.
			if pos > expected {
				if err := out.Truncate(0); err != nil {
					select {
					case errCh <- err:
					default:
					}
					return
				}
				pos = 0
			}
			if _, err := out.Seek(pos, io.SeekStart); err != nil {
				select {
				case errCh <- err:
				default:
				}
				return
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
			// hwm is the highest offset ever reached for this part, seeded with
			// the initial on-disk offset. The retry budget is refunded only when
			// an attempt moves STRICTLY above it, so an ignored-Range restart or
			// oscillation never refills the budget; the absolute ceiling then
			// bounds a server that streams a strictly growing prefix (see
			// downloadSingle for the two-term rationale).
			hwm := pos

			for {
				select {
				case <-partCtx.Done():
					return
				default:
				}

				// Per-attempt context: the stall watchdog cancels only this
				// part's attempt, leaving sibling parts and the job untouched.
				attemptCtx, attemptCancel := context.WithCancel(partCtx)

				rq, _ := http.NewRequestWithContext(attemptCtx, "GET", it.URL, nil)
				addAuth(rq, token)
				rq.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start+pos, end))

				preHwm := hwm
				var waitResp *http.Response

				rs, err := httpc.Do(rq)
				if err != nil {
					lastErr = err
				} else if rs.StatusCode == http.StatusPartialContent {
					// The stall watchdog wraps the raw network body before the
					// speed limiter's pacing.
					body := newStallReader(rs.Body, stall, attemptCancel)
					_, cerr := io.Copy(out, cfg.SpeedLimiter.Reader(attemptCtx, body))
					rs.Body.Close()
					attemptCancel()
					if cerr == nil {
						return
					}
					// A fired stall watchdog marks this attempt's failure as
					// a stall; a done partCtx keeps the genuine-cancel error
					// (see classifyStall precedence).
					lastErr = classifyStall(partCtx, body, cerr)
					// Advance pos by what we actually wrote so the next retry
					// Range request picks up from the correct offset.
					if cur, serr := out.Seek(0, io.SeekCurrent); serr == nil {
						pos = cur
					}
				} else {
					// Any non-206 is a failure. A permanent 401/403/404 fails
					// the whole job immediately; a throttling 429/503 shapes
					// the next wait; a 200 means the server ignored Range (a
					// multipart protocol violation) and is retried as before.
					rs.Body.Close()
					if apiErr := classifyFileResponse(rs, it.URL); apiErr != nil {
						lastErr = apiErr
						if apiErr.IsFailFast() {
							// Permanent for the whole file/job: cancel sibling
							// parts with the original error as the cause, record
							// it for the caller, and return. Close out BEFORE
							// removing the (possibly empty) partial so Windows
							// can unlink the still-open file — mirroring the
							// single-file ordering.
							attemptCancel()
							partErrMu.Lock()
							if partErr == nil {
								partErr = lastErr
							}
							partErrMu.Unlock()
							partCancel(abortError(lastErr))
							out.Close()
							removeEmptyPartialFile(tmp)
							select {
							case errCh <- lastErr:
							default:
							}
							return
						}
						if !apiErr.IsRetryable() {
							attemptCancel()
							partErrMu.Lock()
							if partErr == nil {
								partErr = lastErr
							}
							partErrMu.Unlock()
							partCancel(abortError(lastErr))
							out.Close()
							select {
							case errCh <- lastErr:
							default:
							}
							return
						}
						waitResp = rs
					} else {
						lastErr = fmt.Errorf("range not supported (status %s)", rs.Status)
					}
				}
				attemptCancel()

				// Refund the retry budget only when this attempt moved STRICTLY
				// above the high-water mark captured BEFORE the attempt.
				// preHwm is read first because hwm is updated unconditionally
				// below, so a restart's higher offset is recorded and can never
				// later earn a free refund from a bodyless failure. Multipart
				// has no truncation path.
				advanced := pos > preHwm
				if pos > hwm {
					hwm = pos
				}
				if advanced {
					retry.reset()
					remaining = attempts
				}

				if remaining == 0 || attempt >= ceiling {
					break
				}
				remaining--
				attempt++
				emit(ProgressEvent{Event: "retry", Path: it.RelativePath, Attempt: attempt, Message: lastErr.Error()})
				if d := NextRetryWait(waitResp, retry.Next(), DefaultMaxRetryAfter, time.Now()); !sleepCtx(partCtx, d) {
					return
				}
			}

			select {
			case errCh <- lastErr:
			default:
			}
		}()
	}

	// Emit periodic progress while parts download. The ticker is stopped
	// cleanly after wg.Wait() so it cannot observe mid-assembly state (parts
	// being deleted) and emit a bogus 0-byte progress event — the bug behind
	// the "progress jumps 2.4% ↔ 2.5% for hours" symptom in github #75.
	tickerDone := make(chan struct{})
	var tickerWG sync.WaitGroup
	tickerWG.Add(1)
	go func() {
		defer tickerWG.Done()
		t := time.NewTicker(200 * time.Millisecond) // More frequent updates for responsive UI
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tickerDone:
				return
			case <-t.C:
				var downloaded int64
				for _, p := range tmpParts {
					if fi, err := os.Stat(p); err == nil {
						downloaded += fi.Size()
					}
				}
				emit(ProgressEvent{Event: "file_progress", Path: it.RelativePath, Downloaded: downloaded, Total: it.Size})
			}
		}
	}()

	wg.Wait()

	// Stop the progress ticker and wait for it to exit before touching the
	// part files. Any in-flight tick will finish and emit one final event
	// while parts are still on disk at their real sizes.
	close(tickerDone)
	tickerWG.Wait()

	// A permanent (fail-fast) part error cancels partCtx and is the error the
	// caller must see; return it in preference to the sibling "context
	// canceled" that the ctx check below would surface. An internal abort
	// must NOT run cancel-cleanup (T3): only genuinely empty partials are
	// removed, by the failing part itself, and resumable bytes are preserved.
	partErrMu.Lock()
	permanentErr := partErr
	partErrMu.Unlock()
	if permanentErr != nil {
		return permanentErr
	}

	// If the context was cancelled while parts were running (pause / abort /
	// timeout), return the cancellation error immediately. Part goroutines
	// that exit via their ctx-aware retry/sleep path do NOT push to errCh,
	// so we cannot rely on the errCh drain to catch this — we must check
	// ctx.Err() explicitly. Returning here is critical: it prevents the
	// bogus "downloaded == total" progress emit below AND stops the
	// assembly loop from stitching an incomplete part set into a corrupt
	// final file and deleting the partial bytes the next resume needs.
	//
	// The guard keys off partCtx — the context a part fail-fast actually
	// cancels (via abortCause) — not the file ctx, so it stays correct even
	// if the cancel wiring changes. (A file fail-fast is already returned
	// above via partErr, so this branch handles user cancel/pause and any
	// internal abort of partCtx.)
	if ctx.Err() != nil || partCtx.Err() != nil {
		if !abortPartAttempt(partCtx) {
			cleanupPartialsOnCancel(cfg, dst)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return partCtx.Err()
	}

	select {
	case e := <-errCh:
		return e
	default:
	}

	// Emit one explicit full-progress reading so the caller's last observed
	// file_progress value is the full byte count, regardless of when the
	// ticker happened to last fire.
	emit(ProgressEvent{Event: "file_progress", Path: it.RelativePath, Downloaded: it.Size, Total: it.Size})

	// Everything past this point is local disk I/O: stitching the part files
	// back into one rewrites the entire payload, which takes real time for
	// multi-GB files. Signal it so the UI doesn't sit silently at 100%.
	emit(ProgressEvent{Event: "file_finalizing", Path: it.RelativePath, Message: "assembling parts"})

	// Abort before touching the part files if the job/part context was
	// cancelled between the guard above and here. Preserve the resumable
	// .part-NN part files (they are the resumable data); an internal fail-fast
	// abort must NOT run the user-cancel cleanup. Remove any incomplete
	// intermediate dst.part this call may have created — none yet at this
	// point, so just return the cancellation cause.
	if cerr := partCtx.Err(); cerr != nil {
		if !abortPartAttempt(partCtx) {
			cleanupPartialsOnCancel(cfg, dst)
		}
		return cerr
	}

	// Assemble parts. The context-checking reader and the per-part check make
	// the local copy stop promptly once a sibling's permanent error (or a user
	// cancel) has cancelled partCtx, instead of hashing/stitching a multi-GB
	// sibling to completion. An internal abort preserves the .part-NN files
	// (resumable data) and removes the incomplete intermediate dst.part.
	out, err := os.Create(dst + ".part")
	if err != nil {
		return err
	}

	for i := 0; i < n; i++ {
		if cerr := partCtx.Err(); cerr != nil {
			out.Close()
			removeIncompleteAssembly(cfg, dst, partCtx)
			return cerr
		}
		p := tmpParts[i]
		in, err := os.Open(p)
		if err != nil {
			out.Close()
			removeIncompleteAssembly(cfg, dst, partCtx)
			return err
		}
		if _, err := io.Copy(out, contextReader{ctx: partCtx, r: in}); err != nil {
			in.Close()
			out.Close()
			if cerr := partCtx.Err(); cerr != nil {
				removeIncompleteAssembly(cfg, dst, partCtx)
				return cerr
			}
			return err
		}
		in.Close()
	}
	out.Close()

	if cerr := partCtx.Err(); cerr != nil {
		removeIncompleteAssembly(cfg, dst, partCtx)
		return cerr
	}

	if err := os.Rename(dst+".part", dst); err != nil {
		return err
	}

	for _, p := range tmpParts {
		_ = os.Remove(p)
	}
	_ = os.Remove(multipartLayoutMetaPath(dst))

	return nil
}

// removeIncompleteAssembly handles a cancellation that landed during part
// assembly. On an internal fail-fast abort it preserves the resumable .part-NN
// part files and removes only the incomplete intermediate dst.part (which is
// not resumable and would otherwise be a corrupt leftover). On a user cancel
// it keeps the existing cleanupPartialsOnCancel semantics unchanged.
func removeIncompleteAssembly(cfg Settings, dst string, ctx context.Context) {
	if abortPartAttempt(ctx) {
		// lgtm[go/path-injection]
		if err := os.Remove(dst + ".part"); err != nil && !os.IsNotExist(err) {
			log.Printf("warning: cleanup incomplete assembly %s: %v", dst+".part", err)
		}
		return
	}
	cleanupPartialsOnCancel(cfg, dst)
}
