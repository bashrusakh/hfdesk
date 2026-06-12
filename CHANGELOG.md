# Changelog

## [Unreleased]

### Changed

- Model detail panel now uses a single scroll instead of two nested scrolls (quantization list + README preview). Section headers stick to the top while scrolling. Contextual floating arrows appear: down-arrow at the bottom when scrolled to top, up-arrow at the top when scrolled down, both arrows visible in the middle.

## [1.0.10] - 2026-06-12

### Added

- Repository names in Active Jobs are now clickable and jump straight back to model analysis.

### Changed

- Redesigned the Settings page layout with proper scrolling, clearer section grouping, and a sticky save action.
- Refined the dark theme typography and text hierarchy, increased model row readability, and widened the analysis panel.

### Fixed

- Download deduplication is now filter-aware, so downloading a quant and its `mmproj` companion creates separate jobs instead of silently collapsing into one.
- Newly created queued jobs are broadcast to the UI immediately, so they appear in Active Jobs without waiting for a later state change.
- The refresh action now reloads the selected model reliably, and the Hugging Face Hub button lives in the model detail header.
- GGUF detection now recognizes more quant names correctly, handles `BF16` files properly, and filters out `*imatrix.gguf` companion files from quant lists.

## [1.0.9] - 2026-06-11

### Fixed

- Download speed reading is steadier: averaged over a longer window with EMA smoothing instead of a raw 4-second window.
- ETA no longer bounces by minutes: it is computed server-side from the whole-run average rate and displayed with coarse units (seconds only show under two minutes).
- The finalizing status now covers the per-file post-download work — part assembly, SHA-256 verification, and the cache store — which is where a large download actually spends its "stuck at 100%" time. Previously the status only appeared for the final manifest/friendly-view step and never appeared at all in local-folder mode.

## [1.0.8] - 2026-06-06

### Added

- Retry button on failed and cancelled downloads to restart them with their original settings.
- Full README renderer: server-side sanitized Markdown with GitHub-Flavored tables and code, relative image/link rewriting, an authenticated image proxy for gated repos, and lazy-loaded images.
- "Open on Hugging Face Hub" button in the model detail panel.
- Finalizing status on the job card while post-download processing (friendly view, manifest) runs, so a finished download no longer looks stuck at 100%.

### Changed

- Active Jobs now sorts by date added (newest first); completed downloads move to History automatically, while failed, cancelled, and paused jobs stay visible.
- Compact download rows and hide the redundant `main` revision label.
- Model analysis panel now shows the Description (README) below the quantizations list.
- Local cache page now defaults to the list view.
- Web UI version is read from the build automatically (no hardcoded number to edit each release).

### Fixed

- Pause button no longer goes missing after a download starts; it now appears when a queued job begins running.
- Quantization download buttons are disabled while that quant's download is active or queued, instead of letting it be started again.
- Download speed reading is smoothed so it no longer jumps around.
- `max-active` now limits concurrent download jobs: jobs above the limit queue and start as slots free, and lowering the limit re-queues the most-recently-started excess downloads so they auto-resume as slots free.
- Download speed now reflects current transfer throughput over a short window; resuming a partial download no longer shows an inflated speed from already-downloaded bytes.
- tools/reasoning capability badges now also show for cached models, detected from the repo name and tags.
- GGUF downloads now fetch only the selected quant's files (plus any mmproj companion) instead of also pulling the repo's config/tokenizer JSON, `README`, `.gitattributes`, and `fp16/` transformers metadata. Non-GGUF downloads (e.g. safetensors) still include their required config files.

## [1.0.7] - 2026-06-05

### Fixed

- GitHub Release descriptions now use the matching section from `CHANGELOG.md`.

## [1.0.6] - 2026-06-05

### Fixed

- Recognize APEX GGUF quantization profiles (`APEX_*`, `APEX_I_*`) instead of showing them as `Unknown`.
- Keep exact download matching working for hyphenated APEX profile filenames.

## [1.0.5] - 2026-06-05

### Removed

- Removed the obsolete multi-select download toolbar from analyzed model results.

## [1.0.4] - 2026-06-05

### Fixed

- Recognize `MXFP4_MOE` GGUF files instead of showing them as `Unknown`.
- Sort GGUF quantization choices from least-compressed/largest to most-compressed/smallest.

## [1.0.3] - 2026-06-05

### Fixed

- Release packaging now runs on pushed `v*` tags, so new binaries include the latest UI fixes.

## [1.0.2] - 2026-06-05

### Added

- Default download layout setting: choose HF cache layout or local `owner/model` folders for LM Studio-style storage.
- Extra local model folders in Settings for scan-only cache discovery.
- Local cache rows now show downloaded GGUF quantization types.
- Sticky `Download selected` action above selectable quantization lists.

### Fixed

- Removed the duplicate read-only HuggingFace cache directory field from Settings.
- Fixed Storage card overflow when Local files layout is selected.
- Preserved `UD_` quantization labels such as `UD_Q3_K_XL`.
- Improved model search clear button visibility and default model-list column width.

## [1.0.1] - 2026-06-04

### Fixed

- **Data race in WebSocket hub** (`internal/server/websocket.go`): `WSHub.Run()` was calling
  `delete(h.clients, client)` while holding an `RLock`. Since `ClientCount()` also acquires
  `RLock`, both could run concurrently, resulting in a concurrent map write + read (data race).
  Fixed by upgrading the broadcast case to a full write lock (`Lock`/`Unlock`).

- **OOM when mirroring large files** (`internal/server/mirror.go`): `copyFileForMirror` used
  `os.ReadFile` to read the entire source file into memory before writing it. For multi-GB model
  files this would exhaust RAM and crash the process. Replaced with streaming `io.Copy`.

- **Search ignores proxy config** (`internal/server/search.go`): `handleSearch` was creating a
  plain `&http.Client{}`, bypassing any proxy configured via `--proxy` or the settings API.
  Fixed by using `hfdownloader.BuildHTTPClient(s.config.Proxy)` so search requests honor the
  same proxy settings as downloads.
