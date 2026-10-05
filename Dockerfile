# =============================================================================
# HFDesk - Docker Image
# =============================================================================
# Multi-stage build for minimal image size
#
# Build:
#   docker build -t hfdesk .
#
#   Match your host user so mounted files are owned by you:
#   docker build --build-arg UID=$(id -u) --build-arg GID=$(id -g) -t hfdesk .
#
# Run Web Server (single /data volume for cache, state, and models):
#   docker run --rm -p 8080:8080 \
#     -v hfdesk-data:/data \
#     hfdesk --port 8080
#
# Run as a specific UID/GID (NAS/homelab):
#   docker run --rm -p 8080:8080 \
#     -e PUID=1026 -e PGID=100 -e UMASK=002 \
#     -v /mnt/user/appdata/hfdesk:/data \
#     hfdesk
#
# Run as an arbitrary UID without using the image user (enterprise/k8s):
#   docker run --rm --user 568:568 -p 8080:8080 \
#     -v hfdesk-data:/data hfdesk
#
# With a HuggingFace token (for private/gated models). The token is resolved as
# the --token flag, then the HF_TOKEN environment variable, then the config file
# token, so any of these works:
#   docker run --rm -p 8080:8080 \
#     -v hfdesk-data:/data hfdesk --token hf_xxx
#   docker run --rm -p 8080:8080 \
#     -e HF_TOKEN=hf_xxx -v hfdesk-data:/data hfdesk
# You can also set it in the HFDesk settings UI/API.
#
# The container starts as root only so the entrypoint can honor PUID/PGID and
# then drop privileges; the app process always runs as the requested UID/GID.
# When started with --user or a k8s runAsUser, the entrypoint does not touch
# users or ownership and just execs the app.
#
# Credits: Original Docker support suggested by cdeving (#50)
# =============================================================================

# Build stage
FROM golang:1.24-alpine AS builder

# Install build dependencies
RUN apk add --no-cache git ca-certificates

WORKDIR /src

# Cache dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the binary.
# Version precedence: (1) the VERSION build-arg, passed by CI from the release
# tag, which is the single source of truth for released artifacts; (2) the
# tracked VERSION file as a fallback for local builds. "latest" is a Docker tag
# alias, not a release version, so it is treated as unset.
ARG VERSION=""
RUN BUILD_VERSION="${VERSION}" && \
    if [ -z "$BUILD_VERSION" ] || [ "$BUILD_VERSION" = "latest" ]; then \
      BUILD_VERSION="$(cat VERSION 2>/dev/null | tr -d '[:space:]')"; \
    fi && \
    BUILD_VERSION="${BUILD_VERSION#v}" && \
    if [ -z "$BUILD_VERSION" ]; then \
      echo "error: no build version (pass --build-arg VERSION=<tag> or populate the VERSION file)" >&2; \
      exit 1; \
    fi && \
    CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w -X main.Version=${BUILD_VERSION}" -o /hfdesk ./cmd/hfdesk

# =============================================================================
# Final stage - minimal image
# =============================================================================
FROM alpine:3.19

# UID/GID for the image user. Override at build time to match your host user.
ARG UID=1000
ARG GID=1000

# Install ca-certificates for HTTPS, tzdata for timezones, and su-exec for the
# entrypoint privilege drop (Alpine has no su-exec/setpriv --reuid by default).
RUN apk add --no-cache ca-certificates tzdata su-exec

