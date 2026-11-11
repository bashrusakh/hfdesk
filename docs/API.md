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

`recommended_download` is a ready-to-send request body for `POST /api/download`. It replaces the old CLI-command string contract. GGUF recommendations use exact artifact selectors from `selectable_items[].files` rather than the legacy quant token, and set `exactMatch: true`: root-level paths are prefixed with `./`, while nested paths keep their complete repository-relative spelling. Dataset recommendations also set `exactMatch: true`, but use the group's `filter_value` as the selector. A dataset that holds config folders additionally exposes one `config` category item per config (see the dataset selection notes below); when exactly one config holds the train split — the established dataset recommendation rule — that config preset is the single recommended row and `recommended_download` selects its config-folder `filter_value` instead of the bare `train` split, while with no unique train-bearing config the recommendation stays on the split rows as before. Other model recommendations keep their existing filter values and leave `exactMatch` unset — except a Diffusers component recommendation whose row carries its chosen weight set (see the Diffusers component selection below): it contributes the chosen files' exact paths like a GGUF artifact and sets `exactMatch: true`, while components without weights keep their folder filter and unset `exactMatch`. Exactly one non-MTP GGUF quantization artifact is recommended: the first `Q4_K_M`/`UD_Q4_K_M` in the analyzer's deterministic order, or otherwise the first non-MTP quality>=4 artifact under 10 GiB. When the `availableRamBytes` setting is set (> 0, see Settings), that pick is restricted to non-MTP artifacts whose estimated RAM fits the budget: the same two rules run over the fitting artifacts, a fitting artifact matched by neither rule is picked by the existing quality ordering (highest quality, then smallest size), and when nothing fits the smallest-RAM non-MTP artifact is recommended. With the setting unset (`0`/absent) the recommendation is byte-identical to the rule stated here, and MTP variants are never auto-recommended in either mode. A preferred local `mmproj` companion remains separately recommended and is bundled into the same request. Branch rows never displace this: even when EXL2/EXL3 branch refs are listed as selectable items (see below), `recommended_download` names a branch only when the main tree carries no recognized model payload by name/layout; branch rows are always available and downloadable, and file names never prove calibration.

`recommended_download` targets exactly one repository (`repo`, the analyzed repository). A recommended item whose `upstream_repo` is set is an mmproj companion discovered in an upstream base-model repository; its `files` paths are relative to that upstream repository, so they are omitted from `recommended_filters` and `recommended_download`. Download such a companion through its individual `selectable_items[]` entry, which retains `upstream_repo` and the exact upstream `files`. Recommended items from the analyzed repository are still included. If every recommended item is upstream, `recommended_download` is omitted.

### EXL2/EXL3 branch variants

For a model repository whose refs contain EXL2/EXL3 bitrate branches (for
example `4.00bpw`, `SC_6.00bpw_H6_V6`, `4_25`, or `exl/4_25`), `GET /api/analyze`
exposes each branch as a selectable item. Visibility depends only on the real
branch refs and the branch-name shape: the analyzer never reads main-tree file
content to decide which branches to offer, so weights, calibration artifacts,
or an unreadable header on main neither hide a branch nor fail the analysis:

```json
{
  "id": "exl-branch:4.00bpw-h6",
  "label": "4.00bpw-h6",
  "category": "quantization",
  "revision": "4.00bpw-h6",
  "branch_variant": true,
  "method": "exl3",
  "bits_per_weight": 4,
  "head_bits": 6,
  "recommended": true
}
```

`revision` is the branch to download, `branch_variant` distinguishes it from an
ordinary revision-backed selection, and `bits_per_weight`/`head_bits` describe
the branch metadata when it is available. One branch is marked `recommended`
only when the main tree carries no recognized model payload (by name/layout) —
the classic EXL branch layout, where main carries configs and docs while the
quantized payloads live on the bitrate branches. When the main tree carries
payload — base's name-layer weight-role recognition over the whole main
tree (names only; a safetensors-named file counts as payload by the
user-decided names-decide rule) — no branch row is
marked and the analysis's own output stands: a known format's file-based
`recommended_download` where it has one, no recommendation where its type
yields none. File names never prove calibration, so a calibration-looking or
renamed weight-shaped file counts as payload exactly like `model.safetensors`.
The branch rows themselves stay offered and downloadable either way, because
availability never depends on main-tree content. `method` and the widths are
best-effort display enrichment, never a visibility
condition: they come from the branch's own quantization config when it is
readable, otherwise from the main configuration, the repository name, or the
branch name. `method` is omitted when no EXL2/EXL3 evidence exists, and a known
non-EXL `quant_method` keeps it empty instead of guessing a flavor. The
per-branch metadata and size lookups are bounded; when they fail or time out,
only this display enrichment degrades — every recognized branch stays listed
and downloadable.

When a branch row carries `recommended` (main tree without recognized
payload, see above), `recommended_download` then names that branch and asks
for the whole
branch without file filters:

```json
{
  "repo": "owner/name",
  "revision": "4.00bpw-h6",
  "branchVariant": true
}
```

That is the whole body: a branch variant is unfiltered and model-only, so the
empty `dataset`, `filters` and `exactMatch` fields are omitted from the JSON
rather than sent as empty values.

GGUF, APEX, MTP, mmproj, and lineage behavior is unchanged.

Isolation is a property of `branchVariant` downloads: they never write the
shared friendly view, while their blobs and per-commit snapshots remain
content/commit isolated. A generic revision download of the same branch (for
example through the revision picker) remains an ordinary download and follows
the existing revisionless friendly-view behavior. The existing rebuild fallback
that mirrors an arbitrary snapshot when no main ref exists likewise may
populate the friendly view. Neither path overwrites the stored blobs or another
variant's per-commit snapshot.

### Analysis warnings

`analysis_warnings` is omitted when no warning applies. Otherwise it is an
array of bounded, non-fatal diagnostics:

```json
{
  "code": "both_namespace_detection_degraded",
  "message": "Both-namespaces (ErrBothExist) detection was degraded: the dataset-namespace existence probe failed, so this repository was not checked for also existing as a dataset (cause: Get \"https://huggingface.co/api/models/owner/name\": dial tcp: lookup huggingface.co: no such host). Retry analysis to try again.",
  "causes": ["probe_unavailable"],
  "retryable": true
}
```

