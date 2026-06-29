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
        exit 1
    fi

    # Remove any stale .ico from a previous run
    rm -f "$ico_path"

    echo "  Generating Windows icon from $png_src..."
    (cd "$SCRIPT_DIR" && go run "${MAIN_PKG}/genico.go") || {
        echo "  Error: icon generation failed"
        exit 1
    }

    if [ ! -f "$ico_path" ]; then
        echo "  Error: $ico_path was not created after generation"
        exit 1
    fi
}

# Generate Windows version info if goversioninfo is available
generate_windows_versioninfo() {
    if ! command -v goversioninfo &> /dev/null; then
        echo "  Note: goversioninfo not found, skipping Windows metadata"
        echo "  Install with: go install github.com/josephspurrier/goversioninfo/cmd/goversioninfo@latest"
        return 1
    fi
    
    # Generate .ico first
    generate_windows_icon
    
    # Parse version components (handle -dev suffix)
    local ver_clean="${VERSION%-*}"  # Remove -dev or similar suffix
    local major minor patch
    IFS='.' read -r major minor patch <<< "$ver_clean"
    major=${major:-0}
    minor=${minor:-0}
    patch=${patch:-0}
    
    cat > "${MAIN_PKG}/versioninfo.json" << EOF
{
    "FixedFileInfo": {
        "FileVersion": {
            "Major": ${major},
            "Minor": ${minor},
            "Patch": ${patch},
            "Build": 0
        },
        "ProductVersion": {
            "Major": ${major},
            "Minor": ${minor},
            "Patch": ${patch},
            "Build": 0
        },
        "FileFlagsMask": "3f",
        "FileFlags": "00",
        "FileOS": "040004",
        "FileType": "01",
        "FileSubType": "00"
    },
    "StringFileInfo": {
        "Comments": "HuggingFace Model Downloader - Download models from HuggingFace Hub",
        "CompanyName": "Open Source",
        "FileDescription": "HuggingFace Model Downloader",
        "FileVersion": "${VERSION}",
        "InternalName": "hfdesk",
        "LegalCopyright": "Apache-2.0 License",
        "OriginalFilename": "hfdesk.exe",
        "ProductName": "HuggingFace Model Downloader",
        "ProductVersion": "${VERSION}"
    },
    "VarFileInfo": {
        "Translation": {
            "LangID": "0409",
            "CharsetID": "04B0"
        }
    }
}
EOF
    
    echo "  Generating Windows version resource..."
    (cd "${MAIN_PKG}" && goversioninfo -icon hfdesk.ico -o resource_windows_amd64.syso)
    return 0
}

# Cleanup Windows version info files
cleanup_windows_versioninfo() {
    rm -f "${MAIN_PKG}/versioninfo.json"
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

# Generate Windows version info before building.
# If goversioninfo is available, this step is mandatory.
HAS_VERSIONINFO=false
if command -v goversioninfo &> /dev/null; then
    generate_windows_versioninfo
    HAS_VERSIONINFO=true
else
    echo "  Note: goversioninfo not found, skipping Windows metadata"
    echo "  Install with: go install github.com/josephspurrier/goversioninfo/cmd/goversioninfo@latest"
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
cp packaging/rpm/hfdesk.desktop "${OUTPUT_DIR}/" 2>/dev/null && echo "  -> hfdesk.desktop" || echo "  (desktop file not found)"
cp packaging/rpm/hfdesk.svg "${OUTPUT_DIR}/" 2>/dev/null && echo "  -> hfdesk.svg" || echo "  (icon not found)"

echo "================================"
echo ""
echo "Build complete! Artifacts are in: ${OUTPUT_DIR}/"
echo ""
ls -lh "${OUTPUT_DIR}"/hfdesk_*_${VERSION}* 2>/dev/null || true

