# =============================================================================
# HFDesk - Docker Image
# =============================================================================
# Multi-stage build for minimal image size
#
# Build:
#   docker build -t hfdesk .
#
# Run Web Server:
#   docker run --rm -p 8080:8080 \
#     -v ~/.cache/huggingface:/home/hfdesk/.cache/huggingface \
#     hfdesk --port 8080
#
# With HuggingFace token (for private/gated models):
#   docker run --rm -e HF_TOKEN=hf_xxx -p 8080:8080 \
#     -v ~/.cache/huggingface:/home/hfdesk/.cache/huggingface \
#     hfdesk
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

# Build the binary
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /hfdesk ./cmd/hfdesk

# =============================================================================
# Final stage - minimal image
# =============================================================================
FROM alpine:3.19

# Install ca-certificates for HTTPS
RUN apk add --no-cache ca-certificates tzdata

# Create non-root user
RUN adduser -D -u 1000 hfdesk

# Copy binary from builder
COPY --from=builder /hfdesk /usr/local/bin/hfdesk

# Create HuggingFace cache directory (v3 default) and legacy data directory
RUN mkdir -p /home/hfdesk/.cache/huggingface/hub \
             /home/hfdesk/.cache/huggingface/models \
             /home/hfdesk/.cache/huggingface/datasets \
             /data/Models /data/Datasets && \
    chown -R hfdesk:hfdesk /home/hfdesk /data

# Switch to non-root user
USER hfdesk

# Set HF_HOME for the container
ENV HF_HOME=/home/hfdesk/.cache/huggingface

WORKDIR /home/hfdesk

# Default to showing help
ENTRYPOINT ["/usr/local/bin/hfdesk"]
CMD []

# Expose web server port
EXPOSE 8080

# Labels
LABEL org.opencontainers.image.source="https://github.com/bashrusakh/hfdesk"
LABEL org.opencontainers.image.description="Desktop-style web UI for finding, analyzing, and downloading Hugging Face models"
LABEL org.opencontainers.image.licenses="Apache-2.0"
