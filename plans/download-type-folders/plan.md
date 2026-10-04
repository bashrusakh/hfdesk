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
- The dashboard shows **human labels** for these settings, never the keys;
  keys are an internal config/advanced-API detail. See §2.3 and §7.2.

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

Design principle: **keys are internal; labels are user-facing.** The config
file and the server store a map keyed by a closed, server-defined set of route
keys. The dashboard UI never shows a key — it renders human labels bound to
those keys (§2.3). A key is never free-form user input; the server enforces the
closed set — the settings save drops unknown keys and the download API rejects
them with 400.

### 2.1 Field name and shape
Add one field to both the persisted file and the in-memory server config:

- `internal/server/config.go` `ConfigFile` (`:41-56`): add
  `DownloadRoutes map[string]string` with tag
  `json:"download-routes,omitempty" yaml:"download-routes,omitempty"`.
  A map marshals cleanly under both the JSON and YAML branches of
  `SaveConfigFile` (`config.go:177-182`).
- `internal/server/server.go` `Config` (`:22-53`): add the same
  `DownloadRoutes map[string]string`.

Example `hfdesk.json` / `hfdesk.yaml` — this is the internal/advanced format;
end users normally set these values through the labeled Settings fields in
§7.2, not by editing JSON:

```json
{
  "local-dir": "/mnt/nvme/models",
  "download-routes": {
    "llm/gguf":        "/mnt/nvme/models/LLM/GGUF",
    "llm/safetensors": "/mnt/nvme/models/LLM/Safetensors",
    "audio":           "/mnt/nvme/models/Audio",
    "diffusion":       "/mnt/nvme/models/Diffusion",
    "embedding":       "/mnt/nvme/models/Embedding"
  }
}
```

### 2.2 Closed key set (internal only)
Namespace-separated, `/`-delimited, most-specific-first. The server defines
and owns this set; it is not user-extensible. Phase 1 keys:

| Key (internal) | Meaning |
|---|---|
| `llm` | Any LLM (gptq/awq/other); coarse fallback for `llm/*` |
| `llm/gguf` | GGUF quantized LLM (fine, wins over `llm`) |
| `llm/safetensors` | Transformers safetensors/bin LLM (fine, wins over `llm`) |
| `diffusion` | Diffusers pipelines / diffusion LoRAs |
| `audio` | Audio models |
| `embedding` | Feature-extraction / embedding models |

Reserved for Phase 3 (not required, do not implement early):
`audio/whisper`, `audio/piper`, `diffusion/checkpoints`, `diffusion/unet`,
`diffusion/lora`, `diffusion/vae`, `diffusion/clip`, `diffusion/controlnet`,
`llm/gptq`, `llm/awq`, optional `dataset`.

### 2.3 User-facing label table (dashboard only)
The UI renders one labeled path field per key and maps label<->key in a single
fixed table. The user never sees `llm/gguf`, `llm`, etc.

| Label shown to the user | Bound internal key | Field state |
|---|---|---|
| `GGUF models` | `llm/gguf` | path or empty |
| `Safetensors / LLM` | `llm/safetensors` | path or empty |
| `Audio` | `audio` | path or empty |
| `Diffusion` | `diffusion` | path or empty |
| `Embedding` | `embedding` | path or empty |

Every field is empty by default and empty means "use current behavior"
(server-global `LocalDir` flat layout, else HF cache). The coarse `llm` key
remains a valid internal/config key and resolution fallback, but is not
surfaced by the Phase 2 UI — see §11 [OPEN] on whether to add an
"Other LLM" field later.

### 2.4 API shape decision — recommend (a), the keyed map
Two options were considered:

- **(a)** Config and API keep the keyed map; the UI maps labels<->keys using
  the fixed table in §2.3.
- **(b)** The API accepts a fixed set of labeled fields (one struct field per
  label), mapping to keys server-side.

**Recommendation: (a).** Justification:

- Single canonical representation: the config file, the in-memory `Config`,
  the `SettingsResponse`, and the `POST /api/settings` body all use the same
  keyed map. There is no parallel labeled schema to keep in sync, and no
  two-way translation layer in the server.
