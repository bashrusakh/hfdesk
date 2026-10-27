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

## Runtime and Logs

```http
GET /api/runtime
GET /api/runtime/logs?after=<cursor>&level=<level>&limit=<n>
GET /api/runtime/logs/stream?after=<cursor>&level=<level>
POST /api/runtime/open-logs
POST /api/runtime/open-ui
```

These endpoints report the status and the log of the running server. They
expose local process diagnostics, so they stay local: whatever address the
server binds to, a request is rejected with `403` unless every one of the
following rules holds.

- The peer address is a loopback address (`127.0.0.0/8` or `::1`). The peer
  is taken from the connection alone, so a forwarded header such as
  `X-Forwarded-For` is ignored and cannot be used to fake a loopback peer.
- The `Origin` header, when the request carries one, names the local
  machine: a loopback address (`127.0.0.0/8` or `::1`) or the name
  `localhost`. A request without an `Origin` header is not rejected by this
  rule, because a native client sends none.
- The `Host` header names the local machine: the host part of the header is
  a loopback address (`127.0.0.0/8` or `::1`) or exactly `localhost`. The
  port is irrelevant and may be any port. An IPv6 literal is written in
  brackets, with or without a port (`[::1]` and `[::1]:8080`). The match is
  exact, so `evil.example`, `localhost.evil.example` and
  `127.0.0.1.evil.example` are rejected, and so is an empty or malformed
  `Host` header.

The third rule closes the DNS rebinding hole. A name of another site can be
pointed at `127.0.0.1`, and the request the browser then makes is same
origin for the browser, so it carries no `Origin` header at all: the peer is
loopback and there is no origin to check. The name the client asked for is
what tells such a request apart from a native client.

The Runtime window is served from the loopback address of the running
server, so its own requests carry a loopback `Host` and keep working. A
server that is reached through another name of the local machine (a name
that resolves to `127.0.0.1` without being `localhost`) does not serve the
runtime endpoints: the name has to be a loopback address or `localhost`.

The requests of these endpoints are written to the log at `debug` level, not
at `info` level. The Runtime window polls the status every few seconds, and
at `info` level that polling filled the bounded history the window exists to
show. Every other request keeps the `info` level, including a request that
answers with an error. A user who asks for `debug` level still sees the
requests of the runtime endpoints.

`GET /api/runtime` returns:

```json
{
  "version": "1.3.0",
  "status": "ok",
  "url": "http://127.0.0.1:8080",
  "address": "127.0.0.1:8080",
  "activeJobs": { "running": 1, "queued": 2 },
  "runId": "3f8a1c06b27d4e59a0c1d8b47e2f6095",
  "logCursor": 42,
  "logFile": "/home/user/.cache/HFDesk/logs/hfdesk.log"
}
```

`url` is the browser-safe URL of the running server, `address` is the
effective address it is bound to (empty while the server is not bound yet)
and `logCursor` is the sequence number of the most recent log record, the
cursor a client sends to read the records emitted after it. `logFile` is the
path of the rotating log file; it is an empty string when the application does
not write a log file.

`runId` identifies the run of the server: it is generated once per server
and it never changes afterwards, so it is stable for the whole life of the
process and different in another process. A client compares it to tell a
restart of the server from a continued run, because the sequence numbers of
the log belong to the run of the process that emits them and start over with
every run. The head of the log cannot answer that question: the status is a
snapshot that was taken before the request itself was written to the log, so
the head it reports is behind the records the client already has. The value
is random: it carries no information about the machine, the user or the
files of the host, and it is not a secret. A server that does not report a
run id (an older build) leaves the run of the client unknown, which is never
read as a restart.

`GET /api/runtime/logs` returns a bounded page of the retained records:

```json
{
  "records": [
    {
      "seq": 42,
      "time": "2026-10-21T10:11:12Z",
      "level": "INFO",
      "component": "server",
      "message": "request",
      "attrs": { "method": "GET", "path": "/api/health" }
    }
  ],
  "cursor": 42,
  "truncated": false
}
```

- `after` — a cursor; only the records with a higher sequence number are
  returned. The default is `0`, which returns the whole retained history.
- `level` — the minimum severity of the returned records: `debug`, `info`,
  `warn` or `error`. An absent value or `all` returns every record. An
  unknown value is rejected with `400`.
- `limit` — the maximum number of returned records. The default is `200`
  and the maximum is `1000`; a larger value is clamped to the maximum. An
  invalid value is rejected with `400`.

`cursor` is the cursor to send for the next page. It is the sequence number
of the last record the server consumed. Without a `level` filter that is the
last record the page carried, so a client never skips a record it did not
receive. With a `level` filter the server also consumes the records it
filtered out, so the cursor can name a record the page did not carry and the
client does not have to ask for those records again. A page that consumed no
record keeps the cursor of the client: the page taught the client nothing, so
the cursor does not move.

