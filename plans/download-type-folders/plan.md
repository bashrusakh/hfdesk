# Plan: Type/Category Download Routing (issue #62)

Status: proposal / not started
Author role: planner
Repository: bashrusakh/hfdesk
Resolved base ref: `origin/main @ 0c18669d7f4b05648b2d3a63eb65db988cc1969a`
Worktree: `/home/bash/.local/share/opencode/worktree/9c0a860b968eea07f09f313dc0265e642cc070aa/cosmic-pangolin`
All file:line references verified against that SHA.

---

## 1. Goal, non-goals, issue mapping

### Goal
Let a user who keeps one shared model library for several containers route
downloads into type/category folders while keeping today's behavior as the
default when nothing is configured. Target tree from issue #62:

```
/mnt/nvme/models/
├── LLM/{GGUF, Safetensors}/
├── Diffusion/{Checkpoints, UNet, LoRA, VAE, CLIP, ControlNet}/
├── Audio/{Whisper, Piper}/
└── Embedding/
```

Chosen direction is **A+B**:
- **A** — a settings map of `route-key -> destination path`. Routes are
  opt-in; a repo is routed only when a matching key is configured.
- **B** — coarse keys (`llm`, `audio`, `diffusion`, `embedding`) plus optional
  finer sub-keys (`llm/gguf`, `llm/safetensors`, later `audio/whisper`, ...).

Minimum accepted outcome: separate LLM vs Audio. Better: also split LLM into
GGUF vs Safetensors. The B scheme below supports both.

### Non-goals
- No change to the default single-`LocalDir` flat layout or HF-cache layout
  when no routes are configured.
- No arbitrary client-supplied destination paths (do not widen the existing
  `/api/download` `localDir` looseness).
- No change to `pkg/hfdownloader/targets.go` (`targets.yaml`), which governs
  **mirror** destinations, not download routing.
- No dataset-specific tree in Phase 1 (datasets keep current behavior).
- No new dependencies; no JS build step; no new scan/format framework.

---

## 2. Config and data model (A+B)

### 2.1 Field name and shape
Add one field to both the persisted file and the in-memory server config:

- `internal/server/config.go` `ConfigFile` (`:41-56`): add
  `DownloadRoutes map[string]string` with tag
  `json:"download-routes,omitempty" yaml:"download-routes,omitempty"`.
  A map marshals cleanly under both the JSON and YAML branches of
  `SaveConfigFile` (`config.go:177-182`).
- `internal/server/server.go` `Config` (`:22-53`): add the same
  `DownloadRoutes map[string]string`.

Example `hfdesk.json` / `hfdesk.yaml`:

```json
{
  "local-dir": "/mnt/nvme/models",
  "download-routes": {
    "llm":            "/mnt/nvme/models/LLM",
    "llm/gguf":       "/mnt/nvme/models/LLM/GGUF",
    "llm/safetensors":"/mnt/nvme/models/LLM/Safetensors",
    "diffusion":      "/mnt/nvme/models/Diffusion/Checkpoints",
    "audio":          "/mnt/nvme/models/Audio",
    "embedding":      "/mnt/nvme/models/Embedding"
  }
}
```

### 2.2 Key scheme
Namespace-separated, `/`-delimited, most-specific-first. Phase 1 defines:

| Key | Meaning |
|---|---|
| `llm` | Any LLM (transformers / gguf / gptq / awq / onnx-as-llm) |
| `llm/gguf` | GGUF quantized LLM (fine, wins over `llm`) |
| `llm/safetensors` | Transformers safetensors/bin LLM (fine, wins over `llm`) |
| `diffusion` | Diffusers pipelines / diffusion LoRAs |
| `audio` | Audio models |
| `embedding` | Feature-extraction / embedding models |

Reserved for Phase 3 (not required, do not implement early):
`audio/whisper`, `audio/piper`, `diffusion/checkpoints`, `diffusion/unet`,
`diffusion/lora`, `diffusion/vae`, `diffusion/clip`, `diffusion/controlnet`,
`llm/gptq`, `llm/awq`, optional `dataset`.

### 2.3 Precedence and fallback
Resolution order for a model download (first match wins):

1. An explicit per-request destination path (`req.localDir`) — unchanged
   existing override.