- Forward-compatible: adding a finer split (e.g. `audio/whisper`,
  `diffusion/unet`) is an additive key plus one UI label row, with no API or
  struct change. Option (b) would require a new field and migration for every
  split, duplicating the same information.
- The closed set is still enforced server-side: the settings save sanitizes the
  map (keys outside the known set are dropped, never stored or echoed), and the
  download API rejects an unknown routeKey with `400`, so keys are never
  free-form user input.
- "Never show keys" is a **UI rule**, satisfied by the §2.3 label table. Keys
  are the internal/advanced format; `docs/API.md` documents them for API
  consumers, who are technical users rather than the dashboard user.
- Alternative (b) remains a possible later hardening if the maintainers want
  zero key exposure even over the HTTP API; it can be layered on top of the
  same keyed storage without a config migration. Flagged as [OPEN] in §11.

Where keys appear, the split is:
- **Internal / config / advanced API**: `config.go`, `server.go`, the
  `download-routes` file field, and the `downloadRoutes` API fields.
- **User-facing**: only the §2.3 labels in `index.html` / `app.js`.

### 2.5 Precedence and fallback
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

### 2.6 Path validation and save flow
Match the current settings behavior; do not add stricter validation than the
existing fields.

- **Normalization**: apply `cleanPathList` semantics to each route value —
  `strings.TrimSpace`, `filepath.Clean`, drop empties
  (`internal/server/api.go:762-782`). Reuse the same helper style rather than
  inventing a new normalizer.
- **No directory creation, no existence requirement**: `POST /api/settings`
  today does not validate, normalize beyond trim/`Clean`, or create
  directories, and a bad path persists and only fails at download time
  (`internal/server/api.go:377-399`, `:436-507`, `:509-563`). Keep that for
  route paths: do **not** silently `MkdirAll`, and do not hard-fail a save for
  a non-existent path.
- **Recommended light warning**: optionally stat each non-empty route path
  and return a non-blocking `warnings` array (e.g. "path does not exist yet")
  in the success response so the UI can show it. This must never fail the
  request and never create the directory. If maintainers prefer zero new
  behavior, omit the warning — [OPEN] in §11.
- **Key handling on settings save (sanitize, not reject)**: `POST /api/settings`
  drops every key outside the closed set (§2.2) via the shared
  `sanitizeDownloadRoutes` helper (`internal/server/routes.go`) instead of
  returning `400`; valid keys are kept, an empty value clears its key, and the
  save still succeeds (`200`). Rationale: the dashboard echoes the loaded map on
  save, so a legacy/hand-edited key must not fail the whole save. The same
  helper is applied at the config-load boundary in `ApplyConfigToServer`
  (`config.go`), so `GET` never advertises an unknown key. An unknown key's
  value is never interpreted as a destination path.
- **Key validation on download (strict)**: `/api/download` keeps strict
  semantics — an unknown `routeKey` is still rejected with `400` before any job
  starts, EXCEPT when `dataset: true`, where `routeKey` is ignored entirely
  (datasets are never routed; §2.5).
- Existing settings fields keep their current (unchanged) validation.

### 2.7 Preset (one-click folder fill)
An optional convenience control on the Settings page:

- One root path input plus a button (working label e.g. "Fill type folders").
- On click, fills the five labeled fields (§2.3) with
  `<root>/LLM/GGUF`, `<root>/LLM/Safetensors`, `<root>/Audio`,
  `<root>/Diffusion`, `<root>/Embedding`. Nothing is typed by hand.
- **Form-only**: it only fills the form. The user still reviews and presses
  Save; the normal settings save flow persists the map. It does **not** create
  directories on disk, and it does not save by itself. Recommended: no
  filesystem mutation at all; whether a future "create folders now" action is
  wanted is [OPEN] in §11.
- An empty root is a no-op with a toast.
- The preset writes into the same labeled fields a user could edit manually;
  it is not a separate storage path.

