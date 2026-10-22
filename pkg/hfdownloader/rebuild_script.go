// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// RebuildScriptVersion is the current version of the generated wrapper.
const RebuildScriptVersion = "4.0.0"

// RebuildScript is an invocation-only wrapper. Rebuild enumeration, ownership,
// locking, restoration, and cleanup are owned by HFCache.Sync in the Go binary.
const RebuildScript = `#!/usr/bin/env bash
# HFDesk Rebuild Script v%s
# Auto-generated invocation wrapper; rebuild behavior is owned by HFDesk.
# Usage: ./rebuild.sh [--clean] [--verbose|-v]

set -euo pipefail

SCRIPT_SOURCE="${BASH_SOURCE[0]}"
SCRIPT_DIR="${SCRIPT_SOURCE%/*}"
if [[ "$SCRIPT_DIR" == "$SCRIPT_SOURCE" ]]; then
    SCRIPT_DIR="."
fi
SCRIPT_DIR="$(cd -- "$SCRIPT_DIR" && pwd -P)"
HUB_DIR=__HFDSK_HUB_DIR__

CLEAN=false
VERBOSE=false
for arg in "$@"; do
    case "$arg" in
        --clean) CLEAN=true ;;
        --verbose|-v) VERBOSE=true ;;
        --help|-h)
            echo "Usage: $0 [--clean] [--verbose|-v]"
            echo "Rebuild the friendly cache view using a compatible HFDesk binary."
            exit 0
            ;;
        *) echo "Unknown rebuild option: $arg" >&2; exit 2 ;;
    esac
done

if [[ -n "${HFDESK_BIN:-}" ]]; then
    HFDSK="$HFDESK_BIN"
elif [[ -x "$SCRIPT_DIR/hfdesk" ]]; then
    HFDSK="$SCRIPT_DIR/hfdesk"
elif HFDSK="$(command -v hfdesk 2>/dev/null)"; then
    :
else
    echo "A compatible HFDesk binary is required (set HFDESK_BIN or put hfdesk beside this script/on PATH)." >&2
    exit 127
fi

if [[ ! -x "$HFDSK" ]]; then
    echo "HFDesk binary is not executable: $HFDSK" >&2
    exit 127
fi

if ! protocol="$("$HFDSK" --hfdesk-rebuild-probe 2>/dev/null)" || [[ "$protocol" != "HFDesk rebuild protocol 1" ]]; then
    echo "HFDesk binary is missing the compatible rebuild entry; cache was not changed." >&2
    exit 126
fi

args=(--hfdesk-rebuild --cache-dir="$SCRIPT_DIR" --hub-dir="$HUB_DIR")
if [[ "$CLEAN" == true ]]; then args+=(--clean); fi
if [[ "$VERBOSE" == true ]]; then args+=(--verbose); fi
exec "$HFDSK" "${args[@]}"
`

// WriteRebuildScript writes the thin Go-dispatch wrapper for this cache's R/H
// association. Default H is derived from the wrapper's current R so relocation
// continues to work; a custom H is embedded as a shell-quoted exact path.
func (c *HFCache) WriteRebuildScript() (string, error) {
	if err := RejectOwnedBranchVariantDirectoryWrites(c.Root); err != nil {
		return "", err
	}
	coordinationRelease, err := acquireCompleteRebuildCoordinationLeases(context.Background(), []string{c.Root}, true, nil)
	if err != nil {
		return "", fmt.Errorf("lease rebuild script destination: %w", err)
	}
	defer coordinationRelease()
	if err := RebindBranchVariantServiceRootUnderLease(c.Root); err != nil {
		return "", err
	}
	return c.writeRebuildScriptUnderLease()
}

func (c *HFCache) writeRebuildScriptUnderLease() (string, error) {
	if err := RejectOwnedBranchVariantDirectoryWrites(c.Root); err != nil {
		return "", err
	}
	if err := os.MkdirAll(c.Root, 0o755); err != nil {
		return "", err
	}
	scriptPath := c.RebuildScriptPath()
	var locks branchVariantWriteLockSet
	defer locks.Close()
	if err := locks.Acquire(scriptPath); err != nil {
		return "", fmt.Errorf("reserve rebuild script path: %w", err)
	}
	if err := RejectOwnedBranchVariantContentWrites(scriptPath); err != nil {
		return "", err
	}
	if info, err := os.Lstat(scriptPath); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("refuse non-regular rebuild script path %q", scriptPath)
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}

	content := strings.Replace(RebuildScript, "v%s", "v"+RebuildScriptVersion, 1)
	root, err := filepath.Abs(filepath.Clean(c.Root))
	if err != nil {
		return "", err
	}
	hub, err := filepath.Abs(filepath.Clean(c.HubDir()))
	if err != nil {
		return "", err
	}
	defaultHub := filepath.Join(root, "hub")
	hubValue := `"$SCRIPT_DIR/hub"`
	if filepath.Clean(hub) != filepath.Clean(defaultHub) {
		hubValue = shellQuoteRebuildArgument(hub)
	}
	content = strings.Replace(content, "__HFDSK_HUB_DIR__", hubValue, 1)

	if existing, err := os.ReadFile(scriptPath); err == nil && string(existing) == content {
		return scriptPath, nil
	}
	if err := os.WriteFile(scriptPath, []byte(content), 0o755); err != nil {
		return "", fmt.Errorf("write rebuild script: %w", err)
	}
	if err := os.Chmod(scriptPath, 0o755); err != nil {
		return "", fmt.Errorf("make rebuild script executable: %w", err)
	}
	return scriptPath, nil
}

func shellQuoteRebuildArgument(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

// RebuildScriptPath returns the expected path of the generated wrapper.
func (c *HFCache) RebuildScriptPath() string {
	return filepath.Join(c.Root, "rebuild.sh")
}
