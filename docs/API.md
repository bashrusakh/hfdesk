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

Returns server status, version, timestamp, and the effective configured Hugging Face endpoint.

```json
{
  "status": "ok",
  "version": "1.3.0",
  "time": "2026-10-19T00:00:00Z",
  "endpoint": "https://huggingface.co"
}
```

`endpoint` is normalized server-side: it defaults to `https://huggingface.co`
when no custom endpoint is configured, never ends with a trailing slash, and
preserves any configured path prefix (for example `https://mirror.example/hf`).

## Search

```http
GET /api/search?q=Qwen%20GGUF&sort=downloads&limit=40&filter=gguf
GET /api/search?datasets=true&q=fineweb
```

Searches Hugging Face models or datasets through the configured endpoint/proxy.

## Analyze

```http
GET /api/analyze/{owner}/{repo}?revision=main
GET /api/analyze/{owner}/{repo}?revision=main&type=model
GET /api/analyze/{owner}/{repo}?revision=main&type=dataset
GET /api/analyze/{owner}/{repo}?revision=main&dataset=true
GET /api/analyze/{owner}/{repo}?revision=main&dataset=false
```

With no `type` or `dataset` parameter, analyze auto-detects whether the
repository is a model or a dataset. `dataset=false` keeps the same
auto-detection behavior for backwards compatibility.

Returns repository metadata, files, type-specific analysis, selectable download items, and recommended download defaults.

`type` is the explicit namespace selector and takes precedence over the legacy
`dataset` parameter:

- `model`: force model analysis. The dataset namespace is not probed.
- `dataset`: force dataset analysis. The model namespace is not probed.

`type` values are matched exactly (no trimming or case folding); any other
value, including a repeated parameter, returns `400` with an
`Invalid repository type` error that lists the accepted values.
`type=model&dataset=true` is contradictory and returns `400`, while
`type=dataset&dataset=true` is consistent and selects the dataset namespace.
When `type` is present, `dataset=false` does not override it.

The legacy `dataset` parameter keeps its pre-#135 semantics when `type` is
absent:

- absent: auto-detect the repository namespace (model first, then dataset). If the same name exists as both, the response is `{"needsSelection": true, "options": ["model", "dataset"]}` instead of repository analysis.
- `true`: force dataset analysis. The model namespace is not probed.
- `false`, any other present value, or an empty value: auto-detect. In particular, `dataset=false` does not force the model namespace; use `type=model` for an explicit model selection.

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
  "recommended_filters": ["./model-Q4_K_M.gguf"],
  "recommended_download": {
    "repo": "owner/name",
    "filters": ["./model-Q4_K_M.gguf"],
    "exactMatch": true
  }
}
```

`recommended_download` is a ready-to-send request body for `POST /api/download`. It replaces the old CLI-command string contract. GGUF recommendations use exact artifact selectors from `selectable_items[].files` rather than the legacy quant token, and set `exactMatch: true`: root-level paths are prefixed with `./`, while nested paths keep their complete repository-relative spelling. Dataset recommendations also set `exactMatch: true`, but use the group's `filter_value` as the selector. Other model recommendations keep their existing filter values and leave `exactMatch` unset. Exactly one non-MTP GGUF quantization artifact is recommended: the first `Q4_K_M`/`UD_Q4_K_M` in the analyzer's deterministic order, or otherwise the first non-MTP quality>=4 artifact under 10 GiB. A preferred local `mmproj` companion remains separately recommended and is bundled into the same request.

`recommended_download` targets exactly one repository (`repo`, the analyzed repository). A recommended item whose `upstream_repo` is set is an mmproj companion discovered in an upstream base-model repository; its `files` paths are relative to that upstream repository, so they are omitted from `recommended_filters` and `recommended_download`. Download such a companion through its individual `selectable_items[]` entry, which retains `upstream_repo` and the exact upstream `files`. Recommended items from the analyzed repository are still included. If every recommended item is upstream, `recommended_download` is omitted.

### Model/dataset selection

If the repository exists as both a model and a dataset, auto-detection returns
HTTP 200 with a selection request instead of analyzing either namespace:

```json
{
  "needsSelection": true,
  "repo": "owner/name",
  "message": "This repository exists as both a model and a dataset. Please select which one you want to analyze.",
  "options": ["model", "dataset"]
}
```

Select the namespace explicitly:

- `type=model` forces a model-only analysis. The dataset namespace is not
  probed, so a dual-type repository does not return `needsSelection` again.
- `type=dataset` forces a dataset analysis. The model namespace is not probed.
- `dataset=true` without `type` forces a dataset analysis.
- Omitting both parameters keeps auto-detection (including the
  `needsSelection` response above).

`type` accepts only the exact values `model` and `dataset`. A contradictory
legacy selector (`type=model&dataset=true`) returns HTTP 400 with error code
`bad_request`, while `type=dataset&dataset=true` is consistent and selects the
dataset namespace. Any other `type` value, including an unknown, repeated, or
case-mismatched value, returns HTTP 400.

The selected model or dataset path then runs the same gated-access auth-check
described in [Gated repositories](#gated-repositories), so a gated dual-type
repository returns the structured HTTP 403 response for the selected
namespace.

### Gated repositories

When Hugging Face reports that read access requires accepting a repository
agreement (`X-Error-Code: GatedRepo`), analyze returns HTTP 403 with a
structured error instead of a partial analysis:

```json
{
  "ok": false,
  "error": "Gated repository",
  "details": "Configure a Hugging Face token in Settings, accept the repository agreement or request access on the repository page, then retry.",
  "error_detail": {
    "code": "gated_repo",
    "message": "Gated repository",
    "details": "Configure a Hugging Face token in Settings, accept the repository agreement or request access on the repository page, then retry."
  },
  "gated": true,
  "repo": "owner/name",
  "repo_type": "model",
  "accept_url": "https://huggingface.co/owner/name",
  "has_token": false
}
```

- `repo_type` is `"model"` or `"dataset"`.
- `accept_url` is the repository page on the configured endpoint
  (`{endpoint}/{repo}` for models, `{endpoint}/datasets/{repo}` for datasets)
  where the agreement can be accepted or access requested.
- `has_token` only reports whether a token is configured. The token value is
  never returned.
- When a token is configured, the guidance explains that the token does not
  currently have access and to check its gated-repository permission.

The backend probes `GET {endpoint}/api/{models|datasets}/{repo}/auth-check`
after resolving or explicitly selecting the repository type. Mirrors that do
not implement the endpoint (404), transient/server failures, network errors, and
non-gated responses do not block analysis; the existing tree/metadata analysis
is used instead.

For model repositories, filtered selection retains every non-LFS file as a
companion, including tokenizer assets and files outside the selected directory;
this is not a size-limited or filename-whitelist policy. Unmatched LFS files
remain excluded, and explicit excludes take precedence. A GGUF-only
quantization selection is the exception: it keeps only matching GGUF/mmproj
files and directly matched files such as a matching README, with no automatic
config, tokenizer, vocabulary, or unmatched README fallback. Explicitly
selecting one of those files remains supported without changing GGUF-only mode;
only positive evidence of another model payload (for example a matched
Safetensors/ONNX/weight-BIN file) selects the ordinary mixed-format behavior.
Unknown file roles do not broaden selection. Unfiltered downloads are
unchanged. Existing path, directory, and extension filters keep their usual
matching rules. In exact mode, a filter ending in `/` selects descendants of
that directory, not a file whose path is the directory name itself.

Exact filters also support an explicit literal root-basename selector:
`./model-Q4_0.gguf` selects only that root-level repository path. Nested files
must use their complete repository-relative path, such as
`weights/model-Q4_0.gguf`. The `./` form is reserved for exact mode; without
`exactMatch: true`, filters retain their existing substring behavior. This
additive selector does not change legacy unqualified basename/token matching.

For GGUF `selectable_items`, `files` contains raw repository-relative paths for
the complete artifact (including every shard). Use those paths as exact
selection identities: prefix root-level entries with `./` in `filters`, and
send nested entries unchanged. `filter_value` is a legacy human/filter token,
not a unique artifact identity when multiple artifacts share a quant label.
The analyzer's `recommended_download` already contains encoded exact selectors.

For datasets, `/api/analyze/...?...dataset=true` exposes only groups whose
membership can be inferred from conventional split paths/names or
`data/<group>/` paths. Send a group's `filter_value` with `exactMatch: true` to
`/api/plan` or `/api/download`; the planner selects that group's data files and
keeps applicable repository companions. A filter whose complete normalized value
is a recognized group label is a membership selector, not a literal path term:
`train` selects files inferred to belong to `train` (and applicable companions)
even when another path merely contains the spelling `train`. Literal files or
directories with that spelling remain selectable through an explicit path form
such as `./train.csv`, `train/`, `train.csv`, or an extension filter. Default
substring filters still retain literal substring matching for filters that are
not themselves a recognized group label, including when a substring occurs
within a recognized split name. Ambiguous files that could affect a
selected group cause planning to fail with the standard JSON error response,
rather than silently broadening or narrowing the selection. `POST /api/plan`
returns HTTP 400 with error code `ambiguous_selection` for this expected
selection refusal. For normal queued (`dryRun: false`) `POST /api/download`
requests, work is admitted asynchronously; ambiguity is discovered by the job's
later planning step and reported as a failed job, not as an immediate HTTP 400.
With `dryRun: true`, `POST /api/download` returns the planning response, and
the same ambiguity returns HTTP 400 with error code `ambiguous_selection`, as
it does for `POST /api/plan`. Excluding the
ambiguous file resolves that ambiguity. This selection behavior does not claim
to identify every possible dataset file layout; use explicit path/extension
filters where appropriate. A README-only filter does not activate GGUF-only
selection.

Quantized models expose a `quantized` projection in the response (also for
quantization-only repositories that have a root `quantization_config.json` but
no root architecture config):

```json
"quantized": {
  "method": "exl3",
  "method_description": "EXL3 - ExLlamaV3 quantization",
  "bits": 0,
  "head_bits": 16,
  "expert_bits_min": 3,
  "expert_bits_max": 4,
  "mixed_precision": true
}
```

`method` is the literal `quant_method` from the repository's quantization
config. `bits` is the declared bit width of the quantized tensors and is only
set when the config declares uniform precision and was read completely enough
to establish it and the declared width is exactly representable as the API's
integer field. Fractional or out-of-range declarations leave `bits` at `0`
(they are retained literally under `metadata`, not rounded). `excluded_modules`
lists the modules the config excludes from
quantization (`modules_to_not_convert`). When `excluded_modules` is non-empty,
part of the model is not quantized at that width, so `bits` is not a model-wide
width and consumers must present the excluded modules alongside it. Exclusions
alone are not mixed precision: `mixed_precision` stays `false` and `bits` stays
set. When `mixed_precision` is true, `bits` is `0` and must not be read as
model-wide precision: `head_bits` is the declared width for head tensors and
`expert_bits_min`/`expert_bits_max` bound the declared per-expert widths
(`expert_bits` / `routed_expert_bits`).

`mixed_precision` is set only when observed declared widths conflict (including
different widths inside expert declarations). Equal local/group/expert widths
without a model-wide `bits` declaration establish neither uniform nor mixed
precision. A cut/null expert declaration is unknown, not mixed, and prevents a
uniform claim; observed variation remains mixed even when the config is
partial. `bits_per_weight` and `routed_expert_bits_avg` are retained as their
own metadata and do not alone establish mixed precision. `bits_per_weight` is
the EXL2 bits per weight (effective precision) and is only ever copied from the
config: nominal `bits` is never presented as bits per weight. Width
declarations without a dedicated field here remain literal under `metadata`.

`config_partial` is `true` when the quantization config exceeded the 10 MiB
metadata bound and only its leading fields were recovered. Precision
declarations past the cut are unknown, so `bits` stays `0` even when the
recovered fields look uniform. Mixed is reported only for variation actually
observed in the recovered widths. A precision declaration whose value was cut at
the bound appears in `metadata` with a `null` value, and the recovered object
carries `"__partial__": true`, so a lost declaration is explicit instead of
silently dropped; declared widths are still presented as declared.

The literal config header remains available under `metadata` (for oversized
configs it is the recovered head with the markers described above). Unknown
methods keep their name with an empty `method_description` and no `backends`.

## Community

```http
GET /api/community/{owner}/{repo}
GET /api/community/{owner}/{repo}?dataset=true&page=0&type=all&status=all
GET /api/community/{owner}/{repo}?num=2
```

Proxies Hugging Face Discussions and Pull Requests read-only through the
configured endpoint, proxy, and token. The token is added server-side and is
never returned to the browser. Both model and dataset repositories are
supported, including gated/private repositories when the configured credentials
allow access.

Query parameters:

- `dataset` – `true` to use the dataset API (case-insensitive; default `false`).
- `num` – when present, return that thread instead of the list page. Must be a
  positive integer.
- `page` – list page, non-negative (default `0`).
- `type` – `all`, `discussion`, or `pull_request` (default `all`).
- `status` – `all`, `open`, or `closed` (default `all`).

`page`, `type`, and `status` apply to the list response only.

List response:

```json
{
  "repo": "owner/name",
  "is_dataset": false,
  "page": 0,
  "count": 289,
  "start": 0,
  "has_more": true,
  "discussions": [
    {
      "num": 294,
      "title": "Question about usage",
      "author": "alice",
      "status": "closed",
      "is_pull_request": false,
      "created_at": "2026-08-17T20:53:39.000Z",
      "num_comments": 1,
      "pinned": false
    }
  ]
}
```

`has_more` is `start + len(discussions) < count`. A deleted or missing upstream
author is normalized to `"deleted"`. Pull requests are distinguished by
`is_pull_request`; `status` can be `draft`, `open`, `closed`, or `merged`.

Thread response (`?num=N`):

```json
{
  "repo": "owner/name",
  "is_dataset": false,
  "num": 2,
  "title": "Upload LICENSE",
  "author": "meg",
  "status": "merged",
  "is_pull_request": true,
  "created_at": "2022-06-02T12:01:38.000Z",
  "events": [
    {
      "id": "6299dba1e0753998f1f6e7df",
      "type": "comment",
      "author": "meg",
      "created_at": "2022-06-03T10:00:01.000Z",
      "body_html": "<p>Sounds good.</p>",
      "edited": false,
      "hidden": false
    },
    {
      "id": "629de1ab4f4ea45707b07143",
      "type": "status-change",
      "author": "meg",
      "created_at": "2022-06-03T10:05:00.000Z",
      "summary": "changed status to merged"
    }
  ]
}
```

Comment `body_html` is rendered from upstream Markdown through the shared README
Markdown and URL-rewrite pipeline, but README frontmatter stripping does not
apply to comments. The result is sanitized with the README sanitizer and then
every `id` and `name` attribute is removed, so comment HTML cannot clobber
application DOM IDs. When raw Markdown is unavailable, upstream HTML goes
through the same URL rewriting, sanitization, and identifier removal. Other
event types carry a human-readable `summary`. Event order matches upstream.

Upstream `400`, `401`, `403`, `404`, and `429` responses keep their status;
network failures, other upstream statuses, and malformed upstream JSON return
`502`.

## README

```http
GET /api/readme/{owner}/{repo}?revision=main&dataset=false
```

Returns the repository README for a revision. `dataset=true` selects the dataset
namespace; `revision` defaults to `main`.

Important response fields:

```json
{
  "repo": "owner/name",
  "revision": "main",
  "path": "README.md",
  "markdown": "# Name\n...",
  "baseRawURL": "https://huggingface.co/owner/name/resolve/main/",
  "baseBlobURL": "https://huggingface.co/owner/name/blob/main/",
  "html": "<h1>Name</h1>"
}
```

`html` is sanitized, URL-rewritten README HTML whose repository links are
classified by the server. It is present on every successful response, including
an empty string when sanitization removed all content. The bundled client
renders only `html`; it does not render raw Markdown, and it treats a successful
response without a string `html` field as a contract error.

`markdown`, `baseRawURL`, and `baseBlobURL` remain in the response for API
compatibility and for consumers that need the raw source or URL bases:
`baseRawURL` is the base for images and `baseBlobURL` is the base for relative
links. The bundled client does not render them.

Internal repository links on the configured endpoint — repository roots and
single-revision `/tree/{revision}` views — carry `data-hf-repo`,
`data-hf-repo-type`, and, for tree views, `data-hf-repo-revision`. File URLs
(`blob`, `resolve`, and `raw`) and unsupported paths remain external and carry
no `data-hf-repo*` attributes. The bundled client uses these server-classified
attributes for in-app Model Details navigation; `href` remains the external
fallback.

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

Returns the files that would be downloaded without starting a job. Each HTTP
response item exposes only `path`, `size`, and `lfs`; `path` is
repository-relative. The response does not include filter-attribution or
destination-prefix fields.

The public Go planning contract is broader. `hfdownloader.PlanRepo` returns a
`*Plan` whose `Items` are `PlanItem`s; the same `Plan` value is passed to
`Settings.OnPlanComplete`. A `PlanItem` also exposes `DestinationSubdir`
(`destinationSubdir`) and `Subdir` (`subdir`). `DestinationSubdir` is the
candidate relative prefix selected by the matched filter; it is populated
independently of `appendFilterSubdir`, and the downloader applies it only when
that job flag is enabled, so Go consumers must check the flag before combining
it with `RelativePath`. `Subdir` is the matched-filter attribution where
applicable; it can be empty for ordinary non-LFS model files whose destination
remains repository-relative. `ScanPlan` preview events and the `/api/plan`
response remain repository-relative.

When `filters` is non-empty and no non-excluded repository-relative path matches
or recognized dataset-group membership selects data, the request fails with
`400` and error code `no_filter_match`; retained companion/metadata files alone
never make a filtered request succeed.

## Download

```http
POST /api/download
```

Uses the same request shape as `/api/plan`. Starts or reuses a download job.

Notes:

- `repo` is required and must be `owner/name`.
- `revision` defaults to `main`.
- A destination folder override must be valid for the selected storage mode and remain within its storage root; invalid values are rejected with `400` before the job is admitted.
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

Cache-mode jobs include `hubDir`, the frozen exact Hub storage directory.
`outputDir` keeps its existing meaning: the app/friendly root in cache mode,
or the local output root in flat mode. Jobs and history record both roots;
changing Settings or restarting with different ENV does not relocate new-format
jobs, including queued, paused, retried, resumed, or requeued jobs. Deduplication
includes the complete root association, so different friendly roots sharing one
Hub remain distinct destinations.
New jobs persist absolute physical paths, including relative local/route inputs,
so another launch directory on restart cannot reinterpret their destinations.

Legacy jobs without `hubDir` are resolved once on restore using their recorded
`outputDir` (the current app root if absent) plus startup `HF_HUB_CACHE`, or
`outputDir/hub` without that override. Their `destinationWarning` and a server
warning explain that the historical Hub path is unknown. The resolved association
is saved through normal state persistence. No old-cache search, file moves, or
recovery of an unrecorded historical ENV path is performed.

## Settings

```http
GET  /api/settings
POST /api/settings
```

Settings include runtime paths, concurrency, verification, endpoint, proxy settings, the HF cache directory, and extra local scan folders.

Download reliability settings:

- `retries` — the non-progress retry budget per file/part request. A non-truncating attempt that advances the on-disk resume offset strictly beyond the highest offset reached refunds this budget and resets the backoff. For a single-file transfer, a response that ignores Range with `200 OK` restarts the partial and does not refund the budget, even if that attempt writes beyond the previous high-water mark; the new high-water mark is still recorded, so a later bodyless failure earns no refund. Multipart transfers still require `206 Partial Content` and do not accept `200` as a part. Progress refunds can increase the number of retries, but a separate lifetime retry-count ceiling limits retries after individual attempts return. That ceiling does not bound the duration or bytes of a single request that continues delivering data.
- `stallTimeout` — a duration string (`"60s"`, `"2m"`). A body read that delivers no bytes for this long is aborted and retried from the current on-disk offset instead of hanging the job. An explicit `"0"` or `"0s"` disables the watchdog; empty preserves the current value. Invalid durations are rejected with `400` and are not persisted.
- Retry backoff defaults to 400ms initially and 10s maximum. Library callers may override these values; the Settings API does not expose them. When a `429`/`503` response carries a `Retry-After` header or the Hub `RateLimit` header, the downloader waits the larger of that server-requested time and its local backoff, capped at 5 minutes. Retries include `429` and all `5xx`; other `4xx` responses are terminal for that file, while `401`, `403`, and `404` fail the whole job. Optional verification `HEAD` metadata failures are ignored; the actual file request remains authoritative. Cache storage normally moves a completed temporary file into place by rename. If that rename fails, the fallback copies to a staging file in the destination directory and renames the completed stage into place; this prevents a partial fallback copy from appearing at the final blob path, but does not provide crash durability or rollback guarantees.

A permanent `401`, `403`, or `404` response for a file fails the whole job immediately with an actionable message (accept the repository terms / use a token with access, or file not found). `429` and every `5xx` are retryable. Other `4xx` statuses fail only the affected file and are not retried. Multipart transfers still require `206 Partial Content`; a `200` response to a part-range request is not accepted as a part.

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
  "configuredCacheDir": "",
  "effectiveHubDir": "J:\\shared-hf-store",
  "hubDirSource": "HF_HUB_CACHE",
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

In GET responses, `cacheDir` remains the effective app/friendly root (R), for
backward compatibility. `configuredCacheDir` is the raw preference, always
present even when empty. Use that raw value to populate an editable app-root
field and for unrelated form saves; do not materialize the effective default
as a saved preference. The three new fields are output-only metadata, not
editable settings. POST `cacheDir` still explicitly writes R, including a
same-path write; empty clears the preference and omission preserves it.
Effective metadata is ignored on POST and never persisted as preferences.

R resolves from nonempty CLI `--cache-dir`, then selected config-file preference,
then startup `HF_HOME`, then the existing `~/.cache/huggingface` default.
`effectiveHubDir` is the exact Hub storage root (H): startup `HF_HUB_CACHE` when
nonempty, otherwise R/hub. The override applies even with an explicit R; H may
have any basename and be outside R. R named `hub` still normally contains a
`hub` child. `hubDirSource` is `HF_HUB_CACHE`, `cacheDir`, `HF_HOME`, or `default`,
describing H's derivation. Environment paths/defaults are captured at server
startup, not re-read by getters or runners. Editing R with ENV H set changes
friendly views, not Hub storage, and does not move existing files.

When `localDir` is set, downloads use real files under
`<localDir>/<owner>/<model>`, which matches LM Studio-style model roots.
`localScanDirs` are independent model roots scanned as `<owner>/<model>` folders
for the Cache browser and local badges in Hub search results. A selected GGUF
entry there can be deleted through `DELETE /api/cache-selection`; these roots do
not thereby become download destinations.

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
GET    /api/cache-selection?repo={owner}/{repo}&type=model[&locationId={id}]
DELETE /api/cache-selection
POST   /api/cache/rebuild
DELETE /api/cache/{owner}/{repo}?type=model
```