---

## 3. `RepoType` -> route-key mapping (internal resolution only)

Current types (`pkg/smartdl/types.go:34-108`) and detection
(`analyzer.go:417-490`, `specialized.go:415-539`). This table is **internal**:
it maps to route keys, which the user never sees. The user only sees the
§2.3 labels. Items marked **[OPEN]** need a maintainer decision and must not
be silently resolved.

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

Every key column above is internal. The corresponding dashboard label (if any)
is defined once in the §2.3 table; no other place in the UI may render a key.

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

User-facing vs internal at this boundary:
- The **route key is internal**. In Phase 1 it is sent over the API by
  technical/raw clients. In Phase 2 the dashboard derives the key from the
  analysis result via the §3 mapping; the user never sees or types it. If a
  user opens the download modal, they see a labeled destination choice
  (e.g. "GGUF models"), and the app maps that label to its key.
- A raw API client that sends no key gets today's behavior exactly.

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
   Exception: `dataset: true` ignores `routeKey` entirely (blanked before
   validation), so a dataset is never routed (§2.5).
2. If a route resolves, set `effectiveLocalDir` to that path and `flat = true`
   (same flat/real-file mode as today).
3. Add a `RouteKey string` field to `Job` (`jobs.go:31-42`) for auditability
   and UI display. Do **not** add a second destination field: `LocalDir`
   already carries the effective destination. `Job.RouteKey` is internal and
   must not be rendered as a raw key in the UI; show the §2.3 label or the
   destination folder instead.

Note on the flat/local mode switch: `Config.LocalDir` non-empty globally puts
the server in flat/local mode (`internal/server/server.go:29-33`; the
`flat := effectiveLocalDir != ""` branch in `jobs.go:360-369`). Setting a
resolved route path as the job's `effectiveLocalDir` intentionally reuses that
same flat-file branch, because route destinations are real-file folders like
`/mnt/nvme/models/LLM/GGUF`. A route path is therefore a per-job use of flat
mode and must not be confused with the server-wide `LocalDir` mode switch —
the server-global `LocalDir` setting is unchanged, and route config is
independent of it (see §7.2 on the layout toggle).

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
  `configured` so the settings page or download modal can request free space
  for a route folder instead of returning `400`. `path` here is a filesystem
  path, not a route key; keys are never sent to this endpoint.
- User-facing note: the Cache browser group label for a route folder may be
  shown as a human label (e.g. the folder name or "Type folders"), never as an
  internal key.

---

## 7. API and UI changes

### 7.1 API
- `DownloadRequest` (`internal/server/api_types.go:7-26`): add
  `RouteKey string json:"routeKey,omitempty"`. Document that it is an
  **internal key from the closed set** (§2.2), not a path, and that
  unknown/unconfigured keys are rejected with `400` (except when
  `dataset: true`, which ignores it). This is an advanced/raw
  API field; the dashboard hides it behind labels.
- `SettingsResponse` (`api_types.go:44-65`): add
  `DownloadRoutes map[string]string json:"downloadRoutes,omitempty"`.
  Populate it in `handleGetSettings` (`api.go:343-358`). The API exposes the
  keyed map (option (a), §2.4); the dashboard maps keys to labels.
- `handleUpdateSettings` request struct (`api.go:377-399`): add
  `DownloadRoutes *map[string]string json:"downloadRoutes,omitempty"`; apply
  via copy-on-write inside `withConfig` (`api.go:436-507`) and persist through
  `SaveConfigFile` (`api.go:530-556`). Sanitize per §2.6 using the shared
  `sanitizeDownloadRoutes` helper: keys outside the closed set are dropped (not
  rejected), valid keys kept, empty values cleared; the save still succeeds.
- `ApplyConfigToServer` (`config.go:193-246`): copy `DownloadRoutes` from the
  file into `serverCfg` when CLI/config precedence allows.
- Sync `docs/API.md` settings section (`docs/API.md:124-143`) with the new
  fields, the closed key set, and the `routeKey` semantics.

