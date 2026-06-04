<p align="center">
  <img src="docs/screenshots/web-dashboard.png" width="640" alt="HFDesk – Analyze view" />
</p>

<h1 align="center">HFDesk</h1>

<p align="center">
  A local web dashboard for searching, analyzing, and downloading models from the Hugging Face Hub.
</p>

<p align="center">
  <a href="https://github.com/bashrusakh/hfdesk/releases"><img src="https://img.shields.io/github/v/release/bashrusakh/hfdesk?style=flat-square&color=4f46e5" alt="Latest release" /></a>
  <a href="https://pkg.go.dev/github.com/bashrusakh/hfdesk"><img src="https://img.shields.io/badge/Go-1.22+-00add8?style=flat-square&logo=go&logoColor=white" alt="Go version" /></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-Apache%202.0-22c55e?style=flat-square" alt="Apache 2.0" /></a>
</p>

---

Run `hfdesk` and get a focused browser UI for the entire model workflow — search the Hub, inspect GGUF quantizations with RAM estimates, start parallel resumable downloads, browse your local cache, and mirror it to other drives.

## Screenshots

| Analyze | Local Cache |
|---|---|
| ![Analyze](docs/screenshots/web-dashboard.png) | ![Local Cache](docs/screenshots/web-dashboard2.png) |

| Mirror |
|---|
| ![Mirror](docs/screenshots/web-dashboar3.png) |

## Features

- **Search** models and datasets on the Hugging Face Hub directly from the UI.
- **Analyze before downloading** — GGUF quantizations are listed with star ratings, RAM estimates, and a recommended pick. Sharded files are grouped as one option.
- **Parallel resumable downloads** — multipart HTTP, configurable concurrency, retry with backoff. Downloads survive interruptions and continue where they left off.
- **HF cache layout** — files land in the standard `~/.cache/huggingface` structure, fully compatible with `transformers`, `ollama`, and other tools.
- **Local-dir mode** — write real files into any folder instead of the cache layout (`--local-dir`).
- **Cache browser** — see every model and dataset stored locally, with size, commit, and download status.
- **Mirror** — push or pull your cache to a NAS, USB drive, or second machine. Diff, verify, force-resync.
- **Proxy support** — HTTP, HTTPS, and SOCKS5 proxies with optional auth and per-domain bypass.
- **Download history** and disk-free indicator in the status bar.
- **Basic auth** for the web UI when running on a shared server.
- Single static binary — no runtime dependencies.

## Install

**From source (Go 1.22+)**

```bash
go install github.com/bashrusakh/hfdesk/cmd/hfdesk@latest
```

**Binary releases**

Download a pre-built binary for your platform from the [Releases](https://github.com/bashrusakh/hfdesk/releases) page.

**Docker**

```bash
docker run -p 8080:8080 \
  -v ~/.cache/huggingface:/root/.cache/huggingface \
  ghcr.io/bashrusakh/hfdesk:latest
```

## Quick Start

```bash
hfdesk
```

Open **http://localhost:8080** in your browser.

```bash
hfdesk --open           # launch and open the browser automatically
hfdesk --port 9090
hfdesk --token hf_xxx   # required for private or gated repos
hfdesk --cache-dir /mnt/ssd/huggingface
hfdesk --local-dir /mnt/models   # flat files instead of HF cache layout
```

## Configuration

HFDesk reads a config file on startup and the UI settings panel writes back to it. Supported formats: `hfdesk.json`, `hfdesk.yaml`, `hfdesk.yml`.

Search order: current directory → `~/.config/`.

```json
{
  "token": "hf_xxx",
  "cache-dir": "/mnt/ssd/huggingface",
  "connections": 8,
  "max-active": 3,
  "multipart-threshold": "32MiB",
  "verify": "size",
  "retries": 4,
  "endpoint": "https://hf-mirror.com"
}
```

| Key | Default | Description |
|---|---|---|
| `token` | — | HF access token |
| `cache-dir` | `~/.cache/huggingface` | Cache root (or `HF_HOME` env) |
| `connections` | `8` | Parallel HTTP connections per file |
| `max-active` | `3` | Simultaneously downloading files |
| `multipart-threshold` | `32MiB` | Minimum size for multipart download |
| `verify` | `size` | `none` / `size` / `sha256` |
| `retries` | `4` | Retry attempts per request |
| `endpoint` | `https://huggingface.co` | Custom mirror URL |

### Proxy

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

## Development

```bash
git clone https://github.com/bashrusakh/hfdesk
cd hfdesk
go build -o hfdesk ./cmd/hfdesk
./hfdesk --open
```

Run tests:

```bash
go test ./...
go test ./... -race
```

## Credits

HFDesk is a fork of [bodaay/HuggingFaceModelDownloader](https://github.com/bodaay/HuggingFaceModelDownloader). Licensed under the [Apache License 2.0](LICENSE).
