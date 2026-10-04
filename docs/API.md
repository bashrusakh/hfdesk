# HFDesk API

HFDesk exposes a local JSON API under `/api`. The web UI uses this API directly.

## Error Format

Errors return JSON with a legacy flat `error` string and a structured `error_detail` object:

```json
{
  "ok": false,
  "error": "Invalid request body",
  "details": "unexpected EOF",
  "error_detail": {
    "code": "bad_request",
    "message": "Invalid request body",
    "details": "unexpected EOF"
  }
}
```

## Health

```http
GET /api/health
```

Returns server status, version, and timestamp.

## Search

```http
GET /api/search?q=Qwen%20GGUF&sort=downloads&limit=40&filter=gguf
GET /api/search?datasets=true&q=fineweb
```

Searches Hugging Face models or datasets through the configured endpoint/proxy.

## Analyze

```http
GET /api/analyze/{owner}/{repo}?revision=main&dataset=false
```

Returns repository metadata, files, type-specific analysis, selectable download items, and recommended download defaults.

Important response fields:

```json
{
  "repo": "owner/name",
  "is_dataset": false,
  "type": "gguf",
  "files": [],
  "selectable_items": [
    {
      "id": "q4_k_m",
      "label": "Q4_K_M",
      "filter_value": "q4_k_m",
      "recommended": true,
      "files": ["model-Q4_K_M.gguf"]
    }
  ],
  "recommended_filters": ["q4_k_m"],
  "recommended_download": {
    "repo": "owner/name",
    "filters": ["q4_k_m"]
  }
}
```

`recommended_download` is a ready-to-send request body for `POST /api/download`. It replaces the old CLI-command string contract.

## Plan

```http
POST /api/plan
```

Request:

```json
{
  "repo": "owner/name",
  "revision": "main",
  "dataset": false,
  "filters": ["q4_k_m"],
  "excludes": [],
  "exactMatch": false
}
```

Returns the files that would be downloaded without starting a job.

## Download

```http
POST /api/download
```

Uses the same request shape as `/api/plan`. Starts or reuses a download job.

Notes:

- `repo` is required and must be `owner/name`.
- `revision` defaults to `main`.
- `cacheDir` and global `localDir` are server-controlled.
- Per-request `localDir` is accepted only where explicitly supported by server configuration.
- `routeKey` (optional) selects a configured download route (see Settings). It
  is an internal key from the closed set below, never a path. A configured key
  routes the download into its destination folder (flat mode); `localDir`
  takes priority when both are set.
- Unknown `routeKey` values are rejected with `400` before contacting the Hub,
  including previews via `dryRun: true` or `POST /api/plan`. Exception: when
  `dataset: true`, `routeKey` is ignored entirely — datasets are never routed,
  and the request follows the normal `localDir`/HF-cache behavior regardless of
  the key. This keeps a dataset request that previously succeeded (for example
  a client that always sends a `routeKey`) from newly failing with `400`.
- Duplicate active downloads return the existing job; two requests for the same
  repo routed to different destinations are distinct jobs.

## Jobs

```http
GET    /api/jobs
GET    /api/jobs/{id}
DELETE /api/jobs/{id}
POST   /api/jobs/{id}/pause
POST   /api/jobs/{id}/resume
POST   /api/jobs/{id}/dismiss
```

`dismiss` removes terminal jobs from the UI/state. Running or queued jobs must be cancelled first.

## Settings

```http
GET  /api/settings
POST /api/settings
```

Settings include runtime paths, concurrency, verification, endpoint, proxy settings, the HF cache directory, and extra local scan folders.

Token handling:

- `GET` omits `token` when unset, otherwise returns a display mask beginning with `********`. Only tokens longer than four bytes include a last-four-byte suffix; short tokens return just `********`.
- `POST` with `token` omitted, `null`, or any string beginning with `********` preserves the active credential and the stored credential. Stale masks are preserve-only too; the prefix is reserved and cannot be a credential.
- An explicitly authored nonempty `token` sets both active and stored credentials, even if it equals a runtime override. `"token": ""` explicitly clears both.
- Other settings updates never implicitly persist a runtime-only token (for example one supplied with `--token`) or overwrite a different file token. Startup precedence is unchanged; startup overrides can restore an active token after a restart even when the stored token was cleared.

Settings updates still apply in memory when file read/parse, permission, or write operations fail. The successful HTTP response then contains `Settings updated (warning: could not persist to config file)`, not `Settings saved`. Within the running process, the latest explicit token set/clear intent is retained for later saves, including when a concurrent update skips an older request's persistence. Fix the underlying config problem and save again; a failed save is not durable across restart. Existing selected paths, JSON/YAML formats, and config symlinks are retained. Saved files use `0600` permissions on Unix; explicitly stored credentials remain plaintext.

Storage fields:

```json
{
  "cacheDir": "I:\\huggingface",
  "localDir": "D:\\Models",
  "localScanDirs": ["D:\\Models", "I:\\LM Studio\\models"],
  "downloadRoutes": {
    "llm/gguf": "D:\\Models\\LLM\\GGUF",
    "llm/safetensors": "D:\\Models\\LLM\\Safetensors",
    "audio": "D:\\Models\\Audio",
    "diffusion": "D:\\Models\\Diffusion",
    "embedding": "D:\\Models\\Embedding"
  }
}
```

`cacheDir` controls where HF cache-layout downloads are written. When `localDir` is set, downloads use real files under `<localDir>/<owner>/<model>`, which matches LM Studio-style model roots. `localScanDirs` are additional model-management roots scanned as `<owner>/<model>` folders for the Cache browser and local badges in Hub search results; repos found there can be deleted from the Cache browser.

`downloadRoutes` is an opt-in map from an internal route key to a destination directory. When a download request sends a `routeKey` that resolves in this map, the job is written to that folder instead of `localDir`/HF cache. Route destinations are also scanned by the Cache browser and accepted by `/api/diskfree?path=...`.

The route key set is closed and server-defined. On `/api/settings`, keys
outside the closed set are ignored and dropped — they never cause a `400` and
are never stored, so a hand-edited or legacy key in the config file is filtered
out on load and is not echoed back by `GET /api/settings`. On `/api/download`,
an unknown `routeKey` is still rejected with `400` (a selector must not silently
become a no-op), except for the `dataset: true` case noted above:

- `llm/gguf` — GGUF quantized LLMs (falls back to `llm`)
- `llm/safetensors` — Transformers safetensors/bin LLMs, including GPTQ/AWQ-quantized models detected via `quantize_config.json` (falls back to `llm`)
- `llm` — any other LLM (internal fallback key)
- `diffusion` — diffusion pipelines / LoRAs
- `audio` — audio models
- `embedding` — feature-extraction / embedding models

Route values are trimmed and `filepath.Clean`ed on save; empty values clear a
key. No directory is created by saving settings.

## Cache

```http
GET    /api/cache
GET    /api/cache/{owner}/{repo}
POST   /api/cache/rebuild
DELETE /api/cache/{owner}/{repo}?type=model&source=Local
```

`DELETE` accepts an optional `source` query parameter that selects which copy of the repo to remove (matched case-insensitively and trimmed):

- `HF cache` (or omitted): deletes the HF hub directory, plus the friendly-view path if present. If the hub directory is gone but the friendly path still exists, that orphan is deleted instead of returning `404`.
- `Friendly view`: deletes the friendly-view directory (and the hub directory when present).
- `Local`: deletes the real `<root>/<owner>/<name>` folder from the local cache root (`localDir`, `localScanDirs`, a configured download-route destination, or the cache dir). Returns `404` when the repo is not found in any local root, or when `type=dataset` is requested (local entries are always models).

Any other `source` value is rejected with `400` before anything is deleted.

`HF cache` and `Friendly view` remove the same storage — the hub directory and its friendly-view projection together — so either label cleans up both. Only `Local` removes a single folder, the exact `<root>/<owner>/<name>` directory.

`DELETE` also accepts an optional `path` query parameter that addresses one exact copy. The server recomputes the allowed copy paths for the repo and deletes only when the requested path equals one of them after normalization; otherwise it returns `400` and deletes nothing. The copy's source decides which safe deleter runs (`HF cache`/`Friendly view` use the hub/friendly logic with containment checks; `Local` uses the local safe delete with root-component symlink checks). When `path` is omitted, the `source`-based behavior above applies.

Without `variant`, `DELETE` removes the **entire copy** at the selected location. A Local whole-copy delete refuses to remove a folder that encloses a configured root (`localDir`, a `localScanDirs` entry, or a `downloadRoutes` destination), returning an error and deleting nothing, because removing the folder would also destroy the nested configured root. Such an enclosing folder may still be listed as a repo (it owns its own weights) and may still support selective delete; only the whole-folder `RemoveAll` is refused.

`DELETE` additionally accepts an optional `variant` query parameter that performs a **selective (per-artifact) delete**: it removes exactly the server-computed file set of one artifact (for example one GGUF quantisation) instead of the whole copy. `variant` is orthogonal to `source` and `path` and is processed for a resolved `Local` copy or an `HF cache`/`Friendly view` copy.