### 7.2 Settings page (labeled destination fields)
Replace the rejected "route-key editor" idea. The user sees labeled path
fields, never keys.

- `internal/assets/static/index.html` Storage card (`:699-725`): add a "Type
  folders" group with five rows, each a label plus a plain text path input
  (same pattern as the existing `localDirInput` / `localScanDirs` text inputs):

  | Label | Bound key (hidden) |
  |---|---|
  | `GGUF models` | `llm/gguf` |
  | `Safetensors / LLM` | `llm/safetensors` |
  | `Audio` | `audio` |
  | `Diffusion` | `diffusion` |
  | `Embedding` | `embedding` |

  Placeholders show an example path (e.g. `/mnt/nvme/models/LLM/GGUF`).
  Each field is empty by default; empty means "use current behavior".
- **No native folder picker.** The frontend cannot hand a server-side
  absolute path to a native directory dialog (the server is remote/headless;
  there is no such picker anywhere in the current UI). Do not promise
  desktop-style "Browse". A text input with placeholder plus the preset
  (§7.2.1) is the working pattern.
- **Preset control** (optional): a root path input plus a button (working
  label e.g. "Fill type folders") that fills the five fields with
  `<root>/LLM/GGUF`, `<root>/LLM/Safetensors`, `<root>/Audio`,
  `<root>/Diffusion`, `<root>/Embedding`. Form-only, per §2.7: nothing is
  created on disk, and the user still presses Save.
- `internal/assets/static/js/app.js` (`loadSettings` `:2423-2486`,
  `saveSettings` `:2574-2625`, `syncStorageFields` `:2627-2637`,
  `resetSettings` `:2639-2664`):
  - `loadSettings`: write `data.downloadRoutes[<key>]` into each labeled
    field via the fixed label<->key table.
  - `saveSettings`: start from the loaded map and apply the labeled fields onto
    it (empty field deletes its key), so a valid non-surfaced key such as `llm`
    survives a dashboard save; never send an empty field as a key path.

#### 7.2.1 Layout-toggle independence (critical)
Current `saveSettings` sends `localDir: ''` whenever
`downloadLayout != 'local'` (`app.js:2585`), i.e. switching to HF cache
**clears** the configured local path. The new route fields are a separate
concern and **must not** be wiped by the HF-cache/local layout toggle:

- Route fields are always included in the `downloadRoutes` body regardless of
  `downloadLayout`.
- `syncStorageFields` continues to show/hide only `cacheDirGroup` and
  `localDirGroup`; the Type-folders group is always visible and unaffected by
  the toggle.
- `saveSettings` must not derive or clear route values from `downloadLayout`.

#### 7.2.2 Reset behavior
`resetSettings` (`app.js:2639-2664`) already clears `localDirInput`,
`localScanDirs`, and `downloadLayout` behind a confirm dialog.
Recommendation: also clear the five Type-folder fields and the preset root
input under the same existing confirm, keeping "reset" consistent. Reset now
resets `state.settings.downloadRoutes` to `{}`, so the next Save clears ALL
route keys (including non-surfaced ones such as `llm`) plus the preset root;
"Reset to defaults" is a full clear, distinct from a normal save which
merges/preserves non-surfaced keys. If a maintainer prefers to preserve route
config across reset, mark that [OPEN] (§11); the default recommendation is to
clear them.

### 7.3 Download modal
- `app.js` `dlModalLocalDir` (`:985-1020`) and the quant modal
  (`:3380-3425`): prefill a **labeled destination choice** derived from the
  existing analysis result (`type`/`task` -> key via §3, then key -> label via
  §2.3). Show labels (e.g. "GGUF models", "Audio") plus a "Default" option;
  never show raw keys or a free path field when routes are configured, so the
  UI cannot drift from server-side keys.
- Keep a "Default (LocalDir / HF cache)" option that sends no `routeKey`, so
  existing users are unaffected.
- Non-selectable types (e.g. audio) also get a labeled "Save to" select
  prefilled from the analysis, so a configured route is reachable for them.
