# Contributing to HFDesk

## Branch model

`main` is the protected default branch:

- direct push is disabled when branch protection is enabled
- every change should go through a Pull Request
- wait for CI/checks and review before merging
- emergency hotfixes should still prefer a short-lived branch and PR

This keeps the history clear and reduces the risk of accidental pushes to the
wrong branch.

## Workflow

```bash
# always start from fresh main
git checkout main
git pull --ff-only

# branch per change
git checkout -b fix/short-description     # or feature/, chore/, docs/
# ... edit, commit ...
git push -u origin fix/short-description

# open PR (via gh CLI)
gh pr create --fill
# merge after CI/checks and review pass
gh pr merge --squash --delete-branch
```

Branch prefixes are a loose convention, not a hard rule:

| Prefix | When |
|---|---|
| `feature/` | new functionality |
| `fix/` | bug fix |
| `chore/` | tooling, deps, CI, refactor without behavior change |
| `docs/` | docs only |

## Commit messages

Commits should use one standard template.

Template:

```text
<scope>: <imperative summary>
```

Rules:

- English only
- lowercase scope
- short imperative summary
- no trailing period
- keep it specific to one logical change

Preferred scopes are based on the touched area:

| Scope | When |
|---|---|
| `server` | `internal/server/` changes |
| `downloader` | `pkg/hfdownloader/` changes |
| `smartdl` | `pkg/smartdl/` changes |
| `ui` | `internal/assets/static/` changes |
| `cmd` | `cmd/hfdesk/` CLI/server entrypoint changes |
| `workflow` | GitHub Actions / CI |
| `docs` | README, docs, changelog, contributing changes |
| `tests` | test-only changes |
| `fix(<area>)` | focused bug fix when that reads better |

Examples:

```text
downloader: resume multipart downloads after retry
server: persist updated proxy settings
ui: show finalizing status on job cards
fix(downloader): discard stale multipart parts on layout change
docs: update API examples
```

## Pull Request titles

Pull Request titles should follow the same template as commit messages.

Template:

```text
<scope>: <imperative summary>
```

Examples:

```text
downloader: resume multipart downloads after retry
server: persist updated proxy settings
fix(ui): refresh queued job actions
```

PR body is free-form, but should usually include:

- summary
- why
- validation
- risk or possible regressions

Use the repository PR template checklist and include manual smoke testing when UI
or download behavior changes.

Co-author trailers are welcome when AI agents contributed:

```text
Co-Authored-By: opencode <noreply@opencode.ai>
```

## Local development

```bash
# run the app
go run ./cmd/hfdesk

# build the binary
go build -o hfdesk ./cmd/hfdesk

# run tests
go test ./...

# optional race check
go test ./... -race
```

Before opening a PR, run the checks relevant to your change:

```bash
gofmt -w <changed-go-files>
go test ./...
```

For UI or downloader changes, also do a quick manual smoke test in the browser.

## Project structure

| Path | What |
|---|---|
| `cmd/hfdesk/` | application entrypoint |
| `internal/server/` | HTTP API, jobs, settings, cache browser |
| `internal/assets/static/` | embedded frontend assets |
| `pkg/hfdownloader/` | Hugging Face scan/download/cache logic |
| `pkg/smartdl/` | model analysis and file selection helpers |
| `docs/` | API docs and screenshots |
| `.github/workflows/` | CI, release, Docker workflows |

## Configuration and state

HFDesk reads `hfdesk.json`, `hfdesk.yaml`, or `hfdesk.yml` from the launch
directory first, then from the per-user config directory. Settings saved from
the UI are persisted through the server config path.

Do not commit personal config files, Hugging Face tokens, cache contents,
downloaded models, or generated binaries.

## License

This project is licensed under the Apache License 2.0. See the root `LICENSE`
file for details.