`code` identifies the warning. The only code currently emitted is
`both_namespace_detection_degraded`, when the optional dataset-namespace
probe after a successful model listing failed, so both-namespaces
(`needsSelection`) detection is unverified for this analysis — the analysis
outcome is unchanged, only the warning is added. `causes` contains stable
reason codes, not upstream response bodies or URLs: `probe_timeout` and
`probe_unavailable`. The message carries the underlying failure text (an
expired tree-page budget keeps its actionable timeout text).
`retryable` indicates that repeating the same
analyze request may complete the optional work. A retry repeats the same
repository, namespace, and revision request; it does not change the selection
or start a download.

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

For Diffusers `selectable_items`, a `component` row whose folder holds weights
carries `files` = exactly that component's best weight set, chosen only among
eligible sets. A non-sharded file is a trivially complete one-member set; a
`<prefix>-NNNNN-of-MMMMM` shard family is eligible only when its present
indices are exactly the distinct set `1..M` — no gaps, no duplicates, no
out-of-range member such as `-00003-of-00002` — with the declared `M` equal to
the member count. Among eligible sets the class order is absolute: fp16
safetensors beat default safetensors, which beat pytorch `.bin` files; within
the winning class plain naming beats a precision-marked twin, and the
lexicographically smallest member set wins. Every shard of the chosen
`<prefix>-NNNNN-of-MMMMM.safetensors` family joins — half a family is never
selected — conversions never join (ONNX `.onnx`, OpenVINO IR: each `.xml` with
its same-basename `.bin`, Flax `.msgpack`, `.tflite`, and any weight file
under `onnx/`/`openvino/`/`flax/`/`tflite/` subfolders), and `size`/`size_human`
sum exactly those files. A component with no eligible set — a family split
across spellings (`m.fp16-00001-of-00002` beside `m-00002-of-00002`),
declared-total mismatches in either direction, index holes, or a
conversions-only folder — falls through to the legacy weightless folder
behavior unchanged: no `files`, the whole-folder size, and its folder-filter
request. The row label names the chosen set's variant (`Unet (fp16)`), and the
row downloads exactly `files` when sent with `exactMatch: true`; `filter_value`
stays the component name as the row's identity for legacy consumers, while
status keys follow the same exact-files gate as the request: a weighted row's
status key is `artifact:[…]` (the JSON array of its `files`, such as
`artifact:["unet/diffusion_pytorch_model.safetensors"]`), and only a
weightless row keeps the component-name token key. The losing sets stay
available as thin alternatives through the Files tab and manual filters, the
`variant` category rows (fp16/fp32/bf16 detection) are unchanged, and a
component without weights (tokenizer/scheduler/config-only folders) carries no
`files` and keeps its legacy folder-filter request. The same chosen set is
exposed as `diffusers.components[].files` in the analysis response.

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

Alongside the `split` rows, dataset analysis emits one `config` category item
per `detectConfig` name (the UI renders the `Dataset Configs` category
above `Dataset Splits`): `files` rolls up the config's member files, `label`
composes the config name with its splits in split-priority order (for example
`cc100 (train+test)`), `size_human` is the rolled-up member size, and both
`id` and `filter_value` are the config's directory selector — `cc100/` for a
root-level config folder, `data/cc100/` under a `data/` prefix — which, sent
with `exactMatch: true`, selects the whole config directory at a
path-component boundary like any other explicit directory filter. Grouping is
per `detectConfig` name, so a config whose members span `cc100/…` and
`data/cc100/…` yields ONE row; in that mixed-layout case the folder selector
falls back to the bare `cc100/` form, whose component-boundary match covers
both spellings. Exactly one
`config` or `split` item carries `recommended`: the unique config holding the
train split when there is one, otherwise the existing split-level rule
(the `train` split row), and no row when neither applies. Split rows, their
`filter_value`s, and dataset membership/ambiguity semantics are unchanged.

