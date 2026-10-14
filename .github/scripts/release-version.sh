#!/usr/bin/env bash

set -euo pipefail

release_version() {
  local tag=${1:?tag required}
  local version=${tag#v}

  local prerelease=false rpm_version=$version
  if [[ "$version" == *-beta* ]]; then
    if [[ ! "$tag" =~ ^v([0-9]+\.[0-9]+\.[0-9]+)-beta\.([0-9]+)$ ]]; then
      echo "Unsupported beta version: $version" >&2
      return 1
    fi
    prerelease=true
    rpm_version="${BASH_REMATCH[1]}~beta.${BASH_REMATCH[2]}"
  fi

  printf '%s\n%s\n%s\n' "$version" "$prerelease" "$rpm_version"
}

extract_release_notes() {
  local version=${1:?version required} changelog=${2:-CHANGELOG.md}
  awk -v version="$version" '
    /^## \[/ && index($0, "## [" version "]") == 1 { in_section=1; print; next }
    in_section && /^## \[/ { exit }
    in_section { print }
  ' "$changelog"
}

if [[ ${BASH_SOURCE[0]} == "$0" ]]; then
  case ${1:-} in
    version)
      release_version "${2:-}"
      ;;
    notes)
      extract_release_notes "${2:-}" "${3:-CHANGELOG.md}"
      ;;
    *)
      echo "usage: $0 {version TAG|notes VERSION [CHANGELOG]}" >&2
      exit 2
      ;;
  esac
fi
