#!/usr/bin/env bash
#
# fetch-policy-contract.sh — deterministic (non-LLM) Policy-SHA contract fetcher.
#
# Resolves the trusted Policy SHA for the current repository, fetches the named
# contract files AT THAT SHA via the GitHub contents API into a read-only
# .policy/<POLICY_SHA>/ directory, exports POLICY_SHA, and fails closed when a
# core contract file cannot be fetched.
#
# SECURITY: the Policy SHA is always resolved from the repository's authoritative
# ref (default-branch head or a PR base-branch head). This script NEVER fetches
# from a pull-request head: PR_NUMBER is accepted only as non-authoritative
# context for logging, and is never used as the fetch ref. A PR must not be able
# to redefine the policy used to evaluate itself.
#
# Inputs (environment):
#   GITHUB_REPO   required. owner/repo, or a single repo name if GITHUB_REPOSITORY is set.
#   POLICY_REF    required. A 40-hex commit SHA, or a ref/tag/branch name such as the
#                 default-branch head or github.event.pull_request.base.sha.
#   PR_NUMBER     optional. Context/traceability only; never used to resolve the SHA.
#
# Optional (environment):
#   CORE_CONTRACT_FILES      space/newline-separated relative paths that MUST be fetched
#                            (default: ".github/triage-policy.md AGENTS.md CONTRIBUTING.md").
#                            A core file that cannot be fetched fails the script closed.
#   OPTIONAL_CONTRACT_FILES  space/newline-separated extra relative paths; a missing file
#                            is reported as a warning, not a failure.
#   POLICY_DIR               output root (default: ".policy").
#   GH_TOKEN / GITHUB_TOKEN  token used by `gh api` (the Actions-provided token is fine).
#
# Output:
#   .policy/<POLICY_SHA>/<fetched contract files>  (directory 0555, files 0444)
#   POLICY_SHA exported for the current process, and appended to $GITHUB_ENV when set.
#
set -euo pipefail

log() { printf '%s\n' "$*" >&2; }
die() { log "ERROR: $*"; exit 1; }

# --- Input validation -------------------------------------------------------

GITHUB_REPO="${GITHUB_REPO:-${GITHUB_REPOSITORY:-}}"
[ -n "${GITHUB_REPO:-}" ] || die "GITHUB_REPO is required (owner/repo)."
[ -n "${POLICY_REF:-}" ] || die "POLICY_REF is required (a 40-hex SHA or a ref/tag/branch name)."

# Accept "owner/repo"; additionally allow a bare repo name when GITHUB_REPOSITORY
# supplies the owner. Reject anything that is not exactly owner/repo.
if [[ "$GITHUB_REPO" != */* ]]; then
  if [ -n "${GITHUB_REPOSITORY:-}" ] && [[ "$GITHUB_REPOSITORY" == */* ]]; then
    GITHUB_REPO="${GITHUB_REPOSITORY%%/*}/$GITHUB_REPO"
  else
    die "GITHUB_REPO must be in owner/repo form, got '$GITHUB_REPO'."
  fi