Quantized models expose a `quantized` projection in the response (also for
quantization-only repositories that have a root `quantization_config.json` but
no root architecture config, and for repositories whose `config.json` carries a
nested `quantization_config` object):

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
  "exactMatch": false,
  "branchVariant": false,
  "shardRanges": "1-100,120-185"
}
```

Returns the files that would be downloaded without starting a job. Each HTTP
response item exposes only `path`, `size`, and `lfs`; `path` is
repository-relative. The response does not include filter-attribution or
destination-prefix fields.

Successful responses to a filtered request may also carry
`notices`, an array of informational strings that is omitted when empty. A
notice reports files the plan deliberately did not select: when a filter
names a standard split and the tree contains files that mention that split
without belonging to it, one notice names how many were excluded, the split
label, the directories they live in, and at most five of the excluded paths
(stating how many more were left unnamed). An ineffective shard-range
selection adds its own notices to the same array (documented under
`shardRanges` below). A filter pattern that selected nothing while the
request's other filters kept the selection alive adds a
`filters_unmatched` notice naming the unmatched patterns (documented
below) — except that a pattern whose miss a split-mention notice already
reports is skipped there, so no pattern is ever reported twice. Notices
are deterministic and
non-fatal: they never change `files`, `totalSize`, `totalFiles`, or the
download outcome, and they never replace an error. The bundled client
renders them in the plan preview, and a download job reports the same
sentences in its `warnings` list (documented under Job below).

The public Go planning contract is broader. `hfdownloader.PlanRepo` returns a
`*Plan` whose `Items` are `PlanItem`s and whose `Notices` carry the same
informational strings; the same `Plan` value is passed to
`Settings.OnPlanComplete`. A `PlanItem` also exposes `DestinationSubdir`
(`destinationSubdir`) and `Subdir` (`subdir`). `DestinationSubdir` is the
candidate relative prefix selected by the matched filter; it is populated
independently of `appendFilterSubdir`, and the downloader applies it only when
that job flag is enabled, so Go consumers must check the flag before combining
it with `RelativePath`. `Subdir` is the matched-filter attribution where
applicable; it can be empty for ordinary non-LFS model files whose destination
remains repository-relative. The `/api/plan` response remains
repository-relative.

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

`shardRanges` (optional, string) narrows a filtered selection to specific
shards of split files. Syntax: comma-separated inclusive spans of 1-based
shard indices, each `N` or `N-M` (`1-100,120-185`, where `N` alone is the
single index `N`); whitespace around a segment is trimmed and segment order
does not matter. The field requires a non-empty `filters` selection. Within
the matched file set, files named in the established split form
`<prefix>-NNNNN-of-MMMMM.<ext>` are grouped per family (the path with the
shard token removed), ordered by the shard number written in their names and
indexed `1..M` in that order — so 1-based safetensors shards and 0-based
dataset shards both line up with the requested indices — and only shards
whose index falls inside the spans are planned. Ranges therefore address
positions in written-number order, not the written numbers themselves: for a
family whose written shard numbers contain gaps the positions and the
numbers differ (for complete families they coincide). Files that are not
shard-shaped (configs, index files, unsharded weights) are never affected,
and ranges only narrow: a filter that names an absent path keeps its
established `no_filter_match` behavior, and a filter whose files all fall
outside the ranges fails the same way when no matched file remains —
retained companion files that matched nothing never keep such a request
alive.

A malformed expression — an empty expression or segment, a non-numeric
token, a zero or negative index, a reversed span such as `10-5`, or ranges
without filters — is rejected with HTTP 400 and error code
`invalid_shard_ranges` before any Hub contact, identically on `POST
/api/plan` and `POST /api/download` (including `dryRun: true`), so a dry run
never accepts a request the real download rejects.

Ranges never fail silently: their diagnostics ride the established `notices`
array defined above (a download job then reports the same sentences in its
`warnings` list), never a separate channel. Two notices exist today:

- the matched set contains no shard files at all, so the ranges narrowed
  nothing — the notice names the whole expression, for example
  `shard ranges "1-5" matched no shard file, so nothing was narrowed`.
- specific spans select no existing shard index (for example `9-10`
  against a four-shard family) — the notice names exactly those spans, for
  example `shard range segments selected no shard (indices present: 1-4):
  9-10`. At most five spans are named; the rest are counted.

Notices are deterministic and non-fatal: they never change `files`,
`totalSize`, `totalFiles`, or the download outcome, and they never replace
an error.

A request whose filters select nothing at all keeps failing with
`no_filter_match`; but when the request's other filters still select files
and a pattern selected none, the plan carries a `filters_unmatched` notice
naming exactly the unmatched patterns — one sentence such as
`filter "q4" matched nothing`, or `filters "q4", "fp16" matched nothing`
when several are unmatched (at most five are named; the rest are counted).
Matching counts only **selected** files — a direct filter match (any
shape: substring, `.ext`, `./name`, `dir/`, a full path) or a
dataset-membership selection of the pattern's own label — so retained
companions never count, excludes apply before the credit, and a
membership selection can leave every direct pattern unselected and named.
filters_unmatched names patterns that matched no file at selection; a
pattern whose matches were all removed by `shardRanges` is not named (the
ranged result is visible in the plan preview, and an ineffective range is
named by the shard-range notices).
The notice never repeats a pattern whose miss the split-mention notices
above already report: that pinned coverage wins for the pattern, and this
producer extends to everything else (direct patterns, shaped selectors,
and dataset labels the split family does not cover), so no pattern is
ever reported twice. Like every notice it rides `notices` → a download
job's `warnings` list, and the bundled start flow shows it as a
non-blocking warning toast right after the download is queued.

### Strict file selection

`strictFiles: true` reinterprets `filters` as a strict manual file selection
(the file-tree picker contract): each filter is one complete
repository-relative **file** path, and the plan contains exactly those files
and nothing else. Directories are expressed as their member file paths; a
trailing-slash directory entry is rejected as non-canonical.

- Matching is exact-spelling (case-sensitive) over the complete path: a
  selected spelling either is the tree file spelled exactly that way or it is
  a miss — a case-variant file is never substituted for the selection.
  `exactMatch` has no effect. There is no substring widening, no basename
  aliasing (a single-segment entry names the root-level file only, never a
  same-named file in a subdirectory), no companion/metadata retention, no GGUF
  inference, and no dataset-membership selection.
- Entries are normalized before storage and identity comparison: `\`
  separators become `/`, a leading `./` root alias is stripped (the
  established `./name` request spelling), and repeated spellings collapse to
  one entry (`./a.bin` and `a.bin` are the same entry). Entries are otherwise
  used verbatim — no whitespace trimming and no case folding.
- The selection is all-or-nothing, and request-shape problems fail with `400`
  before any Hub contact on both endpoints:
  - empty `filters` → `400` `invalid_file_selection`
  - an `owner/name:file` colon shorthand combined with `strictFiles` → `400`
    `invalid_file_selection`, with a hint that the shorthand is not
    supported in strict mode — send the file paths in `filters`; the colon
    in `repo` is rejected this way whether or not `filters` is set
  - a non-canonical entry (empty, absolute, trailing `/`, `.`, `..`, or a
    spelling that does not clean to itself such as `a/../b`) → `400`
    `invalid_file_selection`, naming the entry
  - two entries that differ only in case (for example `a.bin` and `A.bin`) →
    `400` `invalid_file_selection`, naming both entries: on case-insensitive
    filesystems they are one file, so neither may be silently dropped
  - `excludes` set → `400` `invalid_file_selection` (`excludes` are substring
    patterns and contradict exactness)
  - `appendFilterSubdir: true` → `400` `invalid_file_selection` (selection
    paths are not destination directory names)
  - `branchVariant: true` → `400` `invalid_file_selection` (a branch variant
    requires an unfiltered request; a strict selection is a filter list)
- A well-formed path that does not exist at the revision spelled exactly that
  way fails `POST /api/plan` (and a `dryRun: true` download) with `400`
  `no_file_match`, naming the missing paths. A real download fails its job
  with the same error instead, matching how a no-match filter behaves on a
  real download.
- The normalized selection is part of job identity: an identical active strict
  request returns the existing job like any duplicate, a different selection is
  an independent job, and two strict requests differing only in `exactMatch`
  are one job — the flag is a no-op here, so it never splits strict identity.
  The job snapshot exposes the selection as `filters` plus `strictFiles` via
  `GET /api/jobs` and `jobs_state.json`.
- Storage mode, verification, resume, and revision pinning are unchanged;
  `strictFiles` narrows plan composition only.

## Download

```http
POST /api/download
```

Uses the same request shape as `/api/plan`. Starts or reuses a download job.

Notes:

- `repo` is required and must be `owner/name`.
- `revision` defaults to `main`.
- `shardRanges` participates in job identity like `filters`: requests that
  are identical except for their shard ranges start independent jobs, and an
  identical request (same repo, revision, destination, filters and ranges)
  reuses the running one. The job snapshot returned by `POST /api/download`,
  `GET /api/jobs` and `jobs_state.json` exposes the field as `shardRanges`,
  and the bundled Downloads view labels it on the job's chip. Planning,
  `dryRun: true` and the real download apply ranges identically; the syntax,
  validation error code and the notices a doomed range selection produces are
  documented under Plan above.
- `branchVariant` (optional) selects one whole EXL2/EXL3 branch named by
  `revision`. It is model-only and must be unfiltered: `filters` and `excludes`
  must be empty, and `appendFilterSubdir`, `localRepo`, and `dataset: true` are
  rejected with `400`. The revision must have the analyzer's branch shape and
  must also be a valid completion-manifest path component sequence:
  slash-separated, with no empty, `.`, or `..` component, no backslash or
  control characters, and no component longer than 255 bytes. The branch shape
  is either a bitrate token whose digits are immediately followed by `bpw`
  (for example `4.00bpw` or `SC_6.00bpw_H6_V6`, optionally with a head-bits
  token such as `-h6`; the digits must touch `bpw`, so `4_625-bpw-h6` is not a
  recognized branch shape), or a final path component of exactly the form
  `N_M` (for example `4_25` or `exl/4_25`; a component that merely starts with
  `N_M`, such as `4_25-bpw-h6`, is not recognized). A branch variant also
  requires the Hub revision lookup to resolve the branch to a commit, because
  the commit is what keys the stored artifact: the per-commit snapshot tree in
  HF cache mode and the completion record in both modes. When the lookup yields
  no commit, the job fails with
  `branch variant "<revision>" requires a resolved commit from the Hub`. The
  same failure applies in both storage modes, and it happens before anything
  is written to disk: no payload file, no completion record, and no directory
  are created.
  A branch download
  stores the whole revision in an isolated location, so two variants with
  identical file names cannot overwrite each other: HF cache mode uses the
  per-commit `snapshots/<commit>/` tree and content-addressed blobs (with no
  friendly-view entries), while local/flat mode writes into the app-owned
  reserved space
  `<root>/.hfdesk-branch-variants/<owner>/<repo>/payload/<revision>/`.
  That space lives outside the ordinary repository tree, so the ordinary
  payload and the branch payload never share a path: a repository whose own
  tree contains `.hfdesk-variants-payload/...` or `.hfdesk-variants/...` keeps
  those files, and a branch-variant download of the same repository does not
  replace them. The reserved directory is a top-level entry of the local root,
  so it is excluded from the ordinary local repository scan: it is never
  reported as an owner and nothing inside it is listed as a repository. The
  reserved name is reserved: an ordinary local download whose repository
  owner is the reserved name `.hfdesk-branch-variants` is accepted as a job
  and then fails with
  `repository owner "<owner>" is reserved for the app's branch-variant storage`
  instead of writing into the app's own space. The refusal is a job failure,
  not a `400` rejection at admission. A repository whose name is that
  string is not affected, because the space is one level deeper. The app-owned
  trees are verified before use, and a symlinked or non-directory app-owned
  entry fails the download. A completed variant manifest at
  `hub/models--<owner>--<repo>/.hfdesk-variants/<revision>/<commit>/hfd.yaml`
  (HF cache) or
  `<root>/.hfdesk-branch-variants/<owner>/<repo>/records/<revision>/<commit>/hfd.yaml`
  (local) records the exact revision, commit, and files. The manifest is keyed
  by revision and commit, so branches that share a commit keep separate
  records, and its local location is outside the payload tree, so a
  repository-owned `hfd.yaml` is never overwritten. `noManifest` only suppresses
  the optional friendly-view manifest; a branch variant's completion manifest
  is always written, because presence reads it. In HF cache mode a branch
  variant does not skip a file whose content-addressed blob is already being
  downloaded by another download: it waits for that shared download to
  resolve, and then materializes its own snapshot entry for the file, so the
  variant's completion record only ever lists files the branch actually holds.
  The wait is bounded: if the shared download does not resolve, the job fails
  instead of reporting the variant as downloaded, and if the shared download
  is gone or abandoned, the variant fetches the file itself. A branch variant
  that cannot create the snapshot entry for one of its files fails the job for
  the same reason: reporting the variant as downloaded would publish a variant
  that is not complete. After a local branch-variant
  plan succeeds and before any payload byte changes, the download invalidates
  the revision's prior local completion records: the app's own
  `records/<revision>/<commit>/hfd.yaml` records. A file at the same path that
  is not one of those records is left untouched, and the payload is never
  touched. If that invalidation fails, the download fails without touching the
  payload. A local re-download that fails or is cancelled after that
  invalidation reports `missing` instead of complete.
  A local branch-variant transfer stages its bytes in the app-owned,
  commit-scoped staging area
  `<root>/.hfdesk-branch-variants/<owner>/<repo>/staging/<revision>/<commit>/`
  and publishes them into the payload directory afterwards. The staging area
  is transient: it holds the partial bytes of one transfer (`.part`,
  `.part-NN`, `.parts.json`) until they are verified, and the verified file is
  then moved into
  `<root>/.hfdesk-branch-variants/<owner>/<repo>/payload/<revision>/<path>`
  with an atomic rename, so the payload path never holds a partially written
  file and the payload directory holds only the variant's own file names.
  Because the staging area is keyed by the resolved commit, a partial left
  behind by an interrupted transfer belongs to exactly one commit: a transfer
  of a moved branch starts from zero instead of resuming another commit's
  bytes, while a retry of the same commit resumes its own partial. The
  staging area lives inside the reserved space, so it is excluded from the
  ordinary local repository scan like the rest of that space, and it is
  verified like the other app-owned trees.
  A local branch-variant re-download reuses a published file only when its
  content hash matches the planned commit's digest: a file without a known
  digest is re-fetched, because equal size proves nothing. Together with the
  commit-scoped staging area, which keeps a moved branch from resuming another
  commit's bytes, this means a moved branch cannot retain same-size stale
  bytes. An ordinary download is unaffected: it keeps reusing a non-LFS file
  on size equality, and an LFS file with a known SHA on its SHA.
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
  repo routed to different destinations are distinct jobs. The same rule covers
  a strict manual selection: with `strictFiles: true` the normalized selection
  (see Strict file selection in the Plan section) is part of the identity, so
  an identical active strict request returns `200` with
  `"message": "Download already in progress"` while a different selection is an
  independent job; two strict requests differing only in `exactMatch` are the
  same request, since that flag has no effect in strict mode.

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
paths. A file that another live writer owns is not present, so it is never
counted as a completed skip: the item waits for that destination (bounded, up
to five minutes) and then downloads the file or fails with an actionable error.
Such a job reports `failed`, never `completed` with a missing file. A failed
link creation fails the affected item the same way, in the transfer path and in
both skip paths, so a job never reports `completed` with an unreachable path
either. A disabled friendly view is not an error. Skipped same-SHA items are
reconciled after all files finish and receive their own snapshot/friendly
entries. A waiting item does not occupy one of the job's active-download slots,
so other files proceed even with a low limit. Progress reports distinguish
transfer, finalizing, completion, skipped, error, and cancellation states, and
`file_finalizing` is emitted before the pre-publication proof. The detailed
ownership and resume contract is in `pkg/hfdownloader/doc.go`.