# Create (or reuse) the non-root image user/group with the build-time UID/GID.
# Alpine reserves some IDs (for example gid 100 is "users" and uid 65534 is
# "nobody"), so `docker build --build-arg UID=$(id -u) --build-arg GID=$(id -g)`
# on a NAS/homelab host can collide with an existing entry. Rather than fail
# the build, reuse the existing group/account for that ID and record its name;
# the entrypoint remaps by the resolved name.
#
# UID/GID must be numeric so the image never bakes a name into HFDESK_UID/GID
# (which the entrypoint then rejects at runtime with exit 64). A non-numeric
# value is refused here at build time, and addgroup/adduser failures (for
# example an out-of-range numeric ID) are checked explicitly so the build stops
# with a clear error instead of a raw BusyBox message.
RUN set -eu; \
    case "$GID" in \
      '' | *[!0-9]*) echo "error: GID must be a numeric id (got '$GID')" >&2; exit 1 ;; \
    esac; \
    case "$UID" in \
      '' | *[!0-9]*) echo "error: UID must be a numeric id (got '$UID')" >&2; exit 1 ;; \
    esac; \
    if getent group "$GID" >/dev/null; then \
      hfdesk_group="$(getent group "$GID" | cut -d: -f1)"; \
    else \
      if ! addgroup -g "$GID" hfdesk; then \
        echo "error: cannot create group with GID $GID (out of range or already in use)" >&2; \
        exit 1; \
      fi; \
      hfdesk_group=hfdesk; \
    fi; \
    if [ "$hfdesk_group" = root ]; then \
      echo "error: GID $GID resolves to the root group; refusing to use it as the image group" >&2; \
      exit 1; \
    fi; \
    if getent passwd "$UID" >/dev/null; then \
      hfdesk_user="$(getent passwd "$UID" | cut -d: -f1)"; \
    else \
      if ! adduser -D -h /data -u "$UID" -G "$hfdesk_group" hfdesk; then \
        echo "error: cannot create user with UID $UID (out of range or already in use)" >&2; \
        exit 1; \
      fi; \
      hfdesk_user=hfdesk; \
    fi; \
    if [ "$hfdesk_user" = root ]; then \
      echo "error: UID $UID resolves to the root account; refusing to use it as the image user" >&2; \
      exit 1; \
    fi; \
    printf '%s\n' "$hfdesk_group" > /etc/hfdesk-group; \
    printf '%s\n' "$hfdesk_user" > /etc/hfdesk-user

# Copy binary and entrypoint from builder
COPY --from=builder /hfdesk /usr/local/bin/hfdesk
COPY docker/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

# All writable state lives under one data root. HFDesk derives the HF cache
# from HF_HOME and its app config from XDG_CONFIG_HOME/HOME, so pinning these
# lets any UID (including --user / k8s runAsUser) write state without needing
# the image user or an /etc/passwd entry.
#
# HFDESK_UID/HFDESK_GID expose the build-time ARGs so the entrypoint's default
# PUID/PGID matches a custom `--build-arg UID/GID` image instead of forcing 1000.
ENV HOME=/data \
    XDG_CONFIG_HOME=/data/.config \
    HF_HOME=/data/.cache/huggingface \
    HFDESK_UID=$UID \
    HFDESK_GID=$GID

# Create the single writable data root. `adduser -h /data` already owns it as
# the image user, so it is the 1777 (sticky, world-writable) mode - not the
# ownership - that lets an arbitrary `--user <uid>` create its own state.
#
# Do NOT pre-create the app subdirs: a fresh named volume copies this
# directory's contents, and subdirs owned by the image user at mode 0755 would
# not be writable by an arbitrary UID. The entrypoint only owns the root itself
# (never descending into the attacker-writable tree), and the app creates its
# subdirs as the target UID. Model/cache trees are left to the app to create.
RUN mkdir -p /data && chmod 1777 /data

# Note: no USER directive. The entrypoint must start as root to apply
# PUID/PGID, then drops to the requested UID/GID. Starting with --user skips
# the privilege drop entirely.
WORKDIR /data

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
CMD []

# Expose web server port
EXPOSE 8080

# Labels
LABEL org.opencontainers.image.source="https://github.com/bashrusakh/hfdesk"
LABEL org.opencontainers.image.description="Desktop-style web UI for finding, analyzing, and downloading Hugging Face models"
LABEL org.opencontainers.image.licenses="Apache-2.0"
