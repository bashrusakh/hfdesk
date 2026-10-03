# HFDesk

<p align="center">
  <strong>A focused local dashboard for finding, analyzing, and downloading Hugging Face models.</strong>
</p>

<p align="center">
  <a href="https://github.com/bashrusakh/hfdesk/releases"><img src="https://github.com/bashrusakh/hfdesk/actions/workflows/release.yml/badge.svg" alt="Release"></a>
  <a href="https://pkg.go.dev/github.com/bashrusakh/hfdesk"><img src="https://img.shields.io/badge/Go-1.24-00ADD8?style=for-the-badge&logo=go&logoColor=white" alt="Go 1.24"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-Apache--2.0-22c55e?style=for-the-badge" alt="Apache 2.0"></a>
</p>

<p align="center">
  <img src="docs/screenshots/hfdesk-models.png" alt="HFDesk Models view" width="900">
</p>

HFDesk runs as a small local web server and gives you a desktop-style browser UI for the full model workflow: search the Hub, inspect GGUF quantizations, pick files, download with resume support, browse local cache, and mirror model storage to another drive.

## Screenshots

| Models | Active Jobs |
|---|---|
| ![Models](docs/screenshots/hfdesk-models.png) | ![Active Jobs](docs/screenshots/hfdesk-jobs.png) |

| Local Cache | Mirror |
|---|---|
| ![Local Cache](docs/screenshots/hfdesk-cache.png) | ![Mirror](docs/screenshots/hfdesk-mirror.png) |

| Settings |
|---|
| ![Settings](docs/screenshots/hfdesk-settings.png) |

## Highlights

- Search models and datasets directly from the Hugging Face Hub.
- Analyze repositories before downloading, including GGUF quantizations, file groups, RAM estimates, and recommended picks.
- Correctly groups sharded GGUF files into one quantization option.
- Parallel resumable downloads with retries, progress events, and active job tracking.
- Standard Hugging Face cache layout or LM Studio-style local files under `<folder>/<owner>/<model>`.
- Optionally route downloads into per-type folders (LLM/GGUF, LLM/Safetensors, Audio, Diffusion, Embedding).
- Local cache browser for HF cache, friendly folders, and user-added LM Studio-style model directories.
- Mirror cache contents to a NAS, USB drive, or another machine.
- Download history, disk-free indicator, proxy support, and optional basic auth.
- Single static binary with embedded web assets.

## Install