Job responses include `warnings`, an array of strings omitted when empty: the
downloader's non-fatal diagnostics for the job, one entry per distinct
message, in first-arrival order. A repeated diagnostic appears once — a
resumed or retried attempt re-runs the plan and the downloader emits the same
messages again, and a repeat adds no entry and consumes none of the bound. A
warning reports a fact about the download without failing it and without
changing the selection — for example a revision that is not pinned to an
immutable commit, a plan notice such as a split-precedence exclusion, an
ineffective shard range, or a filter pattern that matched nothing
(`filter "q4" matched nothing`), resume
state that cannot be used without HTTP range support (reported once per
download, naming the count and a bounded list of the affected files, not once
per file), or a best-effort metadata write (a ref, friendly directory,
rebuild script or manifest) that failed. The list is bounded: it keeps at
most 20 distinct warnings, and once
full a further distinct diagnostic is not added. It is distinct from
`destinationWarning`, which the server computes once when the job is created.

Cache-mode jobs include `hubDir`, the frozen exact Hub storage directory.
A strict manual selection is exposed on the snapshot the same way as every
other job field: `filters` holds the normalized selected paths and
`strictFiles` is `true`, so a client can render an exact "N files" label and
match the per-file progress entries (`files`) against the selection.
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

Job snapshots also expose `warnings`, an array of strings carrying the
plan's `notices` sentences plus the downloader's non-fatal diagnostics
described above, omitted when empty. The field is an output of the job's
scan, not part of its identity: it never participates in deduplication.

