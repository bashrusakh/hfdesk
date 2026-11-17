# Changelog

## [Unreleased]

## [1.3.0-beta.6] - 2026-11-18

Beta build of the upcoming `v1.3.0`; the artifacts are pre-release builds for testing and are not marked as the latest stable release.

Long-form notes: [docs/releases/1.3.0-beta.6.md](docs/releases/1.3.0-beta.6.md).

### Fixed

- A download reports `writing` only while file creation/preallocation or a payload write is in flight, and its speed/ETA come from network reads; a completed write or a pure network stall does not hold that phase. (#209)

### Changed

- The selected GGUF group deletion confirmation names the quantization in its title and shows left-aligned facts, a one-sentence scope, and file rows with right-aligned sizes; the whole-copy confirmation is unchanged. (#207)
- Repository Details uses a two-line identity and **This copy** facts header, opens the repository from its title, and moves location choice into the contents heading instead of the duplicated **This copy** panel, paths, and **Repository metadata** block. (#213)

## [1.3.0-beta.5] - 2026-11-17

Beta release of the upcoming `v1.3.0`; the artifacts are pre-release builds for testing and are not marked as the latest stable release.

Long-form notes: [docs/releases/1.3.0-beta.5.md](docs/releases/1.3.0-beta.5.md).

### Changed

- Repository Details is a sectioned card: identity and provenance, a **This copy** panel, compact GGUF rows with facts, and a dedicated delete block replace the old stats row, per-row inventory disclosures, and footer delete.
- Repository Details can be resized from its bottom-right grip (the size is remembered per browser), while the delete confirmations keep their default dialog layout.
- CHANGELOG.md is now a one-line-per-change summary; long-form notes live in docs/releases/<version>.md (#203).

## [1.3.0-beta.4] - 2026-11-17

Beta release of the upcoming `v1.3.0`; the artifacts are pre-release builds for testing and are not marked as the latest stable release.

Long-form notes: [docs/releases/1.3.0-beta.4.md](docs/releases/1.3.0-beta.4.md).

### Added

- GGUF recognition adds PrismML `PTQ1_0`/`PQ2_0` ternary quantizations that require the PrismML fork and are never auto-recommended.

### Fixed

- Large ranged downloads now report a preparing phase instead of a silent 0 B/s window.
- Whole-folder deletion treats every recognized mmproj companion spelling as part of its model.
- A GGUF vision-encoder row is recommended only together with a recommended quantization.
- Model Details keeps Overview as the default tab for every recognized repository.
- Community links to a repository root now open it in Model Details instead of the browser.

## [1.3.0-beta.3] - 2026-11-17

Beta release of the upcoming `v1.3.0`; the artifacts are pre-release builds for testing and are not marked as the latest stable release.

Long-form notes: [docs/releases/1.3.0-beta.3.md](docs/releases/1.3.0-beta.3.md).

### Added

- Whole-model **Delete** now covers `Local` and `Friendly view` models, including their complete cache presence (#63).
- Analysis adds Weight Format rows for ONNX/TFLite/Flax/OpenVINO, more quantization methods, and a recommended-quant RAM budget.
- **Dataset Configs** presets download a whole config, and a manual filter that matches nothing now reports a notice.
- Plan and download accept batch shard ranges and exact-file selection, and Model Details adds a **Files** picker for individual paths.
- EXL2/EXL3 bitrate branches are analyzed, recommended and downloadable as whole-branch variants (#100).
- Service mode registers HFDesk with systemd, launchd or the Windows SCM and runs headless with its own runtime paths (#142).
- The Windows desktop build adds a tray app, Runtime/Logs window, structured logging options and local runtime/log endpoints (#141).
- The Windows desktop can install, update and start the service, with safer recovery defaults for new and existing registrations.
- Community browses Discussions and Pull Requests read-only, and repository links open in place with back/forward history (#135, #136).
- Gated repositories report a dedicated access state with token and agreement guidance instead of a partial analysis.

### Changed

- Repository validation now runs before route-key checks in plan requests, matching download requests.
- Filter-subdirectory downloads now apply over HTTP, while plan paths stay repository-relative and path filters are rejected.
- Diffusers component rows download exactly their recommended weight set, without conversions or extra variants.
- Dataset split membership has one precedence rule, with ambiguous overlapping shapes still refused.
- `hfdesk service status` reports persistent and runtime masks distinctly, both with exit code 0.
- Downloads hold one destination ownership scope and pin transfers to the resolved commit (#140).
- Staged multipart data is reused only under a proven identity, and unpinned planning is limited to proven branch-only endpoints (#140).
- Verification settings also govern LFS hashing, with size-only checks by default and no normal-path reread (#140).
- Mirror, rebuild and cache mutations now share the destination ownership admission used by downloads (#151).
- EXL branch discovery no longer depends on main-tree guesses, and nested quantization configs are recognized (#100).
- Metadata fetches are time-bounded per request, and duplicate-download detection uses one shared identity comparison (#151).
- README rendering and Hub link classification now have a single implementation (#135, #147).
- Remote tree paths must use canonical spelling instead of being normalized.

### Fixed

- Malformed repositories and colon/filter shorthand are rejected consistently, and empty filtered scans fail with a no-match error.
- Cache-mode concurrency no longer collapses behind bootstrap locks, and large deletions avoid file-descriptor exhaustion (#151).
- Flat-mode ownership keys are realm-relative, so shared mounts survive common-prefix moves (#151).
- A download no longer reports success while a busy ownership scope leaves a requested file missing (#140).
- Publication verifies range layout and payload length, while unproven staging records are preserved and cleanup stays under the destination scope (#140).
- Cancelling during a digest proof releases the partial handle, and shared staging requires the OS lock.
- A macOS update no longer unloads an already scheduled launchd start, and reinstalls keep `--flag=value` settings.
- Windows installs verify the registered image or reject unreadable registrations, accept execute-only programs, and keep cache paths, settings and autostart.
- Linux updates respect administrator drop-ins, runtime-only enablement and masked units, and wrapper prefixes no longer confuse flag parsing.
- Updates restart a running instance on the new program, refuse disabled, removed or partial registrations, and verify readiness from the started instance.
- Desktop and service coordination uses one liveness model, publishes URLs safely and checks first-install readiness.
- Service handover waits for jobs-state saves and settings writes under one bounded deadline.
- Proxy failures no longer leak credentials into logs or API errors (#141).
- mmproj companions are deletable groups and share one artifact-role rule across picker, cache metadata and selection (#138).
- Analysis accepts an explicit model/dataset selector, and the Hub link uses the effective endpoint (#135).
- Download warnings are retained in job state, with repo-wide resume warnings reported once.
- Local Cache dates ignore staging artifacts and interrupted-publication residue (#175).
- Excludes and path filters fold separators and case identically on every platform.

### Removed

- The unused invalid-repository sentinel, obsolete stale-timeout settings and staging-record heartbeat are gone (#140).
- Service installs now merge with an existing registration, keeping settings and running state the command does not name.

## [1.3.0-beta.1] - 2026-10-15

Beta release; see Beta limitations and compatibility below.

### Added

- Select a cache location and GGUF group for deletion in the cache UI; see partial-failure details when deletion is incomplete (#111, #125).
- Route downloads into optional per-type folders (LLM/GGUF, LLM/Safetensors, LLM, Audio, Diffusion, Embedding) via opt-in type routes (#62)
- `HF_TOKEN` environment variable support: the token is now resolved as `--token` flag, then `HF_TOKEN` env, then the config file token (#72)
- Docker: `PUID`/`PGID`/`UMASK` environment variables let NAS/homelab users run the container as their own UID/GID and control the file creation mask (#61)
- Docker: arbitrary-UID support — `docker run --user 568:568` and Kubernetes `securityContext.runAsUser`/`fsGroup` now work without the image user or an `/etc/passwd` entry (#61)
- Embed the HFDesk icon in Windows binaries and add Linux desktop integration (#54, #56)
- Add custom stepper controls for numeric settings (#58)

### Changed

- Correct misleading `HF_TOKEN` documentation in the library `Settings` docs to state that the hfdesk server reads the env var, while library callers pass the token explicitly (#72)
- Docker: by default, writable application state (settings, jobs, history, HF cache, local models) is consolidated under a single `/data` root, so one volume persists it; `HOME=/data`, `XDG_CONFIG_HOME=/data/.config`, `HF_HOME=/data/.cache/huggingface`, and `WORKDIR /data` (#61)
- Docker: the container starts as root only to apply PUID/PGID, then drops privileges; the app process runs non-root (default UID/GID 1000) unless `PUID=0`/`PGID=0` is set explicitly (#61)
- Docker: runtime UID/GID selection is numeric only and never modifies `/etc/passwd` or `/etc/group`, including account collisions; non-root startup supports a read-only root filesystem with all capabilities dropped and no-new-privileges when `/data` is writable (#71)
- Docker: the image no longer creates a named user/group or accepts `UID`/`GID` build arguments; the runtime identity is configured numerically via `PUID`/`PGID` (root drop) or `--user` (non-root), and the entrypoint's default identity remains `1000:1000`
- Upgrade `golang.org/x/net` to v0.55.0 to close reachable govulncheck findings in `x/net/html`; the minimum build toolchain is now Go 1.25 (#101)
- Preserve existing Hugging Face error messages while using actual HTTP status for namespace detection: HTTP 401/404 responses can trigger model/dataset fallback, HTTP 403 remains terminal, and transport failures retain their diagnostics even when their message contains status-like words (#125)

### Fixed

- Keep GGUF quant selections matched-only when README filenames contain the selected quant token, without treating README metadata as a second payload format; dataset split filters now use inferred file membership and fail clearly when relevant membership is ambiguous (#124).
- Read quantization metadata from Git-LFS-backed config files instead of silently losing it to pointer stubs, show quantization information for quantization-only repositories, and distinguish proven uniform precision, observed width variation, and unknown/incomplete precision in the quantized panel; also show `Not quantized: …` for excluded modules (#123).
- Analyze and plan/download previews honor the configured proxy and report proxy construction failures rather than falling back to direct planning requests (#104).
- Read analyzer metadata completely within the 10 MiB cap, recover completed leading quantization fields from oversized configs, and report unrecoverable overflow/read failures; root Transformers weights take precedence over ONNX exports while specialized model detection is preserved (#105).
- Recover stalled downloads and bound transport waits with deadlines; retry `429` and all `5xx`, fail non-retryable file responses without retrying, and keep optional verification metadata failures non-fatal (#97)
- Keep exact `HF_HUB_CACHE` storage consistent across downloads, cache operations, mirror, rebuild scripts, cleanup, and disk previews; freeze complete job destinations across restart and expose raw app-root/effective Hub metadata without persisting inherited defaults (#76)
- Preserve active and stored Hugging Face credentials during ordinary settings updates; persist token changes only for explicit set/clear requests, including concurrent updates and retries after persistence warnings (#74).
- Treat redacted token display values as preserve-only and avoid exposing complete short tokens in settings responses (#74).
- Save config files with owner-only Unix permissions and preserve the previous file when persistence fails (#74).
- Docker: the README example mounted the host cache to `/root/.cache/huggingface` while the image used `/home/hfdesk/.cache/huggingface`, so the Hugging Face cache was never persisted. Existing users must update their volume mount to `/data` (#61)
- Contain download destination and cache paths at their join boundaries, require canonical SHA-256 for remote hash values, honor `Retry-After` on size HEAD requests, and report stalled transfers as an explicit stall error instead of `context canceled` (#117)
- List repository trees recursively with `Link` pagination and bounded, context-cancellable retries, so repositories with more than 1,000 entries per directory are analyzed and planned completely instead of silently truncated; exceeding the request budget fails loudly rather than returning a truncated result, mirrors that ignore `recursive` or reject unknown query params keep working, and synchronous results have time to reach the client within the existing handler budget (#96)
- Place HF cache snapshot and friendly-view entries with a symlink to hardlink to copy fallback instead of skipping them where symlinks are unavailable (e.g. Windows without Developer Mode), so downloads always leave usable `snapshots/<commit>` entries and a friendly view; rebuild/`Sync` now recreates missing snapshot entries from the offline download manifest, one clear notice per root names the fallback in use, and selected-GGUF deletion handles hardlink/copy entries like their symlink counterparts (#99)
- Selected-GGUF deletion is durable across rebuild when tombstones are successfully recorded in the repository download manifest (`deleted`); if recording fails, the error is reported while deletion proceeds, so rebuild/repair may recreate those names. Recorded tombstones prevent recreation even when a retained payload (shared entry, retained alias, or failed blob unlink) keeps their content available; refused deletions record nothing (#99)
- Restore quant status after cache deletion (#59)
- Keep job-state persistence ordered and atomic (#80)
- Inject build version into Docker images and use the release tag as the artifact version source (#64, #69)

### Beta limitations and compatibility

- Docker now keeps writable application state under `/data` by default. Existing deployments should move their persisted volume to `/data` to retain settings, jobs/history, HF cache, and local models. Existing files in a mounted directory may retain their prior ownership; changing `PUID`/`PGID` does not recursively chown existing data (#61).
- Building from source requires Go 1.25 or newer (#101).
- Selected local deletion is specifically GGUF-group deletion, not general local-model or all-format deletion (issue #63 remains incomplete). The documented selected-delete platforms are Linux, Windows, and macOS; native Windows/macOS verification was unavailable. On Darwin, a process crash can leave a temporary private pin directory, and HFDesk does not automatically clean such orphans. Arbitrary external filesystem changes are not excluded atomically between final identity check and unlink.

## [1.2.2] - 2026-06-29

### Fixed

- Tighten README sanitizer allowlist to only allow safe HTML tags and attributes, fixing CodeQL alerts and preventing XSS via malformed README markdown (#52)
- Resolve README image assets through LFS URLs for gated/private repositories, fixing broken images in model cards for gated repos (#51)

### Changed

- Refine HFDesk icon and add Apple touch icons for better home screen/PWA experience (#53)

## [1.2.1] - 2026-06-27

### Fixed

- Delete stale `.part` files when a paused job is cancelled or dismissed, preventing disk leak from the pause → cancel and pause → dismiss sequences (#49)
- Scope partial-file cleanup to per-job in-flight files so a pause → cancel / pause → dismiss never touches a concurrent job's partials in the same repo (e.g. model + mmproj downloaded in parallel)

## [1.2.0] - 2026-06-26

### Added

- Download deduplication is now case-insensitive: files differing only by case are merged in the plan to prevent duplicate jobs (#46)
- Speed bar with live usage gauge on the Downloads tab: shows current transfer throughput and cap utilization (#43)
- Active Jobs page redesigned — Clean Monitor layout with Compact Strip for finished, paused, and cancelled jobs
- Speed presets: 5, 15, 25, 50 MB/s, and unlimited

### Fixed

- Preserve `DownloadedBytes` on job resume so progress does not drop back to zero after a pause, retry, or server restart (#42)
- Align speed bar with the combined redesign layout; fix pause freeze that stopped live speed updates
- Constrain speed bar max-width to prevent overflow on narrow viewports
- Match speed bar width to downloads section via shared `.jobs-page` container; fix overlap with app footer, white input background, and hidden spinner elements
- Speed bar now uses variable width matching the downloads section, removing the previous fixed max-width
- Delete stale `.part` files on explicit download cancel (#47)

### Security

- Fix CodeQL P0-P5 alerts across the codebase; path-safety hardening in `pkg/hfdownloader` (#45)

## [1.1.0] - 2026-06-26

### Added

- Download speed limit feature with configurable cap
- Inline speed cap display on Downloads tab

### Fixed

- Pre-existing concurrency bugs in Server.config and RateLimiter
- MTP (Multi-Token Prediction) variants are now treated as separate GGUF quantizations

### Changed

- Upgraded GitHub Actions to Node.js 24 compatible versions
- Updated workflow actions (checkout v7, upload-artifact v7)
- Added .graymatter to .gitignore

## [1.0.14] - 2026-06-18

### Fixed

- MTP (Multi-Token Prediction) companion GGUF files without a recognized quantization token are now filtered out of quantization lists. Files starting with `mtp-` (e.g. `mtp-gemma-4-26B-A4B-it.gguf`) are excluded unless they carry a proper quant label (e.g. `-BF16-MTP.gguf`), which still appear as selectable options.

## [1.0.13] - 2026-06-15

### Added

- Upstream mmproj discovery: when a GGUF repo has no local multimodal projector files, hfdesk now traverses the HF model card `base_model` chain (up to two hops) and checks the `{owner}/{name}-GGUF` sibling repo at each level. If found, the mmproj files are listed in the Vision Encoder section with a note showing the source repo (e.g. `Multimodal projector · upstream: unsloth/Qwen3.5-9B-GGUF`). The download button targets the upstream repo automatically.
- Model lineage tree in the GGUF Information panel. When a repo declares a `base_model` in its HF card, the provenance chain is shown as an indented tree (base model → finetune → quantized → this), matching the style of the HF model card. Built from the HF `/api/models` metadata; only shown when at least one ancestor is known.

### Changed

- Vision Encoder items now appear under a "Vision Encoder" section heading in the quantization list (previously fell back to the raw `vision_encoder` string).

### Fixed

- `isMMProjFile` now detects the mradermacher naming convention (`model.mmproj-f16.gguf`, `model.mmproj-Q8_0.gguf`) where the `mmproj` token is dot-separated rather than a leading prefix. Previously these files were misclassified as LLM quantizations (e.g. `F16`, `Q8_0`) and polluted the quant picker.
- Upstream mmproj downloads are now stored under the current model's folder instead of the upstream repo's own folder, so the on-disk cache layout matches the model selected in the UI. Previously a click on an upstream multimodal projector entry (one discovered on a base model's `-GGUF` sibling) saved the file under the upstream repo id and split the local layout between two owners.

## [1.0.12] - 2026-06-14

### Changed

- Pipeline tag text is no longer shown as a badge on model list rows; the full pipeline is still visible in the model detail panel. GGUF and local-cache badges are unchanged. This removes a source of visual noise and inconsistent truncation (e.g. "feature extracti" cut off at 16 characters).
- README links in the model description now underline on hover, making them easier to discover as interactive without adding visual weight at rest.
- Improved contrast of model detail tag chips (e.g. `pytorch`, `safetensors`) by raising the text color from muted to secondary.

### Fixed

- Multipart downloads discard stale part files when the file size, concurrency, or chunk layout changes, preventing silent corruption from incompatible resume data.

## [1.0.11] - 2026-06-12

### Changed

- Model detail panel now uses a single scroll instead of two nested scrolls (quantization list + README preview). Section headers stick to the top while scrolling. Contextual floating arrows appear: down-arrow at the bottom when scrolled to top, up-arrow at the top when scrolled down, both arrows visible in the middle.
- README screenshots were refreshed to match the current UI, including the model detail view with quantizations and the README description visible together.

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
