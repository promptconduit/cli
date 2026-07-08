#!/bin/bash
#
# PromptConduit CLI Installer
#
# Usage:
#   curl -fsSL https://promptconduit.dev/install | bash
#   curl -fsSL https://promptconduit.dev/install | bash -s -- YOUR_API_KEY
#
# Environment variables:
#   PROMPTCONDUIT_VERSION - Install a specific version (default: latest)
#   PROMPTCONDUIT_INSTALL_DIR - Installation directory (default: /usr/local/bin)
#

set -e

# Configuration
REPO="promptconduit/cli"
BINARY_NAME="promptconduit"
DEFAULT_INSTALL_DIR="/usr/local/bin"
GITHUB_API="https://api.github.com"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

info() {
    echo -e "${GREEN}[INFO]${NC} $1"
}

warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

error() {
    echo -e "${RED}[ERROR]${NC} $1"
    exit 1
}

# Detect OS and architecture
detect_platform() {
    local os arch

    case "$(uname -s)" in
        Linux*)  os="linux" ;;
        Darwin*) os="darwin" ;;
        MINGW*|MSYS*|CYGWIN*) os="windows" ;;
        *) error "Unsupported operating system: $(uname -s)" ;;
    esac

    case "$(uname -m)" in
        x86_64|amd64) arch="amd64" ;;
        arm64|aarch64) arch="arm64" ;;
        *) error "Unsupported architecture: $(uname -m)" ;;
    esac

    echo "${os}_${arch}"
}

# Get the latest version from GitHub
get_latest_version() {
    local version
    version=$(curl -sS "${GITHUB_API}/repos/${REPO}/releases/latest" | grep '"tag_name":' | sed -E 's/.*"v([^"]+)".*/\1/')

    if [ -z "$version" ]; then
        error "Failed to get latest version from GitHub"
    fi

    echo "$version"
}

# Download and install the binary
install_binary() {
    local version="$1"
    local platform="$2"
    local install_dir="$3"

    local ext="tar.gz"
    if [[ "$platform" == windows_* ]]; then
        ext="zip"
    fi

    local filename="${BINARY_NAME}_${version}_${platform}.${ext}"
    local download_url="https://github.com/${REPO}/releases/download/v${version}/${filename}"

    info "Downloading ${BINARY_NAME} v${version} for ${platform}..."

    local tmp_dir
    tmp_dir=$(mktemp -d)
    trap "rm -rf $tmp_dir" EXIT

    if ! curl -fsSL "$download_url" -o "${tmp_dir}/${filename}"; then
        error "Failed to download from ${download_url}"
    fi

    info "Extracting..."
    cd "$tmp_dir"

    if [[ "$ext" == "tar.gz" ]]; then
        tar -xzf "$filename"
    else
        unzip -q "$filename"
    fi

    # Install the binary
    local binary="${BINARY_NAME}"
    if [[ "$platform" == windows_* ]]; then
        binary="${BINARY_NAME}.exe"
    fi

    info "Installing to ${install_dir}/${binary}..."

    # Check if we need sudo
    if [ -w "$install_dir" ]; then
        mv "$binary" "${install_dir}/"
        chmod +x "${install_dir}/${binary}"
    else
        warn "Requires sudo to install to ${install_dir}"
        sudo mv "$binary" "${install_dir}/"
        sudo chmod +x "${install_dir}/${binary}"
    fi

    info "Successfully installed ${BINARY_NAME} v${version} to ${install_dir}/${binary}"
}

# Configure API key if provided
configure_api_key() {
    local api_key="$1"

    if [ -z "$api_key" ]; then
        return
    fi

    info "Configuring API key..."

    # Use the CLI to set the API key in the config file
    if command -v promptconduit &> /dev/null; then
        promptconduit config set --api-key="$api_key"
        info "API key configured successfully!"
    else
        # Fallback: manually create config file if CLI not in PATH yet
        local config_dir="$HOME/.config/promptconduit"
        local config_file="$config_dir/config.json"

        mkdir -p "$config_dir"
        echo "{\"api_key\": \"$api_key\"}" > "$config_file"
        info "API key saved to $config_file"
    fi
}

# Run the init wizard after install when in an interactive terminal
run_init_wizard() {
    local api_key="$1"

    if [ ! -t 0 ] || [ ! -t 1 ]; then
        return
    fi

    if ! command -v promptconduit &> /dev/null; then
        return
    fi

    info "Starting setup wizard..."
    echo ""

    if [ -n "$api_key" ]; then
        promptconduit init --yes
    else
        promptconduit init
    fi
}

main() {
    local api_key="${1:-}"
    local version="${PROMPTCONDUIT_VERSION:-}"
    local install_dir="${PROMPTCONDUIT_INSTALL_DIR:-$DEFAULT_INSTALL_DIR}"

    echo ""
    echo "  PromptConduit CLI Installer"
    echo "  ============================"
    echo ""

    # Detect platform
    local platform
    platform=$(detect_platform)
    info "Detected platform: ${platform}"

    # Get version
    if [ -z "$version" ]; then
        info "Fetching latest version..."
        version=$(get_latest_version)
    fi
    info "Version: ${version}"

    # Create install directory if needed
    if [ ! -d "$install_dir" ]; then
        warn "Creating install directory: ${install_dir}"
        sudo mkdir -p "$install_dir"
    fi

    # Install
    install_binary "$version" "$platform" "$install_dir"

    # Configure API key if provided
    configure_api_key "$api_key"

    # Run init wizard if interactive terminal and CLI is available
    run_init_wizard "$api_key"

    echo ""
    info "Installation complete!"
    echo ""

    if [ -n "$api_key" ]; then
        echo "  Next steps:"
        echo "    1. Restart your AI tools to activate hooks"
        echo "    2. Check status:     promptconduit status"
        echo "    3. Test connection:  promptconduit test"
    else
        echo "  Next steps:"
        echo "    1. Run setup wizard: promptconduit init"
        echo "       (or local-only:  promptconduit init --local-only)"
        echo "    2. Check status:     promptconduit status"
    fi
    echo ""
}

main "$@"