## Settings

```http
GET  /api/settings
POST /api/settings
```

Settings include runtime paths, concurrency, verification, endpoint, proxy settings, the HF cache directory, and extra local scan folders.

- `availableRamBytes` — optional RAM budget in bytes for the recommended GGUF quantization; `0` or absent means unset. When set (`> 0`), an analysis restricts the recommended quantization row (and therefore `recommended_download`) to the non-MTP artifacts whose estimated RAM fits the budget, picking the best fitting artifact by the established quality rules and falling back to the smallest-RAM artifact when nothing fits (see Analyze). A negative value is rejected with HTTP 400; sending `0` clears the budget, which is persisted as an absent key.

`POST /api/settings` is refused once settings admission is sealed, which
happens only in the graceful desktop-to-service handover. The seal comes
after the HTTP drain completes, so the observable contract of a normal
handover stays what it was: every update admitted before the seal finishes
normally, and the admitted writes drain and persist before the handover
reports success. The `503` below is a defensive barrier rather than an
outcome a client observes in a normal handover: it refuses anything that
would reach the settings handler after the seal, and the guarantee behind
it is the generation check in `CompleteHandover`, which reports the
handover as failed when the latest settings generation was not confirmed
persisted. A shutdown that is not a committed
handover — a plain desktop quit or the console host stopping — does not seal
admission: settings updates arriving during its HTTP drain are still accepted.
The response uses the standard error format above; `503` has no dedicated
entry in the status-to-code mapping, so `error_detail.code` is the generic
`server_error`:

```json
{
  "ok": false,
  "error": "Server is shutting down",
  "details": "settings cannot be changed during shutdown",
  "error_detail": {
    "code": "server_error",
    "message": "Server is shutting down",
    "details": "settings cannot be changed during shutdown"
  }
}
```

Download reliability settings:

- `verify` — the post-download verification mode, applied to LFS files as well: `none` performs no post-download content hash (HTTP and ranged-response validation still apply during the transfer); `size` (the default) checks the exact expected size and complete validated transfer coverage on both the multipart and single-request paths and performs no full-file SHA-256 reread on the normal transfer path (the pre-download skip check, an inherited single-path `.part` proof, an unpinned-identity proof, and a known digest with no known expected size may still hash); `sha256` verifies the full file against the known digest with at most one full-file read, always before the destination/blob is published when a known digest exists, so a mismatch never publishes corrupt bytes and the staging data is discarded (a retry re-downloads from scratch); when `sha256` has no known digest, the HEAD-ETag fallback remains a post-publication check; `etag` proves a known SHA-256 digest with that same single pre-publication pass, while non-LFS ETag-only files keep the historical no-post-check behavior. A known digest is never hashed only after publication. Resume state from an older or unsupported format is not migrated: only a valid version-4 ranged record, or a single-request `.part` with a matching pinned identity sidecar, is resumed (an exactly-sized partial is published without a request only when clean response framing is also recorded); the file is otherwise re-downloaded from zero while already-published content is untouched, while an unfenced older/unsupported record that cannot be proven dead is refused as unproven and preserved. The detailed proof and resume rules are in `pkg/hfdownloader/doc.go`.
- `retries` — the non-progress retry budget per file/part request. A non-truncating attempt that advances the on-disk resume offset strictly beyond the highest offset reached refunds this budget and resets the backoff. For a single-file transfer, a response that ignores Range with `200 OK` restarts the partial and does not refund the budget, even if that attempt writes beyond the previous high-water mark; the new high-water mark is still recorded, so a later bodyless failure earns no refund. Multipart transfers still require `206 Partial Content` and do not accept `200` as a part. Progress refunds can increase the number of retries, but a separate lifetime retry-count ceiling limits retries after individual attempts return. That ceiling does not bound the duration or bytes of a single request that continues delivering data.
- `stallTimeout` — a duration string (`"60s"`, `"2m"`). A body read that delivers no bytes for this long is aborted and retried from the current on-disk offset instead of hanging the job. An explicit `"0"` or `"0s"` disables the watchdog; empty preserves the current value. Invalid durations are rejected with `400` and are not persisted.
- The obsolete record-age `StaleTimeout` library setting is removed (`Settings.StaleTimeout`, `HFCache.StaleTimeout` and `DefaultStaleTimeout` no longer exist, and the cache constructors no longer take it): ownership is fenced by the destination ownership domain and a record's age never authorizes a takeover, so there is no replacement setting.
- Retry backoff defaults to 400ms initially and 10s maximum. Library callers may override these values; the Settings API does not expose them. When a `429`/`503` response carries a `Retry-After` header or the Hub `RateLimit` header, the downloader waits the larger of that server-requested time and its local backoff, capped at 5 minutes. Retries include `429` and all `5xx`; other `4xx` responses are terminal for that file, while `401`, `403`, and `404` fail the whole job with an actionable message (accept the repository terms / use a token with access, or file not found). Optional verification `HEAD` metadata failures are ignored; the actual file request remains authoritative.
- A completed cache blob is placed with a no-replace primitive, so an existing `blobs/<known-sha>` is not replaced by a domain participant; on a filesystem without hard links the placement is a check-then-rename performed while the blob key's ownership domain is held exclusively. If placement fails (for example a cross-device move), the cache store copies the payload to a staging file in the destination directory under the same domain and renames it into place; that final rename is not a no-replace primitive, and the fallback provides no crash durability or rollback guarantees. A writer that loses the race discards its own staging pair and reports the item complete. Every writer of one blob key, including both transfer routes and the exported `FinalizeBlob`, shares that one domain. The detailed publication tiers are in `pkg/hfdownloader/doc.go`.

Multipart transfers write ranged workers directly into one staging file and
resume from durable per-range metadata, including when the connection count
changes between runs, so finalization is an atomic rename plus link work, never
a full-file assembly copy. Never assemble or publish after cancellation or a
known part error. Resume state is trusted only when it matches the
resolved commit, repository, path and expected size, is pinned on both sides,
and tiles the file with normalized completion; a record whose payload is missing
or resized is discarded rather than published as zeros, and a staging file's
on-disk length is not completion evidence because staging files are preallocated.
The transfer is built from that resolved commit (tree walk and per-file URLs);
when the endpoint resolves no commit, an unpinned identity with a known digest is
content-proved once before publication. The ranged record is version 4 and is
the only resumable version. A fenced other-version record (or one whose owner is
provably dead or is this process) is discarded, never migrated, and the file is
re-downloaded from zero while already-published content is untouched; an
unfenced other-version record whose owner cannot be proven dead is refused as
unproven and preserved. Every pre-#140 `.part-NN`/`.parts.json` family is
likewise discarded and never migrated. Existing version-4 state keeps the
multipart route even below the
configured threshold; without range support the downloader warns, downloads the
full file through the single path, and removes the obsolete state only after the
final file is durable. The single-request `.part` is a sequential prefix
identified by a versioned sidecar; its bytes are inherited only under a matching,
pinned identity, and an exactly-sized partial is published without a request
only when clean framing at the exact expected byte count is recorded. Otherwise
the final byte is re-requested, and a body error during that revalidation leaves
it uncommitted for a later retry; unknown expected size means no resume. The
sidecar survives an interrupted run and is removed with its partial. Every 206
must match the exact requested `Content-Range`, expected total and body length,
writes are bounded to the requested range, and the file must have exactly the
expected size before the rename. The multipart route never imports a
single-route `.part`; a `.part` without a matching identity record is never
inherited. A durable proof-pending flag forces a re-proof on resume in every
mode except `none`. If
an LFS item remains unknown-size after HEAD, Download uses the single-path temp
and the normal cache-store path, so the blob and its snapshot/friendly entries
are created from the completed file.

Cache mode derives its ownership roots from the frozen storage associations:
Hub items use `H/.hfdesk-control/v1`, derived only from `HubDir` H, and
friendly-view items use `R/.hfdesk-control/v1`, derived from cache root R. They
are independent when H and R are independent; a cache link operation admits its
actual snapshot and friendly paths under the corresponding roots. Clients
sharing H converge without a per-user ownership setting — that statement is
cache-scoped: cache keys derive from the control tree persisted under H, not
from the destination path spelling. `OwnershipDir` is a
library `hfdownloader.Settings` option for flat `OutputDir` mode only, where
cross-host convergence is governed by the realm-relative conditions stated
under selected deletion below and is not implied by resolving the same
directory alone; cache-mode
use of a non-empty value is rejected rather than selecting another control
namespace. It is not an HTTP setting. Cache node IDs and profiles persist in a
typed control tree whose `metadata/` branches are separate from user components
in `children/`. The legacy cache constructors that accept `OwnershipDir` return
a cache whose ownership operations report the same configuration error. Current
writers create no payload-side lock siblings. In
particular a flat model named `foo.incomplete.lock` is an ordinary destination,
independent of `foo`.