`truncated` reports that
the caller cannot be served a complete answer for the requested cursor and
has to start over. It is true when the requested window is no longer fully
covered by the retained records, so the client must not assume that the page
continues the history it has. It is also true when the cursor is ahead of
every record the server holds: the sequence numbers of the log belong to the
run of the process that emits them and start over with every run, so such a
cursor comes from an earlier run of the server. A cursor that is exactly at
the newest record is not truncated, because there is simply nothing new after
it. The records come from a bounded in-memory ring: the log file is never
read, so a reconnect does not re-read it and a single request cannot ask for
an unbounded amount of data.

`GET /api/runtime/logs/stream` is a Server-Sent Events stream of the same
records. It replays the retained records after `after` first and then
continues with the live records. The response carries `Content-Type:
text/event-stream` and `Cache-Control: no-cache`, and it carries these
events:

- `log` — a record, with the payload of a record of the page above.
- `dropped` — the subscription lost records because the client did not keep
  up: `{"dropped": 3, "cursor": 42}`. The client re-syncs by reading
  `GET /api/runtime/logs` after the reported cursor.
- `gap` — the requested window had already fallen out of the retained
  records, or the cursor was ahead of every record the server holds:
  `{"dropped": 0, "cursor": 42}`. The client re-syncs the same way.

The stream takes `after` and `level`, like the page. It takes **no**
`limit`: a request that carries a limit value is rejected with `400` and an
error that names the parameter. A limit on a stream is ambiguous — it could
cap the replay, the live records or both — and a client cannot tell a stream
that ignored the limit from a complete answer, so the stream refuses the
parameter instead of accepting it and ignoring it. The page is the endpoint
that bounds the size of an answer.

The replay of the stream is bounded by the retained records of the in-memory
ring: the log file is never read, so a stream replays at most the records
the ring still holds. A window that already fell out of the ring is reported
by the `gap` event, and the client re-syncs it with the page.

The stream moves its position over the records it consumed, so the cursor it
reports follows the same rule as the page: without a `level` filter it never
names a record the client did not receive, and with one it can name a record
the stream filtered out. A replay that consumed no record keeps the cursor of
the client.

The stream ends when the client goes away, and a keep-alive comment is sent
while it is idle. Log traffic does not use the job WebSocket of `/api/ws`.

`POST /api/runtime/open-logs` opens the folder of the active log file with
the file manager of the operating system. It takes no path: the endpoint opens
the log location of the running application and nothing else, so it can never
be used to open another location. It is a POST, because it changes the state
of the desktop, and it is guarded by the same local-only rule as the other
runtime endpoints. It answers `404` when the application does not write a log
file, and `500` when the operating system did not open the folder.

```json
{
  "ok": true,
  "path": "/home/user/.cache/HFDesk/logs"
}
```

`POST /api/runtime/open-ui` opens the web UI of the running server in the
default browser of the operating system. It takes no parameters: the endpoint
opens the URL of the running server and nothing else, so it can never be used
to open another location. It is a POST, because it changes the state of the
desktop, and it is guarded by the same local-only rule as the other runtime
endpoints. It answers `404` when the server is not bound yet, and `500` when
the operating system did not open the browser.

The URL is handed to the default handler of the operating system, the very
mechanism the console host and the desktop host use to open the web UI, so
every caller reaches the browser the same way.

```json
{
  "ok": true
}
```

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