- The `variant` token is validated server-side against the copy's own detected artifacts; a token the copy does not advertise is rejected with `400` and nothing is deleted. The server never trusts an arbitrary token.
- For a `Local` GGUF copy the allowed tokens are the detected quantisation labels (e.g. `Q4_K_M`, `UD_Q6_K`). All files of the quant are removed together, so split/sharded quants (`...-00001-of-00002.gguf`) lose every shard. Matching is anchored to the file's detected quantisation label, so `mmproj` companions and unrelated files are never selected.
- For a `HF cache`/`Friendly view` copy the `variant` is mapped through the copy's `hfd.yaml` per-file manifest (`files[].name`); the matching blob, snapshot links, and friendly-view links are removed. When no usable manifest exists, selective delete is refused with `400` rather than guessing.
- For a Local non-GGUF copy without provenance (for example a `safetensors`/diffusers folder), selective delete is refused with `400` ("selective delete needs provenance"); only an entire-copy delete is available.
- After deleting, directories that became empty are pruned. The copy directory itself is removed only when it is fully empty; files that did not match the variant are never touched.
- The response reports the truthful counts, for example `{"success": true, "variant": "Q4_K_M", "deletedFiles": 2, "deletedBytes": 12345}`. If some matched files could not be removed, the response adds `cleanupIncomplete`/`cleanupWarnings` as above.

Whole-copy delete keeps working unchanged for callers that do not pass `variant`.

When `source` is omitted and no matching HF-cache entry exists, the handler falls back to the local roots so existing clients can still delete locally stored repos.

A successful `DELETE` normally responds with `{"success": true, "message": "..."}`. When the primary copy was removed but a companion cleanup step (the friendly-view projection) could not be completed — for example because the friendly-view directory is a real folder rather than a proven projection, or because removing it failed — the response keeps the same `200` success status and adds:

```json
{
  "success": true,
  "message": "Deleted owner/name from cache",
  "cleanupIncomplete": true,
  "cleanupWarnings": ["friendly view is not a whole-folder projection of this repository; left in place"]
}
```

The primary deletion still succeeded; clients that can warn should surface `cleanupWarnings`. The friendly view is only deleted when it is a proven whole-folder projection of this exact repo, and is left in place otherwise.

Cache entries may come from:

- `HF cache`
- `Friendly view`
- `Local`

Under a raw cache root (the cache dir used directly as a local root), `Local` entries whose owner directory is one of `hub`, `models`, `datasets`, `blobs`, `snapshots`, or `refs` are skipped, so listed and deleted entries agree and HF-cache internals are never treated as deletable repos.

`GET /api/cache/{owner}/{repo}` describes the primary copy in its top-level fields and additionally returns a `copies` array enumerating every distinct deletable physical location for the repo. The first entry is the HF cache (or an orphan `Friendly view` when the hub directory is gone), followed by one `Local` entry per local root that contains the repo. Each entry has:

```json
{
  "source": "HF cache",
  "path": "/home/user/.cache/huggingface/hub/models--owner--name",
  "size": 12345,
  "sizeHuman": "12.1 KiB",
  "fileCount": 4,
  "selectiveVariants": ["Q4_K_M", "Q8_0"]
}
```

`selectiveVariants` lists the artifact tokens this copy supports for selective deletion with `DELETE ...&variant=<token>`: the detected GGUF quantisation labels for a Local copy, or the `hfd.yaml` filename tokens for an `HF cache`/`Friendly view` copy. It is omitted (or empty) when the copy cannot support selective delete — a Local non-GGUF folder without provenance, or an HF/friendly copy with no manifest — in which case the UI offers only an entire-copy delete.

`Friendly view` is not listed alongside the HF cache entry because it is the same underlying storage; it appears only as an orphan when the hub directory is absent. The `copies` key is always present in a current server's response, possibly as an empty `[]` when the repo has no deletable copies; a missing key means an older server and is why the UI keeps a legacy fallback delete button.

`downloadStatus` is one of:

- `complete`
- `filtered`
- `unknown`

## Mirror

```http
GET    /api/mirror/targets
POST   /api/mirror/targets
DELETE /api/mirror/targets/{name}
POST   /api/mirror/diff
POST   /api/mirror/push
POST   /api/mirror/pull
```

Mirror operations compare and synchronize cache roots to configured targets.

## History and Disk

```http
GET /api/history
GET /api/diskfree?path=/path/to/cache
POST /api/diskfree
```

`GET /api/diskfree` defaults to global `localDir`, then configured `cacheDir`,
then the default HF cache directory. An explicit `path` must match a configured
directory (including download route directories).

`POST /api/diskfree` previews the effective download destination using optional
`routeKey`, `localDir`, and `dataset` fields from the download request. `repo` is
not required; other download fields are ignored. Resolution is identical to job
creation: explicit `localDir` > configured route (fine key, then parent) > global
`localDir` > configured/default HF cache. Datasets ignore `routeKey`; unknown
model selectors return `400`, even with an explicit `localDir`. An explicit
`localDir` is accepted as on `POST /api/download`, independently of GET's browsing
allowlist. The response is `{ "path": "...", "free": 123, "total": 456 }` with
byte counts; stat failures return `500`. This read-only check uses the nearest
existing ancestor for a not-yet-created folder, creates no directories or jobs,
and does not contact the Hub. It is advisory: settings and available space may
change before creation; each job's destination remains frozen at creation.

## WebSocket

```http
GET /api/ws
```

Messages use:

```json
{
  "type": "job_update",
  "data": {}
}
```

Known message types:

- `job_update`
- `event`
- `status`
