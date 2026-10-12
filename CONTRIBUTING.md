<!-- generated-by: repo-docs-sync; file: CONTRIBUTING.md -->

# Contributing to hfdesk

## Project Context
Repository: `bashrusakh/hfdesk` — default branch: `main` — observed commit: `501ee25078d14bc000cef7fc92c972e5649d1fcc`.

Project purpose: Not recorded in the collected context.

## Branch Model
Use this stable policy:
- `main` is the protected default branch when it is the repository default branch.
- If the repository default branch has another name, treat that default branch as protected.
- Direct push to the default branch is disabled.
- Force-pushes and branch deletion on protected branches are disabled.
- Every change goes through a Pull Request.
- One approving review is required when the repository policy requires review.
- Emergency hotfixes may temporarily change branch protection only via GitHub UI/admin action.
- This protects against accidental pushes to the wrong branch and keeps a clean history.

## Workflow
```bash
git checkout main
git pull --ff-only
git checkout -b feature/short-description
# or fix/, chore/, docs/
# edit, commit, validate
git push -u origin feature/short-description
gh pr create --fill
```
Replace `main` with the repository's actual default branch when needed. Merge happens after review and CI/checks pass.

Branch prefixes:
- `feature/` — new functionality
- `fix/` — bug fix
- `chore/` — tooling, dependencies, CI, or refactor without behavior change
- `docs/` — documentation only

## Getting Started
Not recorded in the collected context.

## Dev Commands
**install via go** — working directory `.`; evidence: `README.md`

```bash
go install github.com/bashrusakh/hfdesk/cmd/hfdesk@latest
```

**run Docker container** — working directory `.`; evidence: `README.md`

```bash
docker run --rm -p 8080:8080 -v hfdesk-data:/data ghcr.io/bashrusakh/hfdesk:latest
```

**run locally** — working directory `.`; evidence: `README.md`

```bash
hfdesk
```

**run with custom port** — working directory `.`; evidence: `README.md`

```bash
hfdesk --port 9090
```

**run with token** — working directory `.`; evidence: `README.md`

```bash
hfdesk --token hf_xxx
```

## Before Submitting
**run all tests** — working directory `.`; evidence: `README.md`

```bash
go test ./...
```

**run race detector** — working directory `.`; evidence: `README.md`

```bash
go test ./... -race
```
- Check git status.
- Review the full diff.
- Make sure only intended files are included.
- Run the narrowest relevant tests/checks from repository evidence.
- If tests cannot run, report the exact command and exact error.

## Code Style

- Keep changes focused.
- Prefer clear, simple code over clever code.
- Avoid unrelated refactors.
- Do not silently change behavior outside the task scope.
- Reuse existing project style and architecture.
- Do not introduce new dependencies, frameworks, tooling, generated files, or large artifacts unless the change explicitly requires it.
- Do not commit secrets, tokens, credentials, local-only config, logs, cache files, benchmark outputs, or generated runtime artifacts unless explicitly required.

## Commit Messages
Use this convention:
```text
scope: short imperative summary
```
- English only.
- Lowercase scope.
- Short imperative summary.
- No trailing period.
- Keep each commit specific to one logical change.
Observed commit-message scopes:
- `server` — Changes to internal/server (commit messages)
- `downloader` — Changes to pkg/hfdownloader (commit messages)
- `smartdl` — Changes to pkg/smartdl (commit messages)
- `ui` — Changes to embedded static UI (commit messages)
- `cmd` — Changes to CLI entrypoint (commit messages)
- `workflow` — GitHub Actions or CI changes (commit messages)
- `docs` — Documentation updates (commit messages)
- `tests` — Test code modifications (commit messages)
- `fix` — Bug fix with optional area (commit messages)

## Pull Requests
PR titles should be concise and describe the change. When the repository uses scoped PR titles, use:
```text
scope: short imperative summary
```
The PR body should include, at minimum:
- summary
- why/context
- validation/tests run
- screenshots or manual verification steps for UI changes, when relevant

This PR guidance is independent of the commit-message convention above. Do not import an unrelated title grammar or require a scope when the repository does not use scoped PR titles.
Do not mark work as ready if checks are unknown or failing.
AI-assisted contributions should include a co-author trailer when appropriate:

```text
Co-Authored-By: opencode
```

## Triage labels
Labels are triage metadata, not a correctness determination or merge approval. Apply the correct type label when opening an issue or pull request, subject to available labels.

Apply exactly one type label:
- `bug` — something is broken or behaves incorrectly.
- `enhancement` — new functionality or a feature request.
- `documentation` — documentation-only change or doc issue.
- `question` — a usage or support question.
- `refactor` — internal change with no behavior change.
- `ci` — CI, build, or tooling change.

