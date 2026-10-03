#!/bin/sh
# HFDesk container entrypoint.
#
# Default behavior: the container starts as root and the entrypoint applies
# PUID/PGID (default: the build-time image UID/GID, normally 1000) then drops
# privileges with su-exec, so the app process runs non-root unless the caller
# explicitly sets PUID=0/PGID=0.
#
# PUID/PGID let NAS/homelab users match a host user, and UMASK controls the
# file creation mask.
#
# When the container is started non-root (docker run --user 568:568, or a
# Kubernetes securityContext.runAsUser/fsGroup), no user or ownership mutation
# is attempted and the app is exec'd directly. All writable state lives under
# the fixed /data root, so the app never needs the image user or an /etc/passwd
# entry.
#
# Security: /data is world-writable (mode 1777) so any UID can create its own
# state. That also means every entry *inside* it is attacker-influenced: a
# process running as the app UID can replace any app subdirectory with a
# symlink to a root-owned path. The root entrypoint must therefore never
# create, traverse, or dereference a path inside the data tree. It touches only
# the fixed data root itself (a single component whose parent is /, outside the
# writable tree) and leaves every app subdirectory for the app to create as the
# target UID. All root ownership changes are non-recursive and never
# dereference a symlink.
#
# The data root is the literal /data and is deliberately NOT overridable: an
# override with a multi-component value could place an attacker-replaceable
# intermediate directory under a root chown, reintroducing root symlink
# traversal. Keeping it a fixed single-component path makes the invariant
# trivially true.
#
# Environment:
#   PUID  - UID to run the app as (default: build-time image UID)
#   PGID  - GID to run the app as (default: build-time image GID)
#   UMASK - file creation mask for the app process (default: 022)
#
# Debug override:
#   HFDESK_BIN - app binary to exec (default: /usr/local/bin/hfdesk)
set -eu

PUID="${PUID:-${HFDESK_UID:-1000}}"
PGID="${PGID:-${HFDESK_GID:-1000}}"
UMASK="${UMASK:-022}"

APP_BIN="${HFDESK_BIN:-/usr/local/bin/hfdesk}"

# Fixed writable data root. Keep in sync with the Dockerfile ENV defaults.
# Not overridable: see the security note above.
DATA_ROOT="/data"

# Apply the requested umask before either exec path. This is just a process
# attribute, so it is harmless (and still honored) when running non-root.
umask "$UMASK"

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
# usermod, so edit the account database directly. The passwd entry carries the
# primary GID, so remap it whenever either the UID or the GID differs; remapping
# on the UID alone would leave /etc/passwd with a stale GID after a PGID-only
# change.
if [ "$(id -g hfdesk 2>/dev/null || echo x)" != "$PGID" ]; then
    sed -i "s/^\(hfdesk:x:\)[0-9]*:/\1${PGID}:/" /etc/group
fi
if [ "$(id -u hfdesk 2>/dev/null || echo x)" != "$PUID" ] || \
   [ "$(id -g hfdesk 2>/dev/null || echo x)" != "$PGID" ]; then
    sed -i "s/^\(hfdesk:x:\)[0-9]*:[0-9]*:/\1${PUID}:${PGID}:/" /etc/passwd
fi

# Symlink-safe handling of the fixed data root.
#
# Any UID can write under /data, so a process running as the app UID can
# replace an app subdirectory with a symlink into /usr/local/bin (or anywhere
# else) and make a root entrypoint follow it on the next start. To prevent a
# non-root UID from influencing a root-owned path, this entrypoint never
# descends into the tree: it only makes the root itself a real directory and
# hands it to the target UID. The app creates its own subdirectories (config,
# HF cache, models) as that UID.
if [ -L "$DATA_ROOT" ]; then
    echo "entrypoint: refusing data root '$DATA_ROOT': it is a symlink" >&2
    exit 65
fi

if [ ! -e "$DATA_ROOT" ]; then
    # Create only the final component. Plain mkdir does not follow a symlink
    # operand and does not descend through attacker-controlled state to create
    # missing parents; the parent must already be a real directory. For the
    # fixed /data root the parent is /, which is root-owned and outside the
    # writable tree.
    DATA_PARENT="$(dirname "$DATA_ROOT")"
    if [ -L "$DATA_PARENT" ] || [ ! -d "$DATA_PARENT" ]; then
        echo "entrypoint: data root '$DATA_ROOT' is missing and its parent is not a real directory" >&2
        exit 66
    fi
    if ! mkdir "$DATA_ROOT"; then
        echo "entrypoint: data root '$DATA_ROOT' does not exist and could not be created" >&2
        exit 66
    fi
fi

# Re-validate immediately before use to narrow the check/use window. /data is
# the mount root itself, which a process inside the container cannot replace;
# and even if a path were swapped in between, the subsequent chown -h changes
# only the named entry and never dereferences a symlink into its target.
if [ -L "$DATA_ROOT" ] || [ ! -d "$DATA_ROOT" ]; then
    echo "entrypoint: refusing data root '$DATA_ROOT': not a real directory" >&2
    exit 65
fi

# Hand the data root itself to the target UID. -h keeps the change from ever
# dereferencing a symlink operand, and nothing inside the tree is touched, so
# the app can create its own state while a planted subdirectory symlink is
# ignored.
chown -h "$PUID:$PGID" "$DATA_ROOT"

# Drop privileges and exec the app as the requested UID/GID. The umask set
# above is inherited through su-exec.
exec su-exec "$PUID:$PGID" "$APP_BIN" "$@"