An exclusive scope holder is waited for (bounded, context-aware, effective five
minutes), never taken over; unresolved reader contention also fails closed. A
waiting item returns its `MaxActiveDownloads` slot and reacquires it before
transfer. A writer that cannot acquire the applicable H/R cache scope or flat
realm fails before writing a payload and preserves existing partial data. Cache
operations that require cache-owned admission — whole-repository delete, mirror
removal and overwrite, selected HF deletion — likewise fail closed with `500`
(or `errors[]` for mirror) when the control tree cannot be established on the
filesystem, rather than mutating without exclusion.
Mixed-version writers from a branch-only build that still relied on obsolete
payload-side lock files are unsupported. New v4 records carry `LockFenced` and
an `OwnershipFence` bound to the current cache-root/key or flat realm/key;
`LockFenced` alone is not proof. A legacy, unknown or mismatched proof is
preserved/refused unless self-identity or same-host owner death independently
permits reclaim. Only a published content-addressed blob is completion proof by
existence; an arbitrary destination follows the authoritative skip decision.
Cleanup of staging artifacts, including obsolete pre-#140 parts, runs under the
applicable item scope and removes only route-owned artifacts. A route removes
only the artifacts it creates.

An unproven staging record (including a legacy `LockFenced`-only record, an
unknown/mismatched realm proof, or unreadable/unparseable current-format state)
is preserved, never taken over, and refused with
`ErrStagingOwnershipUnproven`; the operator removes the staging pair and retries,
after establishing that no client still uses it. Never remove persistent cache
metadata locks under an H/R `.hfdesk-control/v1` tree or flat-mode locks under
`<OwnershipDir>/v1/` while a participant may hold them.
`CheckBlob` reports
`BlobDownloading`, `BlobStale`, or `BlobOwnershipUnknown` plus an
`ErrBlobOwnershipUnknown` error; the incomplete payload is preserved and an
unknown state is never a successful skip. The exact domain keys, staging names
and publication tiers are in `pkg/hfdownloader/doc.go`.

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

Every scanned local root may contain the app's reserved
`.hfdesk-branch-variants` entry, which holds the local branch-variant payloads
and completion records. It is not a repository owner: the Cache browser never
reports it as an owner and never lists anything inside it as a repository, and
the single-repository lookup and the selection list skip it as well.

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

Every cache surface separates published content from the downloader's staging
artifacts with one exhaustive, name-based classification. Staging names include
the single-request `tmp-<name>` family (with `.part`, `.part.meta` and
`.lock`), the ranged `<name>.incomplete`/`.meta` pair and its temp, the
persistent `<name>.lock`, the obsolete pre-#140 `<name>.part-NN` and
`<name>.parts.json` family, the flat-mode partials, and the cache store's
`<name>.tmp-<random>` copy temp. A published blob is a canonical 64-hex SHA-256
name; any other name in `blobs/` is an unusual published name and is treated as
content. A `blobs/` name that carries a staging shape is treated as a staging
artifact by every surface, including deletion, even if it was published
content, so it is never counted, listed as cached content, copied, verified or
deleted. The exclusion is scoped to `blobs/`: a file outside that namespace
(for example a snapshot entry named `tokenizer.model.meta`) is content and is
both copied and verified by the mirror. A blob key that names a staging shape is
refused before placement by the public storage paths (`StoreDownloadedFile`,
`FinalizeBlob`), so a successful store always has a reachable result. The
canonical list and boundary rules are in `pkg/hfdownloader/doc.go`.

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
selected repository reference namespace and associated friendly view. For an
HF location this now also holds across processes: before the first unlink the
delete takes cache-owned admission over the repository tree and its friendly
view and holds it through the operation, so a live download in another process
sharing the cache refuses the request with the same `409`. The local-root
location kind gets the same protection through the flat ownership realm: entry
admission over the exact member paths is taken before any identity pin or
unlink and held through the deletion, so a flat-mode writer in another process
holding any member refuses with the same `409`. As for flat writers generally,
the flat realm's identity is an opaque ID persisted inside the shared
`Settings.OwnershipDir` (defaulting to the per-user `DefaultOwnershipDir()`;
`<OwnershipDir>/realm-id`) and created atomically under an exclusive
bootstrap gate on first use, so every participant resolving one `OwnershipDir`
reads the same realm ID. That first creation belongs to a single participant:
the bootstrap gate is an advisory lock of the same class as the cross-host
locks described below, so on a filesystem whose advisory locks are not
effective across clients — the same limitation that already bounds those
locks — two hosts can each mint their own ID on first use and will then never
exclude each other, silently, because their lock filenames differ; no safety is
claimed for this mechanism on such filesystems. Path-derived flat keys derive
from that realm ID and the item's path relative to the realm root, so
participants whose
`OwnershipDir` and items sit under one common shared mount exclude each other
across hosts regardless of where that shared root is mounted; resolving the
same `OwnershipDir` is necessary for that exclusion but is not on its own a
complete sufficient condition. The true boundary: a split topology (the
`OwnershipDir` and the items rooted under different mounts) requires
identical rel-token input on every host — the same structure below the lowest
common ancestor and the same descending component spellings — so
independently mounted split topologies that preserve that structure still
converge (the documented residual), and a
target on another volume keeps its absolute-spelling derivation, so same-host
behavior is unaffected in every shape; separately, the dev:ino alias keys
(`item-entry` and the other entry/object keys) and tree-validation identity
stay host-local on purpose for same-host bind-mount aliasing, so a foreign
host's staged `OwnershipFence` never reads as current proof there. The
record's fate follows the claim's two branches, not a blanket re-download: an
UNPROVEN record (a foreign host — the host mismatch defeats death evidence —
or a same-host live or mixed-version owner that cannot be reclaimed) is
preserved and refused with `ErrStagingOwnershipUnproven`; nothing is
re-downloaded automatically, its bytes are never silently reused, and the
operator removes the named staging pair and retries. A RECLAIMABLE record
(the owner proven dead or this process, including one fenced under the former
flat keys) is taken over with its fence re-bound to the current proof and its
committed ranges inherited — resumed, not re-downloaded — and only a version,
range, size or identity mismatch discards it and restarts from zero. That
shared directory is the flat realm's coordination boundary, not
a new restriction. Lock files derived from
the former host-local (device/inode) realm identity, or from the former
absolute-path-spelling flat keys, are harmless residue and
are never unlinked; mixed-version writers deriving either identity remain
unsupported. Local selected-delete admission uses complete frozen plan
destinations when available. A matching run activity with a known and complete plan is
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
whole-repository operation. It returns `409` without removing anything while an
active download or another cache operation is using that repository or its
friendly view, in this process or in another process sharing the cache; when
cache ownership cannot be established on the filesystem it fails closed with
`500` instead of removing anything.

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