Download a prebuilt binary from [Releases](https://github.com/bashrusakh/hfdesk/releases), or install from source:

```bash
go install github.com/bashrusakh/hfdesk/cmd/hfdesk@latest
```

Docker:

```bash
docker run --rm -p 8080:8080 \
  -v hfdesk-data:/data \
  ghcr.io/bashrusakh/hfdesk:latest
```

HFDesk keeps all writable state (settings, jobs, history, HF cache, and local
models) under a single `/data` root. Mount one volume at `/data` to persist
everything. See [Docker](#docker) for UID/GID options.

## Quick Start

```bash
hfdesk
```

HFDesk opens [http://localhost:8080](http://localhost:8080) automatically.

Useful options:

```bash
hfdesk --no-open
hfdesk --port 9090
hfdesk --token hf_xxx
hfdesk --cache-dir /mnt/ssd/huggingface
hfdesk --local-dir /mnt/models
```

## Configuration

HFDesk reads `hfdesk.json`, `hfdesk.yaml`, or `hfdesk.yml` from the launch directory first, then from `~/.config`. Settings saved from the UI are written back to the launch directory.

Ordinary settings updates preserve both the active Hugging Face token and any separately stored token. A runtime override such as `--token` is not copied to disk implicitly. In `POST /api/settings`, an explicitly authored nonempty `token` sets the active and stored credential; `"token": ""` clears both. Omitting `token`, sending `null`, or returning a redacted display value preserves it. Values beginning with `********` are reserved redaction markers, not credentials. Startup overrides still take precedence after a restart.

Saved configuration contains explicitly stored credentials in plaintext. New and existing files saved by HFDesk have owner-only permissions (`0600`) on Unix. A persistence warning means settings took effect in memory but were not saved; repair the file/path permissions or invalid configuration and save again before restarting.

```json
{
  "token": "hf_xxx",
  "cache-dir": "/mnt/ssd/huggingface",
  "connections": 8,
  "max-active": 3,
  "multipart-threshold": "32MiB",
  "verify": "size",
  "retries": 4,
  "endpoint": "https://huggingface.co"
}
```

The Hugging Face token is resolved as the `--token` flag, then the `HF_TOKEN` environment variable, then the `token` field in the config file. Set it at startup with:

```bash
HF_TOKEN=hf_xxx hfdesk
```

### Hub storage and friendly folders

`--cache-dir` / saved `cache-dir` selects the app root for friendly `models/`
and `datasets/` folders. Without that preference, `HF_HOME` or the existing
`~/.cache/huggingface` default supplies the root. Hub repositories normally live
in its `hub/` child, but nonempty `HF_HUB_CACHE` always selects the **exact Hub
directory**, even with an explicit app root:

```bash
HF_HUB_CACHE=/mnt/shared-hf-store hfdesk --cache-dir /mnt/friendly-views
```

Those roots need not be adjacent. Changing the app root does not move Hub files
or unset ENV. Server storage ENV/defaults are captured at startup; current cache,
mirror, rebuild, delete, and disk checks use that association. Existing jobs
freeze both destinations through pause/retry/requeue and restart. Legacy jobs
without a recorded Hub path use a warned, one-time startup fallback: their old
physical Hub cannot be recovered from the state file. No files are moved.

The Settings API separately exposes raw preference, effective Hub path, and its
source; see [API storage fields](docs/API.md#settings). Library `NewHFCache`
captures ENV per construction, while `Settings.HubDir` / `NewHFCacheResolved`
allow callers to reuse an already resolved association without reinterpreting
Hub ENV. Generated rebuild scripts preserve that association; ordinary app-root
scripts remain portable with the complete root, while external-H scripts retain
the selected absolute Hub location.

Proxy example:

```json
{
  "proxy": {
    "url": "socks5://proxy.internal:1080",
    "username": "user",
    "password": "pass",
    "no_proxy": "localhost,127.0.0.1"
  }
}
```

## API

HFDesk's web UI is backed by a local JSON API. See [docs/API.md](docs/API.md) for endpoints, request shapes, and response formats.

## Build

```bash
git clone https://github.com/bashrusakh/hfdesk
cd hfdesk
go build -o hfdesk ./cmd/hfdesk
./hfdesk
```

Run tests:

```bash
go test ./...
go test ./... -race
```

## Docker

The image keeps every writable path under one mounted data root, `/data`:

| Path | Contents |
|---|---|
| `/data/.config/HFDesk` | Settings and job/history state |
| `/data/.cache/huggingface` | Hugging Face cache (`HF_HOME`) |
| `/data/Models`, `/data/Datasets` | LM Studio-style local downloads |

Run it with a single volume:

```bash
docker run --rm -p 8080:8080 \
  -v hfdesk-data:/data \
  ghcr.io/bashrusakh/hfdesk:latest
```

### Run as a specific UID/GID (NAS/homelab)

The default image user is UID/GID `1000`. To run as another user and keep
files owned by that user, set `PUID`/`PGID`:

```bash
docker run --rm -p 8080:8080 \
  -e PUID=1026 -e PGID=100 -e UMASK=002 \
  -v /mnt/user/appdata/hfdesk:/data \
  ghcr.io/bashrusakh/hfdesk:latest
```

- `PUID` / `PGID` — UID/GID the app process runs as (default `1000`).
- `UMASK` — file creation mask (default `022`).

The container starts as root only so the entrypoint can apply these values,
then drops privileges; the app always runs non-root.

### Run as an arbitrary UID (enterprise / Kubernetes)

Pass `--user` (or set `securityContext.runAsUser`/`fsGroup`) and the entrypoint
skips user/ownership changes entirely. Because all state lives under `/data`,
no image user or `/etc/passwd` entry is required:

```bash
docker run --rm --user 568:568 -p 8080:8080 \
  -v hfdesk-data:/data \
  ghcr.io/bashrusakh/hfdesk:latest
```

```yaml
securityContext:
  runAsUser: 568
  runAsGroup: 568
  fsGroup: 568
```

### Build your own

Match the image user to your host user at build time:

```bash
docker build --build-arg UID=$(id -u) --build-arg GID=$(id -g) -t hfdesk .
```

### Migrating from the old cache path

Older images stored the cache at `/home/hfdesk/.cache/huggingface`, so the
documented `-v ~/.cache/huggingface:/root/.cache/huggingface` mount never
persisted anything. The cache now lives at `/data/.cache/huggingface`. Point
your volume at `/data` (recommended), or move existing data:

```bash
mv ~/.cache/huggingface /path/to/your/data/.cache/huggingface
```

## Credits

HFDesk is a fork of [bodaay/HuggingFaceModelDownloader](https://github.com/bodaay/HuggingFaceModelDownloader). Original copyright and license notices are retained. Licensed under the [Apache License 2.0](LICENSE).