`AppendFilterSubdir` is limited to single-component group/token filters because
the filter becomes a destination directory name. `PlanRepo` and `Download`
reject a path-shaped filter (one containing `/` or `\`, including a `./` prefix
or trailing separator, or the `.`/`..` components) with
`ErrAppendFilterSubdirPathFilter` before any network or plan work. Such filters
remain valid when `AppendFilterSubdir` is disabled. `POST /api/plan` and a
dry-run `POST /api/download` request report this combination as HTTP 400 with
error code `invalid_append_filter_subdir`; a normal non-dry-run download remains
admitted and runs without filter subdirectories, matching the existing download
pipeline.

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

A job that reports `completed` means the requested files are present in the
cache (or at the flat output root) AND reachable at their snapshot/friendly
paths. A file that another live writer owns is not
present, so it is never counted as a completed skip: the item waits for that
destination's ownership domain (bounded) and re-checks the published target, and
it fails with an actionable error when the owner is still there. Such a job
reports `failed` with that error, never `completed` with a missing file. A
failed link creation — the snapshot or friendly entry of a cached file could
not be created — fails the affected item the same way, in the transfer path
and in both skip paths, so a job never reports `completed` with an
unreachable path either. The friendly entry is deliberately absent when the
friendly view is disabled; that is not an error.

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

- `verify` — the post-download verification mode, applied to LFS files as well: `none` performs no post-download content hash (HTTP and ranged-response validation still apply during the transfer); `size` (the default) checks the exact expected size and complete validated transfer coverage on both the multipart and single-request paths (every expected byte range validated and written; for a single request, an exact `Content-Range`/body match and the exact on-disk size) — on the normal transfer path it performs no full-file SHA-256 reread (the pre-download skip check over an already-present file and the inherited single-path `.part` proof may still hash); `sha256` verifies the full file content against the known digest with at most one full-file read, always before the destination/blob is published when a known digest exists (only an eligible fresh sequential LFS ranged transfer with one task covering the whole file hashes while writing; every other transfer, including the single-request path, performs one sequential pass before publication, so a mismatch never publishes corrupt bytes, the staging data is discarded, and a retry re-downloads from scratch); `etag` proves a known SHA-256 digest with that same single pre-publication pass, while non-LFS ETag-only files keep the historical no-post-check behavior; when `sha256` has no known digest, the HEAD-ETag fallback remains a post-publication check and can report a mismatch only then. Partial downloads from old or unsupported formats are discarded, not migrated: their bytes are never read into staging and the file is re-downloaded from zero (already-published blobs and completed files are untouched). A single-request `.part` is a sequential prefix with no range-layout record - it is identified by the `<dst>.part.meta` sidecar, not by a per-range layout: in `sha256`/`etag` modes a fresh or resumed transfer is proven with the same one full-file SHA-256 pass before the rename, in `size` mode inherited bytes are proven once (the inherited-bytes exception), a known digest with an unknown expected size is proven too (the partial itself is still discarded rather than resumed), and a mismatch never publishes and the partial is discarded. A multipart staging record also persists a durable `ProofPending` gate: it is set and flushed before any known-digest proof starts and cleared and flushed only after it succeeds, and on resume it forces the proof in every mode except `none` (including `size`) when a known digest exists, so a crash mid-proof, a cleanup failure after a mismatch, or a later mode change cannot publish unproven all-Done ranges; a mismatch keeps the flag set, attempts poison (truncate to zero) only while the claim remains owned under the per-path metadata gate and the held destination domain (a stale writer must not truncate another owner's payload), and attempts the owned discard; if ownership cannot be verified the durable flag remains the guard, and publication is refused while a known digest's proof is pending. `none` is the explicit no-hash opt-out.
- `retries` — the non-progress retry budget per file/part request. A non-truncating attempt that advances the on-disk resume offset strictly beyond the highest offset reached refunds this budget and resets the backoff. For a single-file transfer, a response that ignores Range with `200 OK` restarts the partial and does not refund the budget, even if that attempt writes beyond the previous high-water mark; the new high-water mark is still recorded, so a later bodyless failure earns no refund. Multipart transfers still require `206 Partial Content` and do not accept `200` as a part. Progress refunds can increase the number of retries, but a separate lifetime retry-count ceiling limits retries after individual attempts return. That ceiling does not bound the duration or bytes of a single request that continues delivering data.
- `stallTimeout` — a duration string (`"60s"`, `"2m"`). A body read that delivers no bytes for this long is aborted and retried from the current on-disk offset instead of hanging the job. An explicit `"0"` or `"0s"` disables the watchdog; empty preserves the current value. Invalid durations are rejected with `400` and are not persisted.
- Retry backoff defaults to 400ms initially and 10s maximum. Library callers may override these values; the Settings API does not expose them. When a `429`/`503` response carries a `Retry-After` header or the Hub `RateLimit` header, the downloader waits the larger of that server-requested time and its local backoff, capped at 5 minutes. Retries include `429` and all `5xx`; other `4xx` responses are terminal for that file, while `401`, `403`, and `404` fail the whole job. Optional verification `HEAD` metadata failures are ignored; the actual file request remains authoritative. Cache storage places a completed temporary file into the content-addressed blob path with a no-replace primitive rather than with a plain rename: on a filesystem that can create hard links the placement is atomic no-replace (a hard link fails when the name exists), so the existence decision and the placement are one filesystem operation and an existing `blobs/<known-sha>` is not replaced even by a writer outside the blob key's domain; on a filesystem WITHOUT hard links the placement is a check-then-rename performed while the blob key's ownership domain is held exclusively, so an existing blob is not replaced by a domain participant while a writer outside the domain (the mirror writer) is not fenced by it, and the placement refuses to run at all without that domain. If the placement itself fails (a cross-device move, for example), the fallback copies the payload to a staging file in the destination directory and renames the completed stage into place: the copy runs inside the same ownership domain, so no domain participant can create the target in that window, but that final rename is not a no-replace primitive, so a writer outside the domain is not fenced by it. The fallback prevents a partial copy from appearing at the final blob path, but it does not provide crash durability or rollback guarantees.

A permanent `401`, `403`, or `404` response for a file fails the whole job immediately with an actionable message (accept the repository terms / use a token with access, or file not found). `429` and every `5xx` are retryable. Other `4xx` statuses fail only the affected file and are not retried. Multipart transfers write ranged workers directly into one staging file (`blobs/<sha>.incomplete` in HF cache mode — the future blob itself — or `<destination>.incomplete` otherwise) and resume from versioned per-range committed-prefix metadata next to it, so finalization is an atomic rename plus link work, never a full-file assembly copy. A staging record is trusted only when it matches the resolved commit, repository, path and expected size, its identity is PINNED on both sides (the record's own `commit_pinned` flag and the current plan's), carries the blob SHA on both sides or neither, tiles the file with normalized per-range completion (Done is set only after clean response framing), and its payload exists at the expected logical size before any create/truncate; a completed-looking record whose payload was deleted or resized is discarded and re-downloaded rather than published as zeros. A record written without the pinned flag is unpinned and therefore not resumable, and an unpinned record is discarded even when its repository, commit string, path and size match. Current records use version 4, and it is the only resumable version: a record of any other version is discarded under the claim's ownership and local metadata serialization, and the file is re-downloaded from zero. Fresh claims are installed from complete staged JSON, and a failed takeover replacement retains the validated predecessor until the atomic replacement is ready. Staging identity uses `plan.Commit`; if repository-info lookup fails or returns an empty SHA, the planner falls back to the requested revision string, which is not necessarily an immutable commit. An unpinned identity pins nothing, so no staged bytes are inherited from it - the same rule the single-request path applies. The single-request path carries its own resume identity, because its staging name is not one: for an item without a known digest that name is derived from the item's path (`tmp-path-<sanitized path>`), so two different objects - or two revisions of one object - can share it, and the destination's ownership domain excludes concurrent writers without establishing WHICH object the staged bytes belong to. A sidecar `<dst>.part.meta` (JSON, versioned) records the repository, the resolved commit, the item's relative path, the expected size, whether that commit was actually resolved (pinned), and - independently of the object identity - whether the producing run observed a cleanly-framed response body at the exact expected byte count (`framing_complete`). Bytes already on disk are inherited only when that record exists, is readable and well-formed, matches the current item exactly and is pinned on both sides; otherwise they are discarded and the transfer restarts from zero, so unproven bytes are never published. An unresolved commit (the requested revision standing in for it) pins nothing, so nothing is inherited from it. An exactly-sized partial is published without a request only when the matching sidecar also records the clean-framing completion; otherwise the final byte is reserved and re-requested as the non-empty range `[size-1, size)`, and a body error during that revalidation leaves the final byte uncommitted for a later retry/restart. The multipart route never imports a single-route `.part`: it creates no `.part` of its own, so a `<dst>.part` and its sidecar belong to the single-request route (or to an earlier run). A sidecar-bearing `.part` is resumed only by the single-request route under its own identity rule, and a `.part` without a matching identity record is never inherited. The pre-#140 `.part-NN`/`.parts.json` family is not resume state either: it is ignored, discarded under the claim when the multipart route owns the staging (or left for the existing success/cleanup path), and the file is re-downloaded from zero. A `.part`/sidecar next to the destination is never removed by the multipart route's permanent HEAD failure: that route creates no `.part` of its own, so only the single-request route - which created the partial while holding the destination domain - cleans up its own zero-byte staging file. (In flat/output mode the multipart route now holds that same domain before its HEAD, so a live single-route writer excludes it at the domain acquisition instead.) The record is written before any inherited byte is trusted, is removed together with the partial it describes (on publication and on cancel-cleanup), and survives an interrupted run, so the next run can evaluate it; a `.part` without a matching record is never inherited. The known-digest content proof is unchanged and still applies to the inherited bytes. A ranged response is accepted only with status `206 Partial Content`, a `Content-Range` that exactly matches the requested byte range, an advertised total matching the expected size, and exactly the requested byte count; `200` and any mismatched range response are rejected. The single-request path applies the same contract to a `.part` (exact requested `Content-Range`, expected total and body length, bounded writes, clean response framing through EOF, and exact on-disk size), never resumes when the expected size is unknown. A copy error after the final requested byte leaves that byte uncommitted for a nonempty retry/restart range. It proves a known digest with one full-file pass before the rename in `sha256`/`etag` modes, for inherited bytes in `size` mode (the inherited-bytes exception), and when the expected size is unknown (the partial is still never resumed); an ignored-Range `200` still restarts that partial from zero. A multipart staging record's durable `ProofPending` flag is set before any known-digest proof and cleared only after it succeeds; on resume it forces the proof in every mode except `none` (including `size`), and a mismatch keeps the flag set, attempts poison (truncate to zero) only while the claim is still owned under the per-path metadata gate and lifetime OS lock, and attempts the owned discard, so a cleanup failure or crash cannot launder unproven all-Done ranges into a published blob; if ownership cannot be verified the durable flag remains the guard, and publication is refused while a known digest's proof is pending, and `none` is the explicit no-hash opt-out. An interrupted transfer resumes from its committed high-water marks without re-downloading valid bytes, including when the connection count changes between runs. If an LFS item remains unknown-size after HEAD, Download uses the single-path temp and normal cache-store path so the blob, snapshot, and friendly-view entries are created from the actual completed temp file. Existing multipart resume state (a valid version-4 `<staging>.incomplete`/`.meta` pair, including the blob-keyed LFS pair) keeps the multipart route for a ranged-capable item even below the configured threshold, so the state is resumed; a fresh item with no state keeps the configured routing. A versioned pair counts as existing state only when its identity matches the current item and is pinned on both sides, because a record that cannot be inherited cannot be resumed either. A partial download from an old or unsupported format is not resume state and does not force the multipart route. Without range support the downloader warns, downloads the full file through the single path, and removes the obsolete versioned pair only after the final file is durable and claim-checked (the pre-#140 family is removed by the single-path success cleanup). Ownership is fenced by the DESTINATION's ownership domain: an OS-level exclusive lock on the persistent domain lock (`blobs/tmp-<sha>.lock` for a cache destination, `<destination>.incomplete.lock` for a flat/output-mode one; `flock(LOCK_EX|LOCK_NB)` on Unix, an exclusive-share `CreateFile` handle on Windows), acquired ONCE at item level after the skip decision and before the HEAD/claim, and held through transfer, hashing, publish/store and cancel cleanup: a busy lock is a live owner (a suspended process keeps it, a crashed one releases it at death, so takeover is immediate only after death), and platforms/filesystems unable to provide the domain lock return an operation error before claim/payload mutation and preserve existing partial data; this is runtime failure handling, not permanent support policy. The multipart claim takes no second OS lock: it receives the held domain, verifies that the domain covers its staging path (otherwise it fails closed), performs its record transitions under the intra-item per-path metadata gate, and records `LockFenced` from the held domain. The obsolete `<staging>.lock` claim fence is gone; a branch-only build that still held it is intentionally unsupported. The claim record also carries a per-process instance token, PID/host diagnostics and a heartbeat for identity and resume, so PID reuse or containers sharing PID 1 are not treated as the same writer. Every staging-record transition (claim, take-over, checkpoint, heartbeat, removal) is serialized locally by a per-path metadata gate while the held destination domain excludes every other current-version writer, and re-reads claim identity under that gate, so a heartbeat cannot race a take-over or resurrect a lost claim; the claim reports a record it cannot prove dead as unproven instead of taking it over; the download path waits for the destination's ownership domain and re-checks the published target, and only an exhausted wait turns that into an actionable per-file error, because the file is not present while the live owner holds it. Publish re-reads the record under that lock and checks the ownership lock is still held before the rename, refusing a missing or foreign record instead of renaming another writer's staging file. Before the rename, the publication also asserts that the range layout covers the whole expected payload - every range Done and the committed total equal to the expected size, in both the live layout and the persisted record. An incomplete layout is refused (the staged bytes stay on disk for a resume) and nothing is renamed, so an incomplete payload is never published; the check is asserted there rather than left to the control flow that reaches the publication. A content-addressed target that appears mid-flight is not overwritten by a writer of the same blob key: the publication places the payload with a no-replace primitive, so an existing `blobs/<known-sha>` keeps its bytes — the losing writer discards its own staging pair, and reports completion so the caller treats the item as a skip. The "the blob does not exist yet" decision and the placement that follows it are one step, so a concurrent publisher cannot be overtaken between them. The strength of that step depends on the filesystem: on a filesystem that can create hard links it is atomic no-replace (a hard link fails when the name exists), so the target is not replaced even by a writer outside the blob key's domain. The Go standard library exposes no atomic no-replace rename on any supported platform (`os.Rename` replaces an existing target on Unix and on Windows; the `syscall` package exports neither `renameat2(RENAME_NOREPLACE)` nor `renamex_np(RENAME_EXCL)`), so on a filesystem WITHOUT hard links the placement is a check-then-rename performed while the blob key's ownership domain is held exclusively: there the target is not replaced by a domain participant, while a writer outside the domain (the mirror writer) is not fenced by it. That placement refuses to run without the ownership domain of the blob key it publishes, and hard-link support is not a prerequisite for downloading. The exported `FinalizeBlob` publishes through the same primitive and participates in the same domain, so it never replaces an existing content-addressed blob either. A content-addressed blob key has ONE publication domain shared by every route that writes it (the ranged route and the single-request route both stage under `tmp-<sha>`/`<sha>.incomplete` and publish onto `blobs/<sha>`, and the exported `FinalizeBlob` publishes the same key): an in-process slot keyed by the blob key plus the lifetime OS lock on the persistent sibling `blobs/tmp-<sha>.lock`, held for the whole operation on that key. One domain means the other route can neither adopt nor delete this route's LIVE partial (a live partial is never mistaken for a pre-#140 leftover), so a published blob is always exactly one writer's transferred bytes and never a mixture of two writers. The domain lock file is persistent, is never unlinked, and is excluded from cache statistics, listings, mirrors and cache deletion. EVERY cache destination owns one ownership domain, not only the content-addressed ones, and the domain is keyed on the destination name with the SAME identity for the writer and the cleanup. A content-addressed item (a supplied canonical SHA-256) stages at `blobs/tmp-<sha>`; its name is the digest, so its domain is exactly the blob key's domain: key `blobs/<sha>`, lock `blobs/tmp-<sha>.lock`, and the publication's `holdsBlobKey` check is unchanged. An item without a known digest (the planner allows an empty SHA-256) stages at the STRUCTURALLY NON-CANONICAL `blobs/tmp-path-<sanitized-relative-path>`; its domain key is `blobs/path-<sanitized-relative-path>` and its lock is `blobs/tmp-path-<sanitized-relative-path>.lock`. "path-" is not a 64-hex digest, so neither identity can be reached by a blob key or lock, and the staging name can never equal a content-addressed `tmp-<sha>` name. The prefix is required: before it a digest-less item's destination name was `blobs/tmp-<sanitized path>`, so item A at path `SHA256(Y)` (content X) and item B at path `SHA256(X)` (content Y) held the blob domains each other's store needed, and the unbounded in-process wait deadlocked the job; the same shape also let a digest-less item share the staging file of a content-addressed item with the same digest. With the unambiguous name the domain stays name-based, one destination has one identity, and neither defect can be constructed. The user-visible final/snapshot paths do not change; only the internal staging name does. An item without a known digest has no published content-addressed target to re-check (its bytes are moved into a blob whose name is the digest computed at store time, which the waiting writer cannot know before it transfers the file), so after it waited for the domain the item OBTAINS the file itself: it re-downloads these small non-LFS files, and the store then reuses the already published blob instead of replacing it. A writer that cannot take its domain fails the item before it writes any payload, exactly like the content-addressed case. The legacy/output (flat) mode has no `blobs/`-style internal identity, so it borrows the multipart staging name for the destination's one domain: the domain's lifetime lock is the persistent sibling `<destination>.incomplete.lock`, a zero-byte artifact next to the user's model file that is never unlinked, and no new artifact name appears next to the user's model file. The SAME domain is acquired once at item level for both routes, so one lock file excludes every pair of writers of the same flat destination: single-vs-single, single-vs-multipart, multipart-vs-multipart and cleanup-vs-any-writer. The multipart claim does not take that lock a second time; it trusts the held domain and fails closed when the domain does not cover its staging path, and the unknown-size hand-off to the single-request path keeps the domain the item already holds. A concurrent second writer of the same flat destination therefore waits for the domain (bounded, context-aware, never a takeover) and fails its item with the actionable live-owner error ("blob <sha> is being downloaded by another process; try again later") only when the wait is exhausted, proceeding with the download when the owner releases it — instead of racing, which is safer than the pre-#140 behavior, where two processes wrote the same `.part-NN` payload files and could publish a silently corrupt model. A flat destination whose ownership domain cannot be acquired (the OS lock is unsupported, denied, or the filesystem failed) fails the item BEFORE it writes any payload, exactly like cache mode: that is the fail-closed consequence of every route owning a domain. And because every route takes that domain, EVERY flat download leaves the persistent zero-byte `<destination>.incomplete.lock` next to the file, even when no multipart transfer ran: the shared domain/staging lock is created by the item-level acquisition, not by a multipart transfer. The job contract follows from the domain: a job that reports success means the requested files are present AND reachable at their snapshot/friendly paths, so a file owned by another live writer is never a successful skip. When the domain is held by a proven live owner — another process, or another writer of this process — the item WAITS for it (bounded, context-aware, never a takeover) and, once it holds the domain, RE-CHECKS the published content-addressed target: a blob the owner published while the item waited is reused, the item finishes as an ordinary "blob exists" skip with its own snapshot and friendly entries created and without a second transfer; an owner that released the domain without publishing makes this writer download the file itself; and a live owner that still holds the domain when the wait budget expires makes the item fail with an actionable error ("blob <sha> is being downloaded by another process; try again later"), and the job does not report success while that file is missing. A path-derived destination has no published content-addressed target to re-check, so after it waited for the domain the item obtains the file itself (it re-downloads these small non-LFS files) and the store then reuses the already published blob; a successful skip that leaves the file missing is forbidden in every case. The re-check trusts the EXISTENCE of the published content-addressed blob as completion proof, exactly like the pre-existing "blob exists" skip does, and does not re-verify its content: in `size` mode a torn blob written by a foreign writer is therefore accepted as complete, the same content-addressed assumption the pre-existing skip already makes. A cached file is reachable through its snapshot entry and its friendly entry, and the payload in `blobs/` alone is not the requested path: the transfer path always treated a failed link creation as fatal for the item, and the pre-download "blob exists" skip and the post-domain "blob exists" skip now agree with it — a link-creation failure fails the item with an actionable error ("the file is in the cache but its cache path is not reachable") and emits no `file_done` success. Every path that creates the cache entries returns the exported `ErrCacheLinkUnreachable` sentinel, so the failure is uniform and detectable with `errors.Is`. The end-of-job reconciliation that re-creates the links of skipped items surfaces its link failures as a job error instead of only a warning, so a job cannot finish successful with a missing path. The friendly entry is deliberately absent when the friendly view is disabled, so that is not an error. The wait budget is a package-level knob derived from the existing ownership-lock convention — 60 times the 5s acquisition window, so the effective budget is 5 minutes — not a user setting; the in-process wait is bounded by the job's context only, and cancellation stays prompt. The wait runs inside the item's goroutine, but a DESTINATION-DOMAIN waiter does not hold a `MaxActiveDownloads` slot: the slot is a per-item lease that the ownership-domain acquisition returns for the duration of every wait (the in-process domain slot and the cross-process domain lock alike) and takes back before it returns a domain the caller will transfer under. The multipart claim takes no ownership wait of its own any more (it runs under the held domain and only takes the intra-item metadata gate), so it does not move the slot. The slot therefore bounds concurrent TRANSFERS, not waiters, so the other files of the job proceed while one item waits, even with a low limit; waiters do not consume transfer slots, while concurrent transfers still obey the configured limit. A FAILED acquisition keeps the slot returned: no domain reaches the caller, so the item reports the error instead of transferring. Waiting items are still goroutines bounded by the job's file set, and the slot is taken back before the transfer. The acquisition's waits and budgets are unchanged: an in-process contender waits for the current owner bounded by the item's context; a cross-process PROVEN live owner is waited out until the ownership budget expires and is then reported with the actionable ownership error; and the local metadata gate is waited out within `stagingLockWait`. What the operator sees: the item stays in progress (no progress events are emitted while it waits, so its byte counter does not move), and when a budget expires or a live owner is reported the item fails with the actionable error above and the job's status is failed, not completed. The cache status check distinguishes three outcomes: a proven live owner is reported as downloading (the item must NOT be skipped: the file is not present while that owner holds it, so the item waits for the blob key's ownership domain and re-checks the published target), a provably dead owner is reported as stale (the item may be resumed), and an unknown ownership state — the ownership probe failed, or the platform cannot represent the lock — is reported as an actionable error that fails the affected file instead of skipping it, because no live owner is proven and a skip would report success with the blob still missing; the incomplete payload is preserved in that case. A record written while holding the OS lock (`LockFenced`) makes a free lock authoritative for `CheckBlob`: it reports stale rather than downloading even when the recorded PID is live, so a reused PID cannot hide a resumable crash state; unfenced records are protected unless same-host process evidence proves their owner dead; age or ambiguous identity is not sufficient. The same applies to claims and cleanup. A staging record whose owner can be proven NEITHER alive nor dead - one written without the OS lock fence (`LockFenced` is false, which no released version ever wrote) or one that cannot be read or parsed - is refused by the claim and is NOT reported as 'another live writer': the partial and its record are preserved, the record is never taken over (no age-based takeover, no takeover without proof of same-host death), and the per-file error says so, names the staging pair, and carries the exported `ErrStagingOwnershipUnproven` sentinel so a caller can tell it apart from the proven live owner, whose message stays 'blob <sha> is being downloaded by another process; try again later'. Operator procedure for an unproven record: the automatic recovery is deliberately absent, so remove the staging pair (`<staging>` and `<staging>.meta`, for example `blobs/<sha>.incomplete` and `blobs/<sha>.incomplete.meta`) and retry the download; the item then starts from zero. Never remove the destination's persistent domain lock (`blobs/tmp-<sha>.lock` for a cache destination, `<destination>.incomplete.lock` for a flat one), and never remove a staging pair whose owner is a proven live owner - that owner is still writing to it. Completion is content-addressed: an existing publish target proves completion only when it is `blobs/<known-sha>`; an arbitrary legacy/output destination or `tmp-<sha>` staging name is governed by the pre-download skip decision and is downloaded again and atomically renamed over if that decision declined it (so "blob exists" is reported only for a published blob-keyed target). Cleanup has two removal entry points matching the one fence: an owner-checked removal under the held destination domain and the metadata gate, and a record-checked removal for the external cleanup paths that deletes only a pair no live owner can be using. A cache destination with no derivable domain is skipped with a warning instead of removing unowned artifacts. The cleanup of a cache destination's artifacts participates in the same ownership protocol: the `blobs/tmp-<name>` staging name, its single-request `.part`, the pre-#140 `.part-NN` parts and the blob-keyed staging pair (`blobs/<sha>.incomplete` and its `.meta`) are removed only while that destination's ownership domain is held, so a cancelled or dismissed job never deletes another live writer's partial (that writer would keep filling an unlinked inode and its later publication would fail with ENOENT). The cleanup uses the same name-based identity the writer holds: destination `blobs/tmp-<name>` means key `blobs/<name>` and lock `blobs/tmp-<name>.lock`. For a content-addressed `tmp-<sha>` that is the blob key's domain; for a path-derived `tmp-path-<sanitized path>` it is `blobs/path-<sanitized path>` with lock `blobs/tmp-path-<sanitized path>.lock`, which no blob key or lock can reach. The staging name is unambiguous, so there is one name-based identity per destination. When a live owner holds the destination — in this process or in another one — those artifacts are left alone and the contention is reported. A job can still clean up its own artifacts: its writer releases the domain when it returns, and the downloader's own cancel cleanup removes its staging while it still holds the domain. The residual limitation: the cleanup cannot tell "my own destination" from "another live owner of the same destination" beyond the domain itself, so a destination another writer owns is left alone (the safe choice) even when the cancelling job also had bytes for it. A legacy/output-mode destination has no `blobs/`-style internal identity, so its cleanup derives the same domain identity the writer holds: the destination's domain lock is the persistent sibling `<destination>.incomplete.lock`. Its artifacts are removed only while that domain is held, so a cancelled job cannot delete another live job's `.part`, `.incomplete` or `.meta` for the same path. The versioned staging pair (`<destination>.incomplete` and its `.meta`) is still claim-checked, and the single `.part`, the pre-#140 `.part-NN` parts and `.parts.json` are removed as before; the persistent `<destination>.incomplete.lock` is never unlinked. A partial download from an old or unsupported format is discarded, never migrated: a valid pre-#140 `.part-NN`/`.parts.json` family is ignored (and discarded under the claim when the multipart route owns the staging, or by the existing success/cleanup path), a leftover single-route `<destination>.part` is left to the single-request route (which discards it unless a matching, pinned identity record authorizes a resume), and an unreadable or unproven CURRENT-format record is refused with `ErrStagingOwnershipUnproven` and preserved rather than auto-deleted. The model is re-downloaded from zero and the published content is exact; already-published blobs, completed files and snapshot/friendly entries are untouched. A staging file's on-disk length is never treated as evidence of completion, since staging files are preallocated.

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

Service mode: the order above does not apply. The config file the instance owns
wins over the cache root recorded in its registration, because the settings an
administrator changes through the settings API of a supervised instance have to
survive a restart of the instance.

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

Every cache surface separates published content from the downloader's own
staging artifacts with one exhaustive, name-based classification. The staging
shapes are the single-request staging name `tmp-<name>` (including the
path-derived `tmp-path-<sanitized path>` form; with its `.part`, its
`.part.meta` identity sidecar and its `.lock` domain lock), the
ranged staging pair `<name>.incomplete` + `<name>.incomplete.meta` (with its
atomic record temp `<name>.meta.tmp`), the persistent ownership lock
`<name>.lock`, the
legacy multipart family `<name>.part-NN` and `<name>.parts.json`, the
flat-mode partials `<destination>.part` and `<destination>.incomplete`, and
the cache store's atomic copy temp `<name>.tmp-<random>`. A published blob is
named by its content: a canonical 64-hex SHA-256 digest. A file in `blobs/`
that is neither a staging artifact nor a canonical digest is an unusual
published name - the public storage API accepts such names - so it is content:
the cache statistics count it, the listing shows it, the selection sees it and
the deletion path may delete it. The classification is by name shape, so a
`blobs/` name that CARRIES a staging shape (`tmp-<name>`, `<name>.part`,
`<name>.part.meta`, `<name>.lock`, `<name>.incomplete`,
`<name>.incomplete.meta`, `<name>.meta.tmp`, `<name>.part-NN`,
`<name>.parts.json` or `<name>.tmp-<random>`) is treated as a staging artifact
by every surface, including the deletion path - the conservative choice: a
staging artifact is never deleted - even when such a name was published
content. A canonical 64-hex blob is unaffected: it is content, and the
deletion path may remove it. The exclusion applies inside `blobs/` only, so
a file outside that namespace (a snapshot entry named `tokenizer.model.meta`,
for example) is content in every surface.

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
available. A matching run activity with a known and complete plan is
authoritative: if none of its frozen destinations overlaps the selected entry
(including revalidated raw alias spellings), that activity cannot write the
entry and is skipped; any overlapping or unresolved destination remains
conservative. For queued or incomplete/unknown model plans, it conservatively
considers non-excluded candidate paths within the job's inventoried writable
namespace potentially writable: ordinary model plans may retain unmatched
non-LFS files, including GGUFs and mmproj companions, and a raw quant-filter
mismatch alone is not generally proof that the job is in GGUF-only mode. This
is scoped to reachable writable namespaces, not a global conflict; dataset GGUF
selection keeps its existing narrower direct filter/exclude policy, and unknown
or outward namespace mappings remain fail-closed. Matching, path-scoped, and
aliased writers remain conservative. Partial unlink results use `207` and
identify removed,
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

The mirror's scan, copy and verification use the same staging-artifact
classification as the cache surfaces, and the exclusion is scoped to the
`blobs/` namespace: a staging artifact is never copied, counted or verified, so
an unfinished download or a legacy multipart residue is never published as
ready content. Copy and verification always cover the same set - every file
the copy produces - so a legitimate file outside `blobs/` (a snapshot entry
named `tokenizer.model.meta`, for example) is copied AND verified. A published
blob is verified by size, a snapshot entry by its link target, and an unusual
published blob name (non-canonical, accepted by the public storage API) is
copied and verified like any other content.

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