2. The most specific configured route key, walking from most specific to
   coarse: `llm/gguf` -> `llm` (for a gguf repo); `llm/safetensors` -> `llm`
   (for a transformers repo); then the repo's own category key.
3. Server-global `LocalDir` (flat mode), unchanged.
4. HF cache layout (`CacheDir`), unchanged.

Datasets: unchanged. A dataset uses `LocalDir` or HF cache and is **not**
routed to a model key unless an optional `dataset` key is later added.

Empty or whitespace-only route values are ignored (treated as unconfigured).

---

## 3. `RepoType` -> route-key mapping

Current types (`pkg/smartdl/types.go:34-108`) and detection
(`analyzer.go:417-490`, `specialized.go:415-539`). Mapping below is the
recommended treatment; items marked **[OPEN]** need a maintainer decision and
must not be silently resolved.

| RepoType / condition | Fine key (if configured) | Coarse fallback | Notes |
|---|---|---|---|
| `gguf` | `llm/gguf` | `llm` | Unambiguous. |
| `transformers` | `llm/safetensors` | `llm` | Covers safetensors/bin LLMs. |
| `transformers` + `Task == "feature-extraction"` | `embedding` | `embedding` | **Embedding** — see §3.1. |
| `diffusers` | `diffusion/checkpoints` (Phase 3) | `diffusion` | Model index present. |
| `lora` | `diffusion/lora` (Phase 3) | `diffusion` **[OPEN]** | LLM vs diffusion LoRA — see §3.3. |
| `gptq` | `llm/gptq` (Phase 3) | `llm` | Quantized LLM. |
| `awq` | `llm/awq` (Phase 3) | `llm` | Quantized LLM. |
| `audio` | `audio/whisper` etc. (Phase 3) | `audio` | ASR/TTS. |
| `onnx` | `audio/*` or `llm` **[OPEN]** | fallback (no route) | Format, not capability — see §3.4. |
| `vision` | — | fallback (no route) **[OPEN]** | Not in user tree — see §3.5. |
| `multimodal` | — | fallback (no route) **[OPEN]** | Not in user tree — see §3.5. |
| `generic` / unknown | — | fallback (no route) | Safe default: LocalDir/cache. |
| `dataset` | — | fallback (no route) | Preserve current dataset behavior. |