- Dataset analyses render no destination select and send no `routeKey`.
- The modal sends the internal key under the hood; the label mapping stays in
  one place (§2.3 table).

---

## 8. Backward compatibility and safety

- **Default unchanged**: with an empty `download-routes` map, `resolveRoute`
  returns `""` and `CreateJob` follows the existing LocalDir/cache path
  exactly. No behavior change for existing users. Empty labeled fields mean
  exactly this.
- **Server authority**: the server enforces the closed set (§2.2) at both
  boundaries: the settings save drops an unknown/path-shaped key via
  `sanitizeDownloadRoutes` (never stored, never echoed), and `/api/download`
  rejects an unknown `routeKey` with `400` (except `dataset: true`, which is
  never routed). Route keys are never free-form user input, and the UI never
  exposes them.
- **Existing `req.localDir` looseness is preserved as-is**, not worsened. A
  separate hardening task may add it to the allowlist; that is out of scope.
- **Path safety**: reapply the existing untrusted-path guards used by
  cache/mirror operations; do not follow route paths outside configured roots
  for delete/rebuild.
- **No implicit fs mutation**: saving settings, and the preset button, do not
  create directories; a non-existent path persists and fails at download time
  exactly like today's fields.
- Empty/whitespace route values are ignored; duplicate paths are deduped in
  scan roots.

---

## 9. Phased delivery

### Phase 1 — smallest correct end-to-end slice (server-side, no UI)
1. Config: `DownloadRoutes` in `ConfigFile` + `Config`; `ApplyConfigToServer`
   and `SaveConfigFile`.
2. `resolveRoute` helper + `RouteKey` on `Job`; wire resolver into `CreateJob`
   with closed-set key validation; extend dedup.
3. Scan roots: add route dirs to `localCacheRoots` and its callers.
4. Disk-free allowlist addition.
5. Unit tests (§10).
6. `docs/API.md` settings + `DownloadRequest` update.

Deliverable: Phase 1 is exercised **purely via API / config file** using the
internal keys — a technical user or script can set `download-routes` and POST
`routeKey` and downloads land in the configured folders. No dashboard UI yet,
so no key is shown to a normal user. Cache browser and disk-free work; defaults
unchanged.

### Phase 2a — labeled settings fields (no preset)
- "Type folders" group with five labeled path inputs (index.html + app.js).
- Labeled destination choice in the download/quant modal.
- Independent of the layout toggle; reset clears them per §7.2.2.
- Manual browser smoke test.

### Phase 2b — optional preset
- Root input + "Fill type folders" button that fills the form only (§2.7).

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
- Settings sanitize-to-drop: an unknown key (including a path-shaped value) in
  the settings map is dropped, not rejected — `handleUpdateSettings` still
  returns `200`, and `GET`/`ApplyConfigToServer` never surface it
  (`TestAPI_Settings_DropsUnknownRouteKeys`,
  `TestApplyConfigToServer_FiltersUnknownRouteKeys`,
  `TestSanitizeDownloadRoutes` covering trim/`Clean`, empty-drop, and
  unknown-drop); an all-unknown map clears `downloadRoutes`.
- `CreateJob` destination selection: `routeKey` -> configured path sets
  `LocalDir`/`flat`; unknown key on `/api/download` -> `400`
  (`TestAPI_StartDownload_InvalidRouteKey`); `dataset: true` ignores `routeKey`
  (`TestJobManager_CreateJob_DatasetIgnoresRouteKey`); no key + `LocalDir` set
  -> existing flat path; no key + no `LocalDir` -> HF cache; `req.localDir`
  still wins.
- `localCacheRoots`: configured route paths appear and are deduped;
  `scanLocalCachedRepos`/`findLocalCachedRepo` see a repo in a route folder.
- `handleDiskFree`: configured route path accepted; unconfigured path -> `400`.
- Settings round-trip: POST `downloadRoutes` persists to and reloads from
  `ConfigFile` (JSON and YAML).
- Settings normalization: route values are trimmed/Cleaned and empty values
  dropped; no directory is created by a save.
