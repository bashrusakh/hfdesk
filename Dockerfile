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
# With HuggingFace token (for private/gated models):
#   docker run --rm -e HF_TOKEN=hf_xxx -p 8080:8080 \
#     -v hfdesk-data:/data hfdesk
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

# Create the non-root image user with the build-time UID/GID.
RUN addgroup -g "$GID" hfdesk && \
    adduser -D -h /data -u "$UID" -G hfdesk hfdesk

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
    HFDESK_DATA_ROOT=/data \
    HFDESK_UID=$UID \
    HFDESK_GID=$GID

# Create the single writable data root. Do NOT pre-create the app subdirs:
# a fresh named volume copies this directory's ownership/permissions, so if
# they were owned by the image user an arbitrary `--user <uid>` could not
# create its own state. The sticky, world-writable root lets any UID create
# its state dirs; the entrypoint tightens/owns the small subdirs it creates
# when it runs as root. Model/cache trees are left to the app to create.
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