### Branch-variant presence

A qualified lookup reports whether one EXL branch revision is completely
downloaded:

```http
GET /api/cache/{owner}/{repo}?branchVariant=true&revision=4.00bpw
```

Optional `routeKey` and `localDir` resolve the destination exactly like
`POST /api/download`; either explicit selector checks only that destination.
When both are omitted, the default destination is checked first and then every
configured download route, returning the first complete variant. If none is
complete, the default destination's `missing` response is returned. `type`,
when present, must be `model`. The response is always `200` for a valid query,
with `downloaded: false` when the variant is absent or incomplete:

```json
{
  "repo": "owner/name",
  "type": "model",
  "branchVariant": true,
  "revision": "4.00bpw",
  "downloaded": true,
  "downloadStatus": "complete",
  "storageMode": "hf-cache",
  "destinationRoot": "/cache/hub/models--owner--name",
  "commit": "2222222222222222222222222222222222222222",
  "artifactPath": "/cache/hub/models--owner--name/.hfdesk-variants/4.00bpw/2222.../hfd.yaml",
  "expectedFiles": 3,
  "files": [{"name": "config.json", "size": 123}]
}
```

In local mode the same response reports the app-owned payload directory as
`destinationRoot` and the completion record as `artifactPath`:

```json
{
  "repo": "owner/name",
  "type": "model",
  "branchVariant": true,
  "revision": "4.00bpw",
  "downloaded": true,
  "downloadStatus": "complete",
  "storageMode": "local",
  "destinationRoot": "/models/.hfdesk-branch-variants/owner/name/payload/4.00bpw",
  "commit": "2222222222222222222222222222222222222222",
  "artifactPath": "/models/.hfdesk-branch-variants/owner/name/records/4.00bpw/2222.../hfd.yaml",
  "expectedFiles": 3,
  "files": [{"name": "config.json", "size": 123}]
}
```

`downloadStatus` is `complete` or `missing` for this endpoint. HF cache mode
reads `refs/<revision>` and requires the revision/commit manifest at
`.hfdesk-variants/<revision>/<commit>/hfd.yaml` to match `branchVariant`, `repo`,
`branch == revision`, `commit`, a non-zero completion time, `total_files > 0`,
and `len(files) == total_files`; every snapshot entry must resolve to a regular
file of the recorded size. Local mode reads the completion records under
`<root>/.hfdesk-branch-variants/<owner>/<repo>/records/<revision>/<commit>/` and
considers only the newest valid record for the revision (latest completion time,
then lexicographic commit); it checks every file of that record against the
revision payload directory
`<root>/.hfdesk-branch-variants/<owner>/<repo>/payload/<revision>/`, which
`destinationRoot` reports in local mode. The completion record lives in the
records tree beside the payload tree, never inside it, and the whole reserved
space lives outside the ordinary repository tree. If the newest record's
files are not all present at the recorded sizes, the revision reports
`missing`; older records are not consulted, because local records carry no
content hash and cannot prove that a replaced or mixed payload still matches
them. An ordinary `GET /api/cache/{owner}/{repo}` without `branchVariant=true` is
unchanged.

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
as R and adds local `effectiveHubDir` and `hubDirSource`. A plain mirror
destination gains a hidden `.hfdesk-control` control directory that holds the
cache-owned admission state below. A local `DELETE /api/cache-selection`
similarly creates the flat ownership realm directory
(`<DefaultOwnershipDir()>/v1/`) when it does not exist yet, exactly as a flat
download does.

The mirror's scan, copy and verification use the same staging-artifact
classification as the cache surfaces, scoped to the `blobs/` namespace: a
staging artifact is never copied, counted or verified, so an unfinished
download or legacy residue is never published as ready content. Copy and
verification cover the same set, so a legitimate file outside `blobs/` (a
snapshot entry named `tokenizer.model.meta`, for example) is copied and
verified. A published blob is verified by size, a snapshot entry by its link
target, and an unusual published blob name is copied and verified like any
other content. Each mirror file copy takes cache-owned entry admission for its
destination file and each `deleteExtra` removal takes cache-owned tree
admission over the destination repository, so a live writer in any process
sharing that cache is refused first: an in-place overwrite reports the refusal
in `errors[]` with `success: false` and leaves the destination file intact, and
a refused removal does the same instead of deleting. The copy itself remains a
non-atomic truncate-and-write checked only by `io.Copy`'s error contract: its
copied byte count is discarded, an explicit size comparison happens only in the
opt-in `verify` pass, and that verification compares size rather than content
hashes. It does not use the downloader's no-replace publication tiers. The
mirror's symlink-leaf replacement takes entry admission and its directory
creation takes the shared admitted directory-creation class (a directory
that already exists needs no admission because nothing is mutated). A
destination repository that vanishes between scan and
deletion counts as deleted (at its scan-time size), like `os.RemoveAll`, not as
an error; `deleted`/`deletedSize` count only repositories actually removed, and
`copied`/`copiedSize`/`repos` count only repositories whose copy (and
verification, when requested) completed without error. The canonical
classification is in `pkg/hfdownloader/doc.go`.

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