`GET /api/cache-selection` returns a read-only, fresh view of GGUF groups for
the requested repository and explicit repository type. `type` is required and
must be `model` or `dataset`; dataset requests return `400` because this view is
GGUF/model-only and dataset cache behavior is unchanged. For models, the
response lists the configured local locations and selected HF cache location
that currently contain readable GGUF entries. Groups preserve the full
case-sensitive relative filename/path (including the `.gguf` extension) for a
single file; a recognized numbered split varies only its part index while
preserving its stem, delimiters, extension, and declared total. Display quant
labels do not identify or merge groups. HF members include their saved
snapshot version, and the same concrete group is combined across saved
versions in that HF location. Differently named files, differing split totals,
and unsplit files remain separately selectable. Location and group `canDelete`
fields are capability advertising only: they are not authorization tokens.
They are true only when the current location and its entries were completely
enumerated and the supported mutation scope is available. A location may include
`deleteReason` when it cannot be safely mutated.

Every group carries a `kind` that names the artifact role it represents:
`llm` for a model quantization group, and `vision_encoder` for an mmproj
companion (a multimodal projector). A vision encoder is a different artifact
role, not a model quantization: such a group reports no `quant` label, its
group id is prefixed `mmproj-` instead of `gguf-`, and its precision stays
visible in its own file name. A companion is grouped by its own file name, so
coexisting F16/BF16/F32 companions remain separate groups, while a recognized
numbered companion split stays one group. Deleting a companion group removes
only that companion; it never removes an LLM quantization, and deleting an LLM
group never removes a companion. Companion groups use the same snapshot,
revalidation, reservation, and identity-pinning rules as LLM groups, so an
active or planned writer for the selected companion still blocks its deletion.

