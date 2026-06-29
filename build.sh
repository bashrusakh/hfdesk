#!/bin/bash
# Build script for HFDesk
# Builds fully static binaries for multiple platforms

set -e

# Get the script directory
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

# Add Go bin to PATH (for goversioninfo and other Go tools)
export PATH="$PATH:$(go env GOPATH)/bin"

# Read version from VERSION file
VERSION=$(cat VERSION | tr -d '[:space:]')
if [ -z "$VERSION" ]; then
    echo "Error: VERSION file is empty or not found"
    exit 1
fi

echo "Building hfdesk version: $VERSION"

# Output directory
OUTPUT_DIR="output"
mkdir -p "$OUTPUT_DIR"

# Main package path
MAIN_PKG="./cmd/hfdesk"

# Ldflags for version injection and optimized binary
# Note: -s -w strips debug info (smaller binary but may trigger AV false positives on Windows)
LDFLAGS_STRIP="-s -w -X main.Version=${VERSION}"
LDFLAGS_NOSTRIP="-X main.Version=${VERSION}"

# Build targets: OS_ARCH
TARGETS=(
    "darwin_arm64"
    "darwin_amd64"
    "linux_amd64"
    "linux_arm64"
    "windows_amd64"
)

# Generate Windows .ico from the apple-touch-icon PNG.
# Always regenerates — stale files from previous builds are not reused.
generate_windows_icon() {
    local ico_path="${MAIN_PKG}/hfdesk.ico"
    local png_src="internal/assets/static/apple-touch-icon-180x180.png"

    if [ ! -f "$png_src" ]; then
        echo "  Error: $png_src not found — cannot generate Windows icon"
        return 1
    fi

    # Remove any stale .ico from a previous run
    rm -f "$ico_path"

    echo "  Generating Windows icon from $png_src..."
    (cd "$SCRIPT_DIR" && go run "${MAIN_PKG}/genico.go") || {
        echo "  Error: icon generation failed"
        return 1
    }

    if [ ! -f "$ico_path" ]; then
        echo "  Error: $ico_path was not created after generation"
        return 1
    fi
}

# Generate Windows icon and version resources.
# Uses rsrc for icon embedding (goversioninfo corrupts ICO directory entries).
# Version metadata is set via ldflags (-X main.Version=...) at build time.
generate_windows_versioninfo() {
    # Remove any stale .syso from a previous run before regenerating
    rm -f "${MAIN_PKG}/resource_windows_amd64.syso"

    # Generate .ico first
    generate_windows_icon || return 1

    echo "  Generating Windows icon resource..."
    (cd "${MAIN_PKG}" && rsrc -ico hfdesk.ico -o resource_windows_amd64.syso)

    return 0
}

# Cleanup Windows resource files
cleanup_windows_versioninfo() {
    rm -f "${MAIN_PKG}/resource_windows_amd64.syso"
    rm -f "${MAIN_PKG}/hfdesk.ico"
}

# Build function
build() {
    local os_arch=$1
    local os=${os_arch%_*}
    local arch=${os_arch#*_}
    
    local output_name="hfdesk_${os}_${arch}_${VERSION}"
    local ldflags="$LDFLAGS_STRIP"
    
    # Add .exe extension for Windows
    if [ "$os" = "windows" ]; then
        output_name="${output_name}.exe"
        # Don't strip Windows binaries - reduces AV false positives
        ldflags="$LDFLAGS_NOSTRIP"
    fi
    
    local output_path="${OUTPUT_DIR}/${output_name}"
    
    echo "Building for ${os}/${arch}..."
    
    # Build with CGO disabled for fully static binary
    CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build \
        -ldflags "$ldflags" \
        -trimpath \
        -o "$output_path" \
        "$MAIN_PKG"
    
    echo "  -> ${output_path}"
}

# Clean old builds (optional, uncomment if needed)
# echo "Cleaning old builds..."
# rm -f "${OUTPUT_DIR}"/hfdesk_*

# Build all targets
echo ""
echo "Starting builds..."
echo "================================"

# Generate Windows icon resource before building.
# Requires rsrc for icon embedding.
HAS_VERSIONINFO=false
if command -v rsrc &> /dev/null; then
    generate_windows_versioninfo
    HAS_VERSIONINFO=true
else
    echo "  Note: rsrc not found, skipping Windows icon"
    echo "  Install with: go install github.com/akavel/rsrc@latest"
    # Remove stale .syso so go build doesn't embed old resources
    rm -f "${MAIN_PKG}/resource_windows_amd64.syso"
fi

for target in "${TARGETS[@]}"; do
    build "$target"
done

# Cleanup Windows version info files
if [ "$HAS_VERSIONINFO" = true ]; then
    cleanup_windows_versioninfo
fi

# Copy Linux desktop integration files
echo ""
echo "Copying Linux desktop files..."
if [ -f "${SCRIPT_DIR}/packaging/rpm/hfdesk.desktop" ]; then
    cp "${SCRIPT_DIR}/packaging/rpm/hfdesk.desktop" "${OUTPUT_DIR}/" && echo "  -> hfdesk.desktop"
else
    echo "  (desktop file not found)"
fi
if [ -f "${SCRIPT_DIR}/packaging/rpm/hfdesk.svg" ]; then
    cp "${SCRIPT_DIR}/packaging/rpm/hfdesk.svg" "${OUTPUT_DIR}/" && echo "  -> hfdesk.svg"
else
    echo "  (icon not found)"
fi

echo "================================"
echo ""
echo "Build complete! Artifacts are in: ${OUTPUT_DIR}/"
echo ""
ls -lh "${OUTPUT_DIR}"/hfdesk_*_${VERSION}* 2>/dev/null || true