- Dedup: same repo routed to two different keys is not collapsed.

Regression (must remain unchanged):
- `LocalDir`-only mode produces `<LocalDir>/<owner>/<repo>`.
- HF-cache-only mode is unaffected.
- Datasets are not routed.
- Pause/resume/retry/restart reuse the persisted `job.LocalDir`.
- Existing settings fields keep current validation; a bad path still persists
  and fails only at download time.

UI tests / checks (Phase 2):
- Labeled field <-> key mapping round-trips through save/load.
- A normal save merges the labeled fields onto the loaded map and preserves a
  valid non-surfaced key such as `llm`; reset sends an empty map.
- Non-selectable (audio) analyses show a labeled destination select and send its
  key; dataset analyses show none and send no `routeKey`.
- Route fields survive toggling `downloadLayout` cache<->local (no wipe).
- `resetSettings` clears route fields under the confirm.
- Preset fills the five fields from a root and does not create directories.

Commands:
- Minimum: `go test ./internal/server ./pkg/hfdownloader ./pkg/smartdl`
- Cross-package: `go test ./...`
- Concurrency (jobs/config changes): `go test ./... -race`
- `gofmt -w` changed Go files.
- Manual: use the Settings labeled fields (and preset) to configure routes,
  download an LLM (gguf + safetensors), an audio model, and an embedding model;
  confirm folders, Cache browser visibility, disk-free for a route folder,
  route fields surviving a layout toggle, and unchanged behavior with fields
  empty.
- Docs-only edits state tests were not run.

---

## 11. Risks, regressions, open questions

Risks / regressions
- Scan-root growth: many route paths enlarge the Cache browser scan; dedup and
  `SkipSpecial` handling must be preserved.
- Settings concurrency: route map mutation must follow the existing
  copy-on-write + generation pattern in `handleUpdateSettings`
  (`api.go:430-528`) to avoid racing in-flight jobs.
- Layout-toggle wipe: if the new save path naively reuses the `layout === 'local'
  ? localDir : ''` pattern for route values, route config would be cleared when
  HF cache is selected. §7.2.1 makes route fields independent; a test guards it.
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
6. Should the Phase 2 UI also expose an "Other LLM" field bound to the coarse
   `llm` key, or keep the UI to the five labeled fields and leave `llm` to the
   config file / advanced API?
7. Option (b) (labeled API fields, zero key exposure over HTTP) vs option (a)
   (keyed map, keys visible only to API consumers): confirm (a) is acceptable,
   since `docs/API.md` would otherwise document keys.
8. Save-time non-blocking "path does not exist" warning: wanted, or keep
   today's silent no-validation behavior.
9. Reset behavior: clear route fields on reset (recommended) vs preserve them.
10. Preset scope: form-fill only (recommended) vs optionally create the
    subfolders on disk.

---

## 12. Likely files / modules

- `internal/server/config.go` — `ConfigFile.DownloadRoutes`, `ApplyConfigToServer`
- `internal/server/server.go` — `Config.DownloadRoutes`
- `internal/server/jobs.go` — `Job.RouteKey`, `CreateJob`, dedup
- `internal/server/routes.go` (new) — `resolveRoute` + closed key set
- `internal/server/api_types.go` — `DownloadRequest.RouteKey`, `SettingsResponse.DownloadRoutes`
- `internal/server/api.go` — settings get/update, route normalization, `localCacheRoots` + callers
- `internal/server/search.go` — disk-free allowlist
- `pkg/hfdownloader/plan.go` — unchanged (consumes `job.LocalDir`); verify only
- `internal/assets/static/index.html`, `internal/assets/static/js/app.js` — Phase 2
  (labeled fields + label<->key table + optional preset; layout-toggle independence)
- `docs/API.md`, `README.md`, `CHANGELOG.md` — docs sync
- tests alongside the above packages

Recommended implementation role: `@build` (bounded, non-UI) for Phase 1;
`@ui-implementer` for Phase 2; review by `@reviewer` before PR.