Example:

```json
{
  "repo": "owner/model",
  "type": "model",
  "locations": [{
    "id": "local-4e1a...",
    "source": "Local",
    "path": "/models/owner/model",
    "groups": [{
      "id": "gguf-a392...",
      "label": "weights · Q4_K_M",
      "kind": "llm",
      "quant": "Q4_K_M",
      "members": [{"path": "weights-Q4_K_M.gguf", "size": 1234}]
    }, {
      "id": "mmproj-b71c...",
      "label": "mmproj-F32.gguf",
      "kind": "vision_encoder",
      "members": [{"path": "mmproj-F32.gguf", "size": 5678}]
    }]
  }]
}
```

```http
DELETE /api/cache-selection
```

Request (the `members` array is the exact composition the user confirmed):

```json
{
  "repo": "owner/model",
  "type": "model",
  "locationId": "local-...",
  "groupId": "gguf-...",
  "members": [{"path": "model-Q4_K_M.gguf", "versions": [], "size": 1234, "linkOnly": false}]
}
```

Successful response:

```json
{
  "ok": true,
  "repo": "owner/model",
  "groupId": "gguf-...",
  "removed": ["model-Q4_K_M.gguf"],
  "remaining": [],
  "message": "Removed the selected local GGUF files"
}
```

The server freshly resolves the location and GGUF group and compares their IDs
and exact member composition (paths, saved versions, size, and link role) with
the confirmation. Request paths are not used as deletion authority. Local
deletion removes only selected GGUF entries. The local symlink-member behavior
described below is supported on Linux, Windows, and macOS. On other operating
systems, the current implementation does not support identity-pinning a selected
local symlink member, so the request returns `409` before unlinking, leaving the
selected link and its target unchanged. This platform qualification applies to
selected local symlink entries; it does not imply that other HFDesk features
are unsupported on those operating systems. For an HF location it removes the
selected named snapshot entries across the confirmed saved versions and only
friendly entries whose relative names match the selected remote paths and
which provably resolve to those entries: symlinks by their link chain, and
hardlink/copy fallback entries by shared file or identical content. Other
friendly names are preserved; if one depends on a selected snapshot entry,
deletion is refused before unlinking.
It removes a blob payload only when no other named snapshot entry or retained
friendly entry in that repository references it (link target or shared file);
shared payloads are retained
and listed in `retainedPayloads`. Deletion is durable across rebuild when its
tombstones are recorded: after deletion is attempted, the confirmed names are
recorded in the repository manifest (`deleted`), so rebuild/repair never
recreates them from the manifest's file mapping even while a retained payload
keeps their content available. If manifest recording fails, the response
reports the error but deletion proceeds, and repair may later recreate those
names. A later download of the repository restores names and clears recorded
tombstones. A refused deletion records nothing and changes no state. The
manifest is updated in place; its other metadata remains. Ordinary
friendly files, other groups, versions, locations, metadata, partial files,
and directories remain. Stale/unsafe selections and conflicting writers return
`409`; this also applies when a selected snapshot entry resolves to a downloader
staging name (`tmp-…`, including part/multipart metadata forms, or
`<blob-key>.tmp-…` atomic-copy stages). The refusal precedes unlinking, leaving
all confirmed members intact. Unrelated staging files do not by themselves
prevent deleting a completed selection; ordinary opaque keys and `.part` /
`.parts.json` names outside these producer-shaped namespaces remain eligible.
The server does not cancel jobs. HF mutation excludes writers to the
selected repository reference namespace and associated friendly view. Local
selected-delete admission uses complete frozen plan destinations when
available. For queued or incomplete model plans, it conservatively considers
non-excluded candidate paths within the job's inventoried writable namespace
potentially writable: ordinary model plans may retain unmatched non-LFS files,
including GGUFs, and a raw quant-filter mismatch alone is not generally proof
that the job is in GGUF-only mode. This is scoped to reachable writable
namespaces, not a global conflict; dataset GGUF selection keeps its existing
narrower direct filter/exclude policy, and unknown or outward namespace mappings
remain fail-closed. A vision-encoder companion is its own artifact role: a
writer whose complete filter set is bare quantization tokens is treated as
non-overlapping with that companion unless a quant token appears in the
companion's path. Matching, path-scoped, and aliased writers remain
conservative. Partial unlink results use `207` and identify removed,
remaining, and failed paths (`errors`). On the supported platforms above, a local symlink member is unlinked
without following or deleting its target. The authoritative delete-time
enumeration and filesystem operations
use one opened repository root and recheck each selected entry immediately
before unlink; a detected configured-root alias change during admission is
refused, and a later alias change does not redirect the operation to another
location. The final identity
check and unlink are not an atomic compare-and-remove against arbitrary external
changes.
On Darwin, when a selected regular file cannot be opened for identity pinning,
the server may retain it with a hard link inside a private, request-local
mode-0700 directory on the same filesystem, preferring the selected member's
admitted parent. If permission prevents pin placement there, the known repository
root may be tried as a fallback; HFDesk does not scan elsewhere for a pin
location. If no approved pin location succeeds, deletion is refused with `409`
before selected entries are unlinked. Pin cleanup is attempted before the
response: cleanup failure before selected effects remains a `409` with artifact diagnostics;
cleanup failure after selected effects returns `207`, with `removed` and
`remaining` describing only selected entries and `errors` identifying cleanup
failures and any possible pin artifacts. A process crash can leave the private
pin directory behind; HFDesk does not scan for or automatically remove such
orphaned directories.
On operating systems other than Linux, Windows, and macOS, selected local
regular files that cannot be read for identity pinning may also be refused with
`409` before unlinking, even when the parent directory would otherwise permit
unlinking. This is a limitation of the current selected-delete identity-pin
implementation, not an inherent operating-system restriction or a general
HFDesk support policy. Regular-file deletion remains supported on the released
platforms above. The Darwin hard-link pin fallback described above is a separate
platform-specific behavior.
The older `DELETE /api/cache/{owner}/{repo}` remains the separate
whole-repository operation.

