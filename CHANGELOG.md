# Changelog

## [Unreleased]

### Added

- Analyze and download EXL2/EXL3 bitrate branch quantizations (for example `4.00bpw` or `4_25`) as whole-branch variants: Model Details lists each branch with its BPW/head bits and recommends one, `POST /api/download` accepts `branchVariant: true` with the branch `revision`, and the downloader stores each `branchVariant` download without writing the shared friendly view, using HF cache blobs plus per-commit snapshots (or, in local mode, an app-owned reserved space `<root>/.hfdesk-branch-variants/<owner>/<repo>/` that holds the branch payload, its completion records and the transient staging area of a transfer, outside the ordinary repository tree, so a repository's own files are never replaced by a branch download). A generic revision download of the same branch remains an ordinary download and keeps the existing friendly-view behavior. `GET /api/cache/{owner}/{repo}?branchVariant=true&revision=...` reports a variant as downloaded only when its completed manifest and every expected file are present (#100).

- Add a service mode: `hfdesk service install|uninstall|start|stop|restart|status` registers HFDesk with the native supervisor of the platform (systemd on Linux, launchd on macOS, the Windows Service Control Manager on Windows). The supervisor owns the lifecycle and HFDesk stays a headless foreground process, the instance resolves deterministic runtime paths of its scope (its own config file, state root and log directory), and it binds loopback by default; exposing it to the network needs an explicit `--listen`. Credentials stay in the service-owned config file and are never written into a supervisor registration (#142).

- A second Windows artifact, `hfdesk-desktop_windows_amd64_v1.3.0.exe`, is now built and published next to the console binary: it is a GUI program without a console window that runs the same HFDesk server in-process behind a notification-area icon and a small Runtime / Logs window showing the server status, the active job count and the live log records. A left click on the icon opens that window, the tray menu offers Open HFDesk, Runtime / Logs and Quit HFDesk (a graceful shutdown), and the host is single-instance per user and session: a second launch detects the running instance and opens the web UI of it. The Runtime / Logs window needs the Microsoft Edge WebView2 Evergreen runtime, and a missing runtime produces an actionable message while the server and the tray keep working. The desktop binary is a separate download and the console binary is unchanged (#141).
- A structured logging pipeline with the new `--log-file` and `--log-level` options: every record carries a level, a timestamp, the component that produced it and structured attributes, the console sink writes to standard error so redirecting standard output keeps working, and the file sink rotates. The desktop host always writes a log file, because a GUI program has no console, and it lands in `<user cache dir>/HFDesk/logs/hfdesk.log` unless `--log-file` names another path (#141).
- Local-only runtime and log endpoints: `GET /api/runtime` reports the version, the status, the URL, the active job counts, the run id of the server and the log cursor, `GET /api/runtime/logs` returns a bounded page of the retained records, `GET /api/runtime/logs/stream` streams them as Server-Sent Events, and `POST /api/runtime/open-logs` and `POST /api/runtime/open-ui` open the log folder and the web UI of the running instance. They take no path and no URL from the caller, and three rules keep them local: the peer address has to be a loopback address, an `Origin` header of another site is rejected, and the `Host` header has to name the local machine, so a name of another site that is pointed at `127.0.0.1` cannot read the runtime data of the local machine. A request that breaks a rule is rejected with `403` and never reaches the handler (#141).
- The requests of the runtime endpoints are written to the log at `debug` level and not at `info` level, so the polling of the Runtime / Logs window (every four seconds) does not fill the bounded log history that the window exists to show. Every other request keeps the `info` level, including a request that answers with an error (#141).

- Add a read-only Community feature for repository Discussions and Pull Requests: `GET /api/community/{owner}/{repo}` lists both types with paging and type/status filters, while `?num=N` returns a thread with sanitized comments and human-readable event summaries. The Model Details view now has Overview and Community tabs; the read-only Community tab is lazy-loaded, lists Discussions and Pull Requests, opens a thread with comments and events, and supports model and dataset repositories. The proxy uses the configured endpoint, proxy, and HF token without exposing the token to the browser (#136).
- Report gated Hugging Face repositories as a dedicated access state instead of a partial analysis: the API returns HTTP 403 with error code `gated_repo`, the repository type, an accept link for the configured endpoint, and boolean-only token state, and the Model Details view shows configured-token and repository-agreement guidance without exposing the token.
- Model Details now opens README, lineage, and related-download repository links in-place, with Back/Forward history across models, datasets, and revisions (#135).
- Opt-in synthetic HF-cache benchmark (`go test -run '^$' -bench BenchmarkRangedFinalization ./pkg/hfdownloader/`) uses public `Download` and records client-observed network-complete → `file_done` time plus payload reads/writes during that interval for known-LFS `Verify=size` and `Verify=sha256` with concurrency. `TestCacheDownloadMeasurementBoundary` (64 KiB) asserts this deterministically at the public `Download` boundary: `Verify=size` at 0×S post-network payload reads/writes and `Verify=sha256` at exactly 1×S reads/0×S writes. It checks snapshot/friendly reachability and counts verification and cache-copy fallback I/O; timings are environment-specific, not a latency budget or CI-sized fixture (#140).

- The Windows desktop host can install, update and start the HFDesk service from its own interface. The tray menu gained **Install service…**, which confirms the machine-wide change, requires the console program in the folder of the desktop program — the program paired with this desktop program, with no other candidate when the name of the desktop program carries a version, so a missing pair is an error that names the console file it expected, while a local build without a version falls back to `hfdesk.exe` and then to exactly one `hfdesk*.exe` of a release — because nothing is downloaded or copied, and then asks Windows for administrator rights through the standard prompt: the elevated helper waits for the desktop app to stop its own server through the graceful shutdown and exit, so the two instances never serve the same address, and then installs and starts the service from that folder and reports the result. An installation that registered the service and then failed to start it, or failed to verify the start, removes the registration it wrote again when the machine had no registration before — the registration only, the data stays — and its report says that the desktop app can be started normally again; a registration that existed before the attempt, and a machine whose state before the attempt could not be read, are left in place and the report names `hfdesk service uninstall` from an elevated shell. A launch that finds a registration asks before anything serves: it offers Open HFDesk / Update service / Cancel for a running service and for a broken one whose instance still runs, Start service / Update service / Cancel for a failed one and for a broken one without a running instance, and — only for a positively confirmed stopped service — Work in desktop mode as its default answer next to Start service / Update service / Cancel. Work in desktop mode continues the launch exactly like a launch without a registration: it starts the server of this app, never starts the service and never changes the registration, so the stopped service stays stopped. Every other answer ends the launch, which then starts no server of its own and publishes no state, where Open opens the registered address, Start starts the registered service, Update re-registers the service from the folder of the new desktop program while keeping its settings and its running state (a running service keeps running on the new version, a stopped one stays stopped), and Cancel changes nothing. A launch that cannot read the registration starts no server and publishes no state either, because an installed service may be running. The choice dialog is a Windows task dialog from the embedded application manifest, which declares the common controls version 6; a local build without that resource falls back to message boxes that map Yes, No and Cancel onto the actions in their text. The service keeps its own data under `%ProgramData%\HFDesk`, and the settings, jobs and models of the desktop app are not migrated into it.
- Windows recovery defaults apply to new registrations and to existing registrations whose action list is successfully read as empty: three restarts, a one-day reset and non-crash recovery enabled, while the recovery command and reboot message are preserved. A nonempty custom action list and its non-crash flag remain unchanged, including intentional `NoAction`, which can leave a failed service without an automatic restart; this exception does not change the defaults for new or proven-empty registrations. Read errors cause no blind policy write, and failed default writes report whether restoration was verified.

### Changed

- Mirror and rebuild mutations now join the destination ownership admission end
  to end (#151): the mirror's snapshot symlink-leaf replacement takes entry
  admission and its directory creation takes admitted directory creation, and
  the sync/rebuild path creates, replaces and removes snapshot/friendly
  entries, prunes empty directories and writes the rebuild script through the
  same shared admission funnels the download path uses. A second HFDesk
  instance (or another machine sharing the control filesystem) holding an
  entry's or tree's scope now excludes these mutations — refused with the
  actionable ownership error after the established bounded wait on the link and
  script funnels, immediately on the non-waiting admission surface the mirror
  uses for file writes and tree removals — instead of racing a live writer.
  Coordination stays file-based per path: independent files and repositories
  remain parallel, and no publication guarantee is added, since these
  operations still perform no domain-exclusive placement.

- Replace payload-adjacent destination lock files with one external ownership
  realm for every download route (#140). `Settings.OwnershipDir` selects the
  realm; empty uses the captured per-user HFDesk config directory. Clients that
  share storage across profiles must configure the same accessible, nonoverlapping
  realm. An item takes its scope after the skip decision and before resume/HEAD/
  claim, and holds it through transfer, publication/store and cancel cleanup.
  Cache snapshots and friendly entries acquire scopes for their actual paths
  while the blob item key remains held. No new `.lock` payload sibling is
  created; flat items named `foo.incomplete.lock` are ordinary independent
  destinations. `CheckBlob` and cleanup use the same external realm/key
  protocol. External locks are hashed beneath `<OwnershipDir>/v1/`, persistent
  and never unlinked. A busy exclusive scope is waited for (bounded,
  context-aware, effective five minutes), never taken over or reported as a
  successful skip; unresolved reader contention also fails closed. Waiters
  return and reacquire transfer slots. Mixed-version writers remain unsupported:
  `LockFenced` alone is not current proof. New v4 records carry `OwnershipFence`
  for the current realm/key; absent, unknown or mismatched live proofs are
  preserved/refused unless independent self/death evidence permits reclaim.
- The exported `(*hfdownloader.RepoDir).CleanupIncomplete` helper has been
  removed (#140): it deleted a staging payload and its record without the
  destination ownership domain or a record check, so it could remove another
  live writer's partial. No in-tree code called it, and cleanup now runs only
  through the two domain-fenced entry points.
- Multipart downloads write ranged workers directly into one preallocated
  staging file at their assigned offsets and resume from durable per-range
  metadata, so finalization is an atomic rename plus link work, never a
  full-file assembly copy (#140). A record is trusted only when it matches the
  resolved commit, repository, path and expected size, is pinned on both sides,
  tiles the file with normalized completion, and its payload exists at the
  expected size before any create/truncate; a record whose payload was deleted
  or resized is discarded rather than published as zeros. The ranged record is
  version 4 and is the only resumable version; a record with a matching current
  fence (or one whose owner is provably dead or is this process, including a
  branch-only version-3 record that described imported bytes) is discarded,
  never migrated, and the file is re-downloaded from zero while already-published
  content stays untouched, while a record without a matching current fence whose
  owner cannot be proven dead is refused as unproven and preserved. Every pre-#140
  `.part-NN`/`.parts.json` family is likewise discarded, never migrated. Existing version-4 state keeps the
  multipart route for a ranged-capable item even below the configured threshold,
  so a threshold change does not ignore valid state; an item without usable
  state keeps the configured routing. Without range support the downloader
  warns, downloads the full file through the single path, and removes the
  obsolete versioned pair only after the final file is durable (claim-checked).
  A staging record is installed as a complete document; hard-link support is not
  required because an atomic rename is the fallback.
- The transfer is pinned to the commit the planner resolved (#140): the tree
  walk and every per-file URL are built from that commit. When the endpoint
  resolves no commit, the requested revision is used and an unpinned identity
  with a known digest is content-proved once before publication, on both
  routes, so a moved branch cannot publish unproven bytes.
- The single-request path inherits staged bytes only under a proven identity
  (#140): its versioned sidecar records repository, resolved commit, path,
  expected size and whether that commit was pinned, and separately whether the
  producing run observed clean response framing. An exactly-sized partial is
  published without a request only when the identity matches and clean framing
  is recorded; otherwise the final byte is reserved and re-requested. A
  permanent 401/403/404 keeps the sidecar with a non-empty partial, and an
  oversized partial is discarded rather than clamped. The sidecar is removed
  only when the partial it describes is actually gone; a stat failure other
  than not-exist leaves it in place so the bytes keep their resume identity.
- `Settings.Verify` now controls LFS verification too (#140): `none` performs
  no post-download content hash; `size` (the default) checks exact expected
  size and complete validated transfer coverage on both paths and performs no
  full-file SHA-256 reread on the normal path; `sha256` verifies with at most
  one full-file read, always before publication when a known digest exists;
  `etag` proves a known digest with that same single pre-publication pass while
  non-LFS ETag-only files keep the historical no-post-check behavior. A known
  digest is never hashed only after publication. A durable `ProofPending` flag
  forces a re-proof on resume in every mode except `none`, so a crash mid-proof
  or a later mode change cannot publish unproven ranges.
- The mirror file copy, the mirror's `deleteExtra` removal and the HF branch of
  selected-GGUF deletion now take cache-owned admission — entry admission per
  copied destination file, tree admission per removed repository or per the
  selected repository and its friendly view — instead of remaining writers
  outside the destination ownership domain, while the downloader's publication
  invariant still does not depend on their behavior: none of them publishes
  through the no-replace primitive. The mirror copy itself checks nothing
  beyond `io.Copy`'s error contract (its explicit size comparison happens only
  in the opt-in verify pass, at size granularity rather than hashes), the
  mirror's symlink-leaf replacement and directory creation join that admission
  too (entry admission and admitted directory creation respectively), and
  selected deletion's local-root branch takes flat-realm entry
  admission over exactly the member paths it unlinks, resolved through the same
  `Settings.OwnershipDir` its writers use (default `DefaultOwnershipDir()`) —
  as strong as the flat realm's writer coordination: the flat realm's identity
  is an opaque ID persisted inside the shared `OwnershipDir`
  (`<OwnershipDir>/realm-id`), so every participant resolving one
  `OwnershipDir` reads the same realm ID, and its path-derived resource keys
  and lock filenames converge across hosts only under the conditions the
  downloader contract states: the `OwnershipDir` and the items under one
  common shared mount, where any prefix movement cancels out, with identical
  path spellings required on a split topology and the dev:ino alias keys
  staying host-local on purpose — resolving the same `OwnershipDir` is
  necessary, not a complete sufficient condition on its own (#140).
  Device/inode-derived lock files from the former
  host-local realm identity are harmless residue (never unlinked);
  mixed-version writers deriving it remain unsupported.
- Ordinary downloads keep commit pinning on Hugging Face and commit-capable
  endpoints. A custom endpoint proven to serve files only by branch name may
  select one coherent unpinned plan before reservation; it starts retries from
  zero, inherits no partials, and makes no immutable commit/ref claim. Branch
  variants remain strictly commit-pinned.
- A Transformers repository whose `config.json` carries a nested `quantization_config` object is now reported with the same `quantized` projection as a repository that has a root `quantization_config.json`: `GET /api/analyze` fills the projection for it instead of leaving it empty, which is also how the quant method of an EXL branch is discovered (#100).
- `hfdownloader.PlanRepo` and `hfdownloader.Download` now reject a path-shaped filter combined with `AppendFilterSubdir` (typed `ErrAppendFilterSubdirPathFilter`) before any network or plan work: only single-component group/token filters can name a destination subdirectory. Path filters remain valid with `AppendFilterSubdir` disabled.
- README rendering and Hugging Face repository-link classification now have a single Go implementation: the bundled UI renders the server-sanitized `html` from `GET /api/readme` and no longer carries a client Markdown renderer or duplicate URL classifier (#135, #147).
- Remote repository tree paths must now use their canonical spelling: a raw path whose `path.Clean` form differs (for example a `./` segment, duplicate slash, or trailing slash) fails planning with a non-canonical-path error instead of being normalized, so two spellings of one remote file cannot both be planned and race to write a single cache entry.
- Every Hugging Face metadata fetch of planning and repository analysis is now time-bounded per request (a 2-minute total cap over dial, response headers, and body) instead of being bounded only by the caller's deadline or an unbounded body read: the repo-info lookup and tree pages of planning, and the analyze flow's tree pages, refs, config/metadata file content, safetensors header reads, and model-info lineage lookup. When a required read stalls — a repo-info or tree-page body, or a metadata body read or LFS/pointer resolve that the analysis reports as a named-file failure — the operation fails at the cap with an actionable timeout naming the budget and the remedy: a failure, never caller cancellation and never a silently partial result. Optional fetches — refs, the lineage lookup, safetensors header probes, the EXL config lookups, and plain non-LFS transport failures of config/metadata content — keep their existing degradation (a silent skip or an uncertainty warning) and are now bounded by the same cap; the post-success dataset namespace probe's failure, previously silent, now surfaces as an analysis warning (the analysis outcome is unchanged). A caller's own deadline still wins over the cap; for the production analysis — one shared 60-second caller deadline, with the EXL branch pass already under its own 15-second context — the caps are defense in depth, while for deadline-less or longer-deadline callers they are the new bound, and only for such callers could a stalled endpoint previously hang an analysis or planning run (#151).

### Removed

- The obsolete stale-timeout configuration was removed with the age-based
  ownership lease (#140): `hfdownloader.Settings.StaleTimeout`,
  `hfdownloader.HFCache.StaleTimeout`, `hfdownloader.DefaultStaleTimeout` and
  the third `staleTimeout` argument of `NewHFCache`/`NewHFCacheResolved` no
  longer exist. Ownership uses the external realm selected by
  `Settings.OwnershipDir`; v4 record ownership uses `LockFenced` plus the exact
  current `OwnershipFence` realm/key proof, never record age. There is no stale
  timeout compatibility shim or replacement setting.
- The diagnostic-only staging-record heartbeat was removed (#140). The record
  is not rewritten on a timer; it is diagnostics/resume state, not a
  freshness-based lease; only the held external scope and matching
  `LockFenced`/`OwnershipFence` pair prove current ownership.

- `hfdesk service install` over an existing registration on Windows and Linux now merges with that registration instead of overwriting it: every setting the command does not name again is kept, the program always comes from the request (the invoked binary or `--executable`), a running instance is restarted on the new program and is only reported as installed once the supervisor runs it, a stopped or failed one stays stopped, and a broken one follows the running fact: it stays stopped when no instance runs and keeps running when one does, and a failed installation restores the previous registration and running state and names what it restored instead of claiming success. On macOS, `install` now preserves the registration across an update as well: the same merge keeps every setting the command does not name again, the autostart key `RunAtLoad` keeps the value the existing property list carries (a property list that cannot be read records no setting: the installation repairs it from the request and writes the default instead), a stopped instance is never started by a repeat installation, and an instance that runs is restarted on the new program through the manager's existing stop/start lifecycle; a first installation — no property list and no job launchd holds — still registers the job with `RunAtLoad` and starts it. HFDesk owns the property list it writes: the settings carried in the registered program arguments, the working directory and the service account are the ones the merge preserves, `RunAtLoad` keeps its found value — a property list that is missing or cannot be read leaves no value to keep, so the default that starts the instance is written instead — and a key that property list does not carry (a hand-added scheduling key) or a hand edit of the policy keys it does carry (`KeepAlive`, `ProcessType`, `ExitTimeOut`) is rewritten to HFDesk's rendering by the next update. macOS keeps its existing verification depth: the update reports the registration launchd accepted, without the readiness and version probe of the other platforms.

### Fixed

- Many concurrent cache-mode downloads in one process no longer collapse behind the shared node bootstrap locks (#151): resolving an already-published cache node is read-only validation that takes no exclusive gate (the parent's child-bootstrap gate belongs to the node-creation path only, and creation stays gated), a preparation resolves each path once instead of re-walking the same shared chain, and ownership retries back off exponentially with jitter instead of a fixed 20ms quantum, so a 100-file job at the product's default concurrency completes in line with the work instead of exhausting the five-minute ownership budget. Same-process download contention is classified and reported as such: it never surfaces the external "another process" ownership error, which within download contention stays reserved for a proven foreign holder, and the keyed in-process waiter times out with the same classification.
- Deleting a repository with thousands of directories no longer fails with "too many open files" under common file-descriptor limits (#151): cache tree admission (server `DELETE /api/cache/...`, mirror `deleteExtra` and HF selected-GGUF deletion) holds two open lock files per directory (entry and children locks) for the whole tree until removal finishes, while flat tree admission (branch-variant completion-record invalidation under a flat `OutputDir`) holds three for each existing directory (path, entry and object keys), and each such admission now raises the process soft file-descriptor limit toward the hard limit for exactly its own window (restored on release) instead of exhausting the common 1024-fd default after roughly 500 cache-mode directories. The whole-tree exclusion guarantee is unchanged — the entire tree is still exclusively admitted before any removal, and a live writer anywhere in the tree still refuses the admission. A tree beyond the raiseable hard limit or the OS per-process file cap fails with an actionable error naming the observed limits and the remedy (`ulimit -n` / `LimitNOFILE=`, raising the hard limit first when needed), never a raw per-process descriptor-exhaustion (EMFILE) error — system-wide ENFILE keeps its raw identity — and nothing is removed.
- Flat-mode ownership keys no longer embed the absolute path spelling of the item (#151): the path-derived keys (`item-path` and `directory-path`) now derive from the persisted realm ID and the item's path relative to the realm root, so when the shared `OwnershipDir` and the coordinated items sit under one common shared mount, any common-prefix movement of that topology cancels out — two hosts mounting the same tree at different prefixes (for example `/mnt/nas/...` and `/vol/nas/...`) derive identical resource keys and lock filenames and genuinely exclude each other. That convergence presumes one shared realm ID, which must be minted once by a single participant: the ID is minted on first use under an exclusive bootstrap gate, and on a filesystem whose advisory locks are not effective across clients — the same limitation that already bounds the cross-host locks themselves — two hosts can each mint their own ID and will then never exclude each other, silently, because their lock filenames differ; no safety is claimed for the mechanism on such filesystems. A split topology (`OwnershipDir` and the items rooted under different mounts) requires identical rel-token input on every host — the same structure below the lowest common ancestor and the same descending component spellings — so independently mounted split topologies that preserve that structure still converge (the documented residual), and a target on another volume (Windows cross-drive) keeps its absolute-spelling derivation, so same-host behavior is unchanged in every shape. The dev:ino alias keys (`item-entry`, `directory-entry`, `directory-object`, tree-validation identity) stay host-local on purpose, so a foreign host's staged `OwnershipFence` never reads as current proof there: the record's consequence is the claim's two branches, not a blanket re-download — an unproven record (a foreign host, or a same-host live or mixed-version owner that cannot be reclaimed) is preserved and refused with `ErrStagingOwnershipUnproven` and is re-downloaded only after the operator removes the named staging pair and retries, while a reclaimable record (the owner proven dead or this process, including one fenced under the former keys) is taken over with its fence re-bound to the current proof and its committed ranges inherited (resume, not re-download), discarding and restarting from zero only on a version, range, size or identity mismatch. Flat lock files derived from the former absolute spelling become harmless residue (never unlinked), alongside the former device/inode-derived ones; mixed-version writers remain unsupported. The flat-identity claims in this changelog and the docs are corrected to state exactly these conditions.
- A download job no longer reports success while a requested file is missing
  because its external ownership scope is busy (#140): `completed` now means the
  file is present and reachable at its snapshot/friendly path. The item waits
  for an exclusive scope holder (bounded, effective five minutes) and re-checks
  the published content-addressed blob, downloads the file itself when the
  scope becomes free without publishing, or fails with an actionable scope-held
  or unresolved-contention error. A path-derived destination always
  re-downloads after the wait. A waiting item no longer occupies a
  `MaxActiveDownloads` slot, so other files of the job proceed even with a low
  limit. A link-creation failure now
  fails the item in both transfer and skip paths with
  `ErrCacheLinkUnreachable`, and the end-of-job link reconciliation reports a
  job error, so a job cannot finish successful with a missing path; a disabled
  friendly view is not an error.
- The ranged publication now asserts two independent gates: a complete range
  layout (every range `Done` and the committed total equal to the expected
  size, in both the live and persisted layout) and the exact payload length
  after the handle is flushed. A failure renames nothing and leaves the staged
  bytes for a resume. A content-addressed blob is placed with a no-replace
  primitive: atomic hard-link on filesystems that support it,
  domain-exclusive check-then-rename otherwise. `FinalizeBlob` now uses that
  same primitive and domain instead of renaming over a published blob, and a
  writer that loses the race discards its own staging pair.
- A staging record whose owner cannot be proven alive or dead is no longer
  reported as "another live writer" (#140). Such a record (unfenced, unreadable
  or unparseable current-format state) is preserved and refused with
  `ErrStagingOwnershipUnproven`; the operator removes the staging pair and
  retries, and never removes the persistent domain lock or a pair whose owner
  is a proven live owner. `CheckBlob` now distinguishes `BlobDownloading` for a
  proven live owner, `BlobStale` for a provably dead one, and
  `BlobOwnershipUnknown` plus an `ErrBlobOwnershipUnknown` error for an unknown
  state; the incomplete payload is preserved and an unknown state is never a
  successful skip.
- Cache surfaces and the mirror now separate published content from staging
  artifacts with one exhaustive name-shape classification (#140): the
  single-request `tmp-<name>` family, the ranged `.incomplete`/`.meta` pair and
  temp, persistent `.lock`, obsolete pre-#140 `.part-NN`/`.parts.json`,
  flat-mode partials and the cache store's `.tmp-<random>` copy temp are never
  counted, listed as cached content, copied, verified or deleted, even when a
  published name carries such a shape. Unusual non-canonical published names
  remain content. A blob key whose name belongs to the staging namespace is
  refused by `StoreDownloadedFile` and `FinalizeBlob` before placement.
- Cleanup now participates in the destination ownership protocol (#140):
  staging artifacts are removed only while the destination's domain is held, so
  cancelling a job no longer deletes another live job's partial, and each
  obsolete pre-#140 `<dst>.part-<digits>` file is unlinked only under that exact
  path's entry admission in the destination realm — a residue file whose
  admission a live holder refuses survives, and the refusal does not fail the
  download or cleanup. The entry points collapse to one owner-checked
  removal and one record-checked removal. A route removes only the artifacts it
  creates: the multipart HEAD failure no longer removes a single-route
  `.part`/sidecar.
- An explicit cancel during the single-path known-digest proof now closes the
  partial's writable handle before cleanup, so Windows does not retain the
  cancelled `.part` and its sidecar; pause/internal aborts still preserve
  resumable state. Unknown-size LFS items that fall back from multipart to the
  single path are stored through the cache store, creating the blob and its
  snapshot/friendly entries.
- A shared staging mutation now requires the supported lifetime OS lock:
  unsupported, denied or failed acquisition returns an operation error before
  claim/payload mutation and preserves partial data; there is no lease-only
  fallback. Unfenced records are retained unless same-host owner death is
  proven, and cancellation while waiting to publish is observed before the
  rename.

- A macOS update no longer boots out a start that launchd has already scheduled: a loaded job whose state names a start before any process of it exists (`waiting`, `spawn scheduled`) is read as not running by the update, so the stopped-instance branch would have unloaded the pending start and left the instance down. The state is now read before anything is written and the update refuses instead — quoting the state it read and the stop that clears it — so the pending start survives, while every other state updates exactly as before.
- A reinstalled macOS registration no longer drops the settings it carries when the property list spells them `--flag=value`: the merge read a flag only in its paired-words form, so a hand-edited or foreign property list carrying `--listen=127.0.0.1:9090` had that value silently replaced by the default of the request. The equals form now decides on the same terms as the paired words — the effective occurrence of the flag wins, the last one, which is the value `service run` really runs with, whichever form it uses, and everything after the first `=` is the value — so a reinstall preserves `--flag=value` arguments like any other registered setting, and a repeated flag keeps its effective last value instead of the first, non-effective one.
- A Windows installation now stamps the verified version only when the running image is proven to be the registered file: the identity comes from an open handle of the module path the kernel loaded the image from, so `os.SameFile` compares the file behind that handle with the registered file instead of two loose path walks, and an image whose identity cannot be proved — a path that no longer resolves — withholds the claim instead of naming a program the claim cannot prove. Before, the identity was read from an answer Windows does not have, so a Windows update stamped no version at all; now it stamps one exactly when the platform can prove it, and every unprovable case still reports no claim.
- The tray no longer launches an elevation that the serving-desktop guard would refuse: **Install service…** and the update action asked Windows for administrator rights even when the mutation was already decided against beside this serving desktop — a start beside it, an update that is not a pure rewrite of a positively confirmed stopped registration — so the user paid a user account control prompt only to read the same refusal afterwards. The decision now runs in the desktop host through the guard of the helper itself — the singleton probe beside the live published claim — so the two surfaces can never disagree, the refusal is reported before any elevation, and the helper still decides authoritatively when an elevation actually happens.
- A CLI installation that cannot parse the command line of a Windows registration no longer changes the cache tree: the merge treated the mere presence of the registration as a merge source, so an unreadable command line — the documented repair path of a damaged registration — replaced the cache root and hub directory of the request with an empty result and silently pointed the instance at another tree. Only a parsed registration is a merge source now: a command line that cannot be read records no setting and reports that fact so the defaults of the request stand, while a readable command line keeps its registered values — including the real absence of a directory — exactly as before.
- An executable-only Linux update no longer breaks its own verification through a `User=` drop-in: the base `Group=` followed the primary group of the account the installation resolved, and an account a `User=` drop-in of the loaded configuration provides carries a group that describes the drop-in identity, so the write paired that group with the base unit's own `User=` and the loaded-identity verification failed after the write. The `Group=` of the written base unit now follows the account the write names: when `--service-user` is not named and a registration is replaced, the previous base `Group=` is preserved byte for byte — including its absence — a named flag still writes the primary group of that account, and a first installation keeps the rule it had, so the service identity changes only when the matching flag is named.
- A Windows service start now proves readiness from its own instance instead of from any HFDesk on the address: the readiness of a start requires the own host to report its listener bound and not ended, combined with the unchanged HFDesk probe answer. Before, another HFDesk that already owned the address could make the start report ready although the own instance had failed to bind or had not bound yet; such a start now fails with the cause of its own host, and the probe semantics (HFDesk answer with the version where observable, no credentials, no redirects, no proxy) are unchanged.
- A Windows desktop host whose effective URL cannot be published no longer keeps serving: the mutation guard of the session decides whether a desktop serves from the published state next to the singleton probe, so a serving desktop without one would let `hfdesk service install|start|restart` start the service beside it. A publish that fails now stops the server through the graceful path and ends the launch like every other startup failure — nothing is served, the report names the publish error as the cause, and the one next action is to make the configuration directory writable and start HFDesk again; a publish that succeeds serves exactly as before.
- The guard that refuses `hfdesk service install|start|restart` beside a serving desktop now runs on one liveness model: a published claim whose process has died — even while an open handle still references the exited process object, the state the elevated helper itself holds through a handover — is stale and counts as absent, so the leftover state file of a crashed desktop no longer refuses anything, while a live claim refuses a mutation in both phases of the desktop — the starting window in which the launch has not bound its address yet as well as the serving one — and an elevation approved by another administrator reads the claim of the launching desktop from the state path that travelled with the singleton instead of the empty claim directory of the approving account. The claim file itself now belongs to one Windows session — its name carries the user and the session of the desktop, the identity of the singleton — so one session's desktop can neither hide nor delete another session's claim, while the guard still only protects the caller's own session. The refusal needs both facts — a singleton probe that found a pre-existing holder and a live published claim — so a stale claim or a probe without a holder proceeds exactly as a launch that never published one; and a claim that exists but cannot be read or parsed is no longer read as absent: it is indeterminable, so the mutation refuses before it changes anything, stating that cause alone, instead of proceeding beside a desktop it cannot see.
- A Windows desktop first install now starts and checks readiness/version through the service manager's creation handle. An observable version mismatch fails; authentication and local `dev` starts remain ready without a version claim. Cleanup settles and removes only the unchanged registration/process this attempt still owns; changed or unobservable ownership is kept and reported. Candidate PE structure is checked before registration writes, Windows host-native/guest support replaces installer-architecture matching, and executable/data access denials come only from `AccessCheck` using the running LocalService token when available; otherwise access remains inconclusive.
- A running-service update now proves what it reports. It compares the version of the restarted instance when the build version is known — the build version of the console binary it runs, or the release version of the desktop host that updates — while an explicit `--executable`, a local `dev` build and a Basic-auth-protected instance count as ready without a version claim. The readiness probe asks the listen address directly and never through a proxy configured in the environment, so a healthy instance on a LAN address is no longer failed by an ambient proxy. Linux preflight now checks effective execute access under the instance identity without requiring a native executable's read bit, and detects a truncated ELF header, a missing ELF `PT_INTERP` or shebang interpreter in a readable short file, or short plain text without a shebang only when the visible `binfmt_misc` state is readable and empty. An active handler or unavailable handler state leaves those formats unclassified. It does not reject compatible ELF machines or other unclassified formats just because a local parser cannot prove them; an unavailable effective-access check is logged as inconclusive, not treated as a denial or as verified access. Windows keeps its ACL check for the program and its parent directories for `LocalService`. No permission is changed. A kept registration whose state cannot be read after a failed start is reported as an unconfirmed start, not as a stopped service, and the service choice is passed to the TaskDialog API as packed records, so every button id and label lands on the field the API reads.
- A repeat `hfdesk service install` no longer turns a runtime-only systemd enablement into a permanent one: the installation treated any zero exit status of `systemctl is-enabled` as "enabled", so a unit an administrator enabled with `systemctl enable --runtime` was enabled for good by the next installation, and a `static`, `indirect`, `alias` or `linked` unit — which also exits zero — was enabled as if it had been enabled for boot. The installation now reads the printed state and leaves the enablement of a repeat installation alone: no `enable` runs at all, so `enabled` stays enabled, `enabled-runtime` stays runtime-only, and every other state enables nothing; a first installation still enables its registration persistently, and a masked unit is still refused.
- A Windows service update that the Service Control Manager refuses part way through no longer leaves a partially changed registration behind without saying so: `Service.UpdateConfig` writes the program, the account, the delayed auto start and the description in separate calls, so a later failure could leave the new command line registered while the old process kept running, and the installation reported only the platform error. The failure is now read back and the previous configuration is restored, and the report states what was proved: that the previous configuration was restored, or that the registration may be partially changed together with the reason. The rollback of a failed installation and this failure path restore the configuration through one helper, and neither stops nor starts the running instance.
- A Windows service whose executable was deleted while the instance still runs is no longer treated as a stopped service: `Status` reported such a registration as `broken`, and the desktop startup gate therefore offered **Start service** and the update helper expected a stopped service, so an update reported a failure although the manager had restarted the running instance. `Status` now carries the supervisor's running fact next to the state the registration is diagnosed with, the gate offers **Open HFDesk** for a broken-but-running service, and an update keeps it running and reports the address it serves; a broken registration without a running instance stays stopped. `hfdesk service status` keeps its exit code contract: the code follows the state, so a broken registration still exits with `1`, and the detail says whether the instance still runs.
- An update of a running Windows service whose start type is disabled is now refused before the instance is stopped: Windows refuses to start a disabled service, so the installation would have left the machine without the service that ran before, and the rollback could not have started it again either. The error names the start type change (`sc config hfdesk start= demand`, or an automatic start) and asks for another installation; a stopped service with a disabled start type stays allowed.
- `hfdesk service install` over a masked systemd unit is now refused instead of silently replacing the mask: a masked unit is a link to `/dev/null`, and writing the unit file over it unmasked the unit without the decision of the administrator being visible. The mask is read from the supervisor whether or not the unit file exists, so a runtime mask (`systemctl mask --runtime`), which lives in `/run` and leaves no file at the unit path, is refused as well instead of registering a service that stays masked and cannot start. The error names the unmask command of the observed mask kind — `systemctl unmask hfdesk.service` for a persistent mask, `systemctl unmask --runtime hfdesk.service` for a runtime one — with the scope prefix of the command, and the enabled or disabled autostart state of an ordinary registration is kept as before. A machine with no registration still installs as before.
- The readiness probe no longer follows HTTP redirects: a `3xx` is returned as it is and rejected, so a foreign server on the listen address can no longer point the probe at the health document of a real HFDesk on another address and have the start counted as proven.

- A proxy URL without a host leaked its password: a proxy URL of the shape `http://user:password` is read by a parser as the host `user` with the port `password`, and the parse failure of that URL quotes the port, so the reason of a proxy configuration failure carried the proxy password into the log records and into the error response of `GET /api/search`, which is not served to loopback clients only. The reason of a proxy failure now names the cause of the failure without quoting any part of the URL that could be a credential, and the human readable proxy description drops the userinfo of the URL and the separately configured credentials as well. A normal proxy URL still reports its host and its port, so a misconfiguration stays visible (#141).
- Cache selection now exposes a downloaded mmproj companion as its own deletable group instead of dropping it from the selectable model: `GET /api/cache-selection` reports a group `kind` of `llm` or `vision_encoder`, a companion group carries an `mmproj-` group id and no model-quantization label, and the Model Details view renders a distinct "Vision encoder" badge with its own Delete action. The delete confirmation names the vision-encoder role in both the local and the HF case and never claims the selected artifact class remains. Deleting a companion removes only that companion, deleting an LLM group never removes a companion, coexisting F16/BF16/F32 companions stay independently selectable and deletable, and the existing writer, snapshot, and identity-pinning protections still apply (#138).
- The artifact-role rule for multimodal projector companions now has a single owner in `pkg/smartdl` (`IsMMProjFile`, the previously unexported helper), which the download picker, the cache metadata, and the cache-selection model all use. The rule is a case-insensitive "mmproj" match anywhere in the file's basename, so a dot-separated companion (`model.mmproj-f16.gguf`) and a name that merely starts with "mmproj" (`mmprojector-f16.gguf`) are both companions. `GET /api/cache/{owner}/{repo}` therefore reports a dot-separated companion under `hasMMProj`/`mmprojFiles` instead of listing it as an `F16` quantization, and a repository whose only companion is dot-separated now also reports the `vision` capability; the cache selection reports a companion as a `vision_encoder` group instead of a model quantization (#138).
- `GET /api/analyze` now supports an explicit `type=model|dataset` selector, so a repository that exists as both namespaces resolves without repeating the type-selection prompt (#135). `type` takes precedence over the legacy `dataset` parameter, and `type=model` combined with `dataset=true` is rejected as a conflict. The legacy `dataset` parameter keeps its pre-#135 behavior: only `dataset=true` forces the dataset namespace, while `dataset=false` still auto-detects.
- The Model Details `HF Hub ↗` action now uses the endpoint reported by `GET /api/health` when settings have not loaded or the settings fetch failed, and stays hidden rather than guessing `huggingface.co`; health now reports the normalized effective `endpoint` (#135).
- Filtered plan/download scans no longer succeed when no non-excluded path directly matches and no recognized dataset membership selects data: `POST /api/plan` now returns `400` with error code `no_filter_match` instead of a metadata-only plan, while empty repositories without filters still plan successfully.
- Cache mutations now exclude live writers in every process sharing the cache, not just jobs of this server (#140): `DELETE /api/cache/{owner}/{repo}` and selected-GGUF deletion — HF and local locations alike — return `409` without unlinking anything while a live download uses the repository, its friendly view, or the selected member file (the local branch through the flat `Settings.OwnershipDir` realm it shares with flat writers), mirror `deleteExtra` removal reports the refusal in `errors[]` with `success: false` instead of deleting, and mirror in-place overwrite of a destination file refuses instead of truncating it. The exclusion lives in the cache itself (`H/.hfdesk-control`, `R/.hfdesk-control`), so a plain mirror target gains a hidden `.hfdesk-control` control directory. A local selected-delete likewise creates the flat ownership realm directory (`<DefaultOwnershipDir()>/v1/`) when it is absent, exactly as a flat download does. A whole-repository DELETE, a mirror `deleteExtra` removal, and the HF selected-GGUF delete treat a target that vanished between enumeration and removal as absent only after re-verifying the vanish while holding the missing target's creator-side admission, so a download starting in that window is excluded; an admission that can be neither established nor re-verified as vanished fails closed (`500`, or `errors[]` for mirror) instead of proceeding without exclusion. Mirror results count only what actually happened: `copied`/`copiedSize`/`repos` include only repositories whose copy (and verification, when requested) completed without error, `deleted`/`deletedSize` include only repositories actually removed, refused entries appear only in `errors`, and a re-verified vanished repository counts as removed at its scan-time size, like `os.RemoveAll`.

- Reinstalling the service no longer resets the settings the command does not name again: an installation over an existing registration on Windows, Linux and macOS replaced the registration with the defaults of the scope, so a reinstall with a new `--executable` silently reset the bind address, the config/state/log paths, the cache root, the hub directory, the local output directory and the service account of the instance. These settings are now read from the registration and kept. On Windows the account identity is never changed either: an update over a registration whose account an administrator changed away from the supported `NT AUTHORITY\LocalService` identity refuses before anything is written or stopped and names the command that restores the supported identity, instead of writing the account back over the administrator's decision; a registration that runs as the supported identity keeps updating as before, and a differently spelled name of that same account (a localized name or a SID string) is stored in its canonical spelling.
- Service updates now keep absent cache/hub settings on their config/environment fallback and readable Windows SCM settings, and interpret registered arguments with the same flag grammar as `service run`; unrelated Linux unit settings live in administrator drop-ins, which an update never touches. Linux compares loaded commands with typed D-Bus argv, not the lossy `systemctl show` display; rollback avoids a recovery restart until the stored unit and applicable loaded command/launch properties are verified.
- A running service no longer keeps the previous program after an installation: the registration was written, but the process kept the image it was started from and served the old version until the next restart. An installation over a running instance now stops it, starts it again on the registered program, checks that the new program is in service, and restores the previous registration and the previous instance when that fails.
- The summary of `install` no longer reports the defaults of the request for settings that were kept: it names the effective registration (bind address, runtime paths, cache root and hub directory) of an installation over an existing registration.
- `hfdesk service install` no longer resets the autostart state of the registration it updates: a repeat installation wrote the automatic start again, so a Windows service an administrator had switched to a manual or a disabled start, or to a delayed automatic start, was turned back into the default start, and a systemd unit an administrator had disabled was enabled again. A repeat installation now keeps the autostart state the registration already has: it writes the `StartType` and the delayed-auto-start flag the Windows registration carries, and on Linux it keeps the enablement the supervisor reports, so an enabled unit stays enabled and a disabled unit is not enabled again, and on macOS the property list keeps the `RunAtLoad` value it carries and a repeat installation never leaves an administrator's `launchctl disable` cleared — where launchd refuses to load a disabled label and the update must load one (restarting a running disabled job, a rollback restoring a loaded disabled registration), it runs `enable` only for that load and restores the flag in the same operation, so an administrator's disable survives every update — and the `print-disabled` read behind that rule fails closed: an answer the supervisor cannot produce aborts the macOS install, update or rollback with that reason instead of being read as "not disabled"; a masked unit refuses the installation instead of being replaced. Only a first installation enables the registration for the boot of the scope.
- The readiness probe of a verified start no longer accepts any HTTP answer: the Windows handler and the restart verification of the systemd manager counted a start as proven as soon as an HTTP server answered on the listen address, so a foreign program bound to the port could be taken for the instance. The probe asks `/api/health` and accepts only a `200` whose body is the health document of HFDesk, with the `status` `ok` and a version, or a `401` that carries the Basic-auth challenge of the instance (`Basic realm="HFDesk"`); every other answer fails the start. The probe sends no credential: it never reads the access data of the config file.
- Windows service preflight no longer demands read access to the registered program image: the program is checked for execute access only, so an execute-only native program (an NTFS "Execute file" entry without "Read data") passes preflight, and the refusal now asks for execute access instead of read and execute access.
- A Linux service update now follows the managed-unit ownership contract: HFDesk owns `hfdesk.service` alone, regenerates it as its canonical rendering on every installation, and never writes, moves or deletes an administrator drop-in — customizations belong in `hfdesk.service.d/*.conf`, which systemd composes into the loaded configuration and whose path the `install` summary now names. Three states refuse an update before anything changes — whether the instance runs or is stopped — and each states exactly one instruction whose steps clear its cause before the attempt runs again: a base file that is not the canonical unit HFDesk generates (a hand edit, a specifier or variable operand, or an `env`-wrapped command, which is now refused as a hand edit instead of being repaired) is reset by moving the customizations into `hfdesk.service.d/local.conf`, removing the unit file and reloading the unit files before the installation runs again; a loaded `ExecStart=` that a drop-in owns (the refusal names the drop-in paths the supervisor reports) is removed from the exact drop-in file that carries it before that reload — a drifted command no reported drop-in explains names the reload alone — because the supervisor holds the loaded command in memory until a reload reads the file system again; and a loaded fragment HFDesk did not write is removed at the path systemd reports before the reload, while a fragment file that is already gone names only the reload that drops the stale load, and a loaded registration with no readable fragment names no command and states that whatever loaded it must remove it before an installation can pass. Drop-in `Environment=`, `EnvironmentFile=`, `SupplementaryGroups=`, `WorkingDirectory=` and `User=` survive an update byte for byte and remain the effective values; only the command may not be overridden. The `ExecStart=` ownership holds on a first installation as well: `install` scans the drop-in directory for an `ExecStart=` override before it writes anything and refuses over one, and after the reload it verifies that the supervisor loads the command it wrote — a drop-in that would own the command of a fresh registration no longer slips through as a successful first install.
- Run flags behind a recognized wrapper prefix are read after the program instead of from the wrapper's own words: `Status` reports the effective values a wrapped registration really runs with, instead of misreading wrapper-prefix words as run flags or dropping the flags behind the wrapper; an installation over such a registration refuses it as a hand edit instead of updating it.
- A Windows service update whose registration is removed while the action runs no longer reports success: the verification treated `not-installed` as the settled stopped state the stopped-service update waits for, so a registration deleted during the update was accepted as a finished update of a service that no longer exists. The state is now rejected the moment it is read: the action fails with the cause — the registration is gone — and the status command as the next action, while registered services keep the existing running and settled checks unchanged.
- A stalled final jobs-state save can no longer hang the graceful service handover forever: `CompleteHandover` joined the job-manager close coordinator with an unbounded context, so a write that never finished blocked the handover, the desktop quit and — through the desktop singleton it never releases — every later launch, while the elevated helper abandoned the attempt after its own 60-second budget. The join now has its own budget of the same size as the other shutdown phases, inside the window the helper waits in: on timeout the handover completes with an error, the desktop reports the failed shutdown and never signals success, so no service is installed or started for persistence that did not complete, and the quit finishes. The normal path still reports both persistence outcomes, settings and jobs, exactly as before.
- A stalled admitted settings write can no longer hang the graceful service handover forever either: `CompleteHandover` drained admitted settings writes with an unbounded wait, so a write stuck on the storage blocked the handover, the desktop quit and every later launch behind the singleton the same way. The drain now shares the bounded handover-join budget with the jobs close under one deadline, which keeps the whole join inside the window the elevated helper waits in: on timeout the handover completes with an error, the desktop reports the failed shutdown and never signals success, so no service is installed or started for persistence that did not complete, and the quit finishes. The normal path still reports both persistence outcomes, settings and jobs, exactly as before.
- A service mutation can now hold a desktop singleton object that another Windows identity owns: an elevated helper or `hfdesk service` command running as a different administrator account was refused with an access denial although the object's descriptor grants the administrators read control — and any open handle is all a hold needs, because a hold never waits on the object. The hold now falls back to opening the existing object with that read control; only an object this identity cannot open at all still refuses before the mutation, with the step that ends it. The descriptor is unchanged (the administrators keep read control only), and a desktop launch that cannot access the object still reports the other Windows identity that owns it.
- A service mutation that starts or restarts the Windows service now refuses before its first service step while a desktop instance serves its address: `hfdesk service install`, `start` and `restart`, and the elevated helper's install/update/start actions, held the per-session desktop singleton only against a *new* desktop launch and never checked whether an already-running desktop owned the address — a registered-but-stopped service could therefore be started beside a desktop instance that serves the very same address. The refusal covers every outcome that starts or restarts the service: `start`, `restart`, a first `install`, and an `install` or a helper update over a running instance (which the installation restarts), while an `install` or update over an existing registration that is positively confirmed stopped only rewrites the registration, starts nothing and proceeds beside that desktop — a registration state that is not positively stopped fails closed. Each mutation now decides with two facts taken around its hold: the desktop's published state (only a started server publishes one, the shutdown removes it) and whether another process of the session held the singleton then (the kernel object dies with its holders, so a state file that outlived a crashed desktop never refuses). Both facts are required: a gate launch that publishes no state and a fresh hold over a stale state file proceed exactly as before, the tray handover flow is unchanged because its authorization already proves the desktop exited, and the refusal — before its first service step — names the one step that ends it: quit HFDesk from the notification-area icon and repeat the command.

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