Automation-managed labels (do not apply by hand): `needs-info` and `duplicate`.
`confirmed` is not automation-managed. It records human or verification-owned confirmation and must be applied only by maintainers or authorized triagers after relevant verification.
`priority-*` and other operational labels are maintainer-owned.

## Issues
Before opening an issue, verify the problem against the current code/state and search existing issues if possible. Describe facts only; do not invent root cause. If root cause is only suspected, label it as a hypothesis. Include the category's relevant reproduction, expected behavior, affected component, environment/version, evidence, and constraints. Keep the issue actionable and specific.

## Documentation Maintenance
Project structure changes over time. AGENTS.md and CONTRIBUTING.md are guidance, not a replacement for checking the actual tree. If project structure, commands, architecture, test flow, contribution rules, release flow, or important entry points change, update the relevant Markdown file in the same PR. Keep docs useful and stable, and keep Markdown updates small. Do not duplicate the full file tree unless the project explicitly requires it. Describe stable conventions and important entry points, not every file.

## Releases
Every release must include meaningful release notes. Before publishing, check previous notes, review changes since the previous tag, and use actual commits, merged PRs, linked issues, and final code changes. Do not invent features, fixes, impact, or breaking changes. Call out user-visible, breaking, migration, compatibility, security, benchmark/default, and data-format changes when relevant. If a release is created automatically by GitHub Actions, its body must still be reviewed and updated before completion. A GitHub Release with an empty, placeholder, too thin, or unreviewed auto-generated-only body is considered unfinished, even if the tag and assets were created successfully. Before marking release work as done, verify the GitHub Release page contains the correct tag, title, body, and assets.

## Project Structure
Components and important paths observed in the repository:
- **hfdesk binary** — Compiled Go binary built in Dockerfile (paths: `Dockerfile`; evidence: `Dockerfile`)
- **cmd/hfdesk/main.go** — Application entrypoint CLI/server (paths: `cmd/hfdesk/main.go`; evidence: `README.md`)
- **internal/server package** — HTTP API, job scheduling, settings handling (paths: `internal/server`; evidence: `AGENTS.md`)
- **pkg/hfdownloader package** — Hugging Face cache scan and resumable download logic (paths: `pkg/hfdownloader`; evidence: `AGENTS.md`)
- **pkg/smartdl package** — Model analysis and download recommendation utilities (paths: `pkg/smartdl`; evidence: `AGENTS.md`)

## Security and Runtime Notes
Runtime environments observed in the repository:
- **HF\_TOKEN** — Environment variable for Hugging Face token, used if --token flag omitted (evidence: `README.md`)
- **HF\_HUB\_CACHE** — Overrides exact Hub cache directory (evidence: `README.md`)
- **HF\_HOME** — Sets cache root inside container (evidence: `README.md`)

Security notes: Not found in repository.

## Questions / Support
Not found in repository.

## License
`LICENSE` is present in the repository.

## Preserved from previous CONTRIBUTING

<!-- carry-forward: bashrusakh/hfdesk@501ee25078d14bc000cef7fc92c972e5649d1fcc; section: Workflow -->
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

<!-- carry-forward: bashrusakh/hfdesk@501ee25078d14bc000cef7fc92c972e5649d1fcc; section: Pull Request titles -->
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

<!-- carry-forward: bashrusakh/hfdesk@501ee25078d14bc000cef7fc92c972e5649d1fcc; section: Local development -->
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

<!-- carry-forward: bashrusakh/hfdesk@501ee25078d14bc000cef7fc92c972e5649d1fcc; section: Versioning -->
## Versioning

`build.sh` and `build-dev.yml` read the tracked `VERSION` file as their primary
source of the version. Released artifacts take the version from the git release
tag instead: GitHub release binaries and the RPM use the tag directly, and the
Docker image receives it as an explicit `VERSION` build arg. A stale `VERSION`
value therefore cannot ship in a released artifact, and no manual sync is
required for releases.

<!-- carry-forward: bashrusakh/hfdesk@501ee25078d14bc000cef7fc92c972e5649d1fcc; section: Configuration and state -->
## Configuration and state

HFDesk reads `hfdesk.json`, `hfdesk.yaml`, or `hfdesk.yml` from the launch
directory first, then from the per-user config directory. Settings saved from
the UI are persisted through the server config path.

Do not commit personal config files, Hugging Face tokens, cache contents,
downloaded models, or generated binaries.

<!-- carry-forward: bashrusakh/hfdesk@501ee25078d14bc000cef7fc92c972e5649d1fcc; section: License -->
## License

This project is licensed under the Apache License 2.0. See the root `LICENSE`
file for details.