`locationId` is optional. When supplied, it must match a currently discovered
location for this repo/type; unknown or stale IDs return `400` rather than
falling back to another location. Local IDs are derived from the configured
root path, not its position in the current root list. HF IDs identify the
currently selected Hub and repo. `source` is a display label, not a permission
or deletion classification. Local symlink members carry `linkOnly: true` and a
message explaining that only the link is represented and its target remains;
this is display information, not deletion authorization. Per-location `warning` fields report incomplete
reads without suppressing healthy locations. A group's optional `warning` marks
an apparently incomplete numbered shard set; it is not proof that missing parts
exist or may be inferred. A confirmed deletion removes only the exact currently
enumerated member paths after a fresh complete local read; it never broadens to
an inferred shard set. A missing quant label still produces a group by filename.
An `mmproj` companion is never merged into a weight group: each companion is
reported as its own `vision_encoder` group.

`GET /api/cache/{owner}/{repo}?type=model|dataset` also honors an explicit
repository type and returns only that cache type (or `404` when absent). An
invalid supplied type returns `400`. Omitting `type` preserves the legacy
model-first, then dataset/local lookup behavior.

Its `quantizations` list contains only model quantization labels. A file is
reported as a multimodal projector companion when its basename contains
"mmproj" (case-insensitively), so a dot-separated companion
(`model.mmproj-f16.gguf`) and a name that merely starts with "mmproj"
(`mmprojector-f16.gguf`) are both companions. A companion is reported
separately: `hasMMProj` is `true` and the companion paths are listed in
`mmprojFiles` instead of appearing as a quantization label.