### 3.1 Embedding
There is no dedicated `RepoType` for embeddings. They arrive as
`TypeTransformers` with `Task == "feature-extraction"` (inferred from
architecture, e.g. `BertModel`, by `inferTaskFromArchitecture`,
`pkg/smartdl/transformers.go:359-398`). Recommendation: derive the route key
in the same place the client already maps analysis output, using
`type == "transformers" && task == "feature-extraction"` -> `embedding`.
Flagged **[OPEN]** only for naming granularity (whether `embedding` is a
top-level key as in the user's tree). Recommended: top-level `embedding`.

### 3.2 Piper
Piper voices are usually `.onnx` + `.onnx.json` with no `config.json`, so
`detectType` returns `TypeONNX` (`analyzer.go:458-463`). Recommendation for
Phase 1: Piper and Whisper both land under coarse `audio` when the repo is
classified `audio`; ONNX TTS/ASR detection is not reliable enough to route
automatically. Finer `audio/piper` vs `audio/whisper` is Phase 3 **[OPEN]**.

### 3.3 LoRA: LLM vs diffusion
`TypeLoRA` is set purely by `adapter_config.json` (`analyzer.go:447-450`) and
does **not** distinguish a text `CAUSAL_LM` adapter from a diffusion adapter.
The user's tree lists LoRA under Diffusion, but a text LoRA routed to the
diffusion folder is wrong. Recommendation: default coarse `diffusion` for
Phase 1 (matches the user's stated tree), and treat base-model/task metadata
refinement as a follow-up. This is **[OPEN]** — a maintainer should confirm
whether the default should instead be `llm` or left un-routed.

### 3.4 ONNX
`TypeONNX` is a file format that can be an audio, vision, or LLM artifact.
`detectType` returns it before the transformers check. Recommendation: **do
not auto-map `onnx` to a route in Phase 1** — leave it un-routed so it falls
back to `LocalDir`. Rationale: mis-routing a Whisper ONNX into LLM would be
worse than leaving it in the shared root. **[OPEN]** for a later TTS/ASR
metadata-based refinement.

### 3.5 Vision / multimodal / generic
Absent from the user's tree. Recommendation: no route, fall back to
`LocalDir`. **[OPEN]** whether to add optional `vision` / `multimodal` keys
later.

---

## 4. How the server learns the type at job-creation time

Options considered:
- **(a)** Client passes a route **key** it derived from its existing analysis;
  server resolves key -> configured path only.
- **(b)** Server runs `smartdl.Analyze` inside `CreateJob` (extra network call).
- **(c)** Server-side heuristics from filters/extensions.

**Recommendation: (a).**

- The UI already calls `GET /api/analyze/{repo}` (`internal/server/api.go:590-624`)
  before a normal download and holds `type`/`task` from `renderSelectableItems`
  (`internal/assets/static/js/app.js`). Reusing that avoids a second, slow,
  rate-limited Hub call.
- The server accepts only a key string and maps it through
  `DownloadRoutes`; the client never supplies a path. This closes the
  security concern instead of widening the existing un-allowlisted
  `req.localDir` (`docs/API.md:100-109`).
- It keeps `internal/server` free of a new analysis dependency in the hot
  job-creation path.
- **(b)** is rejected for Phase 1: it doubles Hub traffic, can fail
  independently of the download, and duplicates work the client already did.
- **(c)** is rejected: extension heuristics duplicate `detectType` and would
  silently mis-handle ONNX, LoRA, and embeddings.

Fallback when no key is sent (raw API users, CLI, or clients that skip
analysis): behave exactly as today (LocalDir / HF cache). Keys for
`mmproj`/`LocalRepo` upstream downloads should be the **same key as the parent
model** so companions land together.

Server-side derivation from `filters`/extensions may be added later as an
optional enhancement behind the same resolution function, but is not Phase 1.

---

## 5. Destination resolution integration point

Primary integration is `JobManager.CreateJob` (`internal/server/jobs.go:342-419`),
which already computes `effectiveLocalDir`, `flat`, and `outputDir`.

Add a resolution helper (e.g. `internal/server/routes.go`):

```go
// resolveRoute returns the configured destination for a route key using the
// most-specific-first precedence in §2.3, or "" when the key is unknown or
// unconfigured.
func resolveRoute(routes map[string]string, key string) string
```

In `CreateJob`:
1. Parse the requested `routeKey` against `cfg.DownloadRoutes`; reject an
   unknown/unconfigured key with `400` rather than treating it as a path.
2. If a route resolves, set `effectiveLocalDir` to that path and `flat = true`
   (same flat/real-file mode as today).
3. Add a `RouteKey string` field to `Job` (`jobs.go:31-42`) for auditability
   and UI display. Do **not** add a second destination field: `LocalDir`
   already carries the effective destination.

Consistency requirements (must hold for resume/retry/restart/companions):
- `job.LocalDir` is persisted in `jobs_state.json` (Job JSON tags,
  `jobs.go:39`; `saveState`/`LoadState`, `jobs.go:171-217`). Because the
  resolved route path is stored in `LocalDir` at creation, resume, retry,
  server restart, and `cleanupPausedJobPartFiles` all reuse the same
  destination with no re-resolution. Re-resolving at run time must **not** be
  introduced, because the config map could change between create and resume.
- `runJob` and the paused-job cleanup both derive `settings.OutputDir` from
  `job.LocalDir` (`jobs.go:1161-1167`, `jobs.go:1256-1265`), so routing flows
  through the existing path unchanged.
- `destinationBase` joins `cfg.OutputDir` with `job.Repo` or `job.LocalRepo`
  (`pkg/hfdownloader/plan.go:333-343`). Because both the parent model and its
  `LocalRepo` (mmproj) downloads share one `job.LocalDir`, setting the same
  route key for the companion keeps them in the same route folder.

Update the dedup guard (`jobs.go:376-387`) to also consider the resolved
destination (or `RouteKey`) so two requests for the same repo routed to
different folders are not collapsed into one job.

---

## 6. Scan roots and disk-free allowlist

New destination leaves must be discoverable and statable:

- `localCacheRoots` (`internal/server/api.go:784-815`) builds the Cache
  browser scan roots. Add the distinct configured route paths as roots
  (source label `"Route"` or `"Local"`). Callers to update:
  `scanLocalCachedRepos` (`api.go:988-994`) and `findLocalCachedRepo`
  (`api.go:1042-1047`). Signature change: add a `routeDirs []string`
  parameter (or pass the config map and flatten it) and dedup as the existing
  helper does.
- This also drives Hub local badges, which read the same root list.
- `/api/diskfree` (`internal/server/search.go:205-248`) validates its `path`
  query param against a fixed allowlist. Add every configured route path to
  `configured` so a settings UI or download modal can request free space for
  a route folder instead of returning `400`.

---

## 7. API and UI changes

### 7.1 API
- `DownloadRequest` (`internal/server/api_types.go:7-26`): add
  `RouteKey string json:"routeKey,omitempty"`. Document that it is a **key**,
  not a path, and that unknown/unconfigured keys are rejected.
- `SettingsResponse` (`api_types.go:44-65`): add
  `DownloadRoutes map[string]string json:"downloadRoutes,omitempty"`.
  Populate it in `handleGetSettings` (`api.go:343-358`).
- `handleUpdateSettings` request struct (`api.go:377-399`): add
  `DownloadRoutes *map[string]string json:"downloadRoutes,omitempty"`; apply
  via copy-on-write inside `withConfig` (`api.go:436-507`) and persist through
  `SaveConfigFile` (`api.go:530-556`). Validate keys against the known key
  scheme (`^[a-z]+(/[a-z]+)?$`) and ignore/clear empty values.
- `ApplyConfigToServer` (`config.go:193-246`): copy `DownloadRoutes` from the
  file into `serverCfg` when CLI/config precedence allows.
- Sync `docs/API.md` settings section (`docs/API.md:124-143`) with the new
  fields and the `routeKey` semantics.

### 7.2 Settings page
- `internal/assets/static/index.html` Storage card (`:689-727`): add a route
  editor (key -> path rows) under the existing storage rows.
- `internal/assets/static/js/app.js`: extend `loadSettings`/`saveSettings`/
  `syncStorageFields` (`:2423-2650`) to read/write `downloadRoutes`.

### 7.3 Download modal
- `app.js` `dlModalLocalDir` (`:985-1020`) and the quant modal
  (`:3380-3425`): prefill a route select from the existing analysis result
  (`type`/`task` -> key via §3 mapping), defaulting to "configured routes
  only" with a clear override. Prefer a route-key selector over a free path
  when routes are configured, so the UI cannot drift from server-side keys.
- Keep a "Default (LocalDir / HF cache)" option that sends no `routeKey`, so
  existing users are unaffected.

---

## 8. Backward compatibility and safety

- **Default unchanged**: with an empty `download-routes` map, `resolveRoute`
  returns `""` and `CreateJob` follows the existing LocalDir/cache path
  exactly. No behavior change for existing users.
- **Server authority**: the server accepts only keys present in
  `cfg.DownloadRoutes`; a path-shaped or unknown value is rejected with `400`.
  The route feature does not broaden the API to arbitrary client paths.
- **Existing `req.localDir` looseness is preserved as-is**, not worsened. A
  separate hardening task may add it to the allowlist; that is out of scope.
- **Path safety**: reapply the existing untrusted-path guards used by
  cache/mirror operations; do not follow route paths outside configured roots
  for delete/rebuild.
- Empty/whitespace route values are ignored; duplicate paths are deduped in
  scan roots.

---

## 9. Phased delivery

### Phase 1 — smallest correct end-to-end slice (server-side, no UI)
1. Config: `DownloadRoutes` in `ConfigFile` + `Config`; `ApplyConfigToServer`
   and `SaveConfigFile`.
2. `resolveRoute` helper + `RouteKey` on `Job`; wire resolver into `CreateJob`
   with key validation; extend dedup.
3. Scan roots: add route dirs to `localCacheRoots` and its callers.
4. Disk-free allowlist addition.
5. Unit tests (§10).
6. `docs/API.md` settings + `DownloadRequest` update.

Deliverable: API users can POST a `routeKey` and downloads land in the
configured folders; Cache browser and disk-free work; defaults unchanged.

### Phase 2 — UI
- Settings route editor (index.html + app.js).
- Download/quant modal route prefill and selector.
- Manual browser smoke test.

### Phase 3 — docs + optional finer splits
- `CHANGELOG.md`, `README.md:39` wording.
- Optional fine keys: `audio/whisper`, `audio/piper`,
  `diffusion/{unet,lora,vae,clip,controlnet}`, `llm/{gptq,awq}`.
- Optional `dataset` key.
- Optional server-side ONNX TTS/ASR refinement.

---

## 10. Test and verification plan

Unit tests (Phase 1):
- `resolveRoute`: most-specific-wins (`llm/gguf` beats `llm`), coarse-only,
  unconfigured key -> `""`, empty values ignored, unknown key.
- `CreateJob` destination selection: `routeKey` -> configured path sets
  `LocalDir`/`flat`; unknown key -> `400`; no key + `LocalDir` set -> existing
  flat path; no key + no `LocalDir` -> HF cache; `req.localDir` still wins.
- `localCacheRoots`: configured route paths appear and are deduped;
  `scanLocalCachedRepos`/`findLocalCachedRepo` see a repo in a route folder.
- `handleDiskFree`: configured route path accepted; unconfigured path -> `400`.
- Settings round-trip: POST `downloadRoutes` persists to and reloads from
  `ConfigFile` (JSON and YAML).
- Dedup: same repo routed to two different keys is not collapsed.

Regression (must remain unchanged):
- `LocalDir`-only mode produces `<LocalDir>/<owner>/<repo>`.
- HF-cache-only mode is unaffected.
- Datasets are not routed.
- Pause/resume/retry/restart reuse the persisted `job.LocalDir`.

Commands:
- Minimum: `go test ./internal/server ./pkg/hfdownloader ./pkg/smartdl`
- Cross-package: `go test ./...`
- Concurrency (jobs/config changes): `go test ./... -race`
- `gofmt -w` changed Go files.
- Manual: configure routes, download an LLM (gguf + safetensors), an audio
  model, and an embedding model; confirm folders, Cache browser visibility,
  disk-free for a route folder, and unchanged behavior with routes empty.
- Docs-only edits state tests were not run.

---

## 11. Risks, regressions, open questions

Risks / regressions
- Scan-root growth: many route paths enlarge the Cache browser scan; dedup and
  `SkipSpecial` handling must be preserved.
- Settings concurrency: route map mutation must follow the existing
  copy-on-write + generation pattern in `handleUpdateSettings`
  (`api.go:430-528`) to avoid racing in-flight jobs.
- Dedup semantics change is user-visible (two jobs vs one); call it out in the
  PR body.
- Stored jobs created before this change have no `RouteKey`; they remain valid
  because `LocalDir` is already persisted.

Open questions for the maintainer
1. LoRA default folder: `diffusion` (matches user tree) vs `llm` vs un-routed.
2. ONNX auto-routing: leave un-routed in Phase 1 vs metadata-based later.
3. Vision/multimodal: add keys later or permanently fall back.
4. `embedding` as a top-level key (as proposed) vs under `llm`.
5. Should the existing `req.localDir` path override be brought under the same
   allowlist as part of this work, or tracked separately.

---

## 12. Likely files / modules

- `internal/server/config.go` — `ConfigFile.DownloadRoutes`, `ApplyConfigToServer`
- `internal/server/server.go` — `Config.DownloadRoutes`
- `internal/server/jobs.go` — `Job.RouteKey`, `CreateJob`, dedup
- `internal/server/routes.go` (new) — `resolveRoute`
- `internal/server/api_types.go` — `DownloadRequest.RouteKey`, `SettingsResponse.DownloadRoutes`
- `internal/server/api.go` — settings get/update, `localCacheRoots` + callers
- `internal/server/search.go` — disk-free allowlist
- `pkg/hfdownloader/plan.go` — unchanged (consumes `job.LocalDir`); verify only
- `internal/assets/static/index.html`, `internal/assets/static/js/app.js` — Phase 2
- `docs/API.md`, `README.md`, `CHANGELOG.md` — docs sync
- tests alongside the above packages

Recommended implementation role: `@build` (bounded, non-UI) for Phase 1;
`@ui-implementer` for Phase 2; review by `@reviewer` before PR.
