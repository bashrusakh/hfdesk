# Changelog

## [Unreleased]

### Added

- Add a read-only Community feature for repository Discussions and Pull Requests: `GET /api/community/{owner}/{repo}` lists both types with paging and type/status filters, while `?num=N` returns a thread with sanitized comments and human-readable event summaries. The Model Details view now has Overview and Community tabs; the read-only Community tab is lazy-loaded, lists Discussions and Pull Requests, opens a thread with comments and events, and supports model and dataset repositories. The proxy uses the configured endpoint, proxy, and HF token without exposing the token to the browser (#136).
- Report gated Hugging Face repositories as a dedicated access state instead of a partial analysis: the API returns HTTP 403 with error code `gated_repo`, the repository type, an accept link for the configured endpoint, and boolean-only token state, and the Model Details view shows configured-token and repository-agreement guidance without exposing the token.
- Model Details now opens README, lineage, and related-download repository links in-place, with Back/Forward history across models, datasets, and revisions (#135).
- Opt-in local-I/O benchmark for multipart finalization (`go test -bench BenchmarkRangedFinalization ./pkg/hfdownloader/`) recording total staging bytes written (including the transfer landing on disk), extra local payload writes beyond one file size, full-file hash bytes read, and the network-complete → file-done elapsed time: `Verify=size` shows ≈0×S extra local payload writes and no hash reread, `Verify=sha256` ≈1×S hash read (#140).

### Changed

- Multipart downloads write ranged workers directly into one preallocated staging file at their assigned offsets instead of downloading `.part-NN` payload files and stitching them with a full-file assembly copy (#140). In HF cache mode an LFS blob stages as `blobs/<sha256>.incomplete` — the future blob itself — so finalization is an atomic rename plus snapshot/friendly link work with no payload-sized copy; other destinations use `<destination>.incomplete`. Resume state persists as versioned per-range committed-prefix metadata next to the staging file and is authoritative for an existing incomplete download, but only when it is provable: the record must match the resolved commit, repo, path and size, its blob SHA must be present on both sides or neither, its ranges must tile the file with normalized completion (`Done` exactly when the range is fully committed), and the staging payload must exist at the expected logical size before any create/truncate. An interrupted transfer resumes from its durable high-water marks, and changing the connection count between runs re-fetches only the missing bytes instead of invalidating valid ones. The claim record is heartbeated through transfers, retry/`Retry-After` waits, strict hash verification and finalization; unreadable or ambiguous legacy records are preserved rather than treated as stale, and identity-mismatched current records are discarded only under lifetime ownership. Ownership uses a random per-process instance token plus heartbeat freshness and a real same-host liveness check (with a Windows `OpenProcess` implementation), so PID reuse and containers sharing PID 1 cannot be mistaken for self-ownership; the record remains the resume/identity state. Exclusion is fenced by an OS-level exclusive lock on the persistent sibling `<staging>.lock` (`flock(LOCK_EX|LOCK_NB)` on Unix, an exclusive-share `CreateFile` handle on Windows), acquired before the claim transition and held for the whole ownership interval — claim read, create/resume, transfer, hash, publish/cleanup. A busy lock is a live owner, including a suspended process that stopped heartbeating but could still resume writing through an open handle; a crashed process releases the lock at death, so takeover/resume is immediate, and there is deliberately no takeover while a suspended owner is alive (terminate it to recover). Every staging-record transition is serialized locally by a per-path metadata gate while the lifetime OS lock excludes current-version foreign mutators, and re-reads claim identity so a heartbeat cannot resurrect a lost claim; a claimant that loses skips the file ("downloading by another process"). Publish re-reads the record under that lock and verifies the OS lock is still held before the rename; a missing or foreign record is refused with `errStagingClaimLost` and the staging file is left for its actual owner. Platforms/filesystems unable to provide the lifetime OS lock return an operation error before claim/payload mutation and preserve existing partials; this is runtime failure handling, not permanent support policy. The lock file is never unlinked and is excluded from cache statistics and cleanup. Completion is content-addressed: an existing publish target proves completion only when it is `blobs/<known-sha>`; an arbitrary legacy/output destination or `tmp-<sha>` staging name follows the pre-download skip decision and is downloaded again and atomically renamed over when that decision declined it (so "blob exists" is reported only for a published blob-keyed target). Progress is reported from in-memory byte counters instead of re-statting part files on every tick. Every ranged response is strictly validated (exact `Content-Range` match, advertised total, exact body byte count, bounded writes) before any byte is written. Cleanup never removes a staging pair while another live claim remains, and re-checks that under the same per-path metadata gate. Valid legacy `.part-NN`/`.parts.json` partials are migrated into the staging file once and the obsolete artifacts are removed only after the migrated layout is durable, and a leftover single `<destination>.part` is migrated as a sequential prefix when no usable multipart layout exists; an oversized single `.part` is discarded rather than clamped, and migrated bytes are content-proven before publication as described for `Verify`.
- `Settings.Verify` now actually controls LFS verification (deliberate semantics change, #140): `none` performs no post-download content hash; `size` (the default) checks the exact expected size and complete validated transfer coverage on both the ranged and single-request paths and performs no full-file SHA-256 reread on the normal transfer path — the pre-download skip check over an already-present destination, the one-time legacy-migration content proof, and the inherited single-path `.part` proof may still hash; `sha256` verifies the full content against the known digest with at most one full-file read, always before publication when a known digest exists — only an eligible fresh sequential LFS ranged transfer with one task covering the whole file hashes while writing, every other transfer (including the single-request path) performs one sequential pass before the rename, a mismatch never publishes corrupt bytes, and the untrustworthy staging data is discarded so a retry re-downloads from scratch; `etag` proves a known SHA-256 digest with that same single pre-publication pass (so a mismatch never reaches `blobs/<sha>` or the destination) while non-LFS ETag-only files keep the historical no-post-check behavior; when `sha256` has no known digest, the HEAD-ETag fallback can report a mismatch only after publication. Bytes migrated from a pre-#140 partial are tagged `LegacyMigrated` and content-proven with one full-file SHA-256 pass before publication whenever the item has a known digest and `verify` is not `none` (the explicit one-time migration exception, even in `size` mode); with `none` they inherit that mode's no-hash semantics. The single-request path is under the same trust model: a `.part` is a sequential prefix with no record, so every 206 must match the exact requested `Content-Range`, expected total and body length, writes are bounded to the requested range, the on-disk size must be exact before the rename, and a known digest is proven with one full-file pass before the rename in `sha256`/`etag` modes (inherited `.part` bytes are also proven once in `size` mode, and a known digest with an unknown expected size is proven because only the digest can validate the stream); a partial with no known expected size is never resumed, and a mismatch never publishes. A multipart staging record also carries a durable `ProofPending` gate: it is set and flushed before any known-digest proof starts and cleared and flushed only after the proof succeeds, and on resume it forces the proof in every mode except `none` (including `size`) when a known digest exists, so a crash mid-proof, a failed mismatch cleanup, or a later mode change cannot publish unproven all-Done ranges; a mismatch keeps the flag set, attempts poison (truncate to zero) only while the claim is still owned under the per-path metadata gate, and attempts the owned discard; if ownership cannot be verified the durable flag remains the guard, and publication is refused while a known digest's proof is pending. `none` is the explicit no-hash opt-out. The `hfdownloader` package docs, the `Settings.Verify` comment, and the settings UI label describe the new semantics.
- `hfdownloader.PlanRepo` and `hfdownloader.Download` now reject a path-shaped filter combined with `AppendFilterSubdir` (typed `ErrAppendFilterSubdirPathFilter`) before any network or plan work: only single-component group/token filters can name a destination subdirectory. Path filters remain valid with `AppendFilterSubdir` disabled.
- README rendering and Hugging Face repository-link classification now have a single Go implementation: the bundled UI renders the server-sanitized `html` from `GET /api/readme` and no longer carries a client Markdown renderer or duplicate URL classifier (#135, #147).
- Remote repository tree paths must now use their canonical spelling: a raw path whose `path.Clean` form differs (for example a `./` segment, duplicate slash, or trailing slash) fails planning with a non-canonical-path error instead of being normalized, so two spellings of one remote file cannot both be planned and race to write a single cache entry.

### Fixed

- The cache status check no longer reports an unknown ownership state as "downloading by another process" (`#140`). `CheckBlob` distinguishes three outcomes: a proven live owner is `BlobDownloading` (the item may be skipped), a provably dead owner is `BlobStale` (the item may be resumed), and an unknown ownership state — the OS-level ownership probe failed (for example `EACCES` on the lock file) or the platform cannot represent the lock — is the new `BlobOwnershipUnknown` status together with an error wrapping the new `ErrBlobOwnershipUnknown` sentinel. The downloader surfaces that error instead of skipping the item: the previous behavior reported "download complete (downloaded 0, skipped 1)" while the blob was still absent and the partial was untouched. The incomplete payload is preserved, and the existing status values keep their meanings.
- The publication of a content-addressed blob no longer overclaims what it guarantees, and the exported `FinalizeBlob` no longer replaces a published blob (`#140`). Three corrections: the hard-link-unsupported branch of the publication used to be a stat-then-rename — and `os.Rename` REPLACES an existing target on Unix and on Windows — so a target that appeared between the stat and the rename was replaced, while the documentation claimed the fallback "still refuses to replace an existing target"; the fallback is now a domain-exclusive placement that runs only while the ownership domain of the blob key it publishes is held, and it refuses to run at all without that domain, so an existing `blobs/<sha>` is never replaced by a domain participant and the weaker guarantee is never reached by accident (hard-link support stays no prerequisite for downloading). `FinalizeBlob`, an exported writer of `blobs/<sha>`, renamed over the target with no domain and no no-replace primitive, so it replaced a blob another writer had already published; it now participates in the blob key's ownership domain and publishes through the same no-replace primitive, and an existing blob is kept while the call reports the conflict. The single-request route also failed late when the lifetime ownership of the blob key was unavailable: it transferred the file, wrote the payload and only failed at the store step, which also removed the partial; the domain is now acquired before the transfer, so the operation fails before it writes a payload and an earlier run's partial is preserved. The Go standard library exposes no atomic no-replace rename on any supported platform — `os.Rename` replaces on Unix and on Windows, and `syscall` exports neither `renameat2(RENAME_NOREPLACE)` nor `renamex_np(RENAME_EXCL)` — so the atomic no-replace tier is the hard link, and the fallback tier is domain-exclusive; the mirror writer and the selected-GGUF delete path remain the documented writers that take no domain.
- A content-addressed blob key now has ONE publication domain shared by every route that writes it, instead of one private domain per staging path (`#140`). The ranged route (`blobs/<sha>.incomplete` → `blobs/<sha>`) and the single-request route (`blobs/tmp-<sha>.part` → the cache store → `blobs/<sha>`) used to be able to write the same key at the same time: both could decide that the blob did not exist yet and then place their own bytes, so the second placement replaced the first writer's blob with bytes that do not hash to the key they are stored under — a corruption `CheckBlob` then reported as complete — and the ranged route could adopt the single route's LIVE partial as a pre-#140 leftover, publish a mixture of two writers' byte streams under the content-addressed name and delete the partial the other writer was still filling. Both routes now hold the same domain (an in-process slot keyed by the blob key plus the lifetime OS lock on the persistent sibling `blobs/tmp-<sha>.lock`, which is never unlinked and is excluded from cache statistics, listings, mirrors and cache deletion), so a live partial of one route is never adopted, migrated or deleted by the other, and the publication decision and the placement that follows it cannot be overtaken. The publication itself has two tiers, chosen by the FILESYSTEM. On a filesystem that can create hard links it is a single atomic no-replace step (a hard link, which fails when the name exists), so an existing `blobs/<sha>` is never replaced by another writer's bytes, proven or not, and the normal path stays rename-only in cost. The Go standard library exposes no atomic no-replace rename on any supported platform — `os.Rename` replaces an existing target on Unix and on Windows, and the `syscall` package exports neither `renameat2(RENAME_NOREPLACE)` nor `renamex_np(RENAME_EXCL)` — so on a filesystem WITHOUT hard links the placement is a check-then-rename performed while the blob key's ownership domain is held exclusively: there an existing `blobs/<sha>` is never replaced by a DOMAIN PARTICIPANT, and a writer outside the domain is not fenced by it. That placement refuses to run without the ownership domain of the blob key it publishes, so the weaker guarantee is never reached by accident, and hard-link support remains no prerequisite for downloading. Known limitation, documented rather than fixed: the mirror writer copies destination blobs with a non-atomic truncate-and-write and a size-only check, and the selected-GGUF delete path removes blob payloads; neither takes the blob key's ownership domain, so neither is fenced by it, and the downloader's publication invariant does not depend on them.
- Correction to the #140 ownership follow-up: shared staging mutations require the supported lifetime OS lock; unsupported, denied, or failed acquisition now returns an operation error before claim/payload mutation and preserves partial data. There is no lease-only mutation fallback. Legacy/unfenced records are retained unless same-host owner death is proven. In-process record transitions use a per-path metadata gate; transition age never authorizes breaking or unlinking a holder's critical section. Cancellation while waiting to publish is observed before rename.
- Ranged body completion now requires clean framing/EOF in addition to receiving the requested bytes. If a transport/stall error occurs after the final byte was written, the final byte remains uncommitted and is re-requested as a nonempty range; it is never marked Done or published from that failed response. Range-record version 3 marks Done only after clean body framing; version-2 Done checkpoints migrate by preserving their prefix and revalidating the final byte. Fresh claim JSON is atomically installed without replacing an appeared record, and a failed takeover replacement preserves the validated predecessor until a complete replacement is ready. The claim record no longer requires hard-link support: the complete record is staged in a fresh file and installed by an atomic hard link, falling back to an atomic rename on a filesystem that cannot create hard links, so a ranged download still succeeds there. The staged copy is always a fresh inode, so a staged path left behind by an interrupted install — which may share the authoritative record's inode — is never written through and can no longer empty the authoritative record before it is replaced.
- Single-path finalization now checks cancellation after `file_finalizing` and again immediately before rename, applying existing pause-preserve versus explicit-cancel cleanup. Unknown-size LFS items that fall back from multipart to single-path are stored through the cache store owner, creating the blob and its snapshot/friendly entries instead of leaving a dangling direct-publish assumption.
- Ranged-download staging now follows one explicit trust model (#140): an existing arbitrary destination is never completion proof (only a content-addressed `blobs/<known-sha>` is); legacy `.part-NN`/`.parts.json` or single-`.part` bytes are content-proven with one SHA-256 pass before publication when a digest is known and `verify` is not `none`, and an oversized single `.part` is discarded instead of clamped; every staging-record transition is serialized locally by a per-path metadata gate under the lifetime OS lock so concurrent take-overs cannot produce two writers or delete the winner's claim; and every post-claim phase, including the legacy migration copy and the strict hash, is cancellation-aware with the same claim-checked cleanup semantics. A moved commit is caught by the content proof (identity alone cannot prove the old bytes match). The single-request path now follows the same model (strict 206 validation, bounded writes, exact on-disk size, and a pre-publication one-pass proof of a known digest for fresh and inherited bytes in `sha256`/`etag`, with inherited bytes also proven in `size`), publish refuses a missing/foreign claim, and a whole-interval OS ownership lock fences in-flight writes that the record lease could not; a multipart layout with no usable part falls back to a valid single `.part` instead of deleting it, and an oversized ranged body no longer poisons the strict hash-on-write state (the retry falls back to exactly one final full-file pass) or double-counts progress. A known-digest mismatch whose owned cleanup fails logs that failure and leaves a durable `ProofPending` record, so a later `size` run cannot publish the corrupt staging with zero ranged GETs; poison (a zero-length payload) is attempted only while the claim is still owned under the per-path metadata gate, so a stale writer cannot truncate another owner's staging, and the pending flag is cleared durably only after a successful proof, immediately before publication.
- Publish no longer overwrites a content-addressed blob that another writer completed while a transfer was in flight: the publication places the payload with a no-replace primitive, so an existing `blobs/<known-sha>` is completion proof and keeps its bytes — the losing writer discards its own staging pair and reports the item as complete (`#140`). The "the blob does not exist yet" decision and the placement that follows it are one step — atomic no-replace on a filesystem with hard links, domain-exclusive on one without, see the publication-domain entry above — so a concurrent publisher can no longer be overtaken between them. A record written while holding the OS ownership lock now persists that mode (`LockFenced`), so `CheckBlob` treats a free ownership lock as authoritative and reports a resumable crash state as stale instead of "downloading" when the recorded PID happens to be reused/live; unfenced legacy records remain protected unless same-host owner death is proven. Mismatch poison is performed only while the claim is still owned under the per-path metadata gate, so an unfenced stale writer cannot truncate another owner's staging. The single path retries a nonempty final-byte range when response framing fails after all requested bytes arrive; it does not publish until framing is clean. It emits `file_finalizing` before the pre-publish proof (and a full-progress reading for an exact-size inherited partial), removes obsolete legacy multipart artifacts on success, and proves a known digest with an unknown expected size. The ownership probe takes a non-owning shared lock, so concurrent probes are never mistaken for a writer. Skipped same-SHA/different-path items are reconciled after all files finish and get their own snapshot/friendly entries. Staging `.meta.tmp` files and the persistent locks are excluded from cache listings/stats/selection and mirrored content, the single-path size check stats the actual verify target, and mirrors skip all staging artifacts.
- A legacy/unfenced staging record (`LockFenced` absent/false) is preserved unless same-host process evidence proves its owner dead; age or ambiguous identity does not prove that an older writer cannot resume. Existing multipart resume state (a valid v2 pair, the blob-keyed LFS pair, or a usable pre-#140 `.part-NN`/`.parts.json` layout) now keeps the multipart route for a ranged-capable item regardless of `MultipartThreshold`, so a threshold change no longer ignores and deletes valid state; without range support the downloader emits an explicit warning, downloads the full file through the single path, and removes the obsolete v2 pair only after the final file is durable (claim-checked), while the legacy family is removed by the single-path success cleanup. An ignored-Range `200` restart now clears the inherited-bytes flag, so `Verify=size` does not perform an unnecessary full SHA proof on the restarted transfer.
- Cache selection now exposes a downloaded mmproj companion as its own deletable group instead of dropping it from the selectable model: `GET /api/cache-selection` reports a group `kind` of `llm` or `vision_encoder`, a companion group carries an `mmproj-` group id and no model-quantization label, and the Model Details view renders a distinct "Vision encoder" badge with its own Delete action. The delete confirmation names the vision-encoder role in both the local and the HF case and never claims the selected artifact class remains. Deleting a companion removes only that companion, deleting an LLM group never removes a companion, coexisting F16/BF16/F32 companions stay independently selectable and deletable, and the existing writer, snapshot, and identity-pinning protections still apply (#138).
- The artifact-role rule for multimodal projector companions now has a single owner in `pkg/smartdl` (`IsMMProjFile`, the previously unexported helper), which the download picker, the cache metadata, and the cache-selection model all use. The rule is a case-insensitive "mmproj" match anywhere in the file's basename, so a dot-separated companion (`model.mmproj-f16.gguf`) and a name that merely starts with "mmproj" (`mmprojector-f16.gguf`) are both companions. `GET /api/cache/{owner}/{repo}` therefore reports a dot-separated companion under `hasMMProj`/`mmprojFiles` instead of listing it as an `F16` quantization, and a repository whose only companion is dot-separated now also reports the `vision` capability; the cache selection reports a companion as a `vision_encoder` group instead of a model quantization (#138).
- `GET /api/analyze` now supports an explicit `type=model|dataset` selector, so a repository that exists as both namespaces resolves without repeating the type-selection prompt (#135). `type` takes precedence over the legacy `dataset` parameter, and `type=model` combined with `dataset=true` is rejected as a conflict. The legacy `dataset` parameter keeps its pre-#135 behavior: only `dataset=true` forces the dataset namespace, while `dataset=false` still auto-detects.
- The Model Details `HF Hub ↗` action now uses the endpoint reported by `GET /api/health` when settings have not loaded or the settings fetch failed, and stays hidden rather than guessing `huggingface.co`; health now reports the normalized effective `endpoint` (#135).
- Filtered plan/download scans no longer succeed when no non-excluded path directly matches and no recognized dataset membership selects data: `POST /api/plan` now returns `400` with error code `no_filter_match` instead of a metadata-only plan, while empty repositories without filters still plan successfully.

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