Cache entries may come from:

- `HF cache`
- `Friendly view`
- `Local`

`downloadStatus` is one of:

- `complete`
- `filtered`
- `unknown`

`GET /api/cache` includes effective R as `cacheDir`, plus `effectiveHubDir` and
`hubDirSource`. HF entries' `path` is under selected H, while `friendlyPath` is
under associated R. A friendly manifest from a different Hub association does
not establish completion in the selected Hub. Search badges can recognize usable
Hub snapshot files without a friendly view; empty/partial-only Hub directories
are not cached-content evidence. Independent local scan source priority remains
unchanged.

Rebuild and generated `R/rebuild.sh` use the same selected H/R association.
Conventional R/hub scripts remain relocatable with their whole root; independent
Hub scripts capture an absolute H, safely quoted, and regenerate when that
association changes. They do not rediscover ambient ENV. Manifest `repo_path`
is relative to R, or absolute when R/H are on different volumes.

Delete removes only the selected repository under H and its friendly view under
R, with separate confinement checks. Shared H deletion can affect other clients
using that repository. Failure to remove the friendly view after Hub deletion
returns `success: false` with `errors`, rather than hiding partial failure.

## Mirror

```http
GET    /api/mirror/targets
POST   /api/mirror/targets
DELETE /api/mirror/targets/{name}
POST   /api/mirror/diff
POST   /api/mirror/push
POST   /api/mirror/pull
```

