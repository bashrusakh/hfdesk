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
- Duplicate active downloads return the existing job.

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

Storage fields:

```json
{
  "cacheDir": "I:\\huggingface",
  "localDir": "D:\\Models",
  "localScanDirs": ["D:\\Models", "I:\\LM Studio\\models"]
}
```

`cacheDir` controls where HF cache-layout downloads are written. When `localDir` is set, downloads use real files under `<localDir>/<owner>/<model>`, which matches LM Studio-style model roots. `localScanDirs` are additional model-management roots scanned as `<owner>/<model>` folders for the Cache browser and local badges in Hub search results; repos found there can be deleted from the Cache browser.

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
- `Local`: deletes the real `<root>/<owner>/<name>` folder from the local cache root (`localDir`, `localScanDirs`, or the cache dir). Returns `404` when the repo is not found in any local root, or when `type=dataset` is requested (local entries are always models).

`HF cache` and `Friendly view` remove the same storage — the hub directory and its friendly-view projection together — so either label cleans up both. Only `Local` removes a single folder, the exact `<root>/<owner>/<name>` directory.

`DELETE` also accepts an optional `path` query parameter that addresses one exact copy. The server recomputes the allowed copy paths for the repo and deletes only when the requested path equals one of them after normalization; otherwise it returns `400` and deletes nothing. The copy's source decides which safe deleter runs (`HF cache`/`Friendly view` use the hub/friendly logic with containment checks; `Local` uses the local safe delete with root-component symlink checks). When `path` is omitted, the `source`-based behavior above applies.

When `source` is omitted and no matching HF-cache entry exists, the handler falls back to the local roots so existing clients can still delete locally stored repos.

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
  "fileCount": 4
}
```

`Friendly view` is not listed alongside the HF cache entry because it is the same underlying storage; it appears only as an orphan when the hub directory is absent.

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
```

`/api/diskfree` defaults to the resolved HF cache directory when no path is provided.

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
