#!/bin/sh
# HFDesk container entrypoint.
#
# Default behavior: the container starts as root and the entrypoint applies
# PUID/PGID (default: the build-time image UID/GID, normally 1000) then drops
# privileges with su-exec, so the app process is always non-root.
#
# PUID/PGID let NAS/homelab users match a host user, and UMASK controls the
# file creation mask.
#
# When the container is started non-root (docker run --user 568:568, or a
# Kubernetes securityContext.runAsUser/fsGroup), no user or ownership mutation
# is attempted and the app is exec'd directly. All writable state lives under
# the env-driven /data root, so the app never needs the image user or an
# /etc/passwd entry.
#
# Environment:
#   PUID  - UID to run the app as (default: build-time image UID)
#   PGID  - GID to run the app as (default: build-time image GID)
#   UMASK - file creation mask for the app process (default: 022)
#
# Overrides (mainly for debugging):
#   HFDESK_DATA_ROOT - writable data root (default: /data)
#   HFDESK_BIN       - app binary to exec (default: /usr/local/bin/hfdesk)
set -eu

PUID="${PUID:-${HFDESK_UID:-1000}}"
PGID="${PGID:-${HFDESK_GID:-1000}}"
UMASK="${UMASK:-022}"

APP_BIN="${HFDESK_BIN:-/usr/local/bin/hfdesk}"

# Writable data root. Keep in sync with the Dockerfile ENV defaults.
DATA_ROOT="${HFDESK_DATA_ROOT:-/data}"

# Already non-root: honor the caller's UID/GID as-is and never chown. The
# world-writable data root lets any UID create its state directories.
if [ "$(id -u)" != "0" ]; then
    exec "$APP_BIN" "$@"
fi

case "$PUID" in
    '' | *[!0-9]*)
        echo "entrypoint: PUID must be a numeric UID (got '$PUID')" >&2
        exit 64
        ;;
esac
case "$PGID" in
    '' | *[!0-9]*)
        echo "entrypoint: PGID must be a numeric GID (got '$PGID')" >&2
        exit 64
        ;;
esac

# Remap the hfdesk account to the requested IDs. Alpine's BusyBox has no
# usermod, so edit the account database directly.
if [ "$(id -g hfdesk 2>/dev/null || echo x)" != "$PGID" ]; then
    sed -i "s/^\(hfdesk:x:\)[0-9]*:/\1${PGID}:/" /etc/group
fi
if [ "$(id -u hfdesk 2>/dev/null || echo x)" != "$PUID" ]; then
    sed -i "s/^\(hfdesk:x:\)[0-9]*:[0-9]*:/\1${PUID}:${PGID}:/" /etc/passwd
fi

# Ensure the writable root and small app/state dirs exist.
mkdir -p \
    "$DATA_ROOT" \
    "$DATA_ROOT/.config/HFDesk" \
    "$DATA_ROOT/.cache/huggingface" \
    "$DATA_ROOT/Models" \
    "$DATA_ROOT/Datasets"

# Fix ownership of only the small app/state dirs and the data root itself.
# The HF cache / model trees can be huge mounted volumes, so they are chowned
# non-recursively (directory entry only) and never with -R.
chown "$PUID:$PGID" "$DATA_ROOT"
chown -R "$PUID:$PGID" "$DATA_ROOT/.config"
chown "$PUID:$PGID" \
    "$DATA_ROOT/.cache" \
    "$DATA_ROOT/.cache/huggingface" \
    "$DATA_ROOT/Models" \
    "$DATA_ROOT/Datasets"

# Apply the requested umask (inherited by the app through su-exec).
umask "$UMASK"

# Drop privileges and exec the app as the requested UID/GID.
exec su-exec "$PUID:$PGID" "$APP_BIN" "$@"