Mirror operations compare and synchronize selected local H with the explicit
target root's `hub` child, regardless of local ENV. Mapping, verification, and
`deleteExtra` remain bounded to those Hub roots. Mirror diff retains `localPath`
as R and adds local `effectiveHubDir` and `hubDirSource`.

## History and Disk

```http
GET /api/history
GET /api/diskfree?path=/path/to/cache
POST /api/diskfree
```

`GET /api/diskfree` defaults to global `localDir`, then selected H. An explicit
`path` must match an allowed configured directory (including selected H, app R,
captured default root, and download routes). A legacy request naming app R is
measured at H and returns H as `path`; it must not report capacity on an unrelated
friendly-view filesystem. Explicit local/route paths retain their behavior.
When R is also a configured local/route destination, an explicit GET naming it
retains that local-path meaning; cache previews without a selector still use H.

`POST /api/diskfree` previews the effective download destination using optional
`routeKey`, `localDir`, and `dataset` fields from the download request. `repo` is
not required; other download fields are ignored. Resolution is identical to job
creation: explicit `localDir` > configured route (fine key, then parent) > global
`localDir` > selected exact H. Datasets ignore `routeKey`; unknown
model selectors return `400`, even with an explicit `localDir`. An explicit
`localDir` is accepted as on `POST /api/download`, independently of GET's browsing
allowlist. The response includes `{ "path": "...", "free": 123, "total": 456 }` with
byte counts; stat failures return `500`. This read-only check uses the nearest
existing ancestor for a not-yet-created folder, creates no directories or jobs,
and does not contact the Hub. It is advisory: settings and available space may
change before creation; each job's destination remains frozen at creation.
Disk responses additionally include current `cacheDir`, `effectiveHubDir`, and
`hubDirSource` as storage metadata. In local mode these describe the inactive HF
association, not the measured local `path`. Cache-mode preview `path` matches
job `hubDir`, not legacy job `outputDir` (R).

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
