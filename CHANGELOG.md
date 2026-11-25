# Changelog

## [Unreleased]

## [1.3.0] - 2026-11-25

Long-form notes: [docs/releases/1.3.0.md](docs/releases/1.3.0.md).

### Added

- HFDesk can run as a supervised background service: `hfdesk service install`, `uninstall`, `start`, `stop`, `restart` and `status` register it with systemd, launchd or the Windows Service Control Manager, the instance runs headless on its own config, state and log paths, and it binds loopback unless `--listen` names another address; `status` reports persistent and runtime masks separately with exit code 0 (#142).
- A second Windows download, `hfdesk-desktop_windows_amd64_v1.3.0.exe`, runs the same server in a GUI process without a console window, with an icon in the notification area, one **Logs** window and a single instance per user and session; a missing WebView2 runtime is reported while the server, the tray icon and the web UI keep working (#141).
- Structured logging adds `--log-file` and `--log-level` with a rotating file sink, and local-only runtime endpoints report the version, address, job counts, run id and log records to the desktop windows (#141).
- The desktop app and the tray companion show one **Logs** window with one frame: the live log page of the address it knows while that address answers — icon, version, facts strip and a filterable log table — and, when it does not, the same frame with the centre replaced by the address that did not answer, **Try again**, the log file's folder and file actions, and the tail of the same log (200 lines, at most the last 64 KiB) in the same table; error reports name the log file of their launch.
- Everything about the service sits in one tray **Service** submenu whose line names a single state — `Service: not installed`, `Service: stopped`, `Service: starting`, `Service: running`, `Service: stopping` or `Service: failed` — with no `installed` or `unknown` word; a read that settled nothing shows no line, no items and no submenu until the next reading succeeds, and the line is re-read after every action and every 30 seconds so the tray and the window keep the state of the machine.
- The **Logs** window header carries that line and its actions as one **Service ▾** control — **Install service…**, **Start service**, **Stop service**, **Update service** and **Delete service…**, with the state on the trigger and not repeated in the menu; the desktop app's own window binds the same five actions, and in a plain browser the line stays a static chip without a menu only where no available action has an executor, while an installation the host itself performs is still offered.
- Service items follow the state they prove: **Install service…** appears only where nothing is registered and only where that host can install, a registration whose program is gone reads as `failed` and offers **Update service** with no start, a crashed `failed` registration offers the start beside that update, a transition greys all four lifecycle items, and one service action runs at a time from either surface.
- Installing asks with the question and two short fact lines — the service starts at boot and keeps running, and its data is separate — adding the removable-drive warning when the program sits on such a drive; updating a running service asks first and states that the service is stopped and started again with running downloads interrupted, a stopped service updates without a question, and a desktop app that hands over asks its own question once; deleting states that a running service is stopped first and removes only the registration, keeping data, cache, config, history and logs.
- The Windows desktop can work as a tray companion beside an installed service: no server of its own, **Open HFDesk** for the service's address, the same **Logs** window, and **Exit**, which closes only the companion; a finished installation restarts the desktop as exactly that.
- When the paired console program is missing, the notice names the file and its folder and can download this release's copy from the official release, verify it against the published checksum and continue the service action, with the manual route beside it; the program is found under either spelling of its version, a build without a version or a release without a checksum file gets no offer, and a download that cannot be offered states the reason with **Try again** (#219).
- The report of a service update names the old program file left behind when the registration moved to another file and offers its deletion with one press; nothing is removed automatically.
- New Windows registrations, and existing ones whose action list is proven empty, get three restarts, a one-day reset and non-crash recovery, while a custom action list and its non-crash flag stay as they are.
- Whole-model **Delete** in Repository Details covers `Local` and `Friendly view` models — the complete folder for a local model, the complete cache presence for a friendly view — and refuses a folder shared with other files (#63).
- Analysis adds **Weight Format** rows for ONNX, TFLite, Flax and OpenVINO weights, recognizes `fp8`, `compressed-tensors` and `mlx` quantization methods and nested MLX configs, and adds a **Recommended quant RAM budget** setting that keeps the recommended GGUF artifact within the budget.
- **Dataset Configs** lists one preset per config folder that downloads the whole config, and a manual filter that matches nothing is reported as a `filters_unmatched` notice in the plan and as a warning on the job instead of disappearing.
- Plan and download accept shard ranges and an exact-list file selection, and Model Details gains a **Files** tab with a directory tree, tri-state checkboxes, sizes and LFS badges that downloads exactly the checked paths.
- EXL2/EXL3 bitrate branch quantizations are analyzed, recommended and downloadable as whole-branch variants, stored with per-commit snapshots and without writing the shared friendly view (#100).
- Downloads can be routed into optional per-type folders — LLM/GGUF, LLM/Safetensors, LLM, Audio, Diffusion, Embedding (#62).
- The Hugging Face token resolves as `--token`, then `HF_TOKEN`, then the config file (#72).
- Docker supports `PUID`, `PGID` and `UMASK`, runs as an arbitrary numeric UID/GID through `--user` without a named user or an `/etc/passwd` entry, drops privileges after the entrypoint's setup, and starts non-root on a read-only root filesystem with all capabilities dropped (#61, #71).
- The cache browser selects a location and a GGUF group for deletion and reports partial-failure details (#111, #125).
- Windows binaries embed the HFDesk icon, and Linux gets desktop integration (#54, #56).
- Numeric settings use custom stepper controls (#58).
- GGUF recognition adds the PrismML `PTQ1_0` and `PQ2_0` ternary quantizations, which require the PrismML llama.cpp fork and are never auto-recommended.
- A read-only **Community** tab lists Discussions and Pull Requests with threads, sanitized comments and event summaries, and repository links — including links to a repository root — open in Model Details with Back/Forward history instead of the browser (#135, #136).
- Gated repositories report a dedicated access state with token and agreement guidance instead of a partial analysis.

### Changed

- Plan validates the repository before the route key, matching download requests; plan paths stay repository-relative, `appendFilterSubdir` now applies over HTTP so a real download lands under the filter directory, and a path-shaped filter with the flag is rejected at the request boundary.
- Diffusers component rows download exactly their recommended weight set — fp16 safetensors over default safetensors over pytorch bin, every shard of one family, conversions excluded — instead of dragging every conversion copy behind a folder filter.
- Dataset split membership has one precedence rule, with ambiguous overlapping shapes still refused.
- A download holds one destination ownership scope from the authoritative skip decision through publication and cancel cleanup, and every per-file URL is pinned to the resolved commit (#140).
- Staged multipart data is reused only under a proven identity pinned on both sides, and unpinned planning is limited to proven branch-only endpoints (#140).
- Verification settings now also govern LFS files: `size` is the default and does not reread a file on the normal path, while `sha256` and `etag` run at most one pre-publication hash pass (#140).
- Mirror, rebuild and cache mutations share the destination ownership admission used by downloads (#151).
- Metadata fetches are time-bounded per request, and duplicate-download detection uses one shared identity comparison (#151).
- EXL branch discovery reads branch configs instead of guessing from the main tree, and nested quantization configs are recognized (#100).
- README rendering and Hub link classification have a single implementation (#135, #147).
- Remote tree paths must use canonical spelling instead of being normalized.
- Repository Details is a sectioned, resizable card: a two-line identity whose title opens the repository, a **This copy** facts header, compact GGUF rows with facts, location choice in the contents heading, and whole-copy deletion fixed above the footer with a target path; the dialog remembers its size per browser (#213).
- The selected GGUF group deletion confirmation names the quantization in its title and shows a one-sentence scope, left-aligned facts and right-aligned file sizes; the whole-copy confirmation is unchanged (#207).
- Cancelling a download, or cleaning a paused job's leftover part files, in local-folder mode removes the directories its removed partials leave completely empty, walking up to but never including the output root (#218).
- Hub error handling keeps the existing messages while using the actual HTTP status for namespace detection: 401 and 404 can fall back between model and dataset, 403 stays terminal, and a transport failure keeps its diagnostics (#125).
- `GET /api/runtime` serves the classified service view as `service` — class, label, tone and per-action `visible`/`enabled` — beside the raw `serviceState` kept for existing clients.
- The version shown by the UI and embedded in Docker images comes from the release tag instead of a hardcoded number (#64, #69).
- A service installation merges with an existing registration, keeping settings and running state the command does not name; a running instance is restarted on the new program and a stopped one stays stopped.
- Docker keeps all writable state — settings, jobs, history, HF cache and local models — under one `/data` root by default, with `HOME`, `XDG_CONFIG_HOME`, `HF_HOME` and the working directory following it (#61).

### Fixed

- GGUF quantization selections stay matched-only when README file names contain the selected quant token, and dataset split filters use inferred membership and fail clearly on ambiguity (#124).
- Quantization metadata is read from Git-LFS-backed config files instead of pointer stubs, quantization-only repositories show their precision, and the panel distinguishes proven uniform precision, observed variation and unknown precision (#123).
- Analyzer metadata is read completely within the 10 MiB cap, completed leading quantization fields are recovered from oversized configs, and unrecoverable overflow is reported (#105).
- Analysis and plan/download previews honor the configured proxy and report proxy construction failures instead of falling back to direct requests (#104).
- Stalled downloads are recovered and transport waits bounded: `429` and every `5xx` retry, non-retryable file responses fail without retrying, and optional verification metadata failures stay non-fatal (#97).
- Exact `HF_HUB_CACHE` storage stays consistent across downloads, cache operations, mirror, rebuild scripts, cleanup and disk previews, and complete job destinations are frozen across restart (#76).
- Settings updates preserve active and stored Hugging Face credentials, keep redacted token values preserve-only, save config files owner-only and keep the previous file when persistence fails (#74).
- Download destination and cache paths are contained at their join boundaries, remote hash values must be canonical SHA-256, size HEAD requests honor `Retry-After`, and a stalled transfer is an explicit stall error instead of `context canceled` (#117).
- Repository trees are listed recursively with `Link` pagination, bounded context-cancellable retries and a loud failure instead of silent truncation, so repositories with more than 1,000 entries per directory analyze and plan completely (#96).
- HF cache snapshot and friendly-view entries fall back from symlink to hardlink to copy where symlinks are unavailable, rebuild recreates missing snapshot entries from the download manifest, one notice per root names the fallback, and selected-GGUF deletion treats those entries like symlinks (#99).
- Selected-GGUF deletion stays durable across rebuild when tombstones are recorded, reports a recording failure while the deletion proceeds, and restores quant status after cache deletion (#99, #59).
- Job state is persisted ordered and atomically (#80).
- Malformed repositories and the colon/filter shorthand are rejected consistently, and an empty filtered scan fails with a no-match error.
- Cache-mode concurrency no longer collapses behind bootstrap locks, and large deletions avoid file-descriptor exhaustion (#151).
- Flat-mode ownership keys are realm-relative, so shared mounts survive a common-prefix move (#151).
- A download no longer reports success while a busy ownership scope leaves a requested file missing (#140).
- Publication asserts a complete range layout and the exact payload length after flush, and an unproven staging record is preserved and refused instead of published (#140).
- Cancelling during a digest proof releases the partial handle, and shared staging requires the OS lock (#140).
- A macOS update no longer unloads an already scheduled launchd start, and a reinstall keeps `--flag=value` settings.
- Windows installs verify the registered image or reject unreadable registrations, accept execute-only programs, and keep cache paths, settings and autostart.
- Linux updates respect administrator drop-ins, runtime-only enablement and masked units, and wrapper prefixes no longer confuse flag parsing.
- Updates restart a running instance on the new program, refuse disabled, removed or partial registrations, and verify readiness from the started instance.
- Desktop and service coordination uses one liveness model, publishes URLs safely, checks first-install readiness, and bounds the handover's wait for jobs-state saves and settings writes.
- Proxy failures no longer leak credentials into logs or API errors (#141).
- mmproj companions are deletable groups and share one artifact-role rule across the picker, cache metadata and selection (#138).
- Analysis accepts an explicit model/dataset selector, and the Hub link uses the effective endpoint (#135).
- Download warnings are retained in job state, with repo-wide resume warnings reported once.
- Local Cache dates ignore staging artifacts and interrupted-publication residue (#175).
- Excludes and path filters fold separators and case identically on every platform.
- A large download names a preparing phase while its staging file is created and preallocated, reports `writing` only while file creation, preallocation or a payload write is in flight, and takes speed and ETA from network reads, so a silent 0 B/s window and a phase held by a finished write are gone (#209).
- Ranged downloads mark their staging payload sparse before sizing it, removing the disk-speed zero-fill window on Windows; filesystems without sparse support keep the previous behaviour (#212).
- The systemd D-Bus client no longer crashes when a socket deadline or close lands mid-message (godbus v5.2.4) (#215).
- README rendering uses a patched `golang.org/x/net` (GO-2026-6866, GO-2026-6860), and the build toolchain is Go 1.26.11 (#216).
- Whole-folder deletion treats every recognized mmproj companion spelling as part of its model.
- A GGUF vision-encoder row is recommended only together with a recommended quantization.
- Model Details keeps Overview as the default tab for every recognized repository; only an unrecognized repository opens on Files.
- Community links to a repository root open it in Model Details instead of the browser.
- The page a browser loads from a running Windows service reports the state of the registration that instance runs from instead of `unknown`, and its install action stays unavailable.
- The service log under `%ProgramData%` is readable from the Logs window: each start grants interactive users read access to the instance's own log directory and active log file and touches no other descriptor.
- Starting, updating or deleting a service that is not stopped switches the desktop app to the tray and performs the action instead of asking the user to quit HFDesk; deleting returns it to desktop mode, and updating a stopped service keeps the window open.
- A failed installation names the cause — the start did not run, the program ended too early, the service did not come up in time, or the started service could not be verified — and a registration removed after a failed attempt no longer reads as never registered.
- Installation and service actions work while the Logs window or the web UI is open, without waiting for the open log view to end.
- A service action that fails during the switch to the tray shows one error window with its cause instead of two.
- A first installation no longer reports that the just-created registration changed before its first start when nothing else touched it.
- The service starts from any folder, a user profile included: the installation grants the service account access to its own program or refuses with instructions to move the pair.
- The log records state changes and actions instead of every state reading while nothing changes.
- Installing from the desktop app no longer flashes a red error in the Logs window: the window closes before the app's own server stops, while a server that fails on its own still shows the error.
- The Logs window follows the service while it stays open: a service that stops, fails or loses its registration redraws it, a missing program offers its update, and returning focus re-reads the state.

### Removed

- The unused invalid-repository sentinel, the obsolete stale-timeout settings and the staging-record heartbeat are gone (#140).
- The 20-file analysis preview and the non-selectable per-file download list are gone; the **Files** tab covers both.

### Compatibility

- Docker keeps its writable state under `/data` by default. An existing deployment must move its persisted volume to `/data`, or settings, jobs/history, the HF cache and local models are not persisted; files already in a mounted directory keep their ownership, and changing `PUID`/`PGID` does not recursively chown existing data (#61).
- The container no longer creates a named user or group and takes no `UID`/`GID` build arguments; the runtime identity is numeric through `PUID`/`PGID` or `--user`, with the entrypoint default `1000:1000` (#61, #71).
- Building from source requires Go 1.26 (`go.mod`); the release builds use Go 1.26.11.
- The default verification level is `size`, and it now also governs LFS files; a whole-file hash pass runs only with `sha256` or `etag` (#140).

## [1.3.0-beta.10] - 2026-11-25

Beta build of the upcoming `v1.3.0`; the artifacts are pre-release builds for testing and are not marked as the latest stable release.

Long-form notes: [docs/releases/1.3.0-beta.10.md](docs/releases/1.3.0-beta.10.md).

### Added

- The Windows desktop can work as a tray companion beside an installed service — without running its own server — and after an installation it restarts as exactly that.
- The service update report names the old program file left behind when the registration moved to another file and offers to delete it with one press; nothing is removed automatically.

### Changed

- The tray menu's state line names the state of the Windows service in words — `Service: not installed`, `Service: installed`, `Service: running`, `Service: starting`, `Service: stopping`, `Service: unknown`.
- The Logs window badge only says whether the service is installed — `Service: installed`, `Service: not installed`, `Service: unknown`.
- The service status in the tray and in the window follows every reading of the service, keeping both surfaces on the real state.
- The tray menu adds Start service, Stop service, Update service and Delete service, and deleting removes only the registration, keeping all data.
- Everything about the service now lives in one **Service** submenu with its status line at the top: the duplicate **Service…** item is gone, and **Install service…** shows only where there is nothing to install.
- One **Logs** window replaces the separate Runtime / Logs window in both desktop and tray-companion mode: the live log while the address answers, and an honest state with actions when it does not.
- The Logs window and the error windows open their log file with one action instead of showing its path as text.
- When the address does not answer, the Logs window also offers the step that fits the state — **Start service** for a registered service that does not run, **Install service…** when none is installed.
- The notice about the missing service program says in one short line which file is needed and where, and keeps its actions — the download and the manual route — visible side by side.
- Error reports now name the log file of the launch they belong to.
- Window texts are now short and plain: the facts of what happened, and a next step only where a failed action has to name one.
- Installing from a removable or network drive now warns that Windows may start the service before the drive is ready.
- The install confirmation is cut to the question and two short fact lines — the service starts at boot and keeps running, and its data is separate — with the removable-drive warning as its own line.

### Fixed

- The Logs page a browser loads from a running Windows service now reports `Service: installed` instead of `Service: unknown`: the page served by the instance reads the state of the registration the instance runs from, and its install action stays unavailable.
- The service log under `%ProgramData%` can be read from the Logs window again: at every start the instance grants interactive users read access to its own log directory and active log file, and touches no other descriptor.
- A start, a delete or an update of a service that is not stopped switches the app from desktop mode to the tray and performs instead of asking the user to quit HFDesk.
- Deleting the service in that switch returns the app to desktop mode.
- Updating a stopped service keeps the window open.
- The missing console program is found under either spelling of its version.
- A download of it that cannot be offered states the reason and offers **Try again** instead of a button that disappears.
- The log no longer records a state reading while nothing changes; it records state changes and actions.
- A failed service installation now says what actually went wrong — the start did not run, the program ended too early, the service did not come up in time, or the started service could not be verified — instead of a vague "could not confirm".
- A registration removed after a failed installation attempt no longer reads as if the service had never been registered.
- Installation and service actions now work while the Logs window or the web UI is open, without waiting for the open log view to end.
- A service action that fails during the switch to the tray now shows one error window with the cause instead of two in a row.
- A first installation no longer reports that the just-created registration changed before its first start when nothing else touched it: the check now compares only what the installation itself wrote.
- The service starts from any folder now, a user profile included: the installation gives the service account access to its own program, and refuses with instructions to move the pair instead of registering a service that cannot start.

## [1.3.0-beta.9] - 2026-11-19

Beta build of the upcoming `v1.3.0`; the artifacts are pre-release builds for testing and are not marked as the latest stable release.

Long-form notes: [docs/releases/1.3.0-beta.9.md](docs/releases/1.3.0-beta.9.md).

### Changed

- The Windows desktop host's missing console notice can download the paired console program and continue the service action (#219).
- The Windows desktop host shows the service registration state on both of its surfaces, with install-service actions sharing one guarded flow (#219).
- Cancelling a download (or cleaning a paused job's leftover part files) in local-folder mode now removes the directories its removed partials leave completely empty, up to the output root (#218).

## [1.3.0-beta.8] - 2026-11-18

Beta build of the upcoming `v1.3.0`; the artifacts are pre-release builds for testing and are not marked as the latest stable release.

Long-form notes: [docs/releases/1.3.0-beta.8.md](docs/releases/1.3.0-beta.8.md).

### Fixed

- The systemd D-Bus client no longer crashes when a socket deadline or close lands mid-message (godbus v5.2.4). (#215)
- README rendering uses a patched `golang.org/x/net` (GO-2026-6866, GO-2026-6860), and the build toolchain moves to Go 1.26.11. (#216)

## [1.3.0-beta.7] - 2026-11-18

Beta build of the upcoming `v1.3.0`; the artifacts are pre-release builds for testing and are not marked as the latest stable release.

Long-form notes: [docs/releases/1.3.0-beta.7.md](docs/releases/1.3.0-beta.7.md).

### Fixed

- Ranged downloads mark their staging payload sparse before sizing it, removing the disk-speed zero-fill window on Windows; filesystems without sparse support keep the previous behaviour. (#212)

### Changed

- Repository Details uses a two-line identity and **This copy** facts header, opens the repository from its title, keeps whole-copy deletion fixed above the footer with a target path, and moves location choice into the contents heading; dataset and shard counts appear once. (#213)

## [1.3.0-beta.6] - 2026-11-18

Beta build of the upcoming `v1.3.0`; the artifacts are pre-release builds for testing and are not marked as the latest stable release.

Long-form notes: [docs/releases/1.3.0-beta.6.md](docs/releases/1.3.0-beta.6.md).

### Fixed

- A download reports `writing` only while file creation/preallocation or a payload write is in flight, and its speed/ETA come from network reads; a completed write or a pure network stall does not hold that phase. (#209)

### Changed

- The selected GGUF group deletion confirmation names the quantization in its title and shows left-aligned facts, a one-sentence scope, and file rows with right-aligned sizes; the whole-copy confirmation is unchanged. (#207)

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
