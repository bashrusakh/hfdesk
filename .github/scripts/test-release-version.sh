#!/usr/bin/env bash

set -euo pipefail
SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
source "$SCRIPT_DIR/release-version.sh"

assert_version() {
  local tag=$1 expected=$2
  local actual
  actual=$(release_version "$tag" | paste -sd '|')
  [[ "$actual" == "$expected" ]] || {
    echo "$tag: expected '$expected', got '$actual'" >&2
    exit 1
  }
}

assert_version v1.3.0-beta.1 '1.3.0-beta.1|true|1.3.0~beta.1'
assert_version v1.3.0-beta.2 '1.3.0-beta.2|true|1.3.0~beta.2'
assert_version v1.3.0 '1.3.0|false|1.3.0'
if release_version v1.3.0-beta >/dev/null 2>&1; then
  echo 'malformed beta tag unexpectedly accepted' >&2
  exit 1
fi

notes=$(extract_release_notes 1.3.0-beta.1 "$SCRIPT_DIR/../../CHANGELOG.md")
[[ "$notes" == *'## [1.3.0-beta.1] - 2026-10-15'* ]]
[[ "$notes" == *'Beta limitations and compatibility'* ]]
[[ "$notes" != *'## [1.2.2]'* ]]
[[ $(extract_release_notes 1.3.0-beta.10 "$SCRIPT_DIR/../../CHANGELOG.md") == '' ]]
echo 'release version and changelog checks passed'