fi
[[ "$GITHUB_REPO" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] \
  || die "GITHUB_REPO must be in owner/repo form, got '$GITHUB_REPO'."

command -v gh >/dev/null 2>&1 || die "'gh' CLI is required but not found in PATH."
command -v jq >/dev/null 2>&1 || die "'jq' is required but not found in PATH."

# Defensive guard: a PR head must never be used as the policy source. Refuse
# obvious PR-head ref forms outright.
case "$POLICY_REF" in
  refs/pull/*|pull/*/head|*/pull/*/head)
    die "POLICY_REF '$POLICY_REF' looks like a pull-request head; refusing to fetch policy from a PR head."
    ;;
esac

if [ -n "${PR_NUMBER:-}" ]; then
  [[ "$PR_NUMBER" =~ ^[0-9]+$ ]] || die "PR_NUMBER must be numeric, got '$PR_NUMBER'."
  log "Context: evaluating policy for PR #$PR_NUMBER (PR_NUMBER is context only; never used as the fetch ref)."
fi

# --- Resolve the Policy SHA -------------------------------------------------

POLICY_REF_TRIMMED="${POLICY_REF#"${POLICY_REF%%[![:space:]]*}"}"
POLICY_REF_TRIMMED="${POLICY_REF_TRIMMED%"${POLICY_REF_TRIMMED##*[![:space:]]}"}"

if [[ "$POLICY_REF_TRIMMED" =~ ^[0-9a-fA-F]{40}$ ]]; then
  POLICY_SHA="$(printf '%s' "$POLICY_REF_TRIMMED" | tr '[:upper:]' '[:lower:]')"
  log "Policy ref '$POLICY_REF_TRIMMED' is already a 40-hex commit SHA."
else
  log "Resolving Policy ref '$POLICY_REF_TRIMMED' to a commit SHA in '$GITHUB_REPO'..."
  # Parse the JSON in-shell so a non-commit API error body cannot leak in as the SHA.
  _commit_json="$(gh api "repos/$GITHUB_REPO/commits/$POLICY_REF_TRIMMED" 2>/dev/null || true)"
  POLICY_SHA="$(printf '%s' "$_commit_json" | jq -r '.sha // empty' 2>/dev/null || true)"
  [ -n "$POLICY_SHA" ] || die "Failed to resolve POLICY_REF '$POLICY_REF_TRIMMED' to a commit in repo '$GITHUB_REPO'."
fi

[[ "$POLICY_SHA" =~ ^[0-9a-f]{40}$ ]] || die "Resolved Policy SHA '$POLICY_SHA' is not a 40-hex commit SHA."
log "Resolved Policy SHA: $POLICY_SHA"

# --- Fetch configuration ----------------------------------------------------

CORE_CONTRACT_FILES="${CORE_CONTRACT_FILES:-\
.github/triage-policy.md
AGENTS.md
CONTRIBUTING.md}"
OPTIONAL_CONTRACT_FILES="${OPTIONAL_CONTRACT_FILES:-}"
POLICY_DIR="${POLICY_DIR:-.policy}"

OUT_DIR="$POLICY_DIR/$POLICY_SHA"

# Idempotent reset: clear any previous run's copy of this SHA before writing.
if [ -e "$OUT_DIR" ]; then
  chmod -R u+w "$OUT_DIR" 2>/dev/null || true
  rm -rf "$OUT_DIR"
fi
mkdir -p "$OUT_DIR"

fetched=()
missing_core=()

fetch_one() { # $1 = path relative to repo root; $2 = core|optional
  local path="$1" kind="$2" dest="$OUT_DIR/$1"
  if [[ "$path" == *".."* ]]; then
    [ "$kind" = "core" ] && die "Refusing path traversal in core contract path '$path'."
    log "WARN: skipping optional contract path with traversal: '$path'"
    return 0
  fi
  mkdir -p "$(dirname "$dest")"
  if gh api -H "Accept: application/vnd.github.raw" \
      "repos/$GITHUB_REPO/contents/$path?ref=$POLICY_SHA" > "$dest" 2>/dev/null; then
    chmod 0444 "$dest"
    fetched+=("$path")
    log "Fetched '$path' @ $POLICY_SHA"
  else
    rm -f "$dest"
    if [ "$kind" = "core" ]; then
      missing_core+=("$path")
      log "MISSING core contract file '$path' @ $POLICY_SHA"
    else
      log "WARN: optional contract file '$path' not found @ $POLICY_SHA"
    fi
  fi
}

read -r -a _core <<< "$CORE_CONTRACT_FILES"
read -r -a _opt <<< "$OPTIONAL_CONTRACT_FILES"

for f in "${_core[@]:-}"; do
  [ -n "$f" ] || continue
  fetch_one "$f" core
done
for f in "${_opt[@]:-}"; do
  [ -n "$f" ] || continue
  fetch_one "$f" optional
done

# --- Fail closed ------------------------------------------------------------

if [ "${#missing_core[@]}" -gt 0 ]; then
  die "Core contract file(s) missing at Policy SHA $POLICY_SHA: ${missing_core[*]}. Failing closed."
fi
if [ "${#fetched[@]}" -eq 0 ]; then
  die "No contract files were fetched at Policy SHA $POLICY_SHA. Failing closed."
fi

# Make the tree read-only so the prompt can only read the resolved policy.
find "$OUT_DIR" -type d -exec chmod 0555 {} +
chmod 0555 "$OUT_DIR"

# --- Export + report --------------------------------------------------------

export POLICY_SHA
if [ -n "${GITHUB_ENV:-}" ]; then
  {
    printf 'POLICY_SHA=%s\n' "$POLICY_SHA"
    printf 'POLICY_DIR=%s\n' "$OUT_DIR"
  } >> "$GITHUB_ENV"
fi

printf 'POLICY_SHA=%s\n' "$POLICY_SHA"
printf 'Policy contract written to %s (read-only):\n' "$OUT_DIR"
for f in "${fetched[@]}"; do
  printf '  - %s\n' "$f"
done
